// SPDX-License-Identifier: Apache-2.0

package nfs

import (
	"fmt"
	"testing"
	"time"

	"github.com/scttfrdmn/lith/internal/blockstore"
	"github.com/scttfrdmn/lith/internal/s3client/fake"
)

// M17-D (#219), gateway seam: what windowBlocks() divides the prefetch budget BY.
//
// This is #301 in the gateway. There, perHandleWindow divided the budget by every open file
// DESCRIPTOR -- including descriptors nothing had ever read -- and a job merely holding files
// open drove every reader to the 2-block floor, measured at 6-10x the wall clock through no
// flag anyone set. The fix was not to remove the division but to change its INPUT: count
// handles the detector is actually prefetching for.
//
// windowBlocks() divides by activeClients(), which counts clients that have sent a MOUNT
// within ClientIdle (5 minutes by default). The code says why:
//
//	"Per-op client activity is not visible through go-nfs's stateless read path -- see the
//	 package/PR notes -- so idle is measured from MOUNT."
//
// So a client that mounts and reads nothing holds a share of the budget for five minutes, and
// enough of them drive an actively-reading client to the floor. That is the #301 shape exactly.
//
// This test measures the effect rather than asserting a fix: it is a characterization, because
// changing a prefetch divisor is the change class that has produced an 11x regression in this
// project and must not ship without a throughput measurement the gateway cannot take locally.
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

	s := &server{cfg: Config{Store: bs, ClientIdle: 5 * time.Minute}, clients: map[string]time.Time{}}

	type row struct {
		mounted int
		window  int64
	}
	var rows []row
	for _, n := range []int{1, 2, 8, 48, 123, 246, 492} {
		s.mu.Lock()
		s.clients = map[string]time.Time{}
		for i := 0; i < n; i++ {
			s.clients[fmt.Sprintf("10.0.%d.%d:0", i/256, i%256)] = time.Now()
		}
		s.mu.Unlock()
		rows = append(rows, row{n, s.windowBlocks()})
	}

	t.Logf("budget %d blocks; window = clamp(budget/mountedClients, 2, 96)", bs.PrefetchBudgetBlocks())
	for _, r := range rows {
		t.Logf("  %4d mounted (ANY of them idle) -> window %2d blocks = %d MiB",
			r.mounted, r.window, r.window*8)
	}

	// THE CHARACTERIZATION. None of these clients has read a byte -- they have only mounted --
	// and the window an actively-reading client would get collapses anyway.
	first, last := rows[0].window, rows[len(rows)-1].window
	if first == last {
		t.Fatalf("window did not move across 1..%d mounted clients (%d): this fixture is not "+
			"exercising the divisor", rows[len(rows)-1].mounted, first)
	}
	if last > 2 {
		t.Errorf("at %d idle mounts the window is %d blocks; expected the 2-block floor — "+
			"if this has changed, the divisor's input has been fixed and this test should "+
			"become the assertion rather than the characterization",
			rows[len(rows)-1].mounted, last)
	}
	t.Logf("collapse: %d -> %d blocks (%.0fx) from mounts alone, no reads",
		first, last, float64(first)/float64(last))

	// And the information needed to divide by READERS instead already exists: roFS.states
	// carries a per-path lastTouch, updated in stateFor, which NFS's statelessness calls on
	// every read. That is the gateway's analogue of #311's streamingHandles.
	fs := &roFS{cfg: s.cfg, srv: s, states: map[string]*seqState{}}
	fs.stateFor("/a")
	fs.stateFor("/b")
	fs.mu.Lock()
	n := len(fs.states)
	touched := 0
	for _, st := range fs.states {
		if !st.lastTouch.IsZero() {
			touched++
		}
	}
	fs.mu.Unlock()
	if n != 2 || touched != 2 {
		t.Errorf("roFS.states tracked %d paths with %d timestamps, want 2 and 2 — the "+
			"read-activity signal a corrected divisor would use is not what this assumed", n, touched)
	}
	t.Logf("read-activity signal available: %d paths with lastTouch, updated per read in stateFor", n)
}
