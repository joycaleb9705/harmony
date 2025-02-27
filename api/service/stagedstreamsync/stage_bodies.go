package stagedstreamsync

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/harmony-one/harmony/core"
	"github.com/harmony-one/harmony/core/types"
	"github.com/harmony-one/harmony/internal/utils"
	sttypes "github.com/harmony-one/harmony/p2p/stream/types"
	"github.com/ledgerwatch/erigon-lib/kv"
	"github.com/pkg/errors"
	"github.com/rs/zerolog"
)

type StageBodies struct {
	configs StageBodiesCfg
}

type StageBodiesCfg struct {
	bc                   core.BlockChain
	db                   kv.RwDB
	blockDBs             []kv.RwDB
	concurrency          int
	protocol             syncProtocol
	isBeaconShard        bool
	extractReceiptHashes bool
	logProgress          bool
	logger               zerolog.Logger
}

type blockTask struct {
	bns    []uint64
	hashes []common.Hash
}

func NewStageBodies(cfg StageBodiesCfg) *StageBodies {
	return &StageBodies{
		configs: cfg,
	}
}

func NewStageBodiesCfg(bc core.BlockChain, db kv.RwDB, blockDBs []kv.RwDB, concurrency int, protocol syncProtocol, isBeaconShard bool, extractReceiptHashes bool, logger zerolog.Logger, logProgress bool) StageBodiesCfg {
	return StageBodiesCfg{
		bc:                   bc,
		db:                   db,
		blockDBs:             blockDBs,
		concurrency:          concurrency,
		protocol:             protocol,
		isBeaconShard:        isBeaconShard,
		extractReceiptHashes: extractReceiptHashes,
		logger: logger.With().
			Str("stage", "StageBodies").
			Str("mode", "long range").
			Logger(),
		logProgress: logProgress,
	}
}

// Exec progresses Bodies stage in the forward direction
func (b *StageBodies) Exec(ctx context.Context, firstCycle bool, invalidBlockRevert bool, s *StageState, reverter Reverter, tx kv.RwTx) (err error) {

	useInternalTx := tx == nil

	// for short range sync, skip this stage
	if !s.state.initSync {
		return nil
	}

	// shouldn't execute for epoch chain
	if s.state.isEpochChain {
		return nil
	}

	if useInternalTx {
		var err error
		tx, err = b.configs.db.BeginRw(ctx)
		if err != nil {
			return err
		}
		defer tx.Rollback()
	}

	if invalidBlockRevert {
		return b.redownloadBadBlock(ctx, tx, s)
	}

	maxHeight := s.state.status.GetTargetBN()
	currentHead := s.state.CurrentBlockNumber()
	if currentHead >= maxHeight {
		return nil
	}
	currProgress := uint64(0)
	targetHeight := s.state.currentCycle.GetTargetHeight()

	if errV := CreateView(ctx, b.configs.db, tx, func(etx kv.Tx) error {
		if currProgress, err = s.CurrentStageProgress(etx); err != nil {
			return err
		}
		return nil
	}); errV != nil {
		return errV
	}

	if currProgress <= currentHead {
		if err := b.cleanAllBlockDBs(ctx); err != nil {
			return err
		}
		currProgress = currentHead
	}

	if currProgress >= targetHeight {
		return nil
	}

	startTime := time.Now()
	// startBlock := currProgress
	if b.configs.logProgress {
		fmt.Print("\033[s") // save the cursor position
	}

	// Fetch blocks from neighbors
	s.state.gbm = newDownloadManager(b.configs.bc, currProgress, targetHeight, BlocksPerRequest, s.state.logger)

	b.runDownloadLoop(ctx, tx, s.state.gbm, s, currProgress, startTime)

	if err := b.saveProgress(ctx, s, targetHeight, tx); err != nil {
		b.configs.logger.Error().
			Err(err).
			Uint64("targetHeight", targetHeight).
			Msg(WrapStagedSyncMsg("save progress failed"))
	}

	if useInternalTx {
		if err := tx.Commit(); err != nil {
			return err
		}
	}

	return nil
}

// runDownloadLoop fetches block hash batches and assigns them to workers dynamically
func (b *StageBodies) runDownloadLoop(ctx context.Context, tx kv.RwTx, gbm *downloadManager, s *StageState, startBlockNumber uint64, startTime time.Time) {
	var currentBlock uint64
	currentBlock = startBlockNumber
	concurrency := s.state.config.Concurrency
	batchChan := make(chan blockTask, concurrency) // Channel for batches
	var wg sync.WaitGroup
	// Start worker pool
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			for batch := range batchChan { // Workers continuously pick up batches
				if err := b.runBlockWorker(ctx, gbm, batch.bns, batch.hashes, workerID); err != nil {
					continue
				}
			}
		}(i)
	}

	defer func() {
		// select {
		// case <-ctx.Done():
		// 	return
		// case <-time.After(100 * time.Millisecond):
		// 	return
		// }
		close(batchChan) // Close channel after all batches are sent
		wg.Wait()        // Wait for all workers to complete
	}()

	// Fetch and send batches to workers
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		batch := gbm.GetNextBatch()
		if len(batch) == 0 { // No more batches to process
			return
		}

		hashes, err := b.fetchBlockHashes(ctx, tx, batch)
		if err != nil {
			utils.Logger().Error().
				Err(err).
				Interface("block numbers", batch).
				Msg(WrapStagedSyncMsg("fetchBlockHashes failed"))
			panic(ErrReadBlockHashesFromDBFailed)
		}

		if len(batch) != len(hashes) {
			utils.Logger().Error().
				Interface("block numbers", batch).
				Msg(WrapStagedSyncMsg("fetchBlockHashes failed: some hashes failed to retrieve"))
			panic(ErrReadBlockHashesFromDBFailed)
		}

		blockTask := blockTask{
			bns:    batch,
			hashes: hashes,
		}
		batchChan <- blockTask // Send batch to an available worker

		// Logging progress
		if b.configs.logProgress {
			lastBlockInBatch := batch[len(batch)-1]
			if lastBlockInBatch > currentBlock {
				currentBlock = lastBlockInBatch
			}
			//calculating block download speed
			dt := time.Since(startTime).Seconds()
			speed := float64(0)
			numBlocks := uint64(len(gbm.details))

			if dt > 0 {
				speed = float64(numBlocks) / dt
			}
			blockSpeed := fmt.Sprintf("%.2f", speed)

			fmt.Print("\033[u\033[K") // restore the cursor position and clear the line
			fmt.Println("downloaded blocks:", currentBlock, "/", int(gbm.targetBN), "(", blockSpeed, "blocks/s", ")")
		}
	}
}

// runBlockWorker downloads and processes a single batch of blocks
func (b *StageBodies) runBlockWorker(ctx context.Context,
	gbm *downloadManager,
	bns []uint64,
	hashes []common.Hash,
	workerID int) error {

	if len(hashes) == 0 {
		return errors.New("empty hashes")
	}

	blockBytes, sigBytes, stid, err := b.configs.protocol.GetRawBlocksByHashes(ctx, hashes)
	if err != nil {
		if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			b.configs.protocol.StreamFailed(stid, "downloadRawBlocks failed")
		}
		utils.Logger().Error().
			Err(err).
			Str("stream", string(stid)).
			Interface("block numbers", bns).
			Msg(WrapStagedSyncMsg("downloadRawBlocks failed"))
		err = errors.Wrap(err, "request error")
		gbm.HandleRequestError(bns, err, stid)
		return err
	} else if blockBytes == nil {
		utils.Logger().Warn().
			Str("stream", string(stid)).
			Interface("block numbers", bns).
			Msg(WrapStagedSyncMsg("downloadRawBlocks failed, received invalid (nil) blockBytes"))
		err := errors.New("downloadRawBlocks received invalid (nil) blockBytes")
		gbm.HandleRequestError(bns, err, stid)
		b.configs.protocol.StreamFailed(stid, "downloadRawBlocks failed")
		return err
	} else if len(blockBytes) == 0 {
		utils.Logger().Warn().
			Str("stream", string(stid)).
			Interface("block numbers", bns).
			Msg(WrapStagedSyncMsg("downloadRawBlocks failed, received empty blockBytes, remote peer is not fully synced"))
		err := errors.New("downloadRawBlocks received empty blockBytes")
		gbm.HandleRequestError(bns, err, stid)
		b.configs.protocol.RemoveStream(stid)
		return err
	} else if len(blockBytes) != len(bns) {
		utils.Logger().Warn().
			Str("stream", string(stid)).
			Interface("block numbers", bns).
			Msg(WrapStagedSyncMsg("downloadRawBlocks failed, received blockBytes length is not match with requested block numbers"))
		err := errors.New("downloadRawBlocks received blockBytes length is not match with requested block numbers")
		gbm.HandleRequestError(bns, err, stid)
		b.configs.protocol.RemoveStream(stid)
		return err
	} else {
		validBlocks := true
		for _, bb := range blockBytes {
			if len(bb) <= 1 {
				validBlocks = false
			}
		}
		if !validBlocks {
			utils.Logger().Warn().
				Str("stream", string(stid)).
				Interface("block numbers", bns).
				Msg(WrapStagedSyncMsg("downloadRawBlocks failed, some block Bytes are not valid"))
			err := errors.New("downloadRawBlocks received blockBytes are not valid")
			gbm.HandleRequestError(bns, err, stid)
			b.configs.protocol.RemoveStream(stid)
			return err
		}
		if err = b.saveBlocks(ctx, nil, bns, blockBytes, sigBytes, workerID, stid); err != nil {
			panic(ErrSaveBlocksToDbFailed)
		}
		gbm.HandleRequestResult(bns, blockBytes, sigBytes, workerID, stid)
		return nil
	}
}

func (b *StageBodies) verifyBlockAndExtractReceiptsData(batchBlockBytes [][]byte, batchSigBytes [][]byte, s *StageState) error {
	var block *types.Block
	for i := uint64(0); i < uint64(len(batchBlockBytes)); i++ {
		blockBytes := batchBlockBytes[i]
		sigBytes := batchSigBytes[i]
		if blockBytes == nil {
			continue
		}
		if err := rlp.DecodeBytes(blockBytes, &block); err != nil {
			b.configs.logger.Error().
				Uint64("block number", i).
				Msg("block size invalid")
			return ErrInvalidBlockBytes
		}
		if sigBytes != nil {
			block.SetCurrentCommitSig(sigBytes)
		}

		// if block.NumberU64() != i {
		// 	return ErrInvalidBlockNumber
		// }
		if err := verifyBlock(b.configs.bc, block); err != nil {
			return err
		}
	}
	return nil
}

// redownloadBadBlock tries to redownload the bad block from other streams
func (b *StageBodies) redownloadBadBlock(ctx context.Context, tx kv.RwTx, s *StageState) error {

	batch := []uint64{s.state.invalidBlock.Number}

badBlockDownloadLoop:

	for {
		if b.configs.protocol.NumStreams() == 0 {
			b.configs.logger.Error().
				Uint64("bad block number", s.state.invalidBlock.Number).
				Msg("[STAGED_STREAM_SYNC] not enough streams to re-download bad block")
			return errors.Errorf("not enough streams to re-download bad block")
		}
		blockBytes, sigBytes, stid, err := b.downloadRawBlocks(ctx, batch)
		if err != nil {
			if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
				b.configs.logger.Error().
					Uint64("bad block number", s.state.invalidBlock.Number).
					Msg("[STAGED_STREAM_SYNC] tried to re-download bad block from this stream, but downloadRawBlocks failed")
				b.configs.protocol.StreamFailed(stid, "tried to re-download bad block from this stream, but downloadRawBlocks failed")
			}
			continue
		}
		for _, id := range s.state.invalidBlock.StreamID {
			if id == stid {
				// TODO: if block is invalid then call StreamFailed
				b.configs.protocol.StreamFailed(stid, "re-download bad block from this stream failed")
				continue badBlockDownloadLoop
			}
		}
		s.state.gbm.SetDownloadDetails(batch, 0, stid)
		if errU := b.configs.blockDBs[0].Update(ctx, func(_tx kv.RwTx) error {
			if err = b.saveBlocks(ctx, tx, batch, blockBytes, sigBytes, 0, stid); err != nil {
				b.configs.logger.Error().
					Uint64("bad block number", s.state.invalidBlock.Number).
					Err(err).
					Msg("[STAGED_STREAM_SYNC] saving re-downloaded bad block to db failed")
				return errors.Errorf("%s: %s", ErrSaveBlocksToDbFailed.Error(), err.Error())
			}
			return nil
		}); errU != nil {
			continue
		}
		break
	}
	return nil
}

func (b *StageBodies) downloadBlocks(ctx context.Context, bns []uint64) ([]*types.Block, sttypes.StreamID, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	blocks, stid, err := b.configs.protocol.GetBlocksByNumber(ctx, bns)
	if err != nil {
		return nil, stid, err
	}
	if err := validateGetBlocksResult(bns, blocks); err != nil {
		return nil, stid, err
	}
	return blocks, stid, nil
}

// TODO: validate block results
func validateGetBlocksResult(requested []uint64, result []*types.Block) error {
	if len(result) != len(requested) {
		return fmt.Errorf("unexpected number of blocks delivered: %v / %v", len(result), len(requested))
	}
	for i, block := range result {
		if block != nil && block.NumberU64() != requested[i] {
			return fmt.Errorf("block with unexpected number delivered: %v / %v", block.NumberU64(), requested[i])
		}
	}
	return nil
}

func (b *StageBodies) downloadRawBlocks(ctx context.Context, bns []uint64) ([][]byte, [][]byte, sttypes.StreamID, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	return b.configs.protocol.GetRawBlocksByNumber(ctx, bns)
}

func (b *StageBodies) fetchBlockHashes(ctx context.Context, tx kv.RwTx, bns []uint64) ([]common.Hash, error) {
	if len(bns) == 0 {
		return nil, errors.New("empty batch of block numbers")
	}

	hashes := make([]common.Hash, 0, len(bns))

	err := CreateView(ctx, b.configs.db, tx, func(etx kv.Tx) error {
		for _, bn := range bns {

			blkKey := marshalData(bn)
			hashBytes, err := etx.GetOne(BlockHashesBucket, blkKey)
			if err != nil {
				utils.Logger().Error().
					Err(err).
					Uint64("block number", bn).
					Msg("[STAGED_STREAM_SYNC] fetching block hash from db failed")
				return err
			}
			var h common.Hash
			copy(h[:], hashBytes)
			hashes = append(hashes, h)
		}
		return nil
	})

	return hashes, err
}

func (b *StageBodies) downloadRawBlocksByHashes(ctx context.Context, tx kv.RwTx, bns []uint64) ([][]byte, [][]byte, sttypes.StreamID, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	if len(bns) == 0 {
		return nil, nil, "", errors.New("empty batch of block numbers")
	}

	tx, err := b.configs.db.BeginRw(ctx)
	if err != nil {
		return nil, nil, "", err
	}
	defer tx.Rollback()

	hashes := make([]common.Hash, 0, len(bns))

	if err := CreateView(ctx, b.configs.db, tx, func(etx kv.Tx) error {
		for _, bn := range bns {
			blkKey := marshalData(bn)
			hashBytes, err := etx.GetOne(BlockHashesBucket, blkKey)
			if err != nil {
				b.configs.logger.Error().
					Err(err).
					Uint64("block number", bn).
					Msg("[STAGED_STREAM_SYNC] fetching block hash from db failed")
				return err
			}
			var h common.Hash
			h.SetBytes(hashBytes)
			hashes = append(hashes, h)
		}
		return nil
	}); err != nil {
		return nil, nil, "", err
	}

	if err := tx.Commit(); err != nil {
		return nil, nil, "", err
	}

	// TODO: check the returned blocks are sorted
	return b.configs.protocol.GetRawBlocksByHashes(ctx, hashes)
}

// saveBlocks saves the blocks into db
func (b *StageBodies) saveBlocks(ctx context.Context, tx kv.RwTx, bns []uint64, blockBytes [][]byte, sigBytes [][]byte, workerID int, stid sttypes.StreamID) error {
	useInternalTx := tx == nil
	if useInternalTx {
		var err error
		tx, err = b.configs.blockDBs[workerID].BeginRw(ctx)
		if err != nil {
			return err
		}
		defer tx.Rollback()
	}
	// The blocks array is sorted by block number
	for i := uint64(0); i < uint64(len(blockBytes)); i++ {
		block := blockBytes[i]
		sig := sigBytes[i]
		if block == nil {
			continue
		}

		blkKey := marshalData(bns[i])

		if err := tx.Put(BlocksBucket, blkKey, block); err != nil {
			b.configs.logger.Error().
				Err(err).
				Uint64("block height", bns[i]).
				Msg("[STAGED_STREAM_SYNC] adding block to db failed")
			return err
		}
		// sigKey := []byte("s" + string(bns[i]))
		if err := tx.Put(BlockSignaturesBucket, blkKey, sig); err != nil {
			b.configs.logger.Error().
				Err(err).
				Uint64("block height", bns[i]).
				Msg("[STAGED_STREAM_SYNC] adding block sig to db failed")
			return err
		}
	}

	if useInternalTx {
		if err := tx.Commit(); err != nil {
			return err
		}
	}

	return nil
}

func (b *StageBodies) saveProgress(ctx context.Context, s *StageState, progress uint64, tx kv.RwTx) (err error) {
	useInternalTx := tx == nil
	if useInternalTx {
		var err error
		tx, err = b.configs.db.BeginRw(ctx)
		if err != nil {
			return err
		}
		defer tx.Rollback()
	}

	// save progress
	if err = s.Update(tx, progress); err != nil {
		b.configs.logger.Error().
			Err(err).
			Msgf("[STAGED_STREAM_SYNC] saving progress for block bodies stage failed")
		return ErrSavingBodiesProgressFail
	}

	if useInternalTx {
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

func (b *StageBodies) cleanBlocksDB(ctx context.Context, workerID int) (err error) {
	tx, errb := b.configs.blockDBs[workerID].BeginRw(ctx)
	if errb != nil {
		return errb
	}
	defer tx.Rollback()

	// clean block bodies db
	if err = tx.ClearBucket(BlocksBucket); err != nil {
		b.configs.logger.Error().
			Err(err).
			Msgf("[STAGED_STREAM_SYNC] clear blocks bucket after revert failed")
		return err
	}
	// clean block signatures db
	if err = tx.ClearBucket(BlockSignaturesBucket); err != nil {
		b.configs.logger.Error().
			Err(err).
			Msgf("[STAGED_STREAM_SYNC] clear block signatures bucket after revert failed")
		return err
	}

	if err = tx.Commit(); err != nil {
		return err
	}

	return nil
}

func (b *StageBodies) cleanAllBlockDBs(ctx context.Context) (err error) {
	//clean all blocks DBs
	for i := 0; i < b.configs.concurrency; i++ {
		if err := b.cleanBlocksDB(ctx, i); err != nil {
			return err
		}
	}
	return nil
}

func (b *StageBodies) Revert(ctx context.Context, firstCycle bool, u *RevertState, s *StageState, tx kv.RwTx) (err error) {

	//clean all blocks DBs
	if err := b.cleanAllBlockDBs(ctx); err != nil {
		return err
	}

	useInternalTx := tx == nil
	if useInternalTx {
		tx, err = b.configs.db.BeginRw(ctx)
		if err != nil {
			return err
		}
		defer tx.Rollback()
	}
	// save progress
	currentHead := s.state.CurrentBlockNumber()
	if err = s.Update(tx, currentHead); err != nil {
		b.configs.logger.Error().
			Err(err).
			Msgf("[STAGED_STREAM_SYNC] saving progress for block bodies stage after revert failed")
		return err
	}

	if err = u.Done(tx); err != nil {
		return err
	}

	if useInternalTx {
		if err = tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

func (b *StageBodies) CleanUp(ctx context.Context, firstCycle bool, p *CleanUpState, tx kv.RwTx) (err error) {
	//clean all blocks DBs
	if err := b.cleanAllBlockDBs(ctx); err != nil {
		return err
	}

	return nil
}
