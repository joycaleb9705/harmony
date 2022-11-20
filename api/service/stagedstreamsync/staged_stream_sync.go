package stagedstreamsync

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/event"
	"github.com/harmony-one/harmony/core"
	"github.com/harmony-one/harmony/core/types"
	"github.com/harmony-one/harmony/internal/utils"
	syncproto "github.com/harmony-one/harmony/p2p/stream/protocols/sync"
	sttypes "github.com/harmony-one/harmony/p2p/stream/types"
	"github.com/ledgerwatch/erigon-lib/kv"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/rs/zerolog"
)

type StagedStreamSync struct {
	ctx        context.Context
	bc         core.BlockChain
	isBeacon   bool
	isExplorer bool
	db         kv.RwDB
	protocol   syncProtocol
	gbm        *getBlocksManager // initialized when finished get block number
	inserted   int
	config     Config
	logger     zerolog.Logger
	status     status //TODO: merge this with currentSyncCycle
	initSync   bool   // if sets to true, node start long range syncing
	UseMemDB   bool

	revertPoint     *uint64 // used to run stages
	prevRevertPoint *uint64 // used to get value from outside of staged sync after cycle (for example to notify RPCDaemon)
	invalidBlock    common.Hash
	currentStage    uint
	LogProgress     bool
	currentCycle    SyncCycle // current cycle
	stages          []*Stage
	revertOrder     []*Stage
	pruningOrder    []*Stage
	timings         []Timing
	logPrefixes     []string

	evtDownloadFinished           event.Feed // channel for each download task finished
	evtDownloadFinishedSubscribed bool
	evtDownloadStarted            event.Feed // channel for each download has started
	evtDownloadStartedSubscribed  bool
}

// BlockWithSig the serialization structure for request DownloaderRequest_BLOCKWITHSIG
// The block is encoded as block + commit signature
type BlockWithSig struct {
	Block              *types.Block
	CommitSigAndBitmap []byte
}

type Timing struct {
	isRevert  bool
	isCleanUp bool
	stage     SyncStageID
	took      time.Duration
}

type SyncCycle struct {
	Number       uint64
	TargetHeight uint64
	lock         sync.RWMutex
}

func (s *StagedStreamSync) Len() int                    { return len(s.stages) }
func (s *StagedStreamSync) Context() context.Context    { return s.ctx }
func (s *StagedStreamSync) Blockchain() core.BlockChain { return s.bc }
func (s *StagedStreamSync) DB() kv.RwDB                 { return s.db }
func (s *StagedStreamSync) IsBeacon() bool              { return s.isBeacon }
func (s *StagedStreamSync) IsExplorer() bool            { return s.isExplorer }
func (s *StagedStreamSync) LogPrefix() string {
	if s == nil {
		return ""
	}
	return s.logPrefixes[s.currentStage]
}
func (s *StagedStreamSync) PrevRevertPoint() *uint64 { return s.prevRevertPoint }

func (s *StagedStreamSync) NewRevertState(id SyncStageID, revertPoint, currentProgress uint64) *RevertState {
	return &RevertState{id, revertPoint, currentProgress, common.Hash{}, s}
}

func (s *StagedStreamSync) CleanUpStageState(id SyncStageID, forwardProgress uint64, tx kv.Tx, db kv.RwDB) (*CleanUpState, error) {
	var pruneProgress uint64
	var err error

	if errV := CreateView(context.Background(), db, tx, func(tx kv.Tx) error {
		pruneProgress, err = GetStageCleanUpProgress(tx, id, s.isBeacon)
		if err != nil {
			return err
		}
		return nil
	}); errV != nil {
		return nil, errV
	}

	return &CleanUpState{id, forwardProgress, pruneProgress, s}, nil
}

func (s *StagedStreamSync) NextStage() {
	if s == nil {
		return
	}
	s.currentStage++
}

// IsBefore returns true if stage1 goes before stage2 in staged sync
func (s *StagedStreamSync) IsBefore(stage1, stage2 SyncStageID) bool {
	idx1 := -1
	idx2 := -1
	for i, stage := range s.stages {
		if stage.ID == stage1 {
			idx1 = i
		}

		if stage.ID == stage2 {
			idx2 = i
		}
	}

	return idx1 < idx2
}

// IsAfter returns true if stage1 goes after stage2 in staged sync
func (s *StagedStreamSync) IsAfter(stage1, stage2 SyncStageID) bool {
	idx1 := -1
	idx2 := -1
	for i, stage := range s.stages {
		if stage.ID == stage1 {
			idx1 = i
		}

		if stage.ID == stage2 {
			idx2 = i
		}
	}

	return idx1 > idx2
}

func (s *StagedStreamSync) RevertTo(revertPoint uint64, invalidBlock common.Hash) {
	utils.Logger().Info().
		Interface("invalidBlock", invalidBlock).
		Uint64("revertPoint", revertPoint).
		Msgf("[STAGED_SYNC] Reverting blocks")
	s.revertPoint = &revertPoint
	s.invalidBlock = invalidBlock
}

func (s *StagedStreamSync) Done() {
	s.currentStage = uint(len(s.stages))
	s.revertPoint = nil
}

func (s *StagedStreamSync) IsDone() bool {
	return s.currentStage >= uint(len(s.stages)) && s.revertPoint == nil
}

func (s *StagedStreamSync) SetCurrentStage(id SyncStageID) error {
	for i, stage := range s.stages {
		if stage.ID == id {
			s.currentStage = uint(i)
			return nil
		}
	}
	utils.Logger().Error().
		Interface("stage id", id).
		Msgf("[STAGED_SYNC] stage not found")

	return ErrStageNotFound
}

func (s *StagedStreamSync) StageState(stage SyncStageID, tx kv.Tx, db kv.RwDB) (*StageState, error) {
	var blockNum uint64
	var err error
	if errV := CreateView(context.Background(), db, tx, func(rtx kv.Tx) error {
		blockNum, err = GetStageProgress(rtx, stage, s.isBeacon)
		if err != nil {
			return err
		}
		return nil
	}); errV != nil {
		return nil, errV
	}

	return &StageState{s, stage, blockNum}, nil
}

func (s *StagedStreamSync) cleanUp(fromStage int, db kv.RwDB, tx kv.RwTx, firstCycle bool) error {
	found := false
	for i := 0; i < len(s.pruningOrder); i++ {
		if s.pruningOrder[i].ID == s.stages[fromStage].ID {
			found = true
		}
		if !found || s.pruningOrder[i] == nil || s.pruningOrder[i].Disabled {
			continue
		}
		if err := s.pruneStage(firstCycle, s.pruningOrder[i], db, tx); err != nil {
			panic(err)
		}
	}
	return nil
}

func New(ctx context.Context,
	bc core.BlockChain,
	db kv.RwDB,
	stagesList []*Stage,
	isBeacon bool,
	protocol syncProtocol,
	useMemDB bool,
	config Config,
	logger zerolog.Logger,
) *StagedStreamSync {

	fmt.Println("NEW STREAM SYNC ---------------> shard id: ", bc.ShardID())

	revertStages := make([]*Stage, len(stagesList))
	for i, stageIndex := range DefaultRevertOrder {
		for _, s := range stagesList {
			if s.ID == stageIndex {
				revertStages[i] = s
				break
			}
		}
	}
	pruneStages := make([]*Stage, len(stagesList))
	for i, stageIndex := range DefaultCleanUpOrder {
		for _, s := range stagesList {
			if s.ID == stageIndex {
				pruneStages[i] = s
				break
			}
		}
	}

	logPrefixes := make([]string, len(stagesList))
	for i := range stagesList {
		logPrefixes[i] = fmt.Sprintf("%d/%d %s", i+1, len(stagesList), stagesList[i].ID)
	}

	status := newStatus()

	return &StagedStreamSync{
		ctx:          ctx,
		bc:           bc,
		isBeacon:     isBeacon,
		db:           db,
		protocol:     protocol,
		gbm:          nil,
		status:       status,
		inserted:     0,
		config:       config,
		logger:       logger,
		stages:       stagesList,
		currentStage: 0,
		revertOrder:  revertStages,
		pruningOrder: pruneStages,
		logPrefixes:  logPrefixes,
		UseMemDB:     useMemDB,
	}
}

func (s *StagedStreamSync) doGetCurrentNumberRequest() (uint64, sttypes.StreamID, error) {
	ctx, cancel := context.WithTimeout(s.ctx, 10*time.Second)
	defer cancel()

	bn, stid, err := s.protocol.GetCurrentBlockNumber(ctx, syncproto.WithHighPriority())
	if err != nil {
		return 0, stid, err
	}
	return bn, stid, nil
}

func (s *StagedStreamSync) promLabels() prometheus.Labels {
	sid := s.bc.ShardID()
	return prometheus.Labels{"ShardID": fmt.Sprintf("%d", sid)}
}

func (s *StagedStreamSync) checkHaveEnoughStreams() error {
	numStreams := s.protocol.NumStreams()
	if numStreams < s.config.MinStreams {
		return fmt.Errorf("number of streams smaller than minimum: %v < %v",
			numStreams, s.config.MinStreams)
	}
	return nil
}

func (s *StagedStreamSync) SetNewContext(ctx context.Context) error {
	for _, s := range s.stages {
		s.Handler.SetStageContext(ctx)
	}
	return nil
}

func (s *StagedStreamSync) Run(db kv.RwDB, tx kv.RwTx, firstCycle bool) error {
	s.prevRevertPoint = nil
	s.timings = s.timings[:0]

	for !s.IsDone() {
		var invalidBlockRevert bool
		if s.revertPoint != nil {
			for j := 0; j < len(s.revertOrder); j++ {
				if s.revertOrder[j] == nil || s.revertOrder[j].Disabled {
					continue
				}
				if err := s.revertStage(firstCycle, s.revertOrder[j], db, tx); err != nil {
					return err
				}
			}
			s.prevRevertPoint = s.revertPoint
			s.revertPoint = nil
			if s.invalidBlock != (common.Hash{}) {
				invalidBlockRevert = true
			}
			s.invalidBlock = common.Hash{}
			if err := s.SetCurrentStage(s.stages[0].ID); err != nil {
				return err
			}
			firstCycle = false
		}

		stage := s.stages[s.currentStage]

		if stage.Disabled {
			utils.Logger().Trace().
				Msgf("[STAGED_SYNC] %s disabled. %s", stage.ID, stage.DisabledDescription)

			s.NextStage()
			continue
		}

		if err := s.runStage(stage, db, tx, firstCycle, invalidBlockRevert); err != nil {
			return err
		}

		s.NextStage()
	}

	if err := s.cleanUp(0, db, tx, firstCycle); err != nil {
		return err
	}
	if err := s.SetCurrentStage(s.stages[0].ID); err != nil {
		return err
	}
	if err := printLogs(tx, s.timings); err != nil {
		return err
	}
	s.currentStage = 0
	return nil
}

func CreateView(ctx context.Context, db kv.RwDB, tx kv.Tx, f func(tx kv.Tx) error) error {
	if tx != nil {
		return f(tx)
	}
	return db.View(context.Background(), func(etx kv.Tx) error {
		return f(etx)
	})
}

func ByteCount(b uint64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%dB", b)
	}
	div, exp := uint64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%cB",
		float64(b)/float64(div), "KMGTPE"[exp])
}

func printLogs(tx kv.RwTx, timings []Timing) error {
	var logCtx []interface{}
	count := 0
	for i := range timings {
		if timings[i].took < 50*time.Millisecond {
			continue
		}
		count++
		if count == 50 {
			break
		}
		if timings[i].isRevert {
			logCtx = append(logCtx, "Revert "+string(timings[i].stage), timings[i].took.Truncate(time.Millisecond).String())
		} else if timings[i].isCleanUp {
			logCtx = append(logCtx, "CleanUp "+string(timings[i].stage), timings[i].took.Truncate(time.Millisecond).String())
		} else {
			logCtx = append(logCtx, string(timings[i].stage), timings[i].took.Truncate(time.Millisecond).String())
		}
	}
	if len(logCtx) > 0 {
		utils.Logger().Info().
			Msgf("[STAGED_SYNC] Timings (slower than 50ms) %v", logCtx...)
	}

	if tx == nil {
		return nil
	}

	if len(logCtx) > 0 { // also don't print this logs if everything is fast
		buckets := Buckets
		bucketSizes := make([]interface{}, 0, 2*len(buckets))
		for _, bucket := range buckets {
			sz, err1 := tx.BucketSize(bucket)
			if err1 != nil {
				return err1
			}
			bucketSizes = append(bucketSizes, bucket, ByteCount(sz))
		}
		utils.Logger().Info().
			Msgf("[STAGED_SYNC] Tables %v", bucketSizes...)
	}
	tx.CollectMetrics()
	return nil
}

func (s *StagedStreamSync) runStage(stage *Stage, db kv.RwDB, tx kv.RwTx, firstCycle bool, invalidBlockRevert bool) (err error) {
	start := time.Now()
	stageState, err := s.StageState(stage.ID, tx, db)
	if err != nil {
		return err
	}

	if err = stage.Handler.Exec(firstCycle, invalidBlockRevert, stageState, s, tx); err != nil {
		utils.Logger().Error().
			Err(err).
			Interface("stage id", stage.ID).
			Msgf("[STAGED_SYNC] stage failed")
		return fmt.Errorf("[%s] %w", s.LogPrefix(), err)
	}
	utils.Logger().Info().
		Msgf("[STAGED_SYNC] stage %s executed successfully", stage.ID)

	took := time.Since(start)
	if took > 60*time.Second {
		logPrefix := s.LogPrefix()
		utils.Logger().Info().
			Msgf("[STAGED_SYNC] [%s] DONE in %d", logPrefix, took)

	}
	s.timings = append(s.timings, Timing{stage: stage.ID, took: took})
	return nil
}

func (s *StagedStreamSync) revertStage(firstCycle bool, stage *Stage, db kv.RwDB, tx kv.RwTx) error {
	start := time.Now()
	utils.Logger().Trace().
		Msgf("[STAGED_SYNC] Revert... stage %s", stage.ID)
	stageState, err := s.StageState(stage.ID, tx, db)
	if err != nil {
		return err
	}

	revert := s.NewRevertState(stage.ID, *s.revertPoint, stageState.BlockNumber)
	revert.InvalidBlock = s.invalidBlock

	if stageState.BlockNumber <= revert.RevertPoint {
		return nil
	}

	if err = s.SetCurrentStage(stage.ID); err != nil {
		return err
	}

	err = stage.Handler.Revert(firstCycle, revert, stageState, tx)
	if err != nil {
		return fmt.Errorf("[%s] %w", s.LogPrefix(), err)
	}

	took := time.Since(start)
	if took > 60*time.Second {
		logPrefix := s.LogPrefix()
		utils.Logger().Info().
			Msgf("[STAGED_SYNC] [%s] Revert done in %d", logPrefix, took)
	}
	s.timings = append(s.timings, Timing{isRevert: true, stage: stage.ID, took: took})
	return nil
}

func (s *StagedStreamSync) pruneStage(firstCycle bool, stage *Stage, db kv.RwDB, tx kv.RwTx) error {
	start := time.Now()
	utils.Logger().Info().
		Msgf("[STAGED_SYNC] CleanUp... stage %s", stage.ID)

	stageState, err := s.StageState(stage.ID, tx, db)
	if err != nil {
		return err
	}

	prune, err := s.CleanUpStageState(stage.ID, stageState.BlockNumber, tx, db)
	if err != nil {
		return err
	}
	if err = s.SetCurrentStage(stage.ID); err != nil {
		return err
	}

	err = stage.Handler.CleanUp(firstCycle, prune, tx)
	if err != nil {
		return fmt.Errorf("[%s] %w", s.LogPrefix(), err)
	}

	took := time.Since(start)
	if took > 60*time.Second {
		logPrefix := s.LogPrefix()
		utils.Logger().Trace().
			Msgf("[STAGED_SYNC] [%s] CleanUp done in %d", logPrefix, took)

		utils.Logger().Info().
			Msgf("[STAGED_SYNC] [%s] CleanUp done in %d", logPrefix, took)
	}
	s.timings = append(s.timings, Timing{isCleanUp: true, stage: stage.ID, took: took})
	return nil
}

// DisableAllStages - including their reverts
func (s *StagedStreamSync) DisableAllStages() []SyncStageID {
	var backupEnabledIds []SyncStageID
	for i := range s.stages {
		if !s.stages[i].Disabled {
			backupEnabledIds = append(backupEnabledIds, s.stages[i].ID)
		}
	}
	for i := range s.stages {
		s.stages[i].Disabled = true
	}
	return backupEnabledIds
}

func (s *StagedStreamSync) DisableStages(ids ...SyncStageID) {
	for i := range s.stages {
		for _, id := range ids {
			if s.stages[i].ID != id {
				continue
			}
			s.stages[i].Disabled = true
		}
	}
}

func (s *StagedStreamSync) EnableStages(ids ...SyncStageID) {
	for i := range s.stages {
		for _, id := range ids {
			if s.stages[i].ID != id {
				continue
			}
			s.stages[i].Disabled = false
		}
	}
}

// GetActivePeerNumber returns the number of active peers
func (ss *StagedStreamSync) GetActiveStreams() int {
	//TODO: return active streams
	return 0
}
