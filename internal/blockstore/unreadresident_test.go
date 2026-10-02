// SPDX-License-Identifier: Apache-2.0

package blockstore

import (
	"math/rand"
	"strconv"
	"testing"
)

// unreadBytes is a MAINTAINED counter, incremented and decremented at six call sites across
// MarkUnread, ClearUnread, merge (insert, length-change, and newly-unread), and both eviction
// paths. That is the class of bookkeeping that drifts, and it drifted once already in this
// package on the stream count (#311).
//
// So the invariant is checked against a recomputation from the authoritative state rather
// than against expected values: unreadBytes must equal the summed length of every resident
// entry flagged unread. A scan is wrong for production (O(resident) under the shard lock) and
// exactly right for a test.
func sumUnread(c *mem2Q) int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	var n int64
	for key := range c.unread {
		el, ok := c.table[key]
		if !ok {
			// A key in `unread` with no table entry is itself a bug: the two are supposed to
			// be dropped together.
			n = -1 << 40
			break
		}
		n += int64(len(el.Value.(*entry).data))
	}
	return n
}

func checkUnread(t *testing.T, c *mem2Q, when string) {
	t.Helper()
	if got, want := c.UnreadBytes(), sumUnread(c); got != want {
		t.Fatalf("%s: unreadBytes = %d, recomputed = %d", when, got, want)
	}
	if c.UnreadBytes() < 0 {
		t.Fatalf("%s: unreadBytes went negative (%d)", when, c.UnreadBytes())
	}
	if c.UnreadBytes() > c.capacity {
		t.Fatalf("%s: unreadBytes %d exceeds the shard capacity %d — resident-unread cannot "+
			"exceed what the tier can hold, which is the whole premise of the gauge",
			when, c.UnreadBytes(), c.capacity)
	}
}

func TestUnreadResidentBytesTracksTheResidentSet(t *testing.T) {
	c := newMem2Q(10 * mib)
	blk := make([]byte, mib)

	checkUnread(t, c, "fresh")

	// Insert unread, which is what a prefetch fill does.
	for i := 0; i < 4; i++ {
		c.MergeUnread("k"+strconv.Itoa(i), blk, fullExtents)
	}
	if got := c.UnreadBytes(); got != 4*mib {
		t.Errorf("after four unread fills: %d, want %d", got, 4*mib)
	}
	checkUnread(t, c, "four unread fills")

	// A demand read clears one.
	c.ClearUnread("k0")
	if got := c.UnreadBytes(); got != 3*mib {
		t.Errorf("after one read: %d, want %d", got, 3*mib)
	}
	checkUnread(t, c, "one consumed")

	// Clearing twice must not double-subtract.
	c.ClearUnread("k0")
	if got := c.UnreadBytes(); got != 3*mib {
		t.Errorf("after a repeated clear: %d, want %d (must be idempotent)", got, 3*mib)
	}

	// Marking twice must not double-add.
	c.MarkUnread("k1")
	if got := c.UnreadBytes(); got != 3*mib {
		t.Errorf("after re-marking an already-unread chunk: %d, want %d", got, 3*mib)
	}
	checkUnread(t, c, "idempotent mark")

	// Marking an absent key is a no-op: nothing resident, nothing to count.
	c.MarkUnread("absent")
	checkUnread(t, c, "mark of an absent key")

	// A read chunk inserted plainly contributes nothing.
	c.Put("read0", blk, fullExtents)
	if got := c.UnreadBytes(); got != 3*mib {
		t.Errorf("after a plain Put: %d, want %d", got, 3*mib)
	}
	checkUnread(t, c, "plain Put")

	// A short trailing chunk contributes its real length, not a whole block.
	short := make([]byte, mib/4)
	c.MergeUnread("tail", short, fullExtents)
	if got, want := c.UnreadBytes(), 3*mib+mib/4; got != want {
		t.Errorf("after a short trailing chunk: %d, want %d", got, want)
	}
	checkUnread(t, c, "short trailing chunk")
}

// Eviction is the path that matters: it is what makes the counter fall under pressure, and the
// entry is removed from the table BEFORE the unread flag is dropped, so the size has to be
// carried rather than looked up. Getting that wrong leaks the counter upward forever, which
// would make the gauge read above the tier it is supposed to be compared against.
func TestUnreadResidentBytesFallsOnEviction(t *testing.T) {
	c := newMem2Q(4 * mib)
	blk := make([]byte, mib)

	// Overfill with unread chunks. Capacity bounds the resident set, so the counter must
	// settle at capacity rather than at everything ever inserted.
	for i := 0; i < 32; i++ {
		c.MergeUnread("k"+strconv.Itoa(i), blk, fullExtents)
		checkUnread(t, c, "overfilling with unread chunks")
	}
	if got := c.UnreadBytes(); got != 4*mib {
		t.Errorf("after inserting 32 MiB of unread into a 4 MiB shard: %d, want %d "+
			"(the resident-unread total is bounded by the tier)", got, 4*mib)
	}

	// Consuming them must drain the counter even as eviction continues.
	for i := 0; i < 32; i++ {
		c.ClearUnread("k" + strconv.Itoa(i))
	}
	if got := c.UnreadBytes(); got != 0 {
		t.Errorf("after consuming every chunk: %d, want 0", got)
	}
	checkUnread(t, c, "fully drained")
}

// A randomized sequence over the whole surface, because the six call sites interact: merge
// can change a resident chunk's length, set its flag for the first time, or insert it; and any
// of those can trigger eviction of a different chunk through either queue.
func TestUnreadResidentBytesInvariantUnderRandomOps(t *testing.T) {
	c := newMem2Q(6 * mib)
	rng := rand.New(rand.NewSource(1))
	sizes := []int64{mib / 4, mib / 2, mib}

	for step := 0; step < 4000; step++ {
		key := "k" + strconv.Itoa(rng.Intn(24))
		switch rng.Intn(6) {
		case 0:
			c.MergeUnread(key, make([]byte, sizes[rng.Intn(len(sizes))]), fullExtents)
		case 1:
			c.Merge(key, make([]byte, sizes[rng.Intn(len(sizes))]), fullExtents)
		case 2:
			c.Put(key, make([]byte, sizes[rng.Intn(len(sizes))]), fullExtents)
		case 3:
			c.MarkUnread(key)
		case 4:
			c.ClearUnread(key)
		case 5:
			c.Get(key)
		}
		if step%37 == 0 {
			checkUnread(t, c, "random ops at step "+strconv.Itoa(step))
		}
	}
	checkUnread(t, c, "after 4000 random ops")
}
