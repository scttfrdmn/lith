// SPDX-License-Identifier: Apache-2.0

package blockstore

import "github.com/zeebo/xxh3"

// memCache is the memory tier: a set of independently-locked 2Q shards keyed by
// chunk-key hash. Sharding keeps the per-shard lock (and its eviction scan)
// short so concurrent readers do not serialize on a single memory-tier mutex.
type memCache struct {
	shards []*mem2Q
	mask   uint64
}

// newMemCache splits capacity across shards (rounded up to a power of two).
func newMemCache(capacity int64, shards int) *memCache {
	n := 1
	for n < shards {
		n <<= 1
	}
	per := capacity / int64(n)
	sh := make([]*mem2Q, n)
	for i := range sh {
		sh[i] = newMem2Q(per)
	}
	return &memCache{shards: sh, mask: uint64(n - 1)}
}

func (m *memCache) shard(key string) *mem2Q {
	return m.shards[xxh3.HashString(key)&m.mask]
}

func (m *memCache) Get(key string) ([]byte, uint16, bool)      { return m.shard(key).Get(key) }
func (m *memCache) Put(key string, data []byte, filled uint16) { m.shard(key).Put(key, data, filled) }
func (m *memCache) Merge(key string, data []byte, filled uint16) {
	m.shard(key).Merge(key, data, filled)
}
func (m *memCache) Pin(key string)                              { m.shard(key).Pin(key) }
func (m *memCache) Unpin(key string)                            { m.shard(key).Unpin(key) }
func (m *memCache) PutUnread(key string, data []byte, f uint16) { m.shard(key).PutUnread(key, data, f) }
func (m *memCache) MarkUnread(key string)                       { m.shard(key).MarkUnread(key) }
func (m *memCache) ClearUnread(key string)                      { m.shard(key).ClearUnread(key) }

// setOnEvictUnread installs the thrash callback on every shard.
func (m *memCache) setOnEvictUnread(fn func(key string)) {
	for _, s := range m.shards {
		s.onEvictUnread = fn
	}
}
