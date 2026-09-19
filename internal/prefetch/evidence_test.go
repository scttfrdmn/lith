// SPDX-License-Identifier: Apache-2.0

package prefetch

import "testing"

// #256: the detector advances in block space, so the kernel's 128 KiB reads are
// d==0 no-ops within a block and a handle establishes at its first *block
// crossing* — one block (8 MiB) of contiguous evidence. Without the evidence gate
// that buys the full NIC-derived window (~223 blocks / ~1.8 GB at 50 Gbps), a
// ~223x overcommit on the evidence presented. The gate bounds a committed window
// by the bytes actually read, so the bet is proportional to demonstrated demand.
//
// Measured on GCHP/HEMCO: five flag settings moved the *volume* of bad prefetch
// almost linearly and the hit rate not at all (16.1% -> 13.3%), so the target is
// the decision, not the window size.

const kread = 128 << 10 // the kernel's read unit (see internal/fuse/mount.go)

// scan issues contiguous 128 KiB reads covering nbytes from `from`, which is what
// the kernel actually delivers — reads inside one block advance nothing, so this
// is also what makes block crossings (and therefore establishment) realistic.
func scan(p *Prefetcher, from, nbytes int64) {
	for off := from; off < from+nbytes; off += kread {
		p.Observe(off/cbs, off, kread, 0)
	}
}

func newEvidence(t *testing.T, maxWindow int64, ratio float64) *Prefetcher {
	t.Helper()
	p := New(maxWindow)
	p.SetGapMax(cbs)
	p.SetCoverage(16, 0.5)
	p.SetEvidence(ratio, cbs)
	p.Open()
	return p
}

// TestEvidence_TileThenJumpDoesNotEarnFullWindow is the #256 case: a reader tiles
// a slab of a large file (so it establishes at the first block crossing) and then
// jumps to the next variable. The window it was handed must be proportional to the
// slab it actually read, not to the NIC.
func TestEvidence_TileThenJumpDoesNotEarnFullWindow(t *testing.T) {
	const maxWindow = 64 // ~512 MiB at an 8 MiB block
	const slab = 24 << 20

	p := newEvidence(t, maxWindow, 8)
	scan(p, 0, slab)
	if !p.Established() {
		t.Fatal("a contiguous slab crossing block boundaries must establish")
	}
	// ratio 8 x 24 MiB consumed = 192 MiB = 24 blocks, well under the 64-block max.
	if got := p.PeakWindow(); got >= maxWindow {
		t.Errorf("window reached %d of max %d on a %d MiB slab; the gate must bound it", got, maxWindow, slab>>20)
	}
	if got := p.PeakWindow(); got < initialWindow {
		t.Errorf("window = %d; the gate must never starve a handle that proved contiguity", got)
	}
	if p.EvidenceHeld() == 0 {
		t.Error("EvidenceHeld should record that the gate bound the window")
	}
	// The gate's action must be observable: `issued` falling cannot distinguish a
	// window the gate refused from one the detector never wanted (#256 follow-up).
	if got := p.EvidenceWithheld(); got <= 0 {
		t.Errorf("EvidenceWithheld = %d, want > 0 blocks withheld", got)
	}
	if p.EvidenceWithheld() < p.EvidenceHeld() {
		t.Errorf("withheld (%d) < holds (%d): each hold withholds at least one block",
			p.EvidenceWithheld(), p.EvidenceHeld())
	}

	// Contrast, stated as fact rather than claim: the same access with the gate off
	// takes the full #229 jump on the same evidence.
	off := newEvidence(t, maxWindow, 0)
	scan(off, 0, slab)
	if got := off.PeakWindow(); got != maxWindow {
		t.Errorf("gate off: window = %d, want the full %d-block #229 jump", got, maxWindow)
	}
}

// TestEvidence_SequentialCopyEarnsFullWindow is the #56 guard. Jumping straight to
// the full window exists because a geometric re-ramp underfed the NIC on cold
// sequential copies, so a copy must still reach the full window — the gate may
// delay it, but only until the reader has demonstrated the demand.
//
// With ratio k the full window is earned once consumed >= maxWindow*blockSize/k,
// i.e. the handle always prefetches ~k times what it has read. The cold-start cost
// that leaves (early blocks served with a narrower window) is real, is the reason
// this ships behind a flag, and is what the cluster rung measures.
func TestEvidence_SequentialCopyEarnsFullWindow(t *testing.T) {
	const maxWindow = 64
	const ratio = 8
	need := int64(maxWindow) * cbs / ratio // 64 MiB

	p := newEvidence(t, maxWindow, ratio)
	scan(p, 0, need+8*cbs) // a little past the bound, to allow the doublings to land
	if p.State() != Sequential {
		t.Fatalf("a contiguous copy must stay Sequential, got %v", p.State())
	}
	if got := p.PeakWindow(); got != maxWindow {
		t.Errorf("a copy that consumed %d MiB reached window %d, want the full %d", need>>20, got, maxWindow)
	}

	// And the bound is proportional, not binary: a copy a quarter of the way there
	// gets roughly a quarter of the window — never zero readahead, never the max.
	q := newEvidence(t, maxWindow, ratio)
	scan(q, 0, need/4)
	if got := q.PeakWindow(); got < initialWindow || got >= maxWindow {
		t.Errorf("quarter-consumption window = %d, want in [%d, %d)", got, initialWindow, maxWindow)
	}
}

// TestEvidence_DisabledIsBehaviorPreserving: ratio <= 0 (or no block size) must
// leave the #229 establishment jump exactly as it was, so the default config is
// untouched.
func TestEvidence_DisabledIsBehaviorPreserving(t *testing.T) {
	const maxWindow = 64
	for _, ratio := range []float64{0, -1} {
		p := newEvidence(t, maxWindow, ratio)
		scan(p, 0, 16<<20)
		if !p.Established() || p.PeakWindow() != maxWindow {
			t.Errorf("ratio=%v: established=%v window=%d; want established with the full %d jump",
				ratio, p.Established(), p.PeakWindow(), maxWindow)
		}
		if p.EvidenceHeld() != 0 {
			t.Errorf("ratio=%v: gate is off but EvidenceHeld=%d", ratio, p.EvidenceHeld())
		}
	}
	p := New(maxWindow)
	p.SetGapMax(cbs)
	p.SetCoverage(16, 0.5)
	p.SetEvidence(8, 0) // no block size to convert the bound with
	p.Open()
	scan(p, 0, 16<<20)
	if p.PeakWindow() != maxWindow {
		t.Errorf("blockSize=0 should disable the gate; window=%d want %d", p.PeakWindow(), maxWindow)
	}
}

// TestEvidence_GatesReEstablishment: within-handle suppression is not sticky —
// coverage recovers over the trailing ring and the handle re-establishes. The gate
// must bound that commitment too, or a tile-jump-tile reader collects the full
// window on its second slab.
func TestEvidence_GatesReEstablishment(t *testing.T) {
	const maxWindow = 64
	p := newEvidence(t, maxWindow, 8)

	scan(p, 0, 16<<20) // establish on the first slab
	for _, blk := range []int64{400, 120, 330, 70, 250, 30, 190, 300} {
		p.Observe(blk, blk*cbs, 4<<10, blk*cbs) // scattered tiny reads de-establish
	}
	if p.Established() {
		t.Fatal("a scattered walk must de-establish the handle")
	}
	before := p.PeakWindow()
	scan(p, 600*cbs, 16<<20) // tile a second slab: re-establishes
	if got := p.PeakWindow(); got >= maxWindow {
		t.Errorf("re-established window reached %d of %d; the gate must bound re-establishment too", got, maxWindow)
	}
	if p.PeakWindow() < before {
		t.Errorf("PeakWindow went backwards (%d -> %d)", before, p.PeakWindow())
	}
}
