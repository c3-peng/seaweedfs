package storage

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/chrislusf/seaweedfs/weed/storage/idx"
	"github.com/syndtr/goleveldb/leveldb/opt"

	"github.com/chrislusf/seaweedfs/weed/glog"
	"github.com/chrislusf/seaweedfs/weed/storage/needle_map"
	. "github.com/chrislusf/seaweedfs/weed/storage/types"
	"github.com/chrislusf/seaweedfs/weed/util"
	"github.com/syndtr/goleveldb/leveldb"
)

type LevelDbNeedleMap struct {
	baseNeedleMapper
	dbFileName string
	db         *leveldb.DB
}

func NewLevelDbNeedleMap(dbFileName string, indexFile *os.File, opts *opt.Options) (m *LevelDbNeedleMap, err error) {
	m = &LevelDbNeedleMap{dbFileName: dbFileName}
	m.indexFile = indexFile
	if !isLevelDbFresh(dbFileName, indexFile) {
		glog.V(0).Infof("Start to Generate %s from %s", dbFileName, indexFile.Name())
		generateLevelDbFile(dbFileName, indexFile)
		glog.V(0).Infof("Finished Generating %s from %s", dbFileName, indexFile.Name())
	}
	glog.V(1).Infof("Opening %s...", dbFileName)

	if m.db, err = leveldb.OpenFile(dbFileName, opts); err != nil {
		return
	}
	glog.V(1).Infof("Loading %s...", indexFile.Name())
	mm, indexLoadError := newNeedleMapMetricFromIndexFile(indexFile)
	if indexLoadError != nil {
		return nil, indexLoadError
	}
	m.mapMetric = *mm
	return
}

func isLevelDbFresh(dbFileName string, indexFile *os.File) bool {
	// normally we always write to index file first
	dbLogFile, err := os.Open(filepath.Join(dbFileName, "LOG"))
	if err != nil {
		return false
	}
	defer dbLogFile.Close()
	dbStat, dbStatErr := dbLogFile.Stat()
	indexStat, indexStatErr := indexFile.Stat()
	if dbStatErr != nil || indexStatErr != nil {
		glog.V(0).Infof("Can not stat file: %v and %v", dbStatErr, indexStatErr)
		return false
	}

	return dbStat.ModTime().After(indexStat.ModTime())
}

func generateLevelDbFile(dbFileName string, indexFile *os.File) error {
	db, err := leveldb.OpenFile(dbFileName, nil)
	if err != nil {
		return err
	}
	defer db.Close()
	return idx.WalkIndexFile(indexFile, func(key NeedleId, offset Offset, size uint32) error {
		if !offset.IsZero() && size != TombstoneFileSize {
			levelDbWrite(db, key, offset, size)
		} else {
			levelDbDelete(db, key)
		}
		return nil
	})
}

func (m *LevelDbNeedleMap) Get(key NeedleId) (element *needle_map.NeedleValue, ok bool) {
	bytes := make([]byte, NeedleIdSize)
	NeedleIdToBytes(bytes[0:NeedleIdSize], key)
	data, err := m.db.Get(bytes, nil)
	if err != nil || len(data) != OffsetSize+SizeSize {
		return nil, false
	}
	offset := BytesToOffset(data[0:OffsetSize])
	size := util.BytesToUint32(data[OffsetSize : OffsetSize+SizeSize])
	return &needle_map.NeedleValue{Key: key, Offset: offset, Size: size}, true
}

func (m *LevelDbNeedleMap) Put(key NeedleId, offset Offset, size uint32) error {
	var oldSize uint32
	if oldNeedle, ok := m.Get(key); ok {
		oldSize = oldNeedle.Size
	}
	m.logPut(key, oldSize, size)
	// write to index file first
	if err := m.appendToIndexFile(key, offset, size); err != nil {
		return fmt.Errorf("cannot write to indexfile %s: %v", m.indexFile.Name(), err)
	}
	return levelDbWrite(m.db, key, offset, size)
}

func levelDbWrite(db *leveldb.DB,
	key NeedleId, offset Offset, size uint32) error {

	bytes := make([]byte, NeedleIdSize+OffsetSize+SizeSize)
	NeedleIdToBytes(bytes[0:NeedleIdSize], key)
	OffsetToBytes(bytes[NeedleIdSize:NeedleIdSize+OffsetSize], offset)
	util.Uint32toBytes(bytes[NeedleIdSize+OffsetSize:NeedleIdSize+OffsetSize+SizeSize], size)

	if err := db.Put(bytes[0:NeedleIdSize], bytes[NeedleIdSize:NeedleIdSize+OffsetSize+SizeSize], nil); err != nil {
		return fmt.Errorf("failed to write leveldb: %v", err)
	}
	return nil
}
func levelDbDelete(db *leveldb.DB, key NeedleId) error {
	bytes := make([]byte, NeedleIdSize)
	NeedleIdToBytes(bytes, key)
	return db.Delete(bytes, nil)
}

func (m *LevelDbNeedleMap) Delete(key NeedleId, offset Offset) error {
	if oldNeedle, ok := m.Get(key); ok {
		m.logDelete(oldNeedle.Size)
	}
	// write to index file first
	if err := m.appendToIndexFile(key, offset, TombstoneFileSize); err != nil {
		return err
	}
	return levelDbDelete(m.db, key)
}

func (m *LevelDbNeedleMap) Close() {
	m.indexFile.Close()
	m.db.Close()
}

func (m *LevelDbNeedleMap) Destroy() error {
	m.Close()
	os.Remove(m.indexFile.Name())
	return os.RemoveAll(m.dbFileName)
}
