// SPDX-License-Identifier: Apache-2.0

package prefetch

import "testing"

// #256: the evidence gate's floor was the constant initialWindow (2 blocks). A window
// has to cover at least one first-byte round trip of the reader's own consumption, or
// the reader drains it before a refill lands and blocks on demand. Two blocks is 38
// round trips of reading at 2.2 ms RTT and 1.4 at 58.6 ms — which is why the gate cost
// a flat ~0.35 s in-region and produced a bimodal 4 s / 20 s split cross-region.
//
// The floor must therefore be settable from the device, and must never lower the
// existing behaviour.
func TestEvidenceFloorCoversARoundTrip(t *testing.T) {
	const blk = int64(8 << 20)
	// 50 Gbps ≈ 6.25 GB/s; 58.6 ms RTT ≈ 366 MB per round trip ≈ 44 blocks.
	const rtBlocks = int64(44)

	// A handle that has read almost nothing: earned would be initialWindow.
	p := New(223)
	p.SetEvidence(4, blk)
	p.SetEvidenceFloor(rtBlocks)
	p.Open()
	if got := p.windowCap(); got != rtBlocks {
		t.Errorf("cap with no consumption = %d, want the floor %d", got, rtBlocks)
	}

	// Once consumption earns more than the floor, the floor stops binding.
	p2 := New(223)
	p2.SetEvidence(4, blk)
	p2.SetEvidenceFloor(rtBlocks)
	p2.Open()
	p2.consumed = 200 << 20 // 200 MiB read → earned = 100 blocks
	if got := p2.windowCap(); got != 100 {
		t.Errorf("cap after 200 MiB = %d, want 100 (earned, above the floor)", got)
	}

	// And it can never reduce the window below what the constant gave.
	p3 := New(223)
	p3.SetEvidence(4, blk)
	p3.SetEvidenceFloor(0) // a device that reports nothing
	p3.Open()
	if got := p3.windowCap(); got != initialWindow {
		t.Errorf("cap with no device info = %d, want initialWindow %d", got, initialWindow)
	}
	p3.SetEvidenceFloor(1) // below the constant
	if got := p3.windowCap(); got != initialWindow {
		t.Errorf("a floor below initialWindow must not lower it: got %d, want %d", got, initialWindow)
	}
}

// The floor must not leak into the disabled case: with the gate off, windowCap is
// maxReadahead and the floor is irrelevant.
func TestEvidenceFloorIsInertWhenTheGateIsOff(t *testing.T) {
	p := New(223)
	p.SetEvidenceFloor(44) // set, but no SetEvidence
	p.Open()
	if got := p.windowCap(); got != 223 {
		t.Errorf("gate off: cap = %d, want maxReadahead 223", got)
	}
}

// In-region the floor is below what a couple of reads already earn, so it changes
// nothing — which is the property that makes this safe to apply unconditionally.
func TestEvidenceFloorChangesNothingAtLowRTT(t *testing.T) {
	const blk = int64(8 << 20)
	// 2.2 ms at 6.25 GB/s ≈ 13.75 MB ≈ 2 blocks, i.e. the existing constant.
	inRegion := (int64(13_750_000) + blk - 1) / blk
	withFloor, without := New(223), New(223)
	for _, p := range []*Prefetcher{withFloor, without} {
		p.SetEvidence(4, blk)
		p.Open()
	}
	withFloor.SetEvidenceFloor(inRegion)
	for _, consumed := range []int64{1 << 20, 8 << 20, 64 << 20, 512 << 20} {
		withFloor.consumed, without.consumed = consumed, consumed
		if a, b := withFloor.windowCap(), without.windowCap(); a != b {
			t.Errorf("at %d MiB consumed: floored cap %d != unfloored %d — the floor must be "+
				"inert at in-region latency", consumed>>20, a, b)
		}
	}
}
