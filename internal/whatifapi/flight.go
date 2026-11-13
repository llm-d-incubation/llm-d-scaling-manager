package whatifapi

import (
	"context"
	"errors"
	"sync"
)

// flight runs one call per key at a time: callers that ask for a key while
// its call runs wait for it and share its result, so identical cold
// requests price once and take one slot.
type flight[V any] struct {
	mu    sync.Mutex
	calls map[string]*flightCall[V]
}

// flightCall is one call in flight.
type flightCall[V any] struct {
	done  chan struct{}
	value V
	err   error
	// waiting is how many callers wait on it, for the tests.
	waiting int
}

// do is fn's result for key: fn's own, or that of the call for key already in
// flight.
func (f *flight[V]) do(key string, fn func() (V, error)) (V, error) {
	f.mu.Lock()
	if f.calls == nil {
		f.calls = map[string]*flightCall[V]{}
	}
	if c, ok := f.calls[key]; ok {
		c.waiting++
		f.mu.Unlock()
		<-c.done
		return c.value, c.err
	}
	c := &flightCall[V]{done: make(chan struct{})}
	f.calls[key] = c
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		delete(f.calls, key)
		f.mu.Unlock()
		close(c.done)
	}()
	c.value, c.err = fn()
	return c.value, c.err
}

// waiters is how many callers wait on the call for key, for the tests.
func (f *flight[V]) waiters(key string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	if c, ok := f.calls[key]; ok {
		return c.waiting
	}
	return 0
}

// doFor is do for a request: fn runs on the context of the call in flight,
// so a caller whose shared call ended with that call's context (its client
// gone) while its own is live asks again.
func (f *flight[V]) doFor(ctx context.Context, key string, fn func() (V, error)) (V, error) {
	for {
		v, err := f.do(key, fn)
		if err != nil && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) && ctx.Err() == nil {
			continue
		}
		return v, err
	}
}
