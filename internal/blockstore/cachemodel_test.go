// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Scott Friedman

package blockstore

import (
	"fmt"
	"testing"
)

const cmMiB = 1 << 20

// The whole point of the model is that eviction is no longer assumed away, so the first
// thing to assert is that it actually happens and is actually counted. A model that
// silently never evicted would report Binding() == false on every trace and look exactly
// like the good news it is supposed to be testing for.
func TestCacheModelEvictsAndCountsRefetches(t *testing.T) {
	m := NewCacheModel(4*cmMiB, 1, cmMiB)
	for i := 0; i < 8; i++ {
		m.Fill(fmt.Sprintf("k/%d", i), cmMiB, false)
	}
	// Eight 1 MiB chunks into a 4 MiB cache: the earliest must be gone.
	if m.Read("k/0") {
		t.Fatal("k/0 still resident after 8 MiB of fills into a 4 MiB cache: the model is not evicting")
	}
	if !m.Read("k/7") {
		t.Error("k/7 (most recent) should still be resident")
	}
	// Re-fetching an evicted chunk is the quantity the no-eviction assumption hid.
	m.Fill("k/0", cmMiB, false)
	s := m.Stats()
	if s.Refills != 1 {
		t.Errorf("Refills = %d, want 1", s.Refills)
	}
	if s.RefillBytes != cmMiB {
		t.Errorf("RefillBytes = %d, want %d", s.RefillBytes, cmMiB)
	}
	if !m.Binding() {
		t.Error("Binding() = false after a re-fetch: eviction plainly affected this replay")
	}
}

// #280, now fixed, and this test is the one that predicted its own replacement value.
//
// Before the fix the production fill path did Merge (which evicts) and only then
// MarkUnread, so a just-filled prefetch chunk was resident and unflagged across eviction --
// and backEvictable(in, wantUnread=false) reads "not flagged" as "already read". Once a
// shard held nothing but unread chunks, the newly arrived fetch was the only eligible
// victim, so it was discarded on arrival while the older correctly-flagged ones survived,
// and the thrash counter never fired because evictInEl saw unread=false.
//
// MergeUnread sets the flag under the same lock acquisition, before evict(). So now the
// NEWEST fetch survives, the oldest unread chunks are evicted, and every one of those
// evictions is counted.
func TestUnreadFlagIsSetBeforeEvictionCanSeeTheChunk(t *testing.T) {
	m := NewCacheModel(2*cmMiB, 1, cmMiB)
	for i := 0; i < 6; i++ {
		m.Fill(fmt.Sprintf("p/%d", i), cmMiB, true)
	}
	// 6 MiB of prefetch into a 2 MiB shard: four must be evicted, and all four counted.
	if s := m.Stats(); s.UnreadEvicts != 4 {
		t.Errorf("UnreadEvicts = %d, want 4: every unread eviction must reach the thrash counter", s.UnreadEvicts)
	} else if s.UnreadBytes != 4*cmMiB {
		t.Errorf("UnreadBytes = %d, want %d", s.UnreadBytes, 4*cmMiB)
	}
	// The two most recent fetches survive; a fetch is no longer discarded on arrival.
	for _, k := range []string{"p/4", "p/5"} {
		if !m.Read(k) {
			t.Errorf("%s not resident: the newest fetch must not be evicted on arrival (#280)", k)
		}
	}
	// And the oldest are the ones gone, which is the intended eviction order.
	m2 := NewCacheModel(2*cmMiB, 1, cmMiB)
	for i := 0; i < 6; i++ {
		m2.Fill(fmt.Sprintf("p/%d", i), cmMiB, true)
	}
	for _, k := range []string{"p/0", "p/1"} {
		if m2.Read(k) {
			t.Errorf("%s still resident: the oldest unread chunks should be the victims", k)
		}
	}
}

// Eviction prefers an already-READ victim over a still-unread one (#55). If the model did
// not inherit that, a prefetched chunk would be evicted ahead of a consumed one and every
// follow-through number would be pessimistic in a way that looks like a finding.
func TestCacheModelPrefersReadVictims(t *testing.T) {
	m := NewCacheModel(4*cmMiB, 1, cmMiB)
	m.Fill("pre/0", cmMiB, true) // prefetched, never read: must be evicted last
	for i := 0; i < 3; i++ {
		k := fmt.Sprintf("dem/%d", i)
		m.Fill(k, cmMiB, false)
		m.Read(k) // consumed, so a preferred victim
	}
	// Now overflow by one chunk. The victim should be a consumed one, not the unread.
	m.Fill("dem/3", cmMiB, false)
	if s := m.Stats(); s.UnreadEvicts != 0 {
		t.Errorf("UnreadEvicts = %d, want 0: a consumed chunk should be evicted before an unread one", s.UnreadEvicts)
	}
	if !m.Read("pre/0") {
		t.Error("the unread prefetched chunk was evicted while consumed chunks were resident")
	}
}

// Sharding is part of the policy, not an implementation detail: capacity is divided 64
// ways and eviction is per shard, so a chunk's effective capacity is capacity/shards and
// an unlucky key hash evicts while other shards sit idle. A model that pooled capacity
// into one cache would over-report residency.
func TestCacheModelShardingIsPartOfThePolicy(t *testing.T) {
	const n = 64
	fill := func(shards int) CacheModelStats {
		m := NewCacheModel(n*cmMiB, shards, cmMiB)
		for i := 0; i < n; i++ {
			m.Fill(fmt.Sprintf("k/%d", i), cmMiB, false)
		}
		for i := 0; i < n; i++ {
			m.Read(fmt.Sprintf("k/%d", i))
		}
		return m.Stats()
	}
	pooled := fill(1)
	sharded := fill(n)
	if pooled.Hits != n {
		t.Errorf("unsharded: hits = %d, want %d — %d MiB of chunks fit in %d MiB",
			pooled.Hits, n, n, n)
	}
	// 64 chunks hashed across 64 shards of one chunk each: the distribution is not a
	// permutation, so some shard takes two and evicts.
	if sharded.Hits >= pooled.Hits {
		t.Errorf("sharded hits = %d >= pooled %d: sharding must be able to evict where a pooled cache of the same total capacity would not",
			sharded.Hits, pooled.Hits)
	}
}
