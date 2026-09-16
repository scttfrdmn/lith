// SPDX-License-Identifier: Apache-2.0

package prefetch

import "testing"

// #229: broad fetch (the initial ramp and, via Established(), open-time
// parts-fetch) is committed only once the coverage signal confirms the access
// tiles. Until then the handle serves precise and dispatches nothing.

func TestEstablish_OpenDispatchesNothingUntilEstablished(t *testing.T) {
	p := New(64)
	p.SetGapMax(cbs)
	p.SetCoverage(16, 0.5)
	// Open commits no ramp under the gate.
	if got := p.Open(); len(got) != 0 {
		t.Fatalf("Open dispatched %v; #229 defers the ramp until established", got)
	}
	if p.Established() {
		t.Fatal("a freshly opened handle is not established")
	}
	// First two contiguous reads: recognized as ordered but not yet established
	// (coverage needs %d reads), so still no dispatch.
	if got := streamRead(p, 0); len(got) != 0 || p.Established() {
		t.Fatalf("read0 dispatched %v established=%v; want none", got, p.Established())
	}
	if got := streamRead(p, 1); len(got) != 0 || p.Established() {
		t.Fatalf("read1 dispatched %v established=%v; want none", got, p.Established())
	}
	// Third contiguous read: coverage confirms tiling → established, ramp begins.
	got := streamRead(p, 2)
	if !p.Established() || p.State() != Sequential {
		t.Fatalf("read2 should establish (Sequential); established=%v state=%v", p.Established(), p.State())
	}
	if len(got) == 0 {
		t.Fatal("the ramp must begin once established")
	}
}

func TestEstablish_WalkNeverEstablishes(t *testing.T) {
	p := New(64)
	p.SetGapMax(cbs)
	p.SetCoverage(16, 0.5)
	p.Open()
	// A netcdf4-style walk: header then scattered tiny reads across the object.
	p.Observe(0, 0, 16<<10, 0)
	p.Observe(0, 16<<10, 32<<10, 0)
	for _, o := range []int64{30 * cbs, 23 * cbs, 30 * cbs, 12 * cbs, 28 * cbs, 3 * cbs} {
		if got := p.Observe(o/cbs, o, 4<<10, o); len(got) != 0 {
			t.Fatalf("walk landing at %d dispatched %v; a non-established handle commits nothing", o, got)
		}
	}
	if p.Established() {
		t.Fatal("a scattered walk must never establish")
	}
}

func TestEstablish_GateDisabledRampsAtOpen(t *testing.T) {
	// With the coverage gate off (default), Open dispatches the initial window and
	// the handle ramps immediately — the pre-#229 behavior, preserved.
	p := New(64)
	if got := p.Open(); len(got) == 0 {
		t.Fatal("with the gate disabled, Open must dispatch the initial ramp")
	}
	if got := p.Observe(0, 0, cbs, 0); len(got) == 0 {
		t.Fatal("with the gate disabled, a sequential read ramps without waiting to establish")
	}
}

func TestEstablish_RevertsThenReEstablishes(t *testing.T) {
	p := New(64)
	p.SetGapMax(cbs)
	p.SetCoverage(16, 0.5)
	p.Open()
	for i := int64(0); i < 20; i++ { // establish
		streamRead(p, i)
	}
	if !p.Established() {
		t.Fatal("a stream must establish")
	}
	// Scatter → revert.
	for _, o := range []int64{200 * cbs, 260 * cbs, 230 * cbs, 290 * cbs, 210 * cbs} {
		p.Observe(o/cbs, o, 4<<10, o)
	}
	if p.Established() {
		t.Fatal("a stream that becomes a walk must de-establish")
	}
	// A windowful of contiguous reads re-establishes (earns it, not latched).
	for i := int64(400); i < 420; i++ {
		streamRead(p, i)
	}
	if !p.Established() {
		t.Fatal("a walk that becomes a stream must re-establish")
	}
}
