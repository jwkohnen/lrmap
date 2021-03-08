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
		left            mapT
		right           mapT
		readMap         unsafe.Pointer
		writeMap        *mapT
		opLog           []op
		readHandlers    map[*readHandlerInner]struct{}
		readHandlerPool sync.Pool
	}

	Key   int
	Value int

	mapT map[Key]Value
)

func New() *LRMap {
	// nolint:exhaustivestruct
	m := &LRMap{
		left:         make(map[Key]Value),
		right:        make(map[Key]Value),
		readHandlers: make(map[*readHandlerInner]struct{}),
	}

	m.readHandlerPool.New = func() interface{} { return m.newReadHandler() }

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

	// nolint:exhaustivestruct
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
			// nolint:goerr113
			panic(fmt.Errorf("op(%d) not implemented", o.t))
		}
	}

	// drop the op log completely and let the GC remove all references to stale keys and values
	m.opLog = nil
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
		// nolint:goerr113
		panic(fmt.Errorf("illegal pointer: %v", r))
	}

	atomic.StorePointer(&(m.readMap), unsafe.Pointer(r))
	m.writeMap = w
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
	inner *readHandlerInner
	ready bool
}

func (rh *ReadHandler) Enter()                    { rh.assertReady(); rh.inner.enter() }
func (rh *ReadHandler) Leave()                    { rh.assertReady(); rh.inner.leave() }
func (rh *ReadHandler) Get(k Key) Value           { rh.assertReady(); return rh.inner.get(k) }
func (rh *ReadHandler) GetOK(k Key) (Value, bool) { rh.assertReady(); return rh.inner.getOK(k) }
func (rh *ReadHandler) Len() int                  { rh.assertReady(); return rh.inner.len() }

func (rh *ReadHandler) Iterate(fn func(k Key, v Value) bool) {
	rh.assertReady()

	if !rh.inner.entered() {
		panic("reader illegal state: must call Enter() before iterating")
	}

	for k, v := range rh.inner.live {
		if ok := fn(k, v); !ok {
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
	live  mapT
	epoch uint64
}

func (r *readHandlerInner) enter() {
	if r.entered() {
		panic("reader illegal state: must not Enter() twice")
	}

	atomic.AddUint64(&r.epoch, 1)
	r.live = r.lrm.getMapAtomic()
}

func (r *readHandlerInner) leave() {
	if !r.entered() {
		panic("reader illegal state: must not Leave() twice")
	}

	atomic.AddUint64(&r.epoch, 1)
}

func (r *readHandlerInner) get(k Key) Value {
	v, _ := r.getOK(k)

	return v
}

func (r *readHandlerInner) getOK(k Key) (Value, bool) {
	if !r.entered() {
		panic("reader illegal state: must Enter() before operating on data")
	}

	v, ok := r.live[k]

	return v, ok
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
