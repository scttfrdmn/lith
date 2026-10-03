// SPDX-License-Identifier: Apache-2.0

package blockstore

import (
	"strconv"
	"testing"
)

// #307: a shard smaller than one chunk refuses EVERY store, silently. At a fixed 64 shards
// that killed the whole memory tier below 64 MiB, and the default being 25% of system RAM made
// the threshold a 256 MiB machine -- a container, a CI runner, a sidecar.
//
// The table is the arithmetic from the issue, plus the cases either side of it.
func TestMemCacheShardsScaleToHoldAChunk(t *testing.T) {
	for _, tc := range []struct {
		capMiB     int64
		wantShards int
	}{
		// The dead range: 64 shards would give 256 KiB and 512 KiB per shard.
		{16, 16},
		{32, 32},
		// The exact boundary, where 64 shards give precisely one chunk each.
		{64, 64},
		// Above it, nothing changes: the shard count was never the binding constraint.
		{96, 64},
		{256, 64},
		{8192, 64},
		// Small but still able to hold chunks.
		{4, 4},
		{1, 1},
	} {
		t.Run(strconv.FormatInt(tc.capMiB, 10)+"MiB", func(t *testing.T) {
			c := newMemCache(tc.capMiB<<20, 64)
			shards, perShard := c.geometry()
			if shards != tc.wantShards {
				t.Errorf("shards = %d, want %d", shards, tc.wantShards)
			}
			// THE PROPERTY, independent of the table: every shard must hold a chunk, or the
			// tier accepts nothing.
			if perShard < ChunkSize {
				t.Errorf("%d bytes per shard is below ChunkSize %d — every store is refused",
					perShard, ChunkSize)
			}
			// And the shard count must stay a power of two, because shard() masks.
			if shards&(shards-1) != 0 {
				t.Errorf("shards = %d is not a power of two; shard() masks with mask=n-1", shards)
			}
		})
	}
}

// The tier must actually STORE at a capacity that used to be dead. This is the regression the
// arithmetic above is a proxy for.
func TestMemCacheStoresAtAFormerlyDeadCapacity(t *testing.T) {
	// 16 MiB: 256 KiB per shard at a fixed 64, which refused everything.
	c := newMemCache(16<<20, 64)
	blk := make([]byte, ChunkSize)
	for i := 0; i < 8; i++ {
		c.Put("k"+strconv.Itoa(i), blk, fullExtents)
	}
	stored := 0
	for i := 0; i < 8; i++ {
		if _, _, ok := c.Get("k" + strconv.Itoa(i)); ok {
			stored++
		}
	}
	if stored == 0 {
		t.Fatal("a 16 MiB tier stored none of 8 chunks: the tier is still dead at a capacity " +
			"that should hold them")
	}
	// Sharding is by hash, so not all 8 land in distinct shards and a few may evict each
	// other; the point is that the tier is not refusing everything.
	if stored < 4 {
		t.Errorf("a 16 MiB tier retained only %d of 8 chunks; expected most of them", stored)
	}
}

// A capacity below one chunk cannot be fixed by scaling, and must be reportable rather than
// silent. This is the case the mount warns on.
func TestMemCacheBelowOneChunkIsDetectable(t *testing.T) {
	c := newMemCache(ChunkSize/2, 64)
	shards, perShard := c.geometry()
	if shards != 1 {
		t.Errorf("shards = %d, want 1: scaling must bottom out rather than loop", shards)
	}
	if perShard >= ChunkSize {
		t.Fatalf("fixture: perShard = %d, this is not the degenerate case", perShard)
	}
	// Detectable is the requirement; the mount turns this into a warning.
	blk := make([]byte, ChunkSize)
	c.Put("k", blk, fullExtents)
	if _, _, ok := c.Get("k"); ok {
		t.Error("a sub-chunk tier stored a chunk; the refusal this warns about is gone, so " +
			"the warning is now wrong")
	}

	// Zero is caching deliberately disabled, not degenerate, and must not be confused with it.
	z := newMemCache(0, 64)
	if _, per := z.geometry(); per != 0 {
		t.Errorf("a zero-capacity tier reports %d bytes per shard, want 0", per)
	}
}
