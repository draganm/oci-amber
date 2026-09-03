// Package keyedmutex provides a mutex keyed by an arbitrary comparable
// value: holders of different keys never block each other, holders of the
// same key are serialized. It is the one implementation shared by the
// packages that need per-digest or per-repository serialization.
package keyedmutex

import (
	"sync"
	"sync/atomic"
)

// Mutex serializes work per key. Its zero value is ready to use. Rows are
// refcounted: an entry is dropped as soon as its last holder or waiter
// releases it, so the map stays bounded by the number of keys currently
// held or awaited rather than by the number of keys ever seen.
//
// A Mutex must not be copied after first use.
type Mutex[K comparable] struct {
	mu    sync.Mutex
	locks map[K]*entry
}

// entry is one key's mutex together with the number of goroutines holding
// or waiting for it.
type entry struct {
	mu   sync.Mutex
	refs int
}

// Lock blocks until k is free and returns the function that releases it.
// The returned function must be called exactly once; calling it a second
// time panics.
func (m *Mutex[K]) Lock(k K) (unlock func()) {
	m.mu.Lock()
	if m.locks == nil {
		m.locks = make(map[K]*entry)
	}
	e, ok := m.locks[k]
	if !ok {
		e = &entry{}
		m.locks[k] = e
	}
	// The reference is taken before blocking, so the entry cannot be
	// dropped from the map while this goroutine waits for it.
	e.refs++
	m.mu.Unlock()

	e.mu.Lock()
	var released atomic.Bool
	return func() {
		if !released.CompareAndSwap(false, true) {
			panic("keyedmutex: unlock called more than once")
		}
		e.mu.Unlock()
		m.mu.Lock()
		e.refs--
		if e.refs == 0 {
			delete(m.locks, k)
		}
		m.mu.Unlock()
	}
}

// Len reports how many keys the mutex is currently holding: the number of
// keys held or awaited right now, never the number of keys ever seen. It
// exists so callers that keep a Mutex as their only per-key state can
// assert in tests that nothing is retained after the work is done.
func (m *Mutex[K]) Len() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.locks)
}
