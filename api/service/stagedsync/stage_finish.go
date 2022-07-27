package stagedsync

import (
	"context"

	"github.com/ledgerwatch/erigon-lib/kv"
)

type StageFinish struct {
	configs StageFinishCfg
}

type StageFinishCfg struct {
	ctx context.Context
	db  kv.RwDB
}

func NewStageFinish(cfg StageFinishCfg) *StageFinish {
	return &StageFinish{
		configs: cfg,
	}
}

func NewStageFinishCfg(ctx context.Context, db kv.RwDB) StageFinishCfg {
	return StageFinishCfg{
		ctx: ctx,
		db:  db,
	}
}

func (finish *StageFinish) Exec(firstCycle bool, badBlockUnwind bool, s *StageState, unwinder Unwinder, tx kv.RwTx) error {
	useExternalTx := tx != nil
	if !useExternalTx {
		var err error
		tx, err = finish.configs.db.BeginRw(context.Background())
		if err != nil {
			return err
		}
		defer tx.Rollback()
	}

	//TODO: manage this for Turbo Mode
	hashesBucketName := GetBucketName(BlockHashesBucket, s.state.isBeacon)
	tx.ClearBucket(hashesBucketName)
	extrahashesBucketName := GetBucketName(ExtraBlockHashesBucket, s.state.isBeacon)
	tx.ClearBucket(extrahashesBucketName)
	blocksBucketName := GetBucketName(DownloadedBlocksBucket, s.state.isBeacon)
	tx.ClearBucket(blocksBucketName)

	// clean up cache
	s.state.purgeAllBlocksFromCache()

	if !useExternalTx {
		if err := tx.Commit(); err != nil {
			return err
		}
	}

	return nil
}

func (bh *StageBlockHashes) clearBucket(tx kv.RwTx, isBeacon bool) error {
	useExternalTx := tx != nil
	if !useExternalTx {
		var err error
		tx, err = bh.configs.db.BeginRw(context.Background())
		if err != nil {
			return err
		}
		defer tx.Rollback()
	}
	bucketName := GetBucketName(BlockHashesBucket, isBeacon)
	if err := tx.ClearBucket(bucketName); err != nil {
		return err
	}

	if !useExternalTx {
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

func (finish *StageFinish) Unwind(firstCycle bool, u *UnwindState, s *StageState, tx kv.RwTx) (err error) {
	useExternalTx := tx != nil
	if !useExternalTx {
		tx, err = finish.configs.db.BeginRw(finish.configs.ctx)
		if err != nil {
			return err
		}
		defer tx.Rollback()
	}

	if err = u.Done(tx); err != nil {
		return err
	}
	if !useExternalTx {
		if err = tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

func (finish *StageFinish) Prune(firstCycle bool, p *PruneState, tx kv.RwTx) (err error) {
	useExternalTx := tx != nil
	if !useExternalTx {
		tx, err = finish.configs.db.BeginRw(finish.configs.ctx)
		if err != nil {
			return err
		}
		defer tx.Rollback()
	}

	if !useExternalTx {
		if err = tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}
