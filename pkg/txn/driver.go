package txn

import (
	"sync"

	"github.com/jiangxinmeng1/logstore/pkg/store"
)

const (
	GroupAC uint32 = iota + 10
	GroupPC
	GroupUC
)

type NodeDriver interface {
	AppendEntry(NodeEntry) (uint64, error)
	Close() error
}

type nodeDriver struct {
	sync.RWMutex
	impl store.Store
	own  bool
}

func NewNodeDriver(dir, name string, cfg *store.StoreCfg) NodeDriver {
	impl, err := store.NewBaseStore(dir, name, cfg)
	if err != nil {
		panic(err)
	}
	driver := NewNodeDriverWithStore(impl, true)
	return driver
}

func NewNodeDriverWithStore(impl store.Store, own bool) NodeDriver {
	driver := new(nodeDriver)
	driver.impl = impl
	driver.own = own
	return driver
}

func (nd *nodeDriver) AppendEntry(e NodeEntry) (uint64, error) {
	id, err := nd.impl.AppendEntry(GroupUC, e)
	return id, err
}

func (nd *nodeDriver) Close() error {
	if nd.own {
		return nd.impl.Close()
	}
	return nil
}
