// SPDX-License-Identifier: Apache-2.0

package prefetch

import "testing"

// #256, hypothesis 4. The three before it were refuted by measurement:
// parts-max (flat), per-handle-one-decision (the bytes are in large files, not
// small ones), and consumption-proportionality (cut the volume of bad prefetch but
// the hit rate went *down*, because accrued evidence turned out to be uncorrelated
// with whether a prefetch is used).
//
// What survived all three is the reporting workload's oscillation:
// establish -> commit a window -> jump -> de-establish -> coverage recovers over
// the trailing ring -> re-establish -> commit again, ~320 times per run
// (prefetch_reset_random_total 307-332 on the HEMCO mount against 113 on met). The
// detector notices every time and then forgets. This gate makes the history of
// failed predictions cost something: it targets *whether to bet at all* rather
// than *how much*, which is the axis the measured hit-rate invariance implicates.

// oscillate performs one establish-then-jump cycle: tile a contiguous slab from
// `at`, then land scattered far away so coverage collapses and the handle
// de-establishes.
func oscillate(p *Prefetcher, at int64) {
	scan(p, at, 16<<20)
	for _, blk := range []int64{900, 150, 730, 40, 610, 25} {
		p.Observe(blk, blk*cbs, 4<<10, blk*cbs)
	}
}

func newReEst(t *testing.T, maxWindow, cap int64) *Prefetcher {
	t.Helper()
	p := New(maxWindow)
	p.SetGapMax(cbs)
	p.SetCoverage(16, 0.5)
	p.SetReEstablishMax(cap)
	p.Open()
	return p
}

// TestReEstablish_OscillatingHandleStopsCommitting: after the allowance, a reader
// that keeps losing establishment commits nothing further — it still reads, it just
// stops betting.
func TestReEstablish_OscillatingHandleStopsCommitting(t *testing.T) {
	const maxWindow = 64
	p := newReEst(t, maxWindow, 2)

	oscillate(p, 0)
	oscillate(p, 200*cbs)
	if got := p.DeEstablished(); got < 2 {
		t.Fatalf("DeEstablished = %d after two oscillations, want >= 2", got)
	}

	// The allowance is spent: a further contiguous slab must dispatch nothing and
	// must not establish.
	dispatched := 0
	for off := int64(400 * cbs); off < 400*cbs+(16<<20); off += kread {
		if got := p.Observe(off/cbs, off, kread, 0); len(got) != 0 {
			dispatched += len(got)
		}
	}
	if dispatched != 0 {
		t.Errorf("a capped handle dispatched %d blocks; it must commit nothing further", dispatched)
	}
	if p.Established() {
		t.Error("a capped handle must not re-establish")
	}
	if p.Suppressed() == 0 {
		t.Error("Suppressed should count the refused re-establishments (the gate must be observable)")
	}
}

// TestReEstablish_PureStreamUnaffected is the #56 guard, and the reason this shape
// is safer than the evidence gate: a sequential copy never loses establishment, so
// the cap is unreachable for it by construction — no ramp, no delay, no cold-start
// cost at all.
func TestReEstablish_PureStreamUnaffected(t *testing.T) {
	const maxWindow = 64
	capped := newReEst(t, maxWindow, 1) // the tightest possible allowance
	scan(capped, 0, 96<<20)

	if got := capped.DeEstablished(); got != 0 {
		t.Errorf("a pure stream lost establishment %d times, want 0", got)
	}
	if got := capped.Suppressed(); got != 0 {
		t.Errorf("a pure stream was suppressed %d times, want 0", got)
	}
	if !capped.Established() || capped.State() != Sequential {
		t.Errorf("a pure stream must stay established/Sequential; established=%v state=%v",
			capped.Established(), capped.State())
	}
	if got := capped.PeakWindow(); got != maxWindow {
		t.Errorf("a pure stream reached window %d, want the full %d", got, maxWindow)
	}

	// Identical to the same stream with the cap off.
	off := newReEst(t, maxWindow, 0)
	scan(off, 0, 96<<20)
	if capped.PeakWindow() != off.PeakWindow() || capped.State() != off.State() {
		t.Errorf("cap changed a pure stream: window %d vs %d, state %v vs %v",
			capped.PeakWindow(), off.PeakWindow(), capped.State(), off.State())
	}
}

// TestReEstablish_DisabledIsBehaviorPreserving: cap 0 must leave oscillation
// re-arming exactly as it is today, so the default configuration is untouched.
func TestReEstablish_DisabledIsBehaviorPreserving(t *testing.T) {
	const maxWindow = 64
	p := newReEst(t, maxWindow, 0)
	for i := int64(0); i < 4; i++ {
		oscillate(p, i*200*cbs)
	}
	// Still willing to re-establish and commit after four oscillations.
	scan(p, 800*cbs, 16<<20)
	if !p.Established() {
		t.Error("with the cap disabled a handle must still re-establish after oscillating")
	}
	if got := p.Suppressed(); got != 0 {
		t.Errorf("cap disabled but Suppressed=%d", got)
	}
	if got := p.DeEstablished(); got == 0 {
		t.Error("DeEstablished should still count oscillation with the cap off (it is the diagnostic)")
	}
}

// TestReEstablish_AllowanceIsSpentNotImmediate: the cap must not fire on the first
// jump — a stream that takes one genuine seek keeps its readahead.
func TestReEstablish_AllowanceIsSpentNotImmediate(t *testing.T) {
	const maxWindow = 64
	p := newReEst(t, maxWindow, 3)

	oscillate(p, 0) // one loss, well inside the allowance
	if got := p.DeEstablished(); got != 1 {
		t.Fatalf("DeEstablished = %d after one oscillation, want 1", got)
	}
	scan(p, 300*cbs, 16<<20)
	if !p.Established() {
		t.Error("one loss out of an allowance of 3 must not stop re-establishment")
	}
	if got := p.Suppressed(); got != 0 {
		t.Errorf("Suppressed = %d inside the allowance, want 0", got)
	}
}
