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
// the frame-refetch / byte over-fetch fixed here). The cache is bounded by a
// byte budget and evicts least-recently-used frames. A single frame larger than
// the whole budget is not cached (served once and dropped) so it cannot pin the
// budget — such a giant frame belongs in a frameless chunk anyway.
type frameCache struct {
	mu     sync.Mutex
	budget int64
	used   int64
	byKey  map[string]*list.Element
	lru    *list.List // front = most recently used
}

// frameEntry is one cached decoded frame. etag is the chunk object's ETag as
// returned by the GET that decoded it, so a fill served entirely from cache can
// still return the ETag the fill lane verifies against the index's ETagHash.
type frameEntry struct {
	key  string
	data []byte
	etag string
}

func newFrameCache(budget int64) *frameCache {
	if budget <= 0 {
		return nil
	}
	return &frameCache{budget: budget, byKey: make(map[string]*list.Element), lru: list.New()}
}

// get returns the decoded bytes and ETag for key, moving it to the front of the
// LRU. ok is false on a miss. Safe on a nil cache.
func (c *frameCache) get(key string) (data []byte, etag string, ok bool) {
	if c == nil {
		return nil, "", false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.byKey[key]
	if !ok {
		return nil, "", false
	}
	c.lru.MoveToFront(el)
	e := el.Value.(*frameEntry)
	return e.data, e.etag, true
}

// has reports whether key is cached, without touching the LRU order. Used to
// decide whether a frame is part of a to-fetch run. Safe on a nil cache.
func (c *frameCache) has(key string) bool {
	if c == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.byKey[key]
	return ok
}

// put inserts a decoded frame, evicting LRU entries until it fits. A frame
// larger than the whole budget is not cached. Re-inserting a present key just
// refreshes its recency. Safe on a nil cache.
func (c *frameCache) put(key string, data []byte, etag string) {
	if c == nil || int64(len(data)) > c.budget {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
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
	e := &frameEntry{key: key, data: data, etag: etag}
	c.byKey[key] = c.lru.PushFront(e)
	c.used += int64(len(data))
}
