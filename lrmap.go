package lrmap

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"
)

type (
	LRMap struct {
		mu           sync.Mutex
		left         mapT
		right        mapT
		readMap      unsafe.Pointer
		writeMap     *mapT
		opLog        []op
		readHandlers map[*ReadHandler]struct{}
	}

	Key   int
	Value int

	mapT map[Key]Value
)

func New() *LRMap {
	m := &LRMap{
		left:         make(map[Key]Value),
		right:        make(map[Key]Value),
		readHandlers: make(map[*ReadHandler]struct{}),
	}

	m.swap()

	return m
}

func (m *LRMap) Set(k Key, v Value) {
	m.mu.Lock()
	defer m.mu.Unlock()

	(*(m.writeMap))[k] = v

	m.opLog = append(m.opLog, op{t: opSet, k: k, v: &v})
}

func (m *LRMap) Delete(k Key) {
	m.mu.Lock()
	defer m.mu.Unlock()

	delete(*(m.writeMap), k)

	m.opLog = append(m.opLog, op{t: opDelete, k: k})
}

func (m *LRMap) Get(k Key) Value {
	v, _ := m.GetOK(k)

	return v
}

func (m *LRMap) GetOK(k Key) (Value, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	v, ok := (*m.writeMap)[k]

	return v, ok
}

func (m *LRMap) Flush() {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.swap()

	m.waitForReaders()

	// redo all operations from the op log into the new write map (old read map) to sync up.
	for _, o := range m.opLog {
		switch o.t {
		case opSet:
			(*(m.writeMap))[o.k] = *(o.v)
		case opDelete:
			delete(*(m.writeMap), o.k)
		default:
			panic(fmt.Errorf("op(%d) not implemented", o.t))
		}
	}

	// drop the op log completely and let the GC remove all references to stale keys and values
	m.opLog = nil
}

func (m *LRMap) NewReadHandler() *ReadHandler {
	m.mu.Lock()
	defer m.mu.Unlock()

	rh := &ReadHandler{lrm: m}

	m.readHandlers[rh] = struct{}{}

	return rh
}

func (m *LRMap) swap() {
	var (
		r = (*mapT)(m.readMap)
		w *mapT
	)

	switch r {
	case nil:
		// initial case: start with left map as reader, right map as writer
		r = &(m.left)
		w = &(m.right)
	case &(m.left):
		r = &(m.right)
		w = &(m.left)
	case &(m.right):
		r = &(m.left)
		w = &(m.right)
	default:
		panic(fmt.Errorf("illegal pointer: %v", r))
	}

	atomic.StorePointer(&(m.readMap), unsafe.Pointer(r))
	m.writeMap = w
}

func (m *LRMap) waitForReaders() {
	readers := make(map[*ReadHandler]uint64)
	for rh := range m.readHandlers {
		if epoch := atomic.LoadUint64(&(rh.epoch)); epoch%2 == 1 {
			readers[rh] = epoch
		}
	}

	delay := time.Microsecond

	for {
		for reader, epoch := range readers {
			if e := atomic.LoadUint64(&(reader.epoch)); e != epoch {
				delete(readers, reader)
			}
		}

		if len(readers) == 0 {
			return
		}

		time.Sleep(delay)

		delay *= 10
		if delay > 5*time.Second {
			delay = 5 * time.Second
		}
	}
}

func (m *LRMap) getMapAtomic() mapT {
	return *(*mapT)(atomic.LoadPointer(&(m.readMap)))
}

type opType int8

const (
	opSet opType = iota
	opDelete
)

type op struct {
	t opType
	k Key
	v *Value
}

type ReadHandler struct {
	lrm    *LRMap
	live   mapT
	epoch  uint64
	closed bool
}

func (r *ReadHandler) Enter() {
	if r.lrm == nil {
		panic("reader illegal state: must create with New()")
	}

	if r.epoch%2 == 1 {
		panic("reader illegal state: must not Enter() twice")
	}

	atomic.AddUint64(&r.epoch, 1)
	r.live = r.lrm.getMapAtomic()
}

func (r *ReadHandler) Leave() {
	if r.epoch%2 == 0 {
		panic("reader illegal state: must not Leave() twice")
	}

	atomic.AddUint64(&r.epoch, 1)
}

func (r *ReadHandler) Get(k Key) Value {
	v, _ := r.GetOK(k)
	return v
}

func (r *ReadHandler) GetOK(k Key) (Value, bool) {
	if r.epoch%2 == 0 {
		panic("reader illegal state: must Enter() before operating on data")
	}

	v, ok := r.live[k]
	return v, ok
}

func (r *ReadHandler) Close() {
	r.lrm.mu.Lock()
	defer r.lrm.mu.Unlock()

	if r.closed {
		panic("reader illegal state: must not Close() more than once")
	}

	delete(r.lrm.readHandlers, r)
	r.closed = true
}
