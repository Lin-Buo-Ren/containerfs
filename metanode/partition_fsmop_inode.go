package metanode

import (
	"github.com/chubaoio/cbfs/proto"
	"github.com/chubaoio/cbfs/util/btree"
)

type ResponseInode struct {
	Status uint8
	Msg    *Inode
}

func NewResponseInode() *ResponseInode {
	return &ResponseInode{
		Msg: NewInode(0, 0),
	}
}

// CreateInode create inode to inode tree.
func (mp *metaPartition) createInode(ino *Inode) (status uint8) {
	status = proto.OpOk
	mp.inodeMu.Lock()
	defer mp.inodeMu.Unlock()
	if mp.inodeTree.Has(ino) {
		status = proto.OpExistErr
		return
	}
	mp.inodeTree.ReplaceOrInsert(ino)
	return
}

func (mp *metaPartition) createLinkInode(ino *Inode) (resp *ResponseInode) {
	resp = NewResponseInode()
	resp.Status = proto.OpOk
	mp.inodeMu.Lock()
	defer mp.inodeMu.Unlock()
	item := mp.inodeTree.Get(ino)
	if item == nil {
		resp.Status = proto.OpNotExistErr
		return
	}
	i := item.(*Inode)
	if i.Type == proto.ModeDir {
		resp.Status = proto.OpArgMismatchErr
		return
	}
	i.NLink++
	resp.Msg = i
	return
}

// GetInode query inode from InodeTree with specified inode info;
func (mp *metaPartition) getInode(ino *Inode) (resp *ResponseInode) {
	resp = NewResponseInode()
	resp.Status = proto.OpOk
	item := mp.inodeTree.Get(ino)
	if item == nil {
		resp.Status = proto.OpNotExistErr
		return
	}
	i := item.(*Inode)
	if i.MarkDelete == 1 {
		resp.Status = proto.OpNotExistErr
	}
	resp.Msg = i
	return
}

func (mp *metaPartition) hasInode(ino *Inode) (ok bool) {
	item := mp.inodeTree.Get(ino)
	if item == nil {
		ok = false
		return
	}
	i := item.(*Inode)
	if i.MarkDelete == 1 {
		ok = false
		return
	}
	ok = true
	return
}

func (mp *metaPartition) internalHasInode(ino *Inode) bool {
	return mp.inodeTree.Has(ino)
}

func (mp *metaPartition) getInodeTree() *btree.BTree {
	return mp.inodeTree
}

func (mp *metaPartition) RangeInode(f func(i btree.Item) bool) {
	mp.inodeTree.Ascend(f)
}

// DeleteInode delete specified inode item from inode tree.
func (mp *metaPartition) deleteInode(ino *Inode) (resp *ResponseInode) {
	resp = NewResponseInode()
	resp.Status = proto.OpOk
	mp.inodeMu.Lock()
	defer mp.inodeMu.Unlock()
	item := mp.inodeTree.Get(ino)
	if item == nil {
		resp.Status = proto.OpNotExistErr
		return
	}
	i := item.(*Inode)
	if i.Type == proto.ModeRegular {
		i.NLink--
		resp.Msg = i
		return
	}
	mp.inodeTree.Delete(ino)
	resp.Msg = i
	return
}

func (mp *metaPartition) internalDeleteInode(ino *Inode) {
	mp.inodeMu.Lock()
	mp.inodeTree.Delete(ino)
	mp.inodeMu.Unlock()
	return
}

func (mp *metaPartition) appendExtents(ino *Inode) (status uint8) {
	exts := ino.Extents
	status = proto.OpOk
	item := mp.inodeTree.Get(ino)
	if item == nil {
		status = proto.OpNotExistErr
		return
	}
	ino = item.(*Inode)
	if ino.MarkDelete == 1 {
		status = proto.OpNotExistErr
		return
	}
	modifyTime := ino.ModifyTime
	exts.Range(func(i int, ext proto.ExtentKey) bool {
		ino.AppendExtents(ext)
		return true
	})
	ino.ModifyTime = modifyTime
	ino.Generation++
	return
}

func (mp *metaPartition) extentsTruncate(ino *Inode) (resp *ResponseInode) {
	resp = NewResponseInode()
	resp.Status = proto.OpOk
	mp.inodeMu.Lock()
	defer mp.inodeMu.Unlock()
	item := mp.inodeTree.Get(ino)
	if item == nil {
		resp.Status = proto.OpNotExistErr
		return
	}
	i := item.(*Inode)
	if i.Type == proto.ModeDir {
		resp.Status = proto.OpArgMismatchErr
		return
	}
	if i.MarkDelete == 1 {
		resp.Status = proto.OpNotExistErr
		return
	}
	ino.Extents = i.Extents
	i.Size = 0
	i.ModifyTime = ino.ModifyTime
	i.Generation++
	i.Extents = proto.NewStreamKey(i.Inode)
	return
}

func (mp *metaPartition) evictInode(ino *Inode) (resp *ResponseInode) {
	resp = NewResponseInode()
	resp.Status = proto.OpOk
	mp.inodeMu.Lock()
	defer mp.inodeMu.Unlock()
	item := mp.inodeTree.Get(ino)
	if item == nil {
		resp.Status = proto.OpNotExistErr
		return
	}
	i := item.(*Inode)
	if i.Type == proto.ModeDir {
		if i.NLink < 2 {
			mp.inodeTree.Delete(ino)
		}
		return
	}
	if i.NLink < 1 {
		i.MarkDelete = 1
		// push to free list
		mp.freeList.Push(i)
	}
	return
}

func (mp *metaPartition) checkAndInsertFreeList(ino *Inode) {
	if ino.Type == proto.ModeDir {
		return
	}
	if ino.MarkDelete == 1 {
		mp.freeList.Push(ino)
	}
}
