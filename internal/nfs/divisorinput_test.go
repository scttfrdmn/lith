// SPDX-License-Identifier: Apache-2.0

package nfs

import (
	"fmt"
	"testing"
	"time"

	"github.com/scttfrdmn/lith/internal/blockstore"
	"github.com/scttfrdmn/lith/internal/s3client/fake"
)

// #337: what windowBlocks() divides the prefetch budget BY — and this test has now become the
// assertion it was written to become.
//
// It began as a CHARACTERIZATION, because changing a prefetch divisor is the change class that
// produced this project's only shipped regression (#309, 11x on concurrent readers) and could
// not ship without a throughput measurement the gateway cannot take locally. That measurement
// exists now: an external cell ran 47 namespaced clients that mounted and read nothing against
// one streaming client and measured **1267-1300 MB/s with one client against 519-558 with 48 --
// 2.4x -- with BYTE-IDENTICAL transfers in every arm** (3.777 GB, 458 GETs, prefetch
// issued == used == 3594). Purely window depth, nothing wasted, invisible to any byte or
// request counter.
//
// So both sides are asserted here: the old input still collapses (kept, because it is the
// record of what was wrong and the fallback path still uses it), and the new input does not.
func TestGatewayWindowShrinksWithIdleMounts(t *testing.T) {
	srv := fake.New()
	bs, err := blockstore.New(srv, blockstore.Config{
		Bucket: "bkt", BlockSize: 8 << 20, MemCache: 512 << 20, MaxRange: 64 << 20,
		PrefetchBudget: 492 * (8 << 20), // the shipping default's shape on a 33 GB box
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(bs.Close)

	mounted := func(n int) *server {
		s := &server{cfg: Config{Store: bs, ClientIdle: 5 * time.Minute, ReadIdle: 30 * time.Second},
			clients: map[string]time.Time{}}
		for i := 0; i < n; i++ {
			s.clients[fmt.Sprintf("10.0.%d.%d:0", i/256, i%256)] = time.Now()
		}
		return s
	}

	// ---- THE OLD INPUT, kept as the record and as the live fallback path (readStreams nil).
	var old []int64
	for _, n := range []int{1, 2, 8, 48, 123, 246, 492} {
		old = append(old, mounted(n).windowBlocks())
	}
	t.Logf("budget %d blocks", bs.PrefetchBudgetBlocks())
	t.Logf("MOUNT-counting divisor (pre-#337, still the fallback when readStreams is unset):")
	for i, n := range []int{1, 2, 8, 48, 123, 246, 492} {
		t.Logf("  %4d mounted, NONE reading -> %2d blocks = %4d MiB", n, old[i], old[i]*8)
	}
	if old[0] == old[len(old)-1] {
		t.Fatalf("window did not move across the mount ladder (%d): the fixture is not "+
			"exercising the divisor", old[0])
	}
	if old[len(old)-1] > 2 {
		t.Errorf("at 492 idle mounts the fallback window is %d blocks, expected the 2-block "+
			"floor", old[len(old)-1])
	}

	// ---- THE NEW INPUT. Same 48 mounted clients, but only ONE path has been read.
	s := mounted(48)
	fs := &roFS{cfg: s.cfg, srv: s, states: map[string]*seqState{}}
	s.readStreams = fs.activeReadStreams
	fs.stateFor("/one-reader")

	got := s.windowBlocks()
	t.Logf("READ-counting divisor (#337): 48 mounted, 1 reading -> %d blocks = %d MiB",
		got, got*8)

	// THE ASSERTION. One reader among 48 mounted clients gets the whole budget, capped at 96 --
	// not the 10 blocks the mount count gave it. This is the 2.4x the external cell measured.
	if got != 96 {
		t.Errorf("one active reader among 48 mounted clients got %d blocks, want the 96-block "+
			"cap: the divisor is still counting registrations rather than reads (#337)", got)
	}
	if got <= old[3] {
		t.Errorf("the corrected divisor gave %d blocks against the mount-counting %d; it must "+
			"be strictly larger when readers < mounted clients", got, old[3])
	}

	// AND IT STILL DIVIDES. The fix changes the divisor's INPUT, not whether it divides --
	// removing the rationing outright costs 13.8-22.4x wall with 81-92% of prefetch evicted
	// unread, and #309 is what happens when a total replaces the share.
	for i := 0; i < 48; i++ {
		fs.stateFor(fmt.Sprintf("/reader-%d", i))
	}
	if w := s.windowBlocks(); w >= 96 {
		t.Errorf("with 49 active read streams the window is %d blocks; the budget must still "+
			"be divided among actual readers", w)
	} else {
		t.Logf("48 mounted, 49 reading -> %d blocks: still rationed, by readers", w)
	}
}

// Recency is load-bearing: without it the divisor would count every path in the LRU-capped
// state map (8192 entries), which over a long-lived gateway is a worse divisor than the client
// count ever was. Asserted by backdating lastTouch rather than sleeping.
func TestGatewayDivisorIgnoresPathsReadLongAgo(t *testing.T) {
	srv := fake.New()
	bs, err := blockstore.New(srv, blockstore.Config{
		Bucket: "bkt", BlockSize: 8 << 20, MemCache: 512 << 20, MaxRange: 64 << 20,
		PrefetchBudget: 492 * (8 << 20),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(bs.Close)

	s := &server{cfg: Config{Store: bs, ClientIdle: 5 * time.Minute, ReadIdle: 30 * time.Second},
		clients: map[string]time.Time{}}
	fs := &roFS{cfg: s.cfg, srv: s, states: map[string]*seqState{}}
	s.readStreams = fs.activeReadStreams

	// One current reader, plus 100 paths last read well outside the window.
	fs.stateFor("/current")
	stale := time.Now().Add(-10 * time.Minute)
	fs.mu.Lock()
	for i := 0; i < 100; i++ {
		fs.states[fmt.Sprintf("/stale-%d", i)] = &seqState{lastTouch: stale}
	}
	fs.mu.Unlock()

	if n := fs.activeReadStreams(); n != 1 {
		t.Errorf("activeReadStreams = %d with 1 current and 100 stale paths, want 1", n)
	}
	if w := s.windowBlocks(); w != 96 {
		t.Errorf("window = %d blocks, want 96: paths read 10 minutes ago are inflating the "+
			"divisor, which is the LRU map becoming the bug the client count used to be", w)
	}

	// A path read just inside the window DOES count, or the recency filter is not a filter.
	fs.mu.Lock()
	fs.states["/recent"] = &seqState{lastTouch: time.Now().Add(-5 * time.Second)}
	fs.mu.Unlock()
	if n := fs.activeReadStreams(); n != 2 {
		t.Errorf("activeReadStreams = %d after a read 5 s ago, want 2", n)
	}

	// Never zero, so the caller cannot divide by zero and a quiet gateway gives its next
	// reader the whole budget.
	empty := &roFS{cfg: s.cfg, srv: s, states: map[string]*seqState{}}
	if n := empty.activeReadStreams(); n != 1 {
		t.Errorf("activeReadStreams on an empty state map = %d, want 1", n)
	}
}

// THE WIRING, end to end: a real read through the gateway's filesystem must move the divisor.
//
// Everything above drives stateFor directly, which proves the arithmetic and not the plumbing.
// This reads bytes through roFS.Open/ReadAt — the path a client's NFS READ takes — and asserts
// the window responds. Without it the fix could be wired to a state map the read path does not
// populate, and every other test here would still pass.
//
// It also covers the specific way this nearly shipped dead: the corrected input was wired in
// Serve(), which no test calls, while the test harness built its own copy of that construction
// and silently kept the MOUNT-counting fallback. newGateway now exists so there is one place,
// and this test goes through it.
func TestGatewayDivisorRespondsToRealReads(t *testing.T) {
	data := make([]byte, 1<<20)
	for i := range data {
		data[i] = byte(i / 4096)
	}
	// SELF-CALIBRATING: the window is capped at 96 blocks, so a fixture has to have more
	// readers than budget/96 before the division is observable at all. The first version of
	// this test read two files and asserted the window dropped; both were at the cap and it
	// failed on correct behaviour. The fixture now sizes itself from the budget.
	objs := map[string][]byte{}
	const probe = 24
	for i := 0; i < probe; i++ {
		objs[fmt.Sprintf("f%02d.bin", i)] = data
	}
	h, _ := testGateway(t, objs)
	s, fs := h.srv, h.fs
	if s.readStreams == nil {
		t.Fatal("the harness did not wire the read-stream divisor; this test would measure " +
			"the fallback path and prove nothing")
	}
	budget := s.cfg.Store.PrefetchBudgetBlocks()
	needed := int(budget/96) + 1 // readers required before the cap stops binding
	if needed >= probe {
		t.Skipf("budget %d blocks needs %d readers to clear the 96-block cap; fixture has %d",
			budget, needed, probe)
	}

	// Eight mounted clients that read nothing.
	for i := 1; i <= 8; i++ {
		s.mountClient(fmt.Sprintf("10.0.0.%d", i))
	}
	full := s.windowBlocks()
	if full != 96 {
		t.Fatalf("8 mounted clients with no reads gave %d blocks, want the 96-block cap: "+
			"mounting must not shrink anyone's readahead (#337)", full)
	}

	readSome := func(name string) {
		f, err := fs.Open(name)
		if err != nil {
			t.Fatalf("open %s: %v", name, err)
		}
		defer func() { _ = f.Close() }()
		buf := make([]byte, 64<<10)
		if _, err := f.ReadAt(buf, 0); err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
	}

	// Read one file: still one stream, so still the whole budget.
	readSome("f00.bin")
	if w := s.windowBlocks(); w != 96 {
		t.Errorf("one reader among 8 mounted clients got %d blocks, want 96", w)
	}

	// Read enough distinct files to clear the cap, and the window must fall.
	for i := 1; i <= needed; i++ {
		readSome(fmt.Sprintf("f%02d.bin", i))
	}
	streams := fs.activeReadStreams()
	w := s.windowBlocks()
	t.Logf("budget %d blocks; 8 mounted, 0 reading -> 96; %d reading -> %d blocks",
		budget, streams, w)

	if streams != needed+1 {
		t.Errorf("activeReadStreams = %d after reading %d files, want %d — the read path is "+
			"not populating the state the divisor reads", streams, needed+1, needed+1)
	}
	if w >= 96 {
		t.Errorf("%d active read streams still got the %d-block cap; the budget must be "+
			"divided among actual readers (the fix changed the divisor's INPUT, not whether "+
			"it divides)", streams, w)
	}
}
