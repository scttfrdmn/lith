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

// newMemCache splits capacity across shards (rounded up to a power of two), reducing the
// shard count as needed so that every shard can hold at least one chunk.
//
// THE SCALING IS NOT AN OPTIMIZATION, it is a correctness fix (#307). mem2Q.Put and
// mem2Q.merge both begin `if c.capacity == 0 || len(data) > c.capacity { return }`, so a shard
// smaller than ChunkSize refuses EVERY store -- silently, with no error and no metric. At a
// fixed 64 shards that killed the whole memory tier below 64 MiB:
//
//	--mem-cache 16 MiB -> 256 KiB per shard -> dead
//	--mem-cache 32 MiB -> 512 KiB per shard -> dead
//	--mem-cache 64 MiB -> 1 MiB   per shard -> the exact boundary
//
// The default is 25% of system RAM, so the threshold was a machine with 256 MiB: a small
// container, a CI runner, a constrained sidecar. Below it every read missed, every re-read
// re-fetched, and lith_mem_hit_total sat at zero, which reads as a cold workload rather than a
// broken tier. It was found by shrinking a test fixture, where three assertions broke because
// committed prefetch bytes never dropped on consumption -- notePrefetchHit only fires when the
// demand read finds the chunk resident, and nothing was ever resident.
//
// Scaling down trades lock contention for a working cache, which is the right trade: a tier
// too small to give 64 shards a chunk each is also too small for 64 readers to contend over.
// A capacity below one chunk cannot be fixed this way and is reported instead -- see
// BlockStore.MemCacheGeometry and the mount's warning.
func newMemCache(capacity int64, shards int) *memCache {
	n := 1
	for n < shards {
		n <<= 1
	}
	for n > 1 && capacity/int64(n) < ChunkSize {
		n >>= 1
	}
	per := capacity / int64(n)
	sh := make([]*mem2Q, n)
	for i := range sh {
		sh[i] = newMem2Q(per)
	}
	return &memCache{shards: sh, mask: uint64(n - 1)}
}

// geometry reports the realized shard count and per-shard capacity.
func (m *memCache) geometry() (shards int, perShard int64) {
	if m == nil || len(m.shards) == 0 {
		return 0, 0
	}
	return len(m.shards), m.shards[0].capacity
}

func (m *memCache) shard(key string) *mem2Q {
	return m.shards[xxh3.HashString(key)&m.mask]
}

func (m *memCache) Get(key string) ([]byte, uint16, bool)      { return m.shard(key).Get(key) }
func (m *memCache) Put(key string, data []byte, filled uint16) { m.shard(key).Put(key, data, filled) }
func (m *memCache) Merge(key string, data []byte, filled uint16) {
	m.shard(key).Merge(key, data, filled)
}
func (m *memCache) Pin(key string)   { m.shard(key).Pin(key) }
func (m *memCache) Unpin(key string) { m.shard(key).Unpin(key) }
func (m *memCache) MergeUnread(key string, data []byte, f uint16) {
	m.shard(key).MergeUnread(key, data, f)
}
func (m *memCache) MarkUnread(key string) { m.shard(key).MarkUnread(key) }

// UnreadBytes sums the resident-unread bytes across shards (#313).
func (m *memCache) UnreadBytes() int64 {
	var n int64
	for _, sh := range m.shards {
		n += sh.UnreadBytes()
	}
	return n
}
func (m *memCache) ClearUnread(key string) { m.shard(key).ClearUnread(key) }

// setOnEvictUnread installs the thrash callback on every shard.
func (m *memCache) setOnEvictUnread(fn func(key string)) {
	for _, s := range m.shards {
		s.onEvictUnread = fn
	}
}
