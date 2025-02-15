package txnimpl

import (
	"sync"
	"tae/pkg/catalog"
	com "tae/pkg/common"
	"tae/pkg/iface/handle"
	"tae/pkg/iface/txnif"
	"tae/pkg/txn/txnbase"
)

type txnBlock struct {
	*txnbase.TxnBlock
	entry *catalog.BlockEntry
	store txnif.TxnStore
}

type blockIt struct {
	rwlock sync.RWMutex
	txn    txnif.AsyncTxn
	linkIt *com.LinkIt
	curr   *catalog.BlockEntry
}

func newBlockIt(txn txnif.AsyncTxn, meta *catalog.SegmentEntry) *blockIt {
	it := &blockIt{
		txn:    txn,
		linkIt: meta.MakeBlockIt(true),
	}
	if it.linkIt.Valid() {
		it.curr = it.linkIt.Get().GetPayload().(*catalog.BlockEntry)
	}
	return it
}

func (it *blockIt) Close() error { return nil }

func (it *blockIt) Valid() bool { return it.linkIt.Valid() }

func (it *blockIt) Next() {
	valid := true
	for {
		it.linkIt.Next()
		node := it.linkIt.Get()
		if node == nil {
			it.curr = nil
			break
		}
		entry := node.GetPayload().(*catalog.BlockEntry)
		entry.RLock()
		valid = entry.TxnCanRead(it.txn, entry.RWMutex)
		entry.RUnlock()
		if valid {
			it.curr = entry
			break
		}
	}
}

func (it *blockIt) GetBlock() handle.Block {
	return newBlock(it.txn, it.curr)
}

func newBlock(txn txnif.AsyncTxn, meta *catalog.BlockEntry) *txnBlock {
	blk := &txnBlock{
		TxnBlock: &txnbase.TxnBlock{
			Txn: txn,
		},
		entry: meta,
	}
	return blk
}

func (blk *txnBlock) GetMeta() interface{} { return blk.entry }
func (blk *txnBlock) String() string       { return blk.entry.String() }
func (blk *txnBlock) ID() uint64           { return blk.entry.GetID() }
