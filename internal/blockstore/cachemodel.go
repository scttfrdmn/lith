// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Scott Friedman

package blockstore

// CacheModel drives the REAL memory tier from an offline replay.
//
// cmd/lith-pfreplay's shared-cache accounting has carried the assumption "no eviction,
// therefore an upper bound on redemption" through three gates (#256). Lifting it needs a
// model of what the memory tier evicts — and the one thing that model must not do is be a
// reimplementation. Two findings make that concrete:
//
//   - #267: an offline replay that did not know it ran a different program (it assumed a
//     223-block window where the mount never exceeded 17) over-reported dispatch 2.70x.
//   - #279: the tier is NOT textbook 2Q. `mem2Q` computes a classic Kin bound on its
//     probationary queue and never enforces it, so until the cache is over capacity it
//     behaves as a FIFO and `main` stays empty. A model written from the Johnson & Shasha
//     paper — or from this package's own type comment — would be MORE scan-resistant than
//     lith is, and therefore optimistic about hit rates.
//
// So the replay drives `memCache` itself, sharded exactly as production shards it, rather
// than a model of it. Drift is impossible by construction; when the policy changes, the
// replay changes with it.
//
// Memory: a model of a 32 GB cache cannot allocate 32 GB. Every chunk handed to the tier
// is a sub-slice of ONE shared backing buffer, so `len(data)` — the only thing the tier's
// size accounting reads — is exact while the model stays O(chunkSize). The tier never
// writes to chunk data, and the model never reads it back, so sharing is safe here and
// nowhere else.
type CacheModel struct {
	mem    *memCache
	back   []byte           // one buffer, sub-sliced for every chunk
	sizes  map[string]int64 // chunk key -> byte length, for costing an eviction
	filled map[string]bool  // chunk key -> has been fetched at least once, ever

	fills        int64 // Fill calls that actually populated the tier
	refills      int64 // Fill calls for a chunk that had been fetched before and was evicted
	refillBytes  int64
	reads        int64
	hits         int64
	unreadEvicts int64
	unreadBytes  int64
}

// NewCacheModel builds a model of a memory tier of `capacity` bytes across `shards`
// shards. Pass production's values (blockstore.go uses 64 shards); the sharding is part
// of the policy, since capacity is divided and eviction is per shard, so a chunk's
// effective capacity is capacity/shards and an unlucky key hash evicts while other shards
// have room.
func NewCacheModel(capacity int64, shards int, chunkSize int64) *CacheModel {
	m := &CacheModel{
		mem:    newMemCache(capacity, shards),
		back:   make([]byte, chunkSize),
		sizes:  map[string]int64{},
		filled: map[string]bool{},
	}
	m.mem.setOnEvictUnread(m.onEvictUnread)
	return m
}

func (m *CacheModel) onEvictUnread(ck string) {
	m.unreadEvicts++
	m.unreadBytes += m.sizes[ck]
}

// Fill records a fetch of one chunk, mirroring BlockStore.complete: every fill Merges,
// and a prefetch fill is additionally marked unread so the tier evicts it last (#55) and
// so an eviction before any demand read is counted as a prefetch that could never be
// redeemed.
//
// A Fill for a chunk that was fetched earlier and is no longer resident is a RE-FETCH —
// the quantity the no-eviction assumption hid. It is counted rather than inferred, since
// the caller knows only that it is filling; the model knows whether it has filled this
// chunk before.
func (m *CacheModel) Fill(ck string, n int64, isPrefetch bool) {
	if n <= 0 {
		return
	}
	if n > int64(len(m.back)) {
		n = int64(len(m.back))
	}
	if _, _, resident := m.mem.Get(ck); !resident {
		if m.filled[ck] {
			m.refills++
			m.refillBytes += n
		}
	} else {
		// Already resident: the real store's singleflight would not fetch at all.
		return
	}
	m.sizes[ck] = n
	m.filled[ck] = true
	m.fills++
	m.mem.Merge(ck, m.back[:n], fullExtents)
	if isPrefetch {
		m.mem.MarkUnread(ck)
	}
}

// Read records a demand read of one chunk and reports whether it was resident. On a hit
// of a prefetched chunk it clears the unread flag, mirroring notePrefetchHit: the chunk
// has been consumed, so eviction may now prefer it over still-unread ones.
func (m *CacheModel) Read(ck string) bool {
	m.reads++
	if _, _, ok := m.mem.Get(ck); ok {
		m.hits++
		m.mem.ClearUnread(ck)
		return true
	}
	return false
}

// CacheModelStats is what the replay reports. Every field is a count of something the
// no-eviction assumption fixed at zero.
type CacheModelStats struct {
	Fills        int64
	Refills      int64 // fetches of a chunk that had been fetched before: pure re-fetch
	RefillBytes  int64
	Reads        int64
	Hits         int64
	UnreadEvicts int64 // prefetched chunks evicted before any demand read consumed them
	UnreadBytes  int64
}

func (m *CacheModel) Stats() CacheModelStats {
	return CacheModelStats{
		Fills: m.fills, Refills: m.refills, RefillBytes: m.refillBytes,
		Reads: m.reads, Hits: m.hits,
		UnreadEvicts: m.unreadEvicts, UnreadBytes: m.unreadBytes,
	}
}

// Binding reports whether eviction affected this replay at all. When it is false the
// no-eviction upper bound was exact on this trace, which is a measurement rather than the
// assertion three gates have been carrying.
func (m *CacheModel) Binding() bool { return m.refills > 0 || m.unreadEvicts > 0 }
