package catalog

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"time"

	"github.com/matrixorigin/matrixone/pkg/container/types"
)

type IndexT uint16

const (
	ZoneMap IndexT = iota
)

type IndexInfo struct {
	Id      uint64
	Name    string
	Type    IndexT
	Columns []uint16
}

func NewIndexInfo(name string, typ IndexT, colIdx ...int) *IndexInfo {
	index := &IndexInfo{
		Name:    name,
		Type:    typ,
		Columns: make([]uint16, 0),
	}
	for _, col := range colIdx {
		index.Columns = append(index.Columns, uint16(col))
	}
	return index
}

type ColDef struct {
	Name string
	Idx  int
	Type types.Type
}

type Schema struct {
	Name             string         `json:"name"`
	ColDefs          []*ColDef      `json:"cols"`
	NameIndex        map[string]int `json:"nindex"`
	BlockMaxRows     uint64         `json:"blkrows"`
	PrimaryKey       int            `json:"primarykey"`
	SegmentMaxBlocks uint64         `json:"segblocks"`
}

func NewEmptySchema(name string) *Schema {
	return &Schema{
		Name:      name,
		ColDefs:   make([]*ColDef, 0),
		NameIndex: make(map[string]int),
	}
}

func (s *Schema) AppendCol(name string, typ types.Type) {
	colDef := &ColDef{
		Name: name,
		Type: typ,
		Idx:  len(s.ColDefs),
	}
	s.ColDefs = append(s.ColDefs, colDef)
	s.NameIndex[name] = colDef.Idx
}

func (s *Schema) String() string {
	buf, _ := json.Marshal(s)
	return string(buf)
}

func (s *Schema) Types() []types.Type {
	ts := make([]types.Type, len(s.ColDefs))
	for i, colDef := range s.ColDefs {
		ts[i] = colDef.Type
	}
	return ts
}

func (s *Schema) Valid() bool {
	if s == nil {
		return false
	}
	if len(s.ColDefs) == 0 {
		return false
	}

	names := make(map[string]bool)
	for idx, colDef := range s.ColDefs {
		if idx != colDef.Idx {
			return false
		}
		_, ok := names[colDef.Name]
		if ok {
			return false
		}
		names[colDef.Name] = true
	}
	return true
}

// GetColIdx returns column index for the given column name
// if found, otherwise returns -1.
func (s *Schema) GetColIdx(attr string) int {
	idx, ok := s.NameIndex[attr]
	if !ok {
		return -1
	}
	return idx
}

func MockSchema(colCnt int) *Schema {
	rand.Seed(time.Now().UnixNano())
	schema := NewEmptySchema(fmt.Sprintf("%d", rand.Intn(1000000)))
	prefix := "mock_"
	for i := 0; i < colCnt; i++ {
		schema.AppendCol(fmt.Sprintf("%s%d", prefix, i), types.Type{Oid: types.T_int32, Size: 4, Width: 4})
	}
	return schema
}

// MockSchemaAll if char/varchar is needed, colCnt = 14, otherwise colCnt = 12
func MockSchemaAll(colCnt int) *Schema {
	schema := NewEmptySchema(fmt.Sprintf("%d", rand.Intn(1000000)))
	prefix := "mock_"
	for i := 0; i < colCnt; i++ {
		name := fmt.Sprintf("%s%d", prefix, i)
		var typ types.Type
		switch i {
		case 0:
			typ = types.Type{
				Oid:   types.T_int8,
				Size:  1,
				Width: 8,
			}
		case 1:
			typ = types.Type{
				Oid:   types.T_int16,
				Size:  2,
				Width: 16,
			}
		case 2:
			typ = types.Type{
				Oid:   types.T_int32,
				Size:  4,
				Width: 32,
			}
		case 3:
			typ = types.Type{
				Oid:   types.T_int64,
				Size:  8,
				Width: 64,
			}
		case 4:
			typ = types.Type{
				Oid:   types.T_uint8,
				Size:  1,
				Width: 8,
			}
		case 5:
			typ = types.Type{
				Oid:   types.T_uint16,
				Size:  2,
				Width: 16,
			}
		case 6:
			typ = types.Type{
				Oid:   types.T_uint32,
				Size:  4,
				Width: 32,
			}
		case 7:
			typ = types.Type{
				Oid:   types.T_uint64,
				Size:  8,
				Width: 64,
			}
		case 8:
			typ = types.Type{
				Oid:   types.T_float32,
				Size:  4,
				Width: 32,
			}
		case 9:
			typ = types.Type{
				Oid:   types.T_float64,
				Size:  8,
				Width: 64,
			}
		case 10:
			typ = types.Type{
				Oid:   types.T_date,
				Size:  4,
				Width: 32,
			}
		case 11:
			typ = types.Type{
				Oid:   types.T_datetime,
				Size:  8,
				Width: 64,
			}
		case 12:
			typ = types.Type{
				Oid:   types.T_varchar,
				Size:  24,
				Width: 100,
			}
		case 13:
			typ = types.Type{
				Oid:   types.T_char,
				Size:  24,
				Width: 100,
			}
		}
		schema.AppendCol(name, typ)
	}
	return schema
}
