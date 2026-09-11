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

	in     *list.List               // A1in: FIFO of recently admitted
	main   *list.List               // Am: LRU of frequently used
	out    *list.List               // A1out: ghost keys only (no data)
	table  map[string]*list.Element // key -> element in in or main
	ghost  map[string]*list.Element // key -> element in out
	pins   map[string]int           // key -> pin count (not evictable while > 0)
	unread map[string]struct{}      // prefetched, not yet demand-read: evicted last (#55)

	inCap    int64 // byte cap for the in queue
	ghostCap int   // entry cap for the ghost list

	// onEvictUnread, if set, is called when an unread (prefetched-but-unconsumed)
	// chunk is evicted — the thrash signal for #55.
	onEvictUnread func(key string)
}

type entry struct {
	key    string
	data   []byte
	filled uint16     // sparse-fill extent bitmap (#118); fullExtents for a whole chunk
	owner  *list.List // in or main (nil for ghost entries)
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
		pins:     make(map[string]int),
		unread:   make(map[string]struct{}),
		inCap:    capacity / 4, // classic 2Q: Kin ~ 25% of capacity
		ghostCap: 4096,
	}
}

// PutUnread inserts a block and flags it unread atomically, so there is no
// window in which a just-prefetched chunk can be evicted as if already read
// (which would re-fetch without tripping the thrash metric). See #55.
func (c *mem2Q) PutUnread(key string, data []byte, filled uint16) {
	if c.capacity == 0 || int64(len(data)) > c.capacity {
		return
	}
	c.mu.Lock()
	if _, ok := c.table[key]; !ok {
		e := &entry{key: key, filled: filled}
		e.data = data
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
	}
	c.unread[key] = struct{}{}
	c.evict()
	c.mu.Unlock()
}

// MarkUnread flags a chunk as prefetched-but-not-yet-demanded, so eviction
// prefers already-read chunks over it (#55).
func (c *mem2Q) MarkUnread(key string) {
	if c.capacity == 0 {
		return
	}
	c.mu.Lock()
	if _, ok := c.table[key]; ok {
		c.unread[key] = struct{}{}
	}
	c.mu.Unlock()
}

// ClearUnread marks a chunk as read (a demand read consumed it), returning it
// to the normal eviction order.
func (c *mem2Q) ClearUnread(key string) {
	if c.capacity == 0 {
		return
	}
	c.mu.Lock()
	delete(c.unread, key)
	c.mu.Unlock()
}

// Pin marks a chunk as not-evictable (its disk write is pending). Balanced by
// Unpin. A pinned chunk is kept in memory even under eviction pressure.
func (c *mem2Q) Pin(key string) {
	if c.capacity == 0 {
		return
	}
	c.mu.Lock()
	c.pins[key]++
	c.mu.Unlock()
}

// Unpin releases one Pin.
func (c *mem2Q) Unpin(key string) {
	if c.capacity == 0 {
		return
	}
	c.mu.Lock()
	if c.pins[key] > 0 {
		c.pins[key]--
		if c.pins[key] == 0 {
			delete(c.pins, key)
		}
	}
	c.mu.Unlock()
}

// Get returns the cached block and whether it was present. A hit in main moves
// it to the front (LRU); a hit in the "in" FIFO leaves it in place.
func (c *mem2Q) Get(key string) ([]byte, uint16, bool) {
	if c.capacity == 0 {
		return nil, 0, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.table[key]
	if !ok {
		return nil, 0, false
	}
	e := el.Value.(*entry)
	if e.owner == c.main {
		c.main.MoveToFront(el)
	}
	return e.data, e.filled, true
}

// Put inserts a block. A key currently on the ghost list is admitted straight
// to main; otherwise it enters the in FIFO. Re-inserting a present key is a
// no-op.
func (c *mem2Q) Put(key string, data []byte, filled uint16) {
	if c.capacity == 0 || int64(len(data)) > c.capacity {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.table[key]; ok {
		return
	}
	e := &entry{key: key, data: data, filled: filled}
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

// Merge upserts a chunk carrying newly filled extents (#118). If the key is
// present, its buffer is replaced with data (same full-chunk length, so cache
// size is unchanged) and its extent bitmap gains filled; the caller has already
// produced the merged buffer (copy-on-merge), so a reader holding the old buffer
// keeps seeing consistent, immutable bytes. If absent, it is inserted like Put.
func (c *mem2Q) Merge(key string, data []byte, filled uint16) {
	if c.capacity == 0 || int64(len(data)) > c.capacity {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.table[key]; ok {
		e := el.Value.(*entry)
		c.size += int64(len(data)) - int64(len(e.data))
		if e.owner == c.in {
			c.inSize += int64(len(data)) - int64(len(e.data))
		}
		e.data = data
		e.filled |= filled
		if e.owner == c.main {
			c.main.MoveToFront(el)
		}
		c.evict()
		return
	}
	e := &entry{key: key, data: data, filled: filled}
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
		// Evict already-read chunks before unread (prefetched-but-unconsumed)
		// ones (#55): scan the in FIFO then the main LRU for a readable victim
		// first, and only fall back to an unread victim when no readable chunk
		// is left. Pinned chunks are never evicted; if every candidate is pinned
		// we stop (temporarily over capacity).
		if c.evictScan(false) {
			continue
		}
		if c.evictScan(true) {
			continue
		}
		return
	}
}

// evictScan evicts one victim. wantUnread selects the class: false = readable
// chunks only, true = unread chunks only. It trims the in FIFO tail first (2Q
// scan resistance) then the main LRU tail.
func (c *mem2Q) evictScan(wantUnread bool) bool {
	if el := c.backEvictable(c.in, wantUnread); el != nil {
		c.evictInEl(el, wantUnread)
		return true
	}
	if el := c.backEvictable(c.main, wantUnread); el != nil {
		c.evictMainEl(el, wantUnread)
		return true
	}
	return false
}

// backEvictable returns the rearmost unpinned element of l whose unread class
// matches wantUnread.
func (c *mem2Q) backEvictable(l *list.List, wantUnread bool) *list.Element {
	for el := l.Back(); el != nil; el = el.Prev() {
		key := el.Value.(*entry).key
		if c.pins[key] != 0 {
			continue
		}
		if _, u := c.unread[key]; u == wantUnread {
			return el
		}
	}
	return nil
}

func (c *mem2Q) evictMainEl(el *list.Element, unread bool) {
	e := el.Value.(*entry)
	c.main.Remove(el)
	delete(c.table, e.key)
	c.size -= int64(len(e.data))
	c.dropUnread(e.key, unread)
}

func (c *mem2Q) evictInEl(el *list.Element, unread bool) {
	e := el.Value.(*entry)
	c.in.Remove(el)
	delete(c.table, e.key)
	c.size -= int64(len(e.data))
	c.inSize -= int64(len(e.data))
	c.dropUnread(e.key, unread)
	// Record a ghost (key only) so a later reference promotes it to main.
	c.ghost[e.key] = c.out.PushFront(&entry{key: e.key})
	for c.out.Len() > c.ghostCap {
		g := c.out.Back()
		c.out.Remove(g)
		delete(c.ghost, g.Value.(*entry).key)
	}
}

// dropUnread clears an evicted key's unread flag and signals the thrash metric.
func (c *mem2Q) dropUnread(key string, unread bool) {
	if !unread {
		return
	}
	delete(c.unread, key)
	if c.onEvictUnread != nil {
		c.onEvictUnread(key)
	}
}

// promoteGhost reports whether key is currently a ghost (used by tests).
func (c *mem2Q) isGhost(key string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.ghost[key]
	return ok
}
