// SPDX-License-Identifier: Apache-2.0

package blockstore

import (
	"container/list"
	"sync"
)

// mem2Q is a byte-bounded 2Q cache (Johnson & Shasha). Newly admitted blocks
// enter a FIFO "in" queue; a reference to a block whose key is on the ghost
// "out" list promotes it to an LRU "main" queue. This resists a sequential
// scan evicting the hot set, which matters for a mixed sequential/random
// workload. All operations are O(1) amortized.
type mem2Q struct {
	mu       sync.Mutex
	capacity int64
	size     int64 // bytes held across in+main
	inSize   int64 // bytes held in the in FIFO

	in    *list.List               // A1in: FIFO of recently admitted
	main  *list.List               // Am: LRU of frequently used
	out   *list.List               // A1out: ghost keys only (no data)
	table map[string]*list.Element // key -> element in in or main
	ghost map[string]*list.Element // key -> element in out

	inCap    int64 // byte cap for the in queue
	ghostCap int   // entry cap for the ghost list
}

type entry struct {
	key   string
	data  []byte
	owner *list.List // in or main (nil for ghost entries)
}

func newMem2Q(capacity int64) *mem2Q {
	if capacity < 0 {
		capacity = 0
	}
	return &mem2Q{
		capacity: capacity,
		in:       list.New(),
		main:     list.New(),
		out:      list.New(),
		table:    make(map[string]*list.Element),
		ghost:    make(map[string]*list.Element),
		inCap:    capacity / 4, // classic 2Q: Kin ~ 25% of capacity
		ghostCap: 4096,
	}
}

// Get returns the cached block and whether it was present. A hit in main moves
// it to the front (LRU); a hit in the "in" FIFO leaves it in place.
func (c *mem2Q) Get(key string) ([]byte, bool) {
	if c.capacity == 0 {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.table[key]
	if !ok {
		return nil, false
	}
	e := el.Value.(*entry)
	if e.owner == c.main {
		c.main.MoveToFront(el)
	}
	return e.data, true
}

// Put inserts a block. A key currently on the ghost list is admitted straight
// to main; otherwise it enters the in FIFO. Re-inserting a present key is a
// no-op.
func (c *mem2Q) Put(key string, data []byte) {
	if c.capacity == 0 || int64(len(data)) > c.capacity {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.table[key]; ok {
		return
	}
	e := &entry{key: key, data: data}
	if gel, isGhost := c.ghost[key]; isGhost {
		c.out.Remove(gel)
		delete(c.ghost, key)
		e.owner = c.main
		c.table[key] = c.main.PushFront(e)
	} else {
		e.owner = c.in
		c.table[key] = c.in.PushFront(e)
		c.inSize += int64(len(data))
	}
	c.size += int64(len(data))
	c.evict()
}

func (c *mem2Q) evict() {
	for c.size > c.capacity {
		// Prefer trimming the in FIFO when it is over its share; otherwise
		// evict the main LRU tail. Fall back to whichever is non-empty.
		if c.in.Len() > 0 && (c.inSize > c.inCap || c.main.Len() == 0) {
			c.evictInTail()
		} else if c.main.Len() > 0 {
			el := c.main.Back()
			e := el.Value.(*entry)
			c.main.Remove(el)
			delete(c.table, e.key)
			c.size -= int64(len(e.data))
		} else {
			return
		}
	}
}

func (c *mem2Q) evictInTail() {
	el := c.in.Back()
	e := el.Value.(*entry)
	c.in.Remove(el)
	delete(c.table, e.key)
	c.size -= int64(len(e.data))
	c.inSize -= int64(len(e.data))
	// Record a ghost (key only) so a later reference promotes it to main.
	c.ghost[e.key] = c.out.PushFront(&entry{key: e.key})
	for c.out.Len() > c.ghostCap {
		g := c.out.Back()
		c.out.Remove(g)
		delete(c.ghost, g.Value.(*entry).key)
	}
}

// promoteGhost reports whether key is currently a ghost (used by tests).
func (c *mem2Q) isGhost(key string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.ghost[key]
	return ok
}
