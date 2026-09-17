// SPDX-License-Identifier: Apache-2.0

package nfs

import (
	"sync"
	"testing"
)

// TestHandleRoundTripConcurrent reproduces the #244 shape at the handler layer:
// many goroutines resolving a path to a handle and back, as GETATTR does. If the
// handler/index round-trip were racy, -race would fire or a valid handle would
// STALE. (go-nfs serializes per connection, so this isolates our code.)
func TestHandleRoundTripConcurrent(t *testing.T) {
	h, _ := testGateway(t, map[string][]byte{
		"GC_14.7.0/restart.c24.nc4": make([]byte, 37<<20),
		"GC_14.7.0/a.nc4":           make([]byte, 1<<20),
		"GC_14.7.0/b.nc4":           make([]byte, 1<<20),
	})
	paths := [][]string{
		{"GC_14.7.0", "restart.c24.nc4"},
		{"GC_14.7.0", "a.nc4"},
		{"GC_14.7.0", "b.nc4"},
	}
	var wg sync.WaitGroup
	var stale int64
	var mu sync.Mutex
	for g := 0; g < 96; g++ { // above the tester's 48 threshold
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				pp := paths[(g+i)%len(paths)]
				fh := h.ToHandle(h.fs, pp)
				if len(fh) == 0 {
					mu.Lock()
					stale++
					mu.Unlock()
					continue
				}
				if _, _, err := h.FromHandle(fh); err != nil {
					mu.Lock()
					stale++
					mu.Unlock()
				}
			}
		}(g)
	}
	wg.Wait()
	if stale != 0 {
		t.Fatalf("%d STALE/empty out of %d round-trips — handler round-trip is not concurrency-safe", stale, 96*200)
	}
}
