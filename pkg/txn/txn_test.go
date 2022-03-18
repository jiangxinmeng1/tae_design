package txn

import (
	"math/rand"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gvec "github.com/matrixorigin/matrixone/pkg/container/vector"
	"github.com/matrixorigin/matrixone/pkg/vm/engine/aoe/storage/common"
	"github.com/matrixorigin/matrixone/pkg/vm/engine/aoe/storage/metadata/v1"
	"github.com/matrixorigin/matrixone/pkg/vm/engine/aoe/storage/mock"
	"github.com/matrixorigin/matrixone/pkg/vm/engine/aoe/storage/mutation/buffer"
	"github.com/panjf2000/ants/v2"
	"github.com/stretchr/testify/assert"
)

// 1. 30 concurrency
// 2. 10000 node
// 3. 512K buffer
// 4. 1K(30%), 4K(25%), 8K(%20), 16K(%15), 32K(%10)

func init() {
	rand.Seed(time.Now().UnixNano())
}

func getNodes() int {
	v := rand.Intn(100)
	if v < 30 {
		return 1 * 2
	} else if v < 55 {
		return 2 * 2
	} else if v < 75 {
		return 3 * 2
	} else if v < 90 {
		return 4 * 2
	}
	return 5 * 2
}

func initTestPath(t *testing.T) string {
	dir := filepath.Join("/tmp", t.Name())
	os.RemoveAll(dir)
	return dir
}

func makeTable(t *testing.T, dir string, colCnt int, bufSize uint64) *txnTable {
	mgr := buffer.NewNodeManager(bufSize, nil)
	driver := NewNodeDriver(dir, "store", nil)
	id := common.NextGlobalSeqNum()
	schema := metadata.MockSchemaAll(colCnt)
	return NewTable(id, schema, driver, mgr)
}

func TestInsertNode(t *testing.T) {
	dir := initTestPath(t)
	tbl := makeTable(t, dir, 1, common.K*6)
	defer tbl.driver.Close()
	bat := mock.MockBatch(tbl.GetSchema().Types(), 1024)
	p, _ := ants.NewPool(5)

	var wg sync.WaitGroup
	var all uint64

	worker := func(id uint64) func() {
		return func() {
			defer wg.Done()
			cnt := getNodes()
			nodes := make([]*insertNode, cnt)
			for i := 0; i < cnt; i++ {
				var cid common.ID
				cid.BlockID = id
				cid.Idx = uint16(i)
				n := NewInsertNode(tbl, tbl.nodesMgr, cid, tbl.driver)
				nodes[i] = n
				h := tbl.nodesMgr.Pin(n)
				var err error
				if err = n.Expand(common.K*1, func() error {
					n.Append(bat, 0)
					return nil
				}); err != nil {
					err = n.Expand(common.K*1, func() error {
						n.Append(bat, 0)
						return nil
					})
				}
				if err != nil {
					assert.NotNil(t, err)
				}
				h.Close()
			}
			for _, n := range nodes {
				n.ToTransient()
				n.Close()
			}
			atomic.AddUint64(&all, uint64(len(nodes)))
		}
	}
	idAlloc := common.NewIdAlloctor(1)
	for {
		id := idAlloc.Alloc()
		if id > 10 {
			break
		}
		wg.Add(1)
		p.Submit(worker(id))
	}
	wg.Wait()
	t.Log(all)
	t.Log(tbl.nodesMgr.String())
	t.Log(common.GPool.String())
}

func TestTable(t *testing.T) {
	dir := initTestPath(t)
	tbl := makeTable(t, dir, 1, common.K*20)
	defer tbl.driver.Close()

	bat := mock.MockBatch(tbl.GetSchema().Types(), 1024)
	for i := 0; i < 100; i++ {
		err := tbl.Append(bat)
		assert.Nil(t, err)
	}
	t.Log(tbl.nodesMgr.String())
	tbl.RangeDeleteLocalRows(1024+20, 1024+30)
	tbl.RangeDeleteLocalRows(1024*2+38, 1024*2+40)
	t.Log(t, tbl.LocalDeletesToString())
	assert.True(t, tbl.IsLocalDeleted(1024+20))
	assert.True(t, tbl.IsLocalDeleted(1024+30))
	assert.False(t, tbl.IsLocalDeleted(1024+19))
	assert.False(t, tbl.IsLocalDeleted(1024+31))
}

func TestUpdate(t *testing.T) {
	dir := initTestPath(t)
	tbl := makeTable(t, dir, 2, common.K*10)
	defer tbl.driver.Close()
	tbl.GetSchema().PrimaryKey = 1
	bat := mock.MockBatch(tbl.GetSchema().Types(), 1024)

	bats := SplitBatch(bat, 2)

	for _, b := range bats {
		err := tbl.BatchDedupLocal(b)
		assert.Nil(t, err)
		err = tbl.Append(b)
		assert.Nil(t, err)
	}

	row := uint32(999)
	assert.False(t, tbl.IsLocalDeleted(row))
	rows := tbl.Rows()
	err := tbl.UpdateLocalValue(row, 0, 999)
	assert.Nil(t, err)
	assert.True(t, tbl.IsLocalDeleted(row))
	assert.Equal(t, rows+1, tbl.Rows())
}

func TestAppend(t *testing.T) {
	dir := initTestPath(t)
	tbl := makeTable(t, dir, 2, common.K*20)
	defer tbl.driver.Close()

	tbl.GetSchema().PrimaryKey = 1

	rows := uint64(MaxNodeRows) / 8 * 3
	brows := rows / 3
	bat := mock.MockBatch(tbl.GetSchema().Types(), rows)

	bats := SplitBatch(bat, 3)

	err := tbl.BatchDedupLocal(bats[0])
	assert.Nil(t, err)
	err = tbl.Append(bats[0])
	assert.Nil(t, err)
	assert.Equal(t, int(brows), int(tbl.Rows()))
	assert.Equal(t, int(brows), int(tbl.index.Count()))

	err = tbl.BatchDedupLocal(bats[0])
	assert.NotNil(t, err)

	err = tbl.BatchDedupLocal(bats[1])
	assert.Nil(t, err)
	err = tbl.Append(bats[1])
	assert.Nil(t, err)
	assert.Equal(t, 2*int(brows), int(tbl.Rows()))
	assert.Equal(t, 2*int(brows), int(tbl.index.Count()))

	err = tbl.BatchDedupLocal(bats[2])
	assert.Nil(t, err)
	err = tbl.Append(bats[2])
	assert.Nil(t, err)
	assert.Equal(t, 3*int(brows), int(tbl.Rows()))
	assert.Equal(t, 3*int(brows), int(tbl.index.Count()))
}

func TestIndex(t *testing.T) {
	index := NewSimpleTableIndex()
	err := index.Insert(1, 10)
	assert.Nil(t, err)
	err = index.Insert("one", 10)
	assert.Nil(t, err)
	row, err := index.Find("one")
	assert.Nil(t, err)
	assert.Equal(t, 10, int(row))
	err = index.Delete("one")
	assert.Nil(t, err)
	_, err = index.Find("one")
	assert.NotNil(t, err)

	schema := metadata.MockSchemaAll(14)
	bat := mock.MockBatch(schema.Types(), 500)

	idx := NewSimpleTableIndex()
	err = idx.BatchDedup(bat.Vecs[0])
	assert.Nil(t, err)
	err = idx.BatchInsert(bat.Vecs[0], 0, gvec.Length(bat.Vecs[0]), 0, true)
	assert.NotNil(t, err)

	err = idx.BatchDedup(bat.Vecs[1])
	assert.Nil(t, err)
	err = idx.BatchInsert(bat.Vecs[1], 0, gvec.Length(bat.Vecs[1]), 0, true)
	assert.Nil(t, err)

	window := gvec.New(bat.Vecs[1].Typ)
	gvec.Window(bat.Vecs[1], 20, 22, window)
	assert.Equal(t, 2, gvec.Length(window))
	err = idx.BatchDedup(window)
	assert.NotNil(t, err)

	idx = NewSimpleTableIndex()
	err = idx.BatchDedup(bat.Vecs[12])
	assert.Nil(t, err)
	err = idx.BatchInsert(bat.Vecs[12], 0, gvec.Length(bat.Vecs[12]), 0, true)
	assert.Nil(t, err)

	window = gvec.New(bat.Vecs[12].Typ)
	gvec.Window(bat.Vecs[12], 20, 22, window)
	assert.Equal(t, 2, gvec.Length(window))
	err = idx.BatchDedup(window)
	assert.NotNil(t, err)
}

func TestLoad(t *testing.T) {
	dir := initTestPath(t)
	tbl := makeTable(t, dir, 14, common.M*1500)
	defer tbl.driver.Close()
	tbl.GetSchema().PrimaryKey = 13

	bat := mock.MockBatch(tbl.GetSchema().Types(), 200000)
	bats := SplitBatch(bat, 5)
	// for _, b := range bats {
	// 	tbl.Append(b)
	// }
	// t.Log(tbl.Rows())
	// t.Log(len(tbl.inodes))

	err := tbl.Append(bats[0])
	assert.Nil(t, err)

	t.Log(tbl.nodesMgr.String())
	v, err := tbl.GetLocalValue(100, 0)
	assert.Nil(t, err)
	t.Log(tbl.nodesMgr.String())
	t.Logf("Row %d, Col %d, Val %v", 100, 0, v)
}
