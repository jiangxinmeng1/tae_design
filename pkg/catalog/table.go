package catalog

import (
	"fmt"
	"sync"
	"tae/pkg/common"
	"tae/pkg/iface/txnif"
)

type TableEntry struct {
	*BaseEntry
	db      *DBEntry
	schema  *Schema
	entries map[uint64]*DLNode
	link    *Link
}

func NewTableEntry(db *DBEntry, schema *Schema, txnCtx txnif.AsyncTxn) *TableEntry {
	id := db.catalog.NextTable()
	e := &TableEntry{
		BaseEntry: &BaseEntry{
			CommitInfo: CommitInfo{
				Txn:    txnCtx,
				CurrOp: OpCreate,
			},
			RWMutex: new(sync.RWMutex),
			ID:      id,
		},
		db:      db,
		schema:  schema,
		link:    new(Link),
		entries: make(map[uint64]*DLNode),
	}
	return e
}

func MockStaloneTableEntry(id uint64, schema *Schema) *TableEntry {
	return &TableEntry{
		BaseEntry: &BaseEntry{
			RWMutex: new(sync.RWMutex),
			ID:      id,
		},
		schema: schema,
	}
}

func (entry *TableEntry) MakeSegmentIt(reverse bool) *LinkIt {
	return NewLinkIt(entry.RWMutex, entry.link, reverse)
}

func (entry *TableEntry) CreateSegment(txn txnif.AsyncTxn) (created *SegmentEntry, err error) {
	entry.Lock()
	defer entry.Unlock()
	created = NewSegmentEntry(entry, txn)
	entry.addEntryLocked(created)
	return
}

func (entry *TableEntry) MakeCommand(id uint32) (cmd txnif.TxnCmd, err error) {
	cmdType := CmdCreateTable
	entry.RLock()
	defer entry.RUnlock()
	if entry.CurrOp == OpSoftDelete {
		cmdType = CmdDropTable
	}
	return newTableCmd(id, cmdType, entry), nil
}

func (entry *TableEntry) addEntryLocked(segment *SegmentEntry) {
	n := entry.link.Insert(segment)
	entry.entries[segment.GetID()] = n
}

func (entry *TableEntry) deleteEntryLocked(segment *SegmentEntry) error {
	if n, ok := entry.entries[segment.GetID()]; !ok {
		return ErrNotFound
	} else {
		entry.link.Delete(n)
	}
	return nil
}

func (entry *TableEntry) GetSchema() *Schema {
	return entry.schema
}

func (entry *TableEntry) Compare(o NodePayload) int {
	oe := o.(*TableEntry).BaseEntry
	return entry.DoCompre(oe)
}

func (entry *TableEntry) GetDB() *DBEntry {
	return entry.db
}

func (entry *TableEntry) PPString(level common.PPLevel, depth int, prefix string) string {
	return fmt.Sprintf("%s%s%s", common.RepeatStr("\t", depth), prefix, entry.String())
}

func (entry *TableEntry) String() string {
	entry.RLock()
	defer entry.RUnlock()
	return fmt.Sprintf("TABLE%s[name=%s]", entry.BaseEntry.String(), entry.schema.Name)
}
