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

// #280, pinned as a regression test rather than as an expectation.
//
// I wrote this case expecting a cache overflowed with nothing but prefetched chunks to
// report unread evictions, since #55 says unread chunks are evicted last. It reports
// ZERO, and the model is right: the production fill path Merges (which evicts) and only
// then MarkUnreads, so the chunk being inserted is not yet flagged and is therefore a
// valid "already-read" victim — the newest fetch is discarded on arrival while the
// correctly-flagged older ones survive. `PutUnread`, which makes insert-and-flag atomic,
// exists for exactly this and has no callers.
//
// Asserted as-is so the model keeps matching production. When #280 is fixed this test
// must fail, and the assertions below say what it should then say.
func TestCacheModelReproducesTheUnflaggedEvictionWindow(t *testing.T) {
	m := NewCacheModel(2*cmMiB, 1, cmMiB)
	for i := 0; i < 6; i++ {
		m.Fill(fmt.Sprintf("p/%d", i), cmMiB, true)
	}
	// Today (#280): the two chunks inserted before the cache filled are the only
	// survivors, and every later fetch was evicted on insertion.
	for _, k := range []string{"p/0", "p/1"} {
		if !m.Read(k) {
			t.Errorf("%s not resident: the early, correctly-flagged chunks should survive", k)
		}
	}
	// Reads above would clear the unread flag, so re-check residency of the rest via a
	// fresh model to keep the two assertions independent.
	m2 := NewCacheModel(2*cmMiB, 1, cmMiB)
	for i := 0; i < 6; i++ {
		m2.Fill(fmt.Sprintf("p/%d", i), cmMiB, true)
	}
	if s := m2.Stats(); s.UnreadEvicts != 0 {
		t.Errorf("UnreadEvicts = %d; #280 says 0 today (the victim is unflagged, so the thrash "+
			"counter cannot fire). If this now reports evictions, #280 is fixed and this test "+
			"should assert UnreadEvicts == 4 instead", s.UnreadEvicts)
	}
	for i := 2; i < 6; i++ {
		if m2.Read(fmt.Sprintf("p/%d", i)) {
			t.Errorf("p/%d resident; #280 says a chunk fetched into a full all-unread shard is "+
				"evicted on insertion", i)
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
