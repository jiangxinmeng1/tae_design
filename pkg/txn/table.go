package txn

import (
	"fmt"

	"github.com/matrixorigin/matrixone/pkg/container/batch"
	gbat "github.com/matrixorigin/matrixone/pkg/container/batch"
	"github.com/matrixorigin/matrixone/pkg/container/vector"
	gvec "github.com/matrixorigin/matrixone/pkg/container/vector"
	"github.com/matrixorigin/matrixone/pkg/vm/engine/aoe/storage/common"
	"github.com/matrixorigin/matrixone/pkg/vm/engine/aoe/storage/metadata/v1"
	"github.com/matrixorigin/matrixone/pkg/vm/engine/aoe/storage/mutation/buffer/base"
	"github.com/sirupsen/logrus"
)

type Table interface {
	GetSchema() *metadata.Schema
	GetID() uint64
	Append(data *batch.Batch) error
	RangeDeleteLocalRows(start, end uint32) error
	LocalDeletesToString() string
	IsLocalDeleted(row uint32) bool
	GetLocalPhysicalAxis(row uint32) (int, uint32)
	UpdateLocalValue(row uint32, col uint16, value interface{}) error
	Rows() uint32
	BatchDedupLocal(data *gbat.Batch) error
	BatchDedupLocalByCol(col *gvec.Vector) error
}

type txnTable struct {
	inodes     []InsertNode
	appendable InsertNode
	driver     NodeDriver
	id         uint64
	schema     *metadata.Schema
	nodesMgr   base.INodeManager
	index      TableIndex
	rows       uint32
}

func NewTable(id uint64, schema *metadata.Schema, driver NodeDriver, mgr base.INodeManager) *txnTable {
	tbl := &txnTable{
		inodes:   make([]InsertNode, 0),
		nodesMgr: mgr,
		id:       id,
		schema:   schema,
		driver:   driver,
		index:    NewSimpleTableIndex(),
	}
	return tbl
}

func (tbl *txnTable) GetSchema() *metadata.Schema {
	return tbl.schema
}

func (tbl *txnTable) GetID() uint64 {
	return tbl.id
}

func (tbl *txnTable) registerInsertNode() error {
	id := common.ID{
		TableID:   tbl.id,
		SegmentID: uint64(len(tbl.inodes)),
	}
	n := NewInsertNode(tbl, tbl.nodesMgr, id, tbl.driver)
	tbl.appendable = n
	tbl.inodes = append(tbl.inodes, n)
	return nil
}

func (tbl *txnTable) Append(data *batch.Batch) error {
	var err error
	if tbl.appendable == nil {
		if err = tbl.registerInsertNode(); err != nil {
			return err
		}
	}
	appended := uint32(0)
	offset := uint32(0)
	length := uint32(vector.Length(data.Vecs[0]))
	for {
		h := tbl.nodesMgr.Pin(tbl.appendable)
		if h == nil {
			panic("unexpected")
		}
		toAppend := tbl.appendable.PrepareAppend(data, offset)
		size := EstimateSize(data, offset, toAppend)
		logrus.Infof("Offset=%d, ToAppend=%d, EstimateSize=%d", offset, toAppend, size)
		err := tbl.appendable.Expand(size, func() error {
			appended, err = tbl.appendable.Append(data, offset)
			return err
		})
		if err != nil {
			logrus.Info(tbl.nodesMgr.String())
			logrus.Error(err)
			h.Close()
			break
		}
		space := tbl.appendable.GetSpace()
		logrus.Infof("Appended: %d, Space:%d", appended, space)
		start := tbl.rows
		if err = tbl.index.BatchInsert(data.Vecs[tbl.schema.PrimaryKey], int(offset), int(appended), start, false); err != nil {
			h.Close()
			break
		}
		h.Close()
		offset += appended
		tbl.rows += appended
		if space == 0 {
			if err = tbl.registerInsertNode(); err != nil {
				break
			}
		}
		if offset >= length {
			break
		}
	}
	return err
}

// 1. Split the interval to multiple intervals, with each interval belongs to only one insert node
// 2. For each new interval, call insert node RangeDelete
// 3. Update the table index
func (tbl *txnTable) RangeDeleteLocalRows(start, end uint32) error {
	first, firstOffset := tbl.GetLocalPhysicalAxis(start)
	last, lastOffset := tbl.GetLocalPhysicalAxis(end)
	var err error
	if last == first {
		node := tbl.inodes[first]
		err = node.RangeDelete(firstOffset, lastOffset)
	} else {
		node := tbl.inodes[first]
		err = node.RangeDelete(firstOffset, MaxNodeRows-1)
		node = tbl.inodes[last]
		err = node.RangeDelete(0, lastOffset)
		if last > first+1 {
			for i := first + 1; i < last; i++ {
				node = tbl.inodes[i]
				if err = node.RangeDelete(0, MaxNodeRows); err != nil {
					break
				}
			}
		}
	}
	return err
}

func (tbl *txnTable) LocalDeletesToString() string {
	s := fmt.Sprintf("<txnTable-%d>[LocalDeletes]:\n", tbl.id)
	for i, n := range tbl.inodes {
		s = fmt.Sprintf("%s\t<INode-%d>: %s\n", s, i, n.PrintDeletes())
	}
	return s
}

func (tbl *txnTable) IsLocalDeleted(row uint32) bool {
	npos, noffset := tbl.GetLocalPhysicalAxis(row)
	n := tbl.inodes[npos]
	return n.IsRowDeleted(noffset)
}

func (tbl *txnTable) GetLocalPhysicalAxis(row uint32) (int, uint32) {
	npos := int(row) / int(MaxNodeRows)
	noffset := row % uint32(MaxNodeRows)
	return npos, noffset
}

// 1. Get insert node and offset in node
// 2. Get row
// 3. Build a new row
// 4. Delete the row in the node
// 5. Append the new row
func (tbl *txnTable) UpdateLocalValue(row uint32, col uint16, value interface{}) error {
	npos, noffset := tbl.GetLocalPhysicalAxis(row)
	n := tbl.inodes[npos]
	window, err := n.Window(uint32(noffset), uint32(noffset))
	if err != nil {
		return err
	}
	if err = n.RangeDelete(uint32(noffset), uint32(noffset)); err != nil {
		return err
	}
	err = tbl.Append(window)
	return err
}

func (tbl *txnTable) Rows() uint32 {
	cnt := len(tbl.inodes)
	if cnt == 0 {
		return 0
	}
	return (uint32(cnt)-1)*MaxNodeRows + tbl.inodes[cnt-1].Rows()
}

func (tbl *txnTable) BatchDedupLocal(bat *gbat.Batch) error {
	return tbl.BatchDedupLocalByCol(bat.Vecs[tbl.schema.PrimaryKey])
}

func (tbl *txnTable) BatchDedupLocalByCol(col *gvec.Vector) error {
	return tbl.index.BatchDedup(col)
}

func (tbl *txnTable) GetLocalValue(row uint32, col uint16) (interface{}, error) {
	npos, noffset := tbl.GetLocalPhysicalAxis(row)
	n := tbl.inodes[npos]
	h := tbl.nodesMgr.Pin(n)
	defer h.Close()
	return n.GetValue(int(col), noffset)
}
