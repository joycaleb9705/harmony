package requestmanager

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/pkg/errors"
	"github.com/rs/zerolog"

	"github.com/ethereum/go-ethereum/event"
	"github.com/harmony-one/harmony/internal/utils"
	"github.com/harmony-one/harmony/p2p/stream/common/streammanager"
	sttypes "github.com/harmony-one/harmony/p2p/stream/types"
)

// requestManager implements RequestManager. It is responsible for matching response
// with requests.
// TODO: each peer is able to have a queue of requests instead of one request at a time.
// TODO: add QoS evaluation for each stream
type requestManager struct {
	streams   *sttypes.SafeMap[sttypes.StreamID, *stream]  // All streams
	available *sttypes.SafeMap[sttypes.StreamID, struct{}] // Streams that are available for request
	pendings  *sttypes.SafeMap[uint64, *request]           // requests that are sent but not received response
	waitings  requestQueues                                // double linked list of requests that are on the waiting list

	// Stream events
	sm         streammanager.Reader
	newStreamC <-chan streammanager.EvtStreamAdded
	rmStreamC  <-chan streammanager.EvtStreamRemoved
	// Request events
	cancelReqC  chan cancelReqData // request being canceled
	deliveryC   chan responseData
	newRequestC chan *request

	subs   []event.Subscription
	logger zerolog.Logger
	stopC  chan struct{}
	lock   sync.Mutex
}

// NewRequestManager creates a new request manager
func NewRequestManager(sm streammanager.ReaderSubscriber) RequestManager {
	return newRequestManager(sm)
}

func newRequestManager(sm streammanager.ReaderSubscriber) *requestManager {
	// subscribe at initialize to prevent misuse of upper function which might cause
	// the bootstrap peers are ignored
	newStreamC := make(chan streammanager.EvtStreamAdded)
	rmStreamC := make(chan streammanager.EvtStreamRemoved)
	sub1 := sm.SubscribeAddStreamEvent(newStreamC)
	sub2 := sm.SubscribeRemoveStreamEvent(rmStreamC)

	logger := utils.Logger().With().Str("module", "request manager").Logger()

	return &requestManager{
		streams:   sttypes.NewSafeMap[sttypes.StreamID, *stream](),
		available: sttypes.NewSafeMap[sttypes.StreamID, struct{}](),
		pendings:  sttypes.NewSafeMap[uint64, *request](),
		waitings:  newRequestQueues(),

		sm:          sm,
		newStreamC:  newStreamC,
		rmStreamC:   rmStreamC,
		cancelReqC:  make(chan cancelReqData, 16),
		deliveryC:   make(chan responseData, 128),
		newRequestC: make(chan *request, 128),

		subs:   []event.Subscription{sub1, sub2},
		logger: logger,
		stopC:  make(chan struct{}),
	}
}

func (rm *requestManager) Start() {
	go rm.loop()
}

func (rm *requestManager) Close() {
	rm.stopC <- struct{}{}
}

// DoRequest do the given request with a stream picked randomly. Return the response, stream id that
// is responsible for response, delivery and error.
func (rm *requestManager) DoRequest(ctx context.Context, raw sttypes.Request, options ...RequestOption) (sttypes.Response, sttypes.StreamID, error) {
	resp := <-rm.doRequestAsync(ctx, raw, options...)
	return resp.resp, resp.stID, resp.err
}

func (rm *requestManager) doRequestAsync(ctx context.Context, raw sttypes.Request, options ...RequestOption) <-chan responseData {
	req := &request{
		Request: raw,
		respC:   make(chan responseData, 1),
		doneC:   make(chan struct{}),
	}
	for _, opt := range options {
		opt(req)
	}
	rm.newRequestC <- req

	go func() {
		select {
		case <-ctx.Done(): // canceled or timeout in upper function calls
			rm.cancelReqC <- cancelReqData{
				req: req,
				err: ctx.Err(),
			}
		case <-req.doneC:
		}
	}()
	return req.respC
}

// DeliverResponse delivers the response to the corresponding request.
// The function behaves non-block
func (rm *requestManager) DeliverResponse(stID sttypes.StreamID, resp sttypes.Response) {
	sd := responseData{
		resp: resp,
		stID: stID,
	}
	go func() {
		select {
		case rm.deliveryC <- sd:
		case <-time.After(deliverTimeout):
			rm.logger.Error().Msg("WARNING: delivery timeout. Possible stuck in loop")
		}
	}()
}

func (rm *requestManager) loop() {
	var (
		throttleC = make(chan struct{}, 1) // throttle the waiting requests periodically
		ticker    = time.NewTicker(throttleInterval)
	)
	defer ticker.Stop()
	throttle := func() {
		select {
		case throttleC <- struct{}{}:
		default:
		}
	}

	for {
		select {
		case <-ticker.C:
			throttle()

		case <-throttleC:
		loop:
			for i := 0; i != throttleBatch; i++ {
				req, st := rm.getNextRequest()
				if req == nil {
					break loop
				}
				rm.logger.Debug().Str("request", req.String()).
					Msg("add new incoming request to pending queue")
				rm.addPendingRequest(req, st)
				b, err := req.Encode()
				if err != nil {
					rm.logger.Warn().Str("request", req.String()).Err(err).
						Msg("request encode error")
				}

				go func() {
					if err := st.WriteBytes(b); err != nil {
						rm.logger.Warn().Str("streamID", string(st.ID())).Err(err).
							Msg("write bytes")
						req.doneWithResponse(responseData{
							stID: st.ID(),
							err:  errors.Wrap(err, "write bytes"),
						})
					}
				}()
			}

		case req := <-rm.newRequestC:
			added := rm.handleNewRequest(req)
			if added {
				throttle()
			}

		case data := <-rm.deliveryC:
			rm.handleDeliverData(data)

		case data := <-rm.cancelReqC:
			rm.handleCancelRequest(data)

		case <-rm.newStreamC:
			rm.refreshStreams()

		case <-rm.rmStreamC:
			rm.refreshStreams()

		case <-rm.stopC:
			rm.logger.Info().Msg("request manager stopped")
			rm.close()
			return
		}
	}
}

func (rm *requestManager) handleNewRequest(req *request) bool {
	rm.logger.Debug().Str("request", req.String()).
		Msg("add new outgoing request to waiting queue")
	err := rm.addRequestToWaitings(req, reqPriorityLow)
	if err != nil {
		rm.logger.Warn().Err(err).Msg("failed to add new request to waitings")
		req.doneWithResponse(responseData{
			err: errors.Wrap(err, "failed to add new request to waitings"),
		})
		return false
	}
	return true
}

func (rm *requestManager) handleDeliverData(data responseData) {
	if err := rm.validateDelivery(data); err != nil {
		// if error happens in delivery, most likely it's a stale delivery. No action needed
		// and return
		rm.logger.Info().Err(err).Str("response", data.resp.String()).Msg("unable to validate deliver")
		return
	}
	// req and st is ensured not to be empty in validateDelivery
	req, _ := rm.pendings.Get(data.resp.ReqID())
	req.doneWithResponse(data)
	rm.removePendingRequest(req)
}

func (rm *requestManager) validateDelivery(data responseData) error {
	if data.err != nil {
		return data.err
	}
	st, _ := rm.streams.Get(data.stID)
	if st == nil {
		return fmt.Errorf("data delivered from dead stream: %v", data.stID)
	}
	req, _ := rm.pendings.Get(data.resp.ReqID())
	if req == nil {
		return fmt.Errorf("stale p2p response delivery")
	}
	if req.owner == nil || req.owner.ID() != data.stID {
		return fmt.Errorf("unexpected delivery stream")
	}
	if st.req == nil || st.req.ReqID() != data.resp.ReqID() {
		// Possible when request is canceled
		return fmt.Errorf("unexpected deliver request")
	}
	return nil
}

func (rm *requestManager) handleCancelRequest(data cancelReqData) {
	var (
		req = data.req
		err = data.err
	)
	rm.waitings.Remove(req)
	rm.removePendingRequest(req)
	var stid sttypes.StreamID
	if req.owner != nil {
		stid = req.owner.ID()
	}
	req.doneWithResponse(responseData{
		resp: nil,
		stID: stid,
		err:  err,
	})
}

func (rm *requestManager) getNextRequest() (*request, *stream) {
	var req *request
	for {
		req = rm.popRequestFromWaitings()
		if req == nil {
			return nil, nil
		}
		if !req.isDone() {
			break
		}
	}

	st, err := rm.pickAvailableStream(req)
	if err != nil {
		rm.logger.Debug().Err(err).Str("request", req.String()).Msg("Pick available streams.")
		rm.addRequestToWaitings(req, reqPriorityHigh)
		return nil, nil
	}
	return req, st
}

func (rm *requestManager) genReqID() uint64 {
	for {
		rid := sttypes.GenReqID()
		if _, ok := rm.pendings.Get(rid); !ok {
			return rid
		}
	}
}

func (rm *requestManager) addPendingRequest(req *request, st *stream) {
	reqID := rm.genReqID()
	req.SetReqID(reqID)

	req.owner = st
	st.req = req

	rm.available.Delete(st.ID())
	rm.pendings.Set(req.ReqID(), req)
}

func (rm *requestManager) removePendingRequest(req *request) {
	if _, ok := rm.pendings.Get(req.ReqID()); !ok {
		return
	}
	rm.pendings.Delete(req.ReqID())

	if st := req.owner; st != nil {
		st.clearPendingRequest()
		rm.available.Set(st.ID(), struct{}{})
	}
}

func (rm *requestManager) pickAvailableStream(req *request) (*stream, error) {
	availableStreamIDs := rm.available.Keys()
	for _, id := range availableStreamIDs {
		if !req.isStreamAllowed(id) {
			continue
		}
		st, ok := rm.streams.Get(id)
		if !ok {
			continue
		}
		if st.req != nil {
			continue
		}
		spec, _ := st.ProtoSpec()
		if req.Request.IsSupportedByProto(spec) {
			return st, nil
		}
	}
	return nil, errors.New("no more available streams")
}

func (rm *requestManager) refreshStreams() {
	added, removed := checkStreamUpdates(rm.streams, rm.sm.GetStreams())

	for _, st := range added {
		rm.logger.Info().Str("streamID", string(st.ID())).Msg("adding new stream")
		rm.addNewStream(st)
	}
	for _, st := range removed {
		rm.logger.Info().Str("streamID", string(st.ID())).Msg("removing stream")
		rm.removeStream(st)
	}
}

func checkStreamUpdates(exists *sttypes.SafeMap[sttypes.StreamID, *stream], targets []sttypes.Stream) (added []sttypes.Stream, removed []*stream) {
	targetM := make(map[sttypes.StreamID]sttypes.Stream)

	for _, target := range targets {
		id := target.ID()
		targetM[id] = target
		if _, ok := exists.Get(id); !ok {
			added = append(added, target)
		}
	}
	exists.Iterate(func(id sttypes.StreamID, st *stream) {
		if _, ok := targetM[id]; !ok {
			removed = append(removed, st)
		}
	})
	return
}

func (rm *requestManager) addNewStream(st sttypes.Stream) {
	rm.streams.Set(st.ID(), &stream{Stream: st})
	rm.available.Set(st.ID(), struct{}{})
}

// removeStream remove the stream from request manager, clear the pending request
// of the stream.
func (rm *requestManager) removeStream(st *stream) {
	id := st.ID()
	rm.available.Delete(id)
	rm.streams.Delete(id)

	cleared := st.clearPendingRequest()
	if cleared != nil {
		cleared.doneWithResponse(responseData{
			stID: id,
			err:  errors.New("stream removed when doing request"),
		})
	}
}

func (rm *requestManager) close() {
	for _, sub := range rm.subs {
		sub.Unsubscribe()
	}
	rm.pendings.Iterate(func(key uint64, req *request) {
		req.doneWithResponse(responseData{err: ErrClosed})
	})
	rm.streams = sttypes.NewSafeMap[sttypes.StreamID, *stream]()
	rm.available = sttypes.NewSafeMap[sttypes.StreamID, struct{}]()
	rm.pendings = sttypes.NewSafeMap[uint64, *request]()
	rm.waitings = newRequestQueues()
	close(rm.stopC)
}

type reqPriority int

const (
	reqPriorityLow reqPriority = iota
	reqPriorityHigh
)

func (rm *requestManager) addRequestToWaitings(req *request, priority reqPriority) error {
	return rm.waitings.Push(req, priority)
}

func (rm *requestManager) popRequestFromWaitings() *request {
	return rm.waitings.Pop()
}
