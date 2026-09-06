// SPDX-License-Identifier: Apache-2.0

package blockstore

import "sync"

// flightGroup deduplicates concurrent fetches for the same key: the first
// caller executes fn, later callers with an in-flight key wait and share its
// result. It is a minimal singleflight to avoid an external dependency.
type flightGroup struct {
	mu sync.Mutex
	m  map[string]*flightCall
}

type flightCall struct {
	wg   sync.WaitGroup
	data []byte
	err  error
}

func newFlightGroup() *flightGroup {
	return &flightGroup{m: make(map[string]*flightCall)}
}

// Do runs fn for key unless an identical call is in flight, in which case it
// waits for and returns the in-flight result. The bool reports whether fn was
// actually executed by this caller (false = shared an in-flight call).
func (g *flightGroup) Do(key string, fn func() ([]byte, error)) ([]byte, error, bool) {
	g.mu.Lock()
	if c, ok := g.m[key]; ok {
		g.mu.Unlock()
		c.wg.Wait()
		return c.data, c.err, false
	}
	c := &flightCall{}
	c.wg.Add(1)
	g.m[key] = c
	g.mu.Unlock()

	c.data, c.err = fn()
	c.wg.Done()

	g.mu.Lock()
	delete(g.m, key)
	g.mu.Unlock()
	return c.data, c.err, true
}
