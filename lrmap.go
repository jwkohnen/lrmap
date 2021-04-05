package lrmap

import (
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"
)

type (
	LRMap struct {
		mu              sync.Mutex
		left            arena
		right           arena
		readMap         unsafe.Pointer
		writeMap        *arena
		redoLog         []operation
		readHandlers    map[*readHandlerInner]struct{}
		readHandlerPool sync.Pool
	}

	Key   int
	Value int

	arena map[Key]Value
)

func New() *LRMap {
	// nolint:exhaustivestruct
	m := &LRMap{
		left:         make(arena),
		right:        make(arena),
		readHandlers: make(map[*readHandlerInner]struct{}),
	}

	m.readHandlerPool.New = func() interface{} { return m.newReadHandler() }

	m.swap()

	return m
}

func (m *LRMap) Set(key Key, value Value) {
	m.mu.Lock()
	defer m.mu.Unlock()

	(*(m.writeMap))[key] = value

	m.redoLog = append(m.redoLog, operation{typ: opSet, key: key, value: &value})
}

func (m *LRMap) Delete(key Key) {
	m.mu.Lock()
	defer m.mu.Unlock()

	delete(*(m.writeMap), key)

	// nolint:exhaustivestruct
	m.redoLog = append(m.redoLog, operation{typ: opDelete, key: key})
}

func (m *LRMap) Get(key Key) Value {
	value, _ := m.GetOK(key)

	return value
}

func (m *LRMap) GetOK(key Key) (Value, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	value, ok := (*m.writeMap)[key]

	return value, ok
}

func (m *LRMap) Flush() {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.swap()

	m.waitForReaders()

	// redo all operations from the redo log into the new write map (old read map) to sync up.
	for _, op := range m.redoLog {
		switch op.typ {
		case opSet:
			(*(m.writeMap))[op.key] = *(op.value)
		case opDelete:
			delete(*(m.writeMap), op.key)
		default:
			// nolint:goerr113
			panic(fmt.Errorf("operation(%d) not implemented", op.typ))
		}
	}

	// drop the redo log completely and let the GC remove all references to stale keys and values
	m.redoLog = nil
}

func (m *LRMap) NewReadHandler() *ReadHandler {
	rh := m.readHandlerPool.Get().(*ReadHandler)
	rh.ready = true

	return rh
}

func (m *LRMap) newReadHandler() *ReadHandler {
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
	inner := &readHandlerInner{lrm: m}

	m.mu.Lock()
	m.readHandlers[inner] = struct{}{}
	m.mu.Unlock()

	outer := &ReadHandler{inner: inner}
	runtime.SetFinalizer(outer, func(rh *ReadHandler) {
		rh.ready = false
		rh.inner.close()
	})

	return outer
}

func (m *LRMap) swap() {
	var (
		read  = (*arena)(m.readMap)
		write *arena
	)

	switch read {
	case nil:
		// initial case: start with left map as reader, right map as writer
		read = &(m.left)
		write = &(m.right)
	case &(m.left):
		read = &(m.right)
		write = &(m.left)
	case &(m.right):
		read = &(m.left)
		write = &(m.right)
	default:
		// nolint:goerr113
		panic(fmt.Errorf("illegal pointer: %v", read))
	}

	atomic.StorePointer(&(m.readMap), unsafe.Pointer(read))
	m.writeMap = write
}

func (m *LRMap) waitForReaders() {
	readers := make(map[*readHandlerInner]uint64)

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

func (m *LRMap) getArenaAtomic() arena {
	return *(*arena)(atomic.LoadPointer(&(m.readMap)))
}

type opType int8

const (
	opSet opType = iota
	opDelete
)

type operation struct {
	typ   opType
	key   Key
	value *Value
}

type ReadHandler struct {
	inner *readHandlerInner
	ready bool
}

func (rh *ReadHandler) Enter()                      { rh.assertReady(); rh.inner.enter() }
func (rh *ReadHandler) Leave()                      { rh.assertReady(); rh.inner.leave() }
func (rh *ReadHandler) Get(key Key) Value           { rh.assertReady(); return rh.inner.get(key) }
func (rh *ReadHandler) GetOK(key Key) (Value, bool) { rh.assertReady(); return rh.inner.getOK(key) }
func (rh *ReadHandler) Len() int                    { rh.assertReady(); return rh.inner.len() }

func (rh *ReadHandler) Iterate(fn func(k Key, v Value) bool) {
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

func (rh *ReadHandler) Close() {
	rh.assertReady()

	runtime.SetFinalizer(rh, nil)
	rh.ready = false
	rh.inner.close()
}

func (rh *ReadHandler) Recycle() {
	if !rh.ready {
		panic("illegal reader state: must not recycle twice")
	}

	if rh.inner.entered() {
		panic("reader illegal state: must call Leave() before recycling")
	}

	rh.ready = false

	rh.inner.lrm.readHandlerPool.Put(rh)
}

func (rh *ReadHandler) assertReady() {
	if rh.inner == nil {
		panic("reader illegal state: must create with NewReadHandler()") // TODO
	}

	if !rh.ready {
		panic(fmt.Errorf("reader illegal state: must not use after Recycle()"))
	}
}

type readHandlerInner struct {
	lrm   *LRMap
	live  arena
	epoch uint64
}

func (r *readHandlerInner) enter() {
	if r.entered() {
		panic("reader illegal state: must not Enter() twice")
	}

	atomic.AddUint64(&r.epoch, 1)
	r.live = r.lrm.getArenaAtomic()
}

func (r *readHandlerInner) leave() {
	if !r.entered() {
		panic("reader illegal state: must not Leave() twice")
	}

	atomic.AddUint64(&r.epoch, 1)
}

func (r *readHandlerInner) get(key Key) Value {
	value, _ := r.getOK(key)

	return value
}

func (r *readHandlerInner) getOK(key Key) (Value, bool) {
	if !r.entered() {
		panic("reader illegal state: must Enter() before operating on data")
	}

	value, ok := r.live[key]

	return value, ok
}

func (r *readHandlerInner) len() int {
	if !r.entered() {
		panic("reader illegal state: must Enter() before operation on data")
	}

	return len(r.live)
}

func (r *readHandlerInner) close() {
	r.lrm.mu.Lock()
	defer r.lrm.mu.Unlock()

	delete(r.lrm.readHandlers, r)
}

func (r *readHandlerInner) entered() bool {
	return r.epoch%2 == 1
}
