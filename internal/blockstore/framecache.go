// SPDX-License-Identifier: Apache-2.0

package blockstore

import (
	"container/list"
	"sync"
)

// frameCache is a bounded LRU of decoded CargoShip zstd frames, keyed by the
// chunk object key and the frame's compressed offset (unique per frame within
// the object). It lets a sequential tree walk — where many small files map into
// one ~16 MiB frame — fetch and decode each frame exactly once: the first fill
// covering a frame GETs its compressed span and decodes it into the cache; every
// later fill covering the same frame is served with no GET and no decode (#137,
// the frame-refetch / byte over-fetch fixed here).
//
// A per-frame singleflight is essential, not incidental: a 16 MiB frame spans 16
// one-MiB cache chunks, which the block store fills with SEPARATE, concurrent
// prefetch operations. Without singleflight those concurrent fills each miss the
// cache and re-fetch+re-decode the same frame; with it, the first claims the
// frame and the rest wait and are served its decoded bytes.
//
// The cache is bounded by a byte budget and evicts least-recently-used frames. A
// single frame larger than the whole budget is decoded and served (waiters get
// its bytes) but not retained, so it cannot pin the budget — such a giant frame
// belongs in a frameless chunk anyway.
type frameCache struct {
	mu       sync.Mutex
	budget   int64
	used     int64
	byKey    map[string]*list.Element
	lru      *list.List              // front = most recently used
	inflight map[string]*frameFlight // frames currently being fetched+decoded
}

// frameEntry is one cached decoded frame. etag is the chunk object's ETag as
// returned by the GET that decoded it, so a fill served entirely from cache can
// still return the ETag the fill lane verifies against the index's ETagHash.
type frameEntry struct {
	key  string
	data []byte
	etag string
}

// frameFlight is an in-flight frame fetch+decode. Waiters block on done, then
// read data/etag/err (set before done is closed).
type frameFlight struct {
	done chan struct{}
	data []byte
	etag string
	err  error
}

func newFrameCache(budget int64) *frameCache {
	if budget <= 0 {
		return nil
	}
	return &frameCache{
		budget:   budget,
		byKey:    make(map[string]*list.Element),
		lru:      list.New(),
		inflight: make(map[string]*frameFlight),
	}
}

// acquire resolves key to one of three outcomes, atomically:
//   - cached=true: the decoded bytes/etag are returned (LRU refreshed);
//   - mine=true: the caller owns the fetch and must call fulfill(fl, …);
//   - otherwise fl is an in-flight fetch owned by another caller: wait on
//     fl.done, then use fl.data/fl.etag (or fl.err).
//
// Safe on a nil cache: it always reports mine=true with a throwaway flight, so a
// disabled cache degrades to fetch-every-time with no caching.
func (c *frameCache) acquire(key string) (data []byte, etag string, cached bool, fl *frameFlight, mine bool) {
	if c == nil {
		return nil, "", false, &frameFlight{done: make(chan struct{})}, true
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.byKey[key]; ok {
		c.lru.MoveToFront(el)
		e := el.Value.(*frameEntry)
		return e.data, e.etag, true, nil, false
	}
	if f, ok := c.inflight[key]; ok {
		return nil, "", false, f, false
	}
	f := &frameFlight{done: make(chan struct{})}
	c.inflight[key] = f
	return nil, "", false, f, true
}

// fulfill completes an owned flight: it publishes data/etag/err to waiters,
// clears the in-flight marker, and (on success, if it fits) inserts the decoded
// frame into the LRU. Safe on a nil cache.
func (c *frameCache) fulfill(key string, fl *frameFlight, data []byte, etag string, err error) {
	fl.data, fl.etag, fl.err = data, etag, err
	if c != nil {
		c.mu.Lock()
		delete(c.inflight, key)
		if err == nil && int64(len(data)) <= c.budget {
			c.insertLocked(key, data, etag)
		}
		c.mu.Unlock()
	}
	close(fl.done)
}

// insertLocked adds a decoded frame, evicting LRU entries until it fits. Caller
// holds c.mu. A key already present just refreshes recency.
func (c *frameCache) insertLocked(key string, data []byte, etag string) {
	if el, ok := c.byKey[key]; ok {
		c.lru.MoveToFront(el)
		return
	}
	for c.used+int64(len(data)) > c.budget && c.lru.Len() > 0 {
		back := c.lru.Back()
		be := back.Value.(*frameEntry)
		c.used -= int64(len(be.data))
		c.lru.Remove(back)
		delete(c.byKey, be.key)
	}
	c.byKey[key] = c.lru.PushFront(&frameEntry{key: key, data: data, etag: etag})
	c.used += int64(len(data))
}
