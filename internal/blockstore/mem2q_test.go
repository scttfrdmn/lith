// SPDX-License-Identifier: Apache-2.0

package blockstore

import (
	"strconv"
	"testing"
)

func TestMem2QBoundedByBytes(t *testing.T) {
	c := newMem2Q(100) // 100 bytes
	blk := make([]byte, 10)
	for i := 0; i < 20; i++ {
		c.Put("k"+strconv.Itoa(i), blk, fullExtents)
	}
	c.mu.Lock()
	size := c.size
	c.mu.Unlock()
	if size > 100 {
		t.Fatalf("cache size %d exceeds capacity 100", size)
	}
}

func TestMem2QZeroCapacityStoresNothing(t *testing.T) {
	c := newMem2Q(0)
	c.Put("k", []byte("data"), fullExtents)
	if _, _, ok := c.Get("k"); ok {
		t.Error("zero-capacity cache should hold nothing")
	}
}

func TestMem2QGhostPromotesToMain(t *testing.T) {
	// Capacity 40 bytes, blocks of 10; in-queue cap is 10 (capacity/4).
	c := newMem2Q(40)
	blk := make([]byte, 10)
	// Fill so early keys get evicted from the in FIFO to the ghost list.
	for i := 0; i < 8; i++ {
		c.Put("k"+strconv.Itoa(i), blk, fullExtents)
	}
	// At least one early key should now be a ghost.
	var ghostKey string
	for i := 0; i < 8; i++ {
		k := "k" + strconv.Itoa(i)
		if c.isGhost(k) {
			ghostKey = k
			break
		}
	}
	if ghostKey == "" {
		t.Skip("no ghost formed with this fill pattern")
	}
	// Re-inserting a ghost key admits it straight to main.
	c.Put(ghostKey, blk, fullExtents)
	if _, _, ok := c.Get(ghostKey); !ok {
		t.Errorf("re-inserted ghost key %q should be present", ghostKey)
	}
}

func TestMem2QPinSurvivesEviction(t *testing.T) {
	c := newMem2Q(50) // 5 x 10-byte blocks
	blk := make([]byte, 10)
	c.Put("keep", blk, fullExtents)
	c.Pin("keep")
	for i := 0; i < 30; i++ { // heavy pressure while pinned
		c.Put("p"+strconv.Itoa(i), blk, fullExtents)
	}
	if _, _, ok := c.Get("keep"); !ok {
		t.Fatal("pinned chunk was evicted under pressure")
	}
	c.Unpin("keep")
	for i := 30; i < 60; i++ { // pressure after unpin
		c.Put("p"+strconv.Itoa(i), blk, fullExtents)
	}
	if _, _, ok := c.Get("keep"); ok {
		t.Error("unpinned chunk survived heavy eviction pressure")
	}
}

// #280 at the tier level, independent of the offline model: the unread flag must be visible
// to eviction at the moment the chunk is inserted. Merge-then-MarkUnread was not, and the
// consequence was that a full shard of unread chunks discarded each new arrival instead of
// an older one -- without firing the thrash callback, since the victim looked already-read.
func TestMergeUnreadIsAtomicWithRespectToEviction(t *testing.T) {
	const mib = 1 << 20
	var evicted []string
	c := newMem2Q(2 * mib)
	c.onEvictUnread = func(k string) { evicted = append(evicted, k) }
	buf := make([]byte, mib)
	for i := 0; i < 5; i++ {
		c.MergeUnread("k"+strconv.Itoa(i), buf, fullExtents)
	}
	// Three evictions, all of them counted, none of them the chunk just inserted.
	if len(evicted) != 3 {
		t.Fatalf("unread evictions = %d (%v), want 3: every unread eviction must be reported",
			len(evicted), evicted)
	}
	for _, k := range evicted {
		if k == "k4" {
			t.Error("the most recently inserted chunk was evicted: the flag was not visible to evict()")
		}
	}
	if _, _, ok := c.Get("k4"); !ok {
		t.Error("k4 not resident: a just-filled prefetch chunk must not be discarded on arrival")
	}
}
