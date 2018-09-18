// Copyright 2018 The Containerfs Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
// implied. See the License for the specific language governing
// permissions and limitations under the License.

package storage

import (
	"os"
	"time"

	"fmt"
	"io/ioutil"
	"strconv"

	"github.com/juju/errors"
	"github.com/tiglabs/containerfs/proto"
	"github.com/tiglabs/containerfs/util"
	"sync/atomic"
)

const (
	TinyChunkCount    = 10
	ChunkOpenOpt      = os.O_CREATE | os.O_RDWR | os.O_APPEND
	CompactThreshold  = 40
	CompactMaxWait    = time.Second * 10
	ReBootStoreMode   = false
	NewStoreMode      = true
	MinWriteAbleChunk = 1
	ObjectIdLen       = 8
)

// BlobStore is a store implement for tiny file storage which container 40 chunk files.
// This store will choose a available chunk file and append data to it.
type BlobStore struct {
	dataDir        string
	chunks         map[int]*Chunk
	availChunkCh   chan int
	unavailChunkCh chan int
	storeSize      int
	chunkSize      int
}

func NewBlobStore(dataDir string, storeSize int) (s *BlobStore, err error) {
	s = new(BlobStore)
	s.dataDir = dataDir
	if err = CheckAndCreateSubdir(dataDir); err != nil {
		return nil, fmt.Errorf("NewBlobStore [%v] err[%v]", dataDir, err)
	}
	s.chunks = make(map[int]*Chunk)
	if err = s.initChunkFile(); err != nil {
		return nil, fmt.Errorf("NewBlobStore [%v] err[%v]", dataDir, err)
	}

	s.availChunkCh = make(chan int, TinyChunkCount+1)
	s.unavailChunkCh = make(chan int, TinyChunkCount+1)
	for i := 1; i <= TinyChunkCount; i++ {
		s.unavailChunkCh <- i
	}
	s.storeSize = storeSize
	s.chunkSize = storeSize / TinyChunkCount

	return
}

func (s *BlobStore) DeleteStore() {
	for index, c := range s.chunks {
		c.file.Close()
		c.tree.idxFile.Close()
		delete(s.chunks, index)
	}
	os.RemoveAll(s.dataDir)
}

func (s *BlobStore) UseSize() (size int64) {
	// TODO: implement this
	return 0
}

func (s *BlobStore) initChunkFile() (err error) {
	for i := 1; i <= TinyChunkCount; i++ {
		var c *Chunk
		if c, err = NewChunk(s.dataDir, i); err != nil {
			return fmt.Errorf("initChunkFile Error %s", err.Error())
		}
		s.chunks[i] = c
	}

	return
}

func (s *BlobStore) chunkExist(chunkId uint32) (exist bool) {
	name := s.dataDir + "/" + strconv.Itoa(int(chunkId))
	if _, err := os.Stat(name); err == nil {
		exist = true
	}

	return
}

func (s *BlobStore) WriteDeleteDentry(objectId uint64, chunkId int, crc uint32) (err error) {
	var (
		fi os.FileInfo
	)
	c, ok := s.chunks[chunkId]
	if !ok {
		return ErrorFileNotFound
	}
	if !c.compactLock.TryLock() {
		return ErrorAgain
	}
	defer c.compactLock.Unlock()
	if fi, err = c.file.Stat(); err != nil {
		return
	}
	o := &Object{Oid: objectId, Size: TombstoneFileSize, Offset: uint32(fi.Size()), Crc: crc}
	if err = c.tree.appendToIdxFile(o); err == nil {
		if c.loadLastOid() < objectId {
			c.storeLastOid(objectId)
		}
	}

	return
}

func (s *BlobStore) Write(fileId uint32, objectId uint64, size int64, data []byte, crc uint32) (err error) {
	var (
		fi os.FileInfo
	)
	chunkId := int(fileId)
	c, ok := s.chunks[chunkId]
	if !ok {
		return ErrorFileNotFound
	}

	if !c.compactLock.TryLock() {
		return ErrorAgain
	}
	defer c.compactLock.Unlock()

	if objectId < c.loadLastOid() {
		msg := fmt.Sprintf("Object id smaller than last oid. DataDir[%v] FileId[%v]"+
			" ObjectId[%v] Size[%v]", s.dataDir, chunkId, objectId, c.loadLastOid())
		err = fmt.Errorf(msg)
		return ErrObjectSmaller
	}

	if fi, err = c.file.Stat(); err != nil {
		return
	}

	newOffset := fi.Size()
	if _, err = c.file.WriteAt(data[:size], newOffset); err != nil {
		return
	}

	if _, _, err = c.tree.set(objectId, uint32(newOffset), uint32(size), crc); err == nil {
		if c.loadLastOid() < objectId {
			c.storeLastOid(objectId)
		}
	}
	return
}

func (s *BlobStore) Read(fileId uint32, offset, size int64, nbuf []byte) (crc uint32, err error) {
	chunkId := int(fileId)
	objectId := uint64(offset)
	c, ok := s.chunks[chunkId]
	if !ok {
		return 0, ErrorFileNotFound
	}

	lastOid := c.loadLastOid()
	if lastOid < objectId {
		return 0, ErrorFileNotFound
	}

	c.commitLock.RLock()
	defer c.commitLock.RUnlock()

	var fi os.FileInfo
	if fi, err = c.file.Stat(); err != nil {
		return
	}

	o, ok := c.tree.get(objectId)
	if !ok {
		return 0, ErrorObjNotFound
	}

	if int64(o.Size) != size || int64(o.Offset)+size > fi.Size() {
		return 0, ErrorParamMismatch
	}

	if _, err = c.file.ReadAt(nbuf[:size], int64(o.Offset)); err != nil {
		return
	}
	crc = o.Crc

	return
}

func (s *BlobStore) Sync(fileId uint32) (err error) {
	chunkId := (int)(fileId)
	c, ok := s.chunks[chunkId]
	if !ok {
		return ErrorFileNotFound
	}

	err = c.tree.idxFile.Sync()
	if err != nil {
		return
	}

	return c.file.Sync()
}

func (s *BlobStore) GetAllWatermark() (chunks []*FileInfo, err error) {
	chunks = make([]*FileInfo, 0)
	for chunkId, c := range s.chunks {
		ci := &FileInfo{FileId: chunkId, Size: c.loadLastOid()}
		chunks = append(chunks, ci)
	}

	return
}

func (s *BlobStore) GetWatermark(fileId uint64) (chunkInfo *FileInfo, err error) {
	chunkId := (int)(fileId)
	c, ok := s.chunks[chunkId]
	if !ok {
		return nil, ErrorFileNotFound
	}
	chunkInfo = &FileInfo{FileId: chunkId, Size: c.loadLastOid()}

	return
}

func (s *BlobStore) GetAvailChunk() (chunkId int, err error) {
	select {
	case chunkId = <-s.availChunkCh:
	default:
		err = ErrorNoAvaliFile
	}

	return
}

func (s *BlobStore) GetChunkForWrite() (chunkId int, err error) {
	chLen := len(s.availChunkCh)
	for i := 0; i < chLen; i++ {
		select {
		case chunkId = <-s.availChunkCh:
			return chunkId, nil
		default:
			return -1, ErrorNoAvaliFile
		}
	}

	return
}

func (s *BlobStore) SyncAll() {
	for _, chunkFp := range s.chunks {
		chunkFp.tree.idxFile.Sync()
		chunkFp.file.Sync()
	}
}
func (s *BlobStore) CloseAll() {
	for _, chunkFp := range s.chunks {
		chunkFp.tree.idxFile.Close()
		chunkFp.file.Close()
	}
}

func (s *BlobStore) PutAvailChunk(chunkId int) {
	s.availChunkCh <- chunkId
}

func (s *BlobStore) GetUnAvailChunk() (chunkId int, err error) {
	select {
	case chunkId = <-s.unavailChunkCh:
	default:
		err = ErrorNoUnAvaliFile
	}

	return
}

func (s *BlobStore) PutUnAvailChunk(chunkId int) {
	s.unavailChunkCh <- chunkId
}

func (s *BlobStore) GetStoreChunkCount() (files int, err error) {
	return TinyChunkCount, nil
}

func (s *BlobStore) MarkDelete(fileId uint32, offset, size int64) error {
	chunkId := int(fileId)
	objectId := uint64(offset)
	c, ok := s.chunks[chunkId]
	if !ok {
		return ErrorFileNotFound
	}
	c.commitLock.RLock()
	defer c.commitLock.RUnlock()
	return c.tree.delete(objectId)
}

func (s *BlobStore) GetUnAvailChanLen() (chanLen int) {
	return len(s.unavailChunkCh)
}

func (s *BlobStore) GetAvailChanLen() (chanLen int) {
	return len(s.availChunkCh)
}

func (s *BlobStore) AllocObjectId(fileId uint32) (uint64, error) {
	chunkId := int(fileId)
	c, ok := s.chunks[chunkId]
	if !ok {
		return 0, ErrorFileNotFound //0 is an invalid object id
	}
	return c.loadLastOid() + 1, nil
}

func (s *BlobStore) GetLastOid(fileId uint32) (objectId uint64, err error) {
	c, ok := s.chunks[int(fileId)]
	if !ok {
		return 0, ErrorFileNotFound
	}

	return c.loadLastOid(), nil
}

func (s *BlobStore) GetObject(fileId uint32, objectId uint64) (o *Object, err error) {
	c, ok := s.chunks[int(fileId)]
	if !ok {
		return nil, ErrorFileNotFound
	}

	o, ok = c.tree.get(objectId)
	if !ok {
		return nil, ErrorObjNotFound
	}

	return
}

func (s *BlobStore) GetDelObjects(fileId uint32) (objects []uint64) {
	objects = make([]uint64, 0)
	c, ok := s.chunks[int(fileId)]
	if !ok {
		return
	}

	syncLastOid := c.loadLastOid()
	c.storeSyncLastOid(syncLastOid)

	c.commitLock.RLock()
	WalkIndexFile(c.tree.idxFile, func(oid uint64, offset, size, crc uint32) error {
		if oid > syncLastOid {
			return errors.New("Exceed syncLastOid")
		}
		if size == TombstoneFileSize {
			objects = append(objects, oid)
		}
		return nil
	})
	c.commitLock.RUnlock()

	return
}

func (s *BlobStore) ApplyDelObjects(chunkId uint32, objects []uint64) (err error) {
	c, ok := s.chunks[int(chunkId)]
	if !ok {
		return ErrorFileNotFound
	}
	err = c.applyDelObjects(objects)
	return
}

// make sure chunkID is valid
func (s *BlobStore) IsReadyToCompact(chunkID, thresh int) (isready bool, deletePercent float64) {
	if thresh < 0 {
		thresh = CompactThreshold
	}

	c := s.chunks[chunkID]
	objects := c.tree
	deletePercent = float64(objects.deleteBytes) / float64(objects.fileBytes)
	maxChunkSize := s.storeSize / TinyChunkCount
	if objects.fileBytes < uint64(maxChunkSize)*CompactThreshold/100 {
		return false, deletePercent
	}

	if objects.deleteBytes < objects.fileBytes*uint64(thresh)/100 {
		return false, deletePercent
	}

	return true, deletePercent
}

func (s *BlobStore) DoCompactWork(chunkID int) (err error, released uint64) {
	_, ok := s.chunks[chunkID]
	if !ok {
		return ErrorFileNotFound, 0
	}

	err, released = s.doCompactAndCommit(chunkID)
	if err != nil {
		return err, 0
	}
	err = s.Sync(uint32(chunkID))
	if err != nil {
		return err, 0
	}

	return nil, released
}

func (s *BlobStore) MoveChunkToUnavailChan() {
	if len(s.unavailChunkCh) >= 2 {
		return
	}
	for i := 0; i < 2; i++ {
		select {
		case chunkId := <-s.availChunkCh:
			s.unavailChunkCh <- chunkId
		default:
			return
		}
	}
}

func (s *BlobStore) doCompactAndCommit(chunkID int) (err error, released uint64) {
	cc := s.chunks[chunkID]
	// prevent write and delete operations
	if !cc.compactLock.TryLockTimed(CompactMaxWait) {
		return nil, 0
	}
	defer cc.compactLock.Unlock()

	sizeBeforeCompact := cc.tree.FileBytes()
	if err = cc.doCompact(); err != nil {
		return ErrorCompaction, 0
	}

	cc.commitLock.Lock()
	defer cc.commitLock.Unlock()

	err = cc.doCommit()
	if err != nil {
		return ErrorCommit, 0
	}

	sizeAfterCompact := cc.tree.FileBytes()
	released = sizeBeforeCompact - sizeAfterCompact
	return nil, released
}

func CheckAndCreateSubdir(name string) (err error) {
	return os.MkdirAll(name, 0755)
}

func (s *BlobStore) GetChunkInCore(fileID uint32) (*Chunk, error) {
	chunkID := (int)(fileID)
	cc, ok := s.chunks[chunkID]
	if !ok {
		return nil, ErrorFileNotFound
	}
	return cc, nil
}

func (s *BlobStore) Snapshot() ([]*proto.File, error) {
	fList, err := ioutil.ReadDir(s.dataDir)
	if err != nil {
		return nil, err
	}
	var (
		ccID int
	)
	files := make([]*proto.File, 0)
	for _, info := range fList {
		var cc *Chunk
		if ccID, err = strconv.Atoi(info.Name()); err != nil {
			continue
		}
		if ccID > TinyChunkCount {
			continue
		}
		if cc, err = s.GetChunkInCore(uint32(ccID)); err != nil {
			continue
		}

		crc, lastOid, vcCnt := cc.getCheckSum()
		f := &proto.File{Name: info.Name(), Crc: crc, Modified: info.ModTime().Unix(), MarkDel: false, LastObjID: lastOid, NeedleCnt: vcCnt}
		files = append(files, f)
	}

	return files, nil
}
