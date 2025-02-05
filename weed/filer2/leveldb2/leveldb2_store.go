package leveldb

import (
	"bytes"
	"context"
	"crypto/md5"
	"fmt"
	"io"
	"os"

	"github.com/chrislusf/seaweedfs/weed/filer2"
	"github.com/chrislusf/seaweedfs/weed/glog"
	weed_util "github.com/chrislusf/seaweedfs/weed/util"
	"github.com/syndtr/goleveldb/leveldb"
	"github.com/syndtr/goleveldb/leveldb/opt"
	leveldb_util "github.com/syndtr/goleveldb/leveldb/util"
)

func init() {
	filer2.Stores = append(filer2.Stores, &LevelDB2Store{})
}

type LevelDB2Store struct {
	dbs []*leveldb.DB
	dbCount int
}

func (store *LevelDB2Store) GetName() string {
	return "leveldb2"
}

func (store *LevelDB2Store) Initialize(configuration weed_util.Configuration) (err error) {
	dir := configuration.GetString("dir")
	return store.initialize(dir, 8)
}

func (store *LevelDB2Store) initialize(dir string, dbCount int) (err error) {
	glog.Infof("filer store leveldb2 dir: %s", dir)
	if err := weed_util.TestFolderWritable(dir); err != nil {
		return fmt.Errorf("Check Level Folder %s Writable: %s", dir, err)
	}

	opts := &opt.Options{
		BlockCacheCapacity:            32 * 1024 * 1024, // default value is 8MiB
		WriteBuffer:                   16 * 1024 * 1024, // default value is 4MiB
		CompactionTableSizeMultiplier: 4,
	}

	for d := 0 ; d < dbCount; d++ {
		dbFolder := fmt.Sprintf("%s/%02d", dir, d)
		os.MkdirAll(dbFolder, 0755)
		db, dbErr := leveldb.OpenFile(dbFolder, opts)
		if dbErr != nil {
			glog.Errorf("filer store open dir %s: %v", dbFolder, dbErr)
			return
		}
		store.dbs = append(store.dbs, db)
	}
	store.dbCount = dbCount

	return
}

func (store *LevelDB2Store) BeginTransaction(ctx context.Context) (context.Context, error) {
	return ctx, nil
}
func (store *LevelDB2Store) CommitTransaction(ctx context.Context) error {
	return nil
}
func (store *LevelDB2Store) RollbackTransaction(ctx context.Context) error {
	return nil
}

func (store *LevelDB2Store) InsertEntry(ctx context.Context, entry *filer2.Entry) (err error) {
	dir, name := entry.DirAndName()
	key, partitionId := genKey(dir, name, store.dbCount)

	value, err := entry.EncodeAttributesAndChunks()
	if err != nil {
		return fmt.Errorf("encoding %s %+v: %v", entry.FullPath, entry.Attr, err)
	}

	err = store.dbs[partitionId].Put(key, value, nil)

	if err != nil {
		return fmt.Errorf("persisting %s : %v", entry.FullPath, err)
	}

	// println("saved", entry.FullPath, "chunks", len(entry.Chunks))

	return nil
}

func (store *LevelDB2Store) UpdateEntry(ctx context.Context, entry *filer2.Entry) (err error) {

	return store.InsertEntry(ctx, entry)
}

func (store *LevelDB2Store) FindEntry(ctx context.Context, fullpath filer2.FullPath) (entry *filer2.Entry, err error) {
	dir, name := fullpath.DirAndName()
	key, partitionId := genKey(dir, name, store.dbCount)

	data, err := store.dbs[partitionId].Get(key, nil)

	if err == leveldb.ErrNotFound {
		return nil, filer2.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get %s : %v", entry.FullPath, err)
	}

	entry = &filer2.Entry{
		FullPath: fullpath,
	}
	err = entry.DecodeAttributesAndChunks(data)
	if err != nil {
		return entry, fmt.Errorf("decode %s : %v", entry.FullPath, err)
	}

	// println("read", entry.FullPath, "chunks", len(entry.Chunks), "data", len(data), string(data))

	return entry, nil
}

func (store *LevelDB2Store) DeleteEntry(ctx context.Context, fullpath filer2.FullPath) (err error) {
	dir, name := fullpath.DirAndName()
	key, partitionId := genKey(dir, name, store.dbCount)

	err = store.dbs[partitionId].Delete(key, nil)
	if err != nil {
		return fmt.Errorf("delete %s : %v", fullpath, err)
	}

	return nil
}

func (store *LevelDB2Store) ListDirectoryEntries(ctx context.Context, fullpath filer2.FullPath, startFileName string, inclusive bool,
	limit int) (entries []*filer2.Entry, err error) {

	directoryPrefix, partitionId := genDirectoryKeyPrefix(fullpath, "", store.dbCount)
	lastFileStart, _ := genDirectoryKeyPrefix(fullpath, startFileName, store.dbCount)

	iter := store.dbs[partitionId].NewIterator(&leveldb_util.Range{Start: lastFileStart}, nil)
	for iter.Next() {
		key := iter.Key()
		if !bytes.HasPrefix(key, directoryPrefix) {
			break
		}
		fileName := getNameFromKey(key)
		if fileName == "" {
			continue
		}
		if fileName == startFileName && !inclusive {
			continue
		}
		limit--
		if limit < 0 {
			break
		}
		entry := &filer2.Entry{
			FullPath: filer2.NewFullPath(string(fullpath), fileName),
		}

		// println("list", entry.FullPath, "chunks", len(entry.Chunks))

		if decodeErr := entry.DecodeAttributesAndChunks(iter.Value()); decodeErr != nil {
			err = decodeErr
			glog.V(0).Infof("list %s : %v", entry.FullPath, err)
			break
		}
		entries = append(entries, entry)
	}
	iter.Release()

	return entries, err
}

func genKey(dirPath, fileName string, dbCount int) (key []byte, partitionId int) {
	key, partitionId = hashToBytes(dirPath, dbCount)
	key = append(key, []byte(fileName)...)
	return key, partitionId
}

func genDirectoryKeyPrefix(fullpath filer2.FullPath, startFileName string, dbCount int) (keyPrefix []byte, partitionId int) {
	keyPrefix, partitionId = hashToBytes(string(fullpath), dbCount)
	if len(startFileName) > 0 {
		keyPrefix = append(keyPrefix, []byte(startFileName)...)
	}
	return keyPrefix, partitionId
}

func getNameFromKey(key []byte) string {

	return string(key[md5.Size:])

}

// hash directory, and use last byte for partitioning
func hashToBytes(dir string, dbCount int) ([]byte, int) {
	h := md5.New()
	io.WriteString(h, dir)

	b := h.Sum(nil)

	x := b[len(b)-1]

	return b, int(x)%dbCount
}
