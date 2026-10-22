package storage

import (
	"context"
	"errors"
	"io"
	"sync"
)

var errFlightPanicked = errors.New("storage: shared call panicked")

// flight runs one call per key at a time; callers that arrive while it runs share its
// result instead of repeating it.
type flight struct {
	mu    sync.Mutex
	calls map[string]*flightCall
}

type flightCall struct {
	done chan struct{}
	dups int // callers waiting on this one
	val  []byte
	err  error
}

func (g *flight) do(key string, fn func() ([]byte, error)) ([]byte, error) {
	g.mu.Lock()
	if c, ok := g.calls[key]; ok {
		c.dups++
		g.mu.Unlock()
		<-c.done
		return c.val, c.err
	}
	if g.calls == nil {
		g.calls = map[string]*flightCall{}
	}
	// Waiters of a call that panics get errFlightPanicked, not a nil result.
	c := &flightCall{done: make(chan struct{}), err: errFlightPanicked}
	g.calls[key] = c
	g.mu.Unlock()

	defer func() {
		g.mu.Lock()
		delete(g.calls, key)
		g.mu.Unlock()
		close(c.done)
	}()
	c.val, c.err = fn()
	return c.val, c.err
}

// ctxReader fails reads once ctx is done, so a long copy stops when its client leaves.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}
