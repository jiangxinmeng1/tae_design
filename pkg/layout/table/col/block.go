package col

import (
	"sync"
)

const (
	NodeRows = 4096
)

//    P0    P1    P2
// |-----|-----|-----|

type PartNode struct {
	// common.ISLLNode
	Pos int
}

type Block struct {
	sync.RWMutex
	Nodes map[int]*PartNode
	Rows  int
}

func (block *Block) AddNode(pos int) bool {
	block.Lock()
	defer block.Unlock()
	_, ok := block.Nodes[pos]
	if ok {
		return false
	}
	block.Nodes[pos] = &PartNode{
		Pos: pos,
	}
	return true
}

func (block *Block) MaxPos() int {
	if block.Rows <= NodeRows {
		return 0
	}
	return (block.Rows - 1) / NodeRows
}

func (block *Block) HasChangeLocked() bool {
	return len(block.Nodes) != 0
}

func (block *Block) HasChange() bool {
	block.RLock()
	defer block.RUnlock()
	return len(block.Nodes) != 0
}

func (block *Block) NewIt() *BlockIt {
	return NewBlockIt(block)
}

type BaseNode struct {
	Host         *Block
	Start, Count int
}

func (bn *BaseNode) NextPos() int {
	return bn.Start + bn.Count
}

func NewBaseNode(host *Block, start, count int) *BaseNode {
	return &BaseNode{
		Host:  host,
		Start: start,
		Count: count,
	}
}

type BlockIt struct {
	Host        *Block
	VersionNode *PartNode
	BaseNode    *BaseNode
}

func NewBlockIt(block *Block) *BlockIt {
	it := &BlockIt{Host: block}
	block.RLock()
	defer block.RUnlock()
	if !block.HasChangeLocked() {
		it.BaseNode = NewBaseNode(block, 0, block.MaxPos()+1)
		return it
	}
	node := block.Nodes[0]
	if node != nil {
		it.VersionNode = node
		return it
	}
	pos := 1
	for pos <= block.MaxPos() {
		node = block.Nodes[pos]
		if node != nil {
			break
		}
		pos++
	}
	it.BaseNode = NewBaseNode(block, 0, pos)
	return it
}

func (it *BlockIt) Valid() bool {
	return it.BaseNode != nil || it.VersionNode != nil
}

func (it *BlockIt) Next() {
	it.Host.RLock()
	defer it.Host.RUnlock()
	var nextPos int
	if it.BaseNode != nil {
		nextPos = it.BaseNode.NextPos()
		it.BaseNode = nil
	} else {
		nextPos = it.VersionNode.Pos + 1
		it.VersionNode = nil
	}
	if nextPos > it.Host.MaxPos() {
		return
	}
	start := nextPos
	count := 0
	for nextPos <= it.Host.MaxPos() {
		node, ok := it.Host.Nodes[nextPos]

		if ok {
			if nextPos == start {
				it.VersionNode = node
			}
			break
		}
		count++
		nextPos++
	}
	if it.VersionNode != nil {
		return
	}
	it.BaseNode = NewBaseNode(it.Host, start, count)
}

func (it *BlockIt) Close() error {
	// TODO
	return nil
}
