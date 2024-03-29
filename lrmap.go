package lrmap

import (
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
)

type (
	LRMap[K comparable, V any] struct {
		mu              sync.Mutex
		left            arena[K, V]
		right           arena[K, V]
		readMap         atomic.Pointer[arena[K, V]]
		writeMap        atomic.Pointer[arena[K, V]]
		redoLog         []operation[K, V]
		readHandlers    map[*readHandlerInner[K, V]]struct{}
		readHandlerPool sync.Pool
	}

	arena[K comparable, V any] map[K]V
)

func New[K comparable, V any]() *LRMap[K, V] {
	// nolint:exhaustivestruct
	m := &LRMap[K, V]{
		left:         make(arena[K, V]),
		right:        make(arena[K, V]),
		readHandlers: make(map[*readHandlerInner[K, V]]struct{}),
	}

	m.readHandlerPool.New = func() interface{} { return m.newReadHandler() }

	m.swap()

	return m
}

func (m *LRMap[K, V]) Set(key K, value V) {
	m.mu.Lock()
	defer m.mu.Unlock()

	(*m.writeMap.Load())[key] = value

	m.redoLog = append(m.redoLog, operation[K, V]{typ: opSet, key: key, value: &value})
}

func (m *LRMap[K, V]) Delete(key K) {
	m.mu.Lock()
	defer m.mu.Unlock()

	delete(*(m.writeMap.Load()), key)

	// nolint:exhaustivestruct
	m.redoLog = append(m.redoLog, operation[K, V]{typ: opDelete, key: key})
}

func (m *LRMap[K, V]) Get(key K) V {
	value, _ := m.GetOK(key)

	return value
}

func (m *LRMap[K, V]) GetOK(key K) (V, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	value, ok := (*m.writeMap.Load())[key]

	return value, ok
}

func (m *LRMap[K, V]) Commit() {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.swap()

	m.waitForReaders()

	// redo all operations from the redo log into the new write map (old read map) to sync up.
	for _, op := range m.redoLog {
		switch op.typ {
		case opSet:
			(*(m.writeMap.Load()))[op.key] = *(op.value)
		case opDelete:
			delete(*(m.writeMap.Load()), op.key)
		default:
			// nolint:goerr113
			panic(fmt.Errorf("operation(%d) not implemented", op.typ))
		}
	}

	// drop the redo log completely and let the GC remove all references to stale keys and values
	m.redoLog = nil
}

func (m *LRMap[K, V]) NewReadHandler() *ReadHandler[K, V] {
	rh := m.readHandlerPool.Get().(*ReadHandler[K, V])
	rh.ready = true

	return rh
}

func (m *LRMap[K, V]) newReadHandler() *ReadHandler[K, V] {
	// Wrap the actual (inner) readHandler in an outer shim and return that shim to the
	// user and only keep a "weak reference" to the inner readHandler, but not the shim.
	// If the user drops any references to the outer shim, (eventually) the runtime runs
	// the finalizer on that shim, so we can call close() on the inner read handler, i.e.
	// remove the weak reference, enabling the GC to remove the actual read handler as well.
	//
	// The reason for doing so is that we can make the user's call to Close() optional.
	// Calling it is still better, as it makes the resources free to remove for the GC
	// immediately.  But if the user chooses to use resource management like `sync.Pool` or
	// possibly other solutions like free-lists, the user may not be able to call Close(),
	// thus leaking resources through the readHandlers list.

	// nolint:exhaustivestruct
	inner := &readHandlerInner[K, V]{lrmap: m}

	m.mu.Lock()
	m.readHandlers[inner] = struct{}{}
	m.mu.Unlock()

	outer := &ReadHandler[K, V]{inner: inner}
	runtime.SetFinalizer(outer, func(rh *ReadHandler[K, V]) {
		rh.ready = false
		rh.inner.close()
	})

	return outer
}

func (m *LRMap[K, V]) swap() {
	switch m.readMap.Load() {
	case nil /* initial case */, &(m.left):
		m.readMap.Store(&(m.right))
		m.writeMap.Store(&(m.left))
	case &(m.right):
		m.readMap.Store(&(m.left))
		m.writeMap.Store(&(m.right))
	default:
		// nolint:goerr113
		panic(fmt.Errorf("illegal pointer: %v", m.readMap.Load()))
	}
}

func (m *LRMap[K, V]) waitForReaders() {
	readers := make(map[*readHandlerInner[K, V]]uint64)

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

		const maxDelay = 5 * time.Second

		delay *= 10
		if delay > maxDelay {
			delay = maxDelay
		}
	}
}

type opType int8

const (
	opSet opType = iota
	opDelete
)

type operation[K comparable, V any] struct {
	typ   opType
	key   K
	value *V
}

type ReadHandler[K comparable, V any] struct {
	inner *readHandlerInner[K, V]
	ready bool
}

func (rh *ReadHandler[K, V]) Enter()                { rh.assertReady(); rh.inner.enter() }
func (rh *ReadHandler[K, V]) Leave()                { rh.assertReady(); rh.inner.leave() }
func (rh *ReadHandler[K, V]) Get(key K) V           { rh.assertReady(); return rh.inner.get(key) }
func (rh *ReadHandler[K, V]) GetOK(key K) (V, bool) { rh.assertReady(); return rh.inner.getOK(key) }
func (rh *ReadHandler[K, V]) Len() int              { rh.assertReady(); return rh.inner.len() }

func (rh *ReadHandler[K, V]) Iterate(fn func(_ K, _ V) bool) {
	rh.assertReady()

	if !rh.inner.entered() {
		panic("reader illegal state: must call Enter() before iterating")
	}

	for key, value := range rh.inner.live {
		if ok := fn(key, value); !ok {
			return
		}
	}
}

func (rh *ReadHandler[K, V]) Close() {
	rh.assertReady()

	runtime.SetFinalizer(rh, nil)
	rh.ready = false
	rh.inner.close()
}

func (rh *ReadHandler[K, V]) Recycle() {
	if !rh.ready {
		panic("illegal reader state: must not recycle twice")
	}

	if rh.inner.entered() {
		panic("reader illegal state: must call Leave() before recycling")
	}

	rh.ready = false

	rh.inner.lrmap.readHandlerPool.Put(rh)
}

func (rh *ReadHandler[K, V]) assertReady() {
	if rh.inner == nil {
		panic("reader illegal state: must create with NewReadHandler()")
	}

	if !rh.ready {
		panic(fmt.Errorf("reader illegal state: must not use after Recycle()"))
	}
}

type readHandlerInner[K comparable, V any] struct {
	lrmap *LRMap[K, V]
	live  arena[K, V]
	epoch uint64
}

func (r *readHandlerInner[K, V]) enter() {
	if r.entered() {
		panic("reader illegal state: must not Enter() twice")
	}

	atomic.AddUint64(&r.epoch, 1)
	r.live = *r.lrmap.readMap.Load()
}

func (r *readHandlerInner[K, V]) leave() {
	if !r.entered() {
		panic("reader illegal state: must not Leave() twice")
	}

	atomic.AddUint64(&r.epoch, 1)
}

func (r *readHandlerInner[K, V]) get(key K) V {
	value, _ := r.getOK(key)

	return value
}

func (r *readHandlerInner[K, V]) getOK(key K) (V, bool) {
	if !r.entered() {
		panic("reader illegal state: must Enter() before operating on data")
	}

	value, ok := r.live[key]

	return value, ok
}

func (r *readHandlerInner[K, V]) len() int {
	if !r.entered() {
		panic("reader illegal state: must Enter() before operation on data")
	}

	return len(r.live)
}

func (r *readHandlerInner[K, V]) close() {
	r.lrmap.mu.Lock()
	defer r.lrmap.mu.Unlock()

	delete(r.lrmap.readHandlers, r)
}

func (r *readHandlerInner[K, V]) entered() bool {
	return r.epoch%2 == 1
}
