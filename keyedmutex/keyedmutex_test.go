package keyedmutex

import (
	"sync"
	"testing"
	"time"
)

// entries reports how many rows the mutex is currently holding.
func entries[K comparable](m *Mutex[K]) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.locks)
}

func TestSameKeySerializes(t *testing.T) {
	var km Mutex[string]
	unlock := km.Lock("a")
	acquired := make(chan struct{})
	go func() {
		u := km.Lock("a")
		close(acquired)
		u()
	}()
	select {
	case <-acquired:
		t.Fatal("second Lock acquired while the first is held")
	case <-time.After(50 * time.Millisecond):
	}
	unlock()
	select {
	case <-acquired:
	case <-time.After(time.Second):
		t.Fatal("second Lock never acquired")
	}
}

func TestSameKeyOrdersCriticalSections(t *testing.T) {
	var km Mutex[int]
	// Two goroutines increment a counter under the same key without any
	// atomics: the increments can only be safe, and the total exact, if the
	// critical sections never overlap.
	var counter int
	var inside int
	var mu sync.Mutex // guards inside, the overlap detector
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 500 {
				u := km.Lock(7)
				mu.Lock()
				inside++
				n := inside
				mu.Unlock()
				if n != 1 {
					t.Errorf("%d goroutines inside the same key's critical section", n)
				}
				counter++
				mu.Lock()
				inside--
				mu.Unlock()
				u()
			}
		}()
	}
	wg.Wait()
	if counter != 1000 {
		t.Fatalf("counter = %d, want 1000", counter)
	}
	if n := entries(&km); n != 0 {
		t.Fatalf("%d entries left after every holder released", n)
	}
}

func TestDifferentKeysDoNotBlock(t *testing.T) {
	var km Mutex[string]
	// Both goroutines take their key and wait for the other to have taken
	// its own; this only completes if different keys are independent.
	gotA := make(chan struct{})
	gotB := make(chan struct{})
	done := make(chan struct{}, 2)
	go func() {
		u := km.Lock("a")
		close(gotA)
		<-gotB
		u()
		done <- struct{}{}
	}()
	go func() {
		u := km.Lock("b")
		close(gotB)
		<-gotA
		u()
		done <- struct{}{}
	}()
	for range 2 {
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("locks on different keys blocked each other")
		}
	}
}

func TestEntryDroppedAfterUnlock(t *testing.T) {
	var km Mutex[string]
	if n := entries(&km); n != 0 {
		t.Fatalf("zero value has %d entries, want 0", n)
	}
	unlock := km.Lock("a")
	if n := entries(&km); n != 1 {
		t.Fatalf("entries while held = %d, want 1", n)
	}
	unlock()
	if n := entries(&km); n != 0 {
		t.Fatalf("entries after unlock = %d, want 0", n)
	}

	// A waiter keeps the entry alive, and it is dropped only once the last
	// holder releases it.
	held := km.Lock("a")
	waiting := make(chan func())
	go func() { waiting <- km.Lock("a") }()
	// Give the waiter time to register its reference.
	deadline := time.Now().Add(time.Second)
	for entries(&km) != 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if n := entries(&km); n != 1 {
		t.Fatalf("entries with a holder and a waiter = %d, want 1", n)
	}
	held()
	second := <-waiting
	if n := entries(&km); n != 1 {
		t.Fatalf("entries while the second holder runs = %d, want 1", n)
	}
	second()
	if n := entries(&km); n != 0 {
		t.Fatalf("entries after the last holder released = %d, want 0", n)
	}

	// Many keys, all released: nothing is retained.
	unlocks := make([]func(), 0, 100)
	for i := range 100 {
		unlocks = append(unlocks, km.Lock(string(rune('a'+i%26))+string(rune('a'+i/26))))
	}
	if n := entries(&km); n != 100 {
		t.Fatalf("entries with 100 keys held = %d, want 100", n)
	}
	for _, u := range unlocks {
		u()
	}
	if n := entries(&km); n != 0 {
		t.Fatalf("entries after releasing 100 keys = %d, want 0", n)
	}
}

func TestUnlockTwicePanics(t *testing.T) {
	var km Mutex[string]
	unlock := km.Lock("a")
	unlock()
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("second unlock did not panic")
		}
		if s, ok := r.(string); !ok || s != "keyedmutex: unlock called more than once" {
			t.Fatalf("panic value = %#v, want the documented message", r)
		}
		// The key is still usable after the misuse.
		u := km.Lock("a")
		u()
		if n := entries(&km); n != 0 {
			t.Fatalf("entries after recovery = %d, want 0", n)
		}
	}()
	unlock()
}

func TestConcurrentKeys(t *testing.T) {
	var km Mutex[int]
	counts := make([]int, 8)
	var wg sync.WaitGroup
	for i := range 8 {
		for range 50 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				u := km.Lock(i)
				defer u()
				counts[i]++
			}()
		}
	}
	wg.Wait()
	for i, c := range counts {
		if c != 50 {
			t.Errorf("key %d counted %d, want 50", i, c)
		}
	}
	if n := entries(&km); n != 0 {
		t.Fatalf("%d entries left, want 0", n)
	}
}
