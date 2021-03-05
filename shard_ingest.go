package badger

import (
	"github.com/pingcap/badger/protos"
	"github.com/pingcap/badger/table/memtable"
	"github.com/pingcap/badger/table/sstable"
	"github.com/pingcap/badger/y"
	"github.com/pingcap/errors"
	"os"
	"sort"
	"sync/atomic"
	"unsafe"
)

type IngestTree struct {
	ChangeSet *protos.ShardChangeSet
	MaxTS     uint64
	Delta     []*memtable.CFTable
	LocalPath string
}

func (sdb *ShardingDB) Ingest(ingestTree *IngestTree) error {
	if shd := sdb.GetShard(ingestTree.ChangeSet.ShardID); shd != nil {
		return errors.New("shard already exists")
	}
	sdb.orc.Lock()
	if sdb.orc.nextCommit < ingestTree.MaxTS {
		sdb.orc.nextCommit = ingestTree.MaxTS
	}
	sdb.orc.Unlock()
	defer sdb.orc.doneCommit(ingestTree.MaxTS)
	guard := sdb.resourceMgr.Acquire()
	defer guard.Done()

	err := sdb.loadFiles(ingestTree)
	if err != nil {
		return err
	}
	l0s, levelHandlers, err := sdb.createIngestTreeLevelHandlers(ingestTree)
	if err != nil {
		return err
	}

	shard := newShardForIngest(ingestTree.ChangeSet, sdb.opt, sdb.metrics)
	atomic.StorePointer(shard.memTbls, unsafe.Pointer(&shardingMemTables{tables: ingestTree.Delta}))
	atomic.StorePointer(shard.l0s, unsafe.Pointer(l0s))
	shard.foreachLevel(func(cf int, level *levelHandler) (stop bool) {
		scf := shard.cfs[cf]
		y.Assert(scf.casLevelHandler(level.level, level, levelHandlers[cf][level.level-1]))
		return false
	})
	// Ingest is manually triggered with meta change, so we don't need to notify meta listener.
	if err = sdb.manifest.writeChangeSet(ingestTree.ChangeSet, false); err != nil {
		return err
	}
	sdb.shardMap.Store(shard.ID, shard)
	return nil
}

func (sdb *ShardingDB) createIngestTreeLevelHandlers(ingestTree *IngestTree) (*shardL0Tables, [][]*levelHandler, error) {
	l0s := &shardL0Tables{}
	newHandlers := make([][]*levelHandler, sdb.numCFs)
	for cf := 0; cf < sdb.numCFs; cf++ {
		for l := 1; l <= ShardMaxLevel; l++ {
			newHandler := newLevelHandler(sdb.opt.NumLevelZeroTablesStall, l, sdb.metrics)
			newHandlers[cf] = append(newHandlers[cf], newHandler)
		}
	}
	snap := ingestTree.ChangeSet.Snapshot
	for _, l0Create := range snap.L0Creates {
		l0Tbl, err := openShardL0Table(sstable.NewFilename(l0Create.ID, sdb.opt.Dir), l0Create.ID)
		if err != nil {
			return nil, nil, err
		}
		l0s.tables = append(l0s.tables, l0Tbl)
	}
	for _, tblCreate := range snap.TableCreates {
		handler := newHandlers[tblCreate.CF][tblCreate.Level-1]
		filename := sstable.NewFilename(tblCreate.ID, sdb.opt.Dir)
		file, err := sstable.NewMMapFile(filename)
		if err != nil {
			return nil, nil, err
		}
		tbl, err := sstable.OpenTable(filename, file)
		if err != nil {
			return nil, nil, err
		}
		handler.tables = append(handler.tables, tbl)
	}
	sort.Slice(l0s.tables, func(i, j int) bool {
		return l0s.tables[i].commitTS > l0s.tables[j].commitTS
	})
	for cf := 0; cf < sdb.numCFs; cf++ {
		for l := 1; l <= ShardMaxLevel; l++ {
			handler := newHandlers[cf][l-1]
			sort.Slice(handler.tables, func(i, j int) bool {
				return handler.tables[i].Smallest().Compare(handler.tables[j].Smallest()) < 0
			})
		}
	}
	return l0s, newHandlers, nil
}

func (sdb *ShardingDB) IngestBuffer(shardID, shardVer uint64, buffer *memtable.CFTable) error {
	shard := sdb.GetShard(shardID)
	if shard == nil {
		return errShardNotFound
	}
	if shard.Ver != shardVer {
		return errShardNotMatch
	}
	memTbls := (*shardingMemTables)(atomic.LoadPointer(shard.memTbls))
	memTbls.tables = append([]*memtable.CFTable{buffer}, memTbls.tables...)
	return nil
}

func (sdb *ShardingDB) loadFiles(ingestTree *IngestTree) error {
	snap := ingestTree.ChangeSet.Snapshot
	for _, l0 := range snap.L0Creates {
		var err error
		if ingestTree.LocalPath != "" {
			err = sdb.loadFileFromLocalPath(ingestTree.LocalPath, l0.ID, true)
		} else {
			err = sdb.loadFileFromS3(l0.ID, true)
		}
		if err != nil {
			return err
		}
	}
	for _, tbl := range snap.TableCreates {
		var err error
		if ingestTree.LocalPath != "" {
			err = sdb.loadFileFromLocalPath(ingestTree.LocalPath, tbl.ID, false)
		} else {
			err = sdb.loadFileFromS3(tbl.ID, false)
		}

		if err != nil {
			return err
		}
	}
	return nil
}

func (sdb *ShardingDB) loadFileFromS3(id uint64, isL0 bool) error {
	localFileName := sstable.NewFilename(id, sdb.opt.Dir)
	_, err := os.Stat(localFileName)
	if err == nil {
		return nil
	}
	blockKey := sdb.s3c.BlockKey(id)
	tmpBlockFileName := localFileName + ".tmp"
	err = sdb.s3c.GetToFile(blockKey, tmpBlockFileName)
	if err != nil {
		return err
	}
	if !isL0 {
		idxKey := sdb.s3c.IndexKey(id)
		idxFileName := sstable.IndexFilename(localFileName)
		err = sdb.s3c.GetToFile(idxKey, idxFileName)
		if err != nil {
			return err
		}
	}
	return os.Rename(tmpBlockFileName, localFileName)
}

func (sdb *ShardingDB) loadFileFromLocalPath(localPath string, id uint64, isL0 bool) error {
	localFileName := sstable.NewFilename(id, localPath)
	dstFileName := sstable.NewFilename(id, sdb.opt.Dir)
	err := os.Link(localFileName, dstFileName)
	if err != nil {
		return err
	}
	if !isL0 {
		localIdxFileName := sstable.IndexFilename(localFileName)
		dstIdxFileName := sstable.IndexFilename(dstFileName)
		err = os.Link(localIdxFileName, dstIdxFileName)
	}
	return err
}
