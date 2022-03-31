package txnimpl

import (
	"tae/pkg/catalog"
	"tae/pkg/iface/handle"
	"tae/pkg/iface/txnif"
	"tae/pkg/txn/txnbase"

	"github.com/matrixorigin/matrixone/pkg/container/batch"
	"github.com/matrixorigin/matrixone/pkg/vm/engine/aoe/storage/mutation/buffer/base"
)

type txnStore struct {
	txnbase.NoopTxnStore
	tables      map[uint64]Table
	driver      txnbase.NodeDriver
	nodesMgr    base.INodeManager
	dbIndex     map[string]uint64
	tableIndex  map[string]uint64
	txn         txnif.AsyncTxn
	catalog     *catalog.Catalog
	database    handle.Database
	createEntry txnif.TxnEntry
}

var TxnStoreFactory = func(catalog *catalog.Catalog) txnbase.TxnStoreFactory {
	return func() txnif.TxnStore {
		return newStore(catalog)
	}
}

func newStore(catalog *catalog.Catalog) *txnStore {
	return &txnStore{
		tables:  make(map[uint64]Table),
		catalog: catalog,
	}
}

func (store *txnStore) Close() error {
	var err error
	for _, table := range store.tables {
		if err = table.Close(); err != nil {
			break
		}
	}
	return err
}

// func (store *txnStore) InitTable(id uint64, schema *catalog.Schema) error {
// 	table := store.tables[id]
// 	if table != nil {
// 		return ErrDuplicateNode
// 	}
// 	store.tables[id] = newTxnTable(nil, id, schema, store.driver, store.nodesMgr)
// 	store.tableIndex[schema.Name] = id
// 	return nil
// }

func (store *txnStore) BindTxn(txn txnif.AsyncTxn) {
	store.txn = txn
}

func (store *txnStore) Append(id uint64, data *batch.Batch) error {
	table := store.tables[id]
	if table.IsDeleted() {
		return txnbase.ErrNotFound
	}
	return table.Append(data)
}

func (store *txnStore) RangeDeleteLocalRows(id uint64, start, end uint32) error {
	table := store.tables[id]
	return table.RangeDeleteLocalRows(start, end)
}

func (store *txnStore) UpdateLocalValue(id uint64, row uint32, col uint16, value interface{}) error {
	table := store.tables[id]
	return table.UpdateLocalValue(row, col, value)
}

func (store *txnStore) AddUpdateNode(id uint64, node txnif.BlockUpdates) error {
	table := store.tables[id]
	return table.AddUpdateNode(node)
}

func (store *txnStore) UseDatabase(name string) (err error) {
	store.database, err = store.txn.GetDatabase(name)
	return err
}

func (store *txnStore) CreateDatabase(name string) (handle.Database, error) {
	meta, err := store.catalog.CreateDBEntry(name, store.txn)
	if err != nil {
		return nil, err
	}
	store.createEntry = meta
	store.database = newDatabase(store.txn, meta)
	return store.database, nil
}

func (store *txnStore) CreateRelation(def interface{}) (relation handle.Relation, err error) {
	schema := def.(*catalog.Schema)
	db := store.database.GetMeta().(*catalog.DBEntry)
	meta, err := db.CreateTableEntry(schema, store.txn)
	if err != nil {
		return
	}
	relation = newRelation(store.txn, meta)

	table := newTxnTable(nil, relation, store.driver, store.nodesMgr)
	table.SetCreateEntry(meta)
	// table.AddCreateCommand() // TODO
	store.tables[relation.ID()] = table

	return
}

func (store *txnStore) Commit() (err error) {
	if store.createEntry != nil {
		if err = store.createEntry.Commit(); err != nil {
			return
		}
	}
	for _, table := range store.tables {
		if err = table.Commit(); err != nil {
			break
		}
	}
	return
}

func (store *txnStore) PrepareCommit() (err error) {
	if store.createEntry != nil {
		if err = store.createEntry.PrepareCommit(); err != nil {
			return
		}
	}
	for _, table := range store.tables {
		if err = table.PrepareCommit(); err != nil {
			break
		}
	}
	return
}

func (store *txnStore) AddTxnEntry(t txnif.TxnEntryType, entry txnif.TxnEntry) {
	// switch t {
	// case TxnEntryCreateDatabase:
	// 	store.createEntry = entry
	// case TxnEntryCretaeTable:
	// 	create := entry.(*catalog.TableEntry)

	// }
}

// func (store *txnStore) PrepareRollback() error { return nil }
// func (store *txnStore) Rollback() error        { return nil }
// func (store *txnStore) Commit() error          { return nil }

// func (store *txnStore) FindKeys(db, table uint64, keys [][]byte) []uint32 {
// 	// TODO
// 	return nil
// }

// func (store *txnStore) FindKey(db, table uint64, key []byte) uint32 {
// 	// TODO
// 	return 0
// }

// func (store *txnStore) HasKey(db, table uint64, key []byte) bool {
// 	// TODO
// 	return false
// }
