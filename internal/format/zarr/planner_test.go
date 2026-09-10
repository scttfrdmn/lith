// SPDX-License-Identifier: Apache-2.0

package zarr

import (
	"reflect"
	"testing"
)

// coordSet renders a [][]int as a comparable set of basenames.
func coordSet(cs [][]int) map[string]bool {
	m := map[string]bool{}
	for _, c := range cs {
		m[FormatCoords(c)] = true
	}
	return m
}

// TestPlaneLineAlongAxis: three opens fixing axes 0,1 and advancing axis 2 in a
// 3×4×5 store classify to the 5-chunk line along axis 2.
func TestPlaneLineAlongAxis(t *testing.T) {
	p := NewPlanner([]int{3, 4, 5}, DefaultK, DefaultMinPlane)
	if plan, rp := p.Observe([]int{0, 0, 0}); plan != nil || rp {
		t.Fatalf("open 1: plan=%v replanned=%v, want nil,false", plan, rp)
	}
	if plan, rp := p.Observe([]int{0, 0, 1}); plan != nil || rp {
		t.Fatalf("open 2: plan=%v replanned=%v, want nil,false", plan, rp)
	}
	plan, rp := p.Observe([]int{0, 0, 2})
	if rp {
		t.Fatalf("open 3: replanned=true, want false (first plan)")
	}
	wantPlane := coordSet([][]int{{0, 0, 0}, {0, 0, 1}, {0, 0, 2}, {0, 0, 3}, {0, 0, 4}})
	if got := coordSet(p.Plane()); !reflect.DeepEqual(got, wantPlane) {
		t.Fatalf("plane = %v, want the 5-chunk axis-2 line", p.Plane())
	}
	// The prefetch list is the un-opened remainder in walk order.
	if !reflect.DeepEqual(plan, [][]int{{0, 0, 3}, {0, 0, 4}}) {
		t.Fatalf("prefetch list = %v, want [(0,0,3) (0,0,4)]", plan)
	}
}

// TestReplanToPlane: a walk along axis 1 that then steps axis 2 re-plans from
// the last K opens to the 4×5 (axes 1,2 varying) plane, counted exactly once.
func TestReplanToPlane(t *testing.T) {
	p := NewPlanner([]int{3, 4, 5}, DefaultK, DefaultMinPlane)
	replans := 0
	observe := func(c []int) {
		if _, rp := p.Observe(c); rp {
			replans++
		}
	}
	observe([]int{0, 0, 0})
	observe([]int{0, 1, 0})
	observe([]int{0, 2, 0}) // classifies the axis-1 line (4 chunks); not a replan
	if replans != 0 {
		t.Fatalf("replans after the first plan = %d, want 0", replans)
	}
	// Still on the axis-1 line: an in-plane open is not a replan.
	observe([]int{0, 3, 0})
	if replans != 0 {
		t.Fatalf("in-plane open counted as replan (%d)", replans)
	}
	// Step axis 2 — out of the axis-1 line → exactly one replan to the 4×5 plane.
	observe([]int{0, 0, 1})
	if replans != 1 {
		t.Fatalf("out-of-plane open replans = %d, want 1", replans)
	}
	// The new plane is axes 1,2 varying with axis 0 fixed at 0 → 20 chunks.
	if got := len(p.Plane()); got != 20 {
		t.Fatalf("replanned plane size = %d, want 20 (4×5)", got)
	}
	for _, c := range p.Plane() {
		if c[0] != 0 {
			t.Fatalf("plane chunk %v has axis0 != 0 (should be fixed)", c)
		}
	}
	// A further in-plane open does not add a replan.
	observe([]int{0, 1, 1})
	if replans != 1 {
		t.Fatalf("replans after in-plane open = %d, want 1", replans)
	}
}

// TestBelowMinPlaneFallsToTier1: a plane smaller than minPlane yields no plan.
func TestBelowMinPlaneFallsToTier1(t *testing.T) {
	// dims axis2 = 3 < minPlane 4 → the line is too short to treat as a selection.
	p := NewPlanner([]int{5, 5, 3}, DefaultK, DefaultMinPlane)
	p.Observe([]int{0, 0, 0})
	p.Observe([]int{0, 0, 1})
	plan, rp := p.Observe([]int{0, 0, 2})
	if plan != nil || rp || p.Plane() != nil {
		t.Fatalf("small plane: plan=%v replanned=%v plane=%v, want nil/false/nil", plan, rp, p.Plane())
	}
}

// TestRemainingOrderedWalkFirst: on a 2-varying-axis plane, the remaining list
// finishes the current primary line before advancing the secondary axis.
func TestRemainingOrderedWalkFirst(t *testing.T) {
	p := NewPlanner([]int{1, 3, 3}, DefaultK, DefaultMinPlane)
	p.Observe([]int{0, 0, 0})
	p.Observe([]int{0, 0, 1}) // axis 2 is the primary (most recently walked)
	plan, _ := p.Observe([]int{0, 1, 0})
	// primary axis = 2 (changed on the last step before this classifying open is
	// ambiguous, but the plane is axes 1,2 varying). The first entries should be
	// the rest of the current axis-2 line at axis1=1 before jumping lines.
	if len(plan) == 0 {
		t.Fatal("expected a non-empty prefetch list")
	}
	// Every returned coord is in-plane, axis0 fixed 0, and none already opened.
	opened := coordSet([][]int{{0, 0, 0}, {0, 0, 1}, {0, 1, 0}})
	for _, c := range plan {
		if c[0] != 0 {
			t.Fatalf("coord %v axis0 not fixed", c)
		}
		if opened[FormatCoords(c)] {
			t.Fatalf("coord %v was already opened", c)
		}
	}
}

// TestObserveNeverPanicsOnRaggedInput: a coord whose length != dims is ignored.
func TestObserveNeverPanicsOnRaggedInput(t *testing.T) {
	p := NewPlanner([]int{3, 3}, DefaultK, DefaultMinPlane)
	if plan, rp := p.Observe([]int{0, 0, 0}); plan != nil || rp {
		t.Fatalf("ragged coord accepted: %v %v", plan, rp)
	}
}

func TestClassifyAxes(t *testing.T) {
	// A walk along axis 2 with axes 0,1 fixed → only axis 2 varies; plane size =
	// dims[2]; primary = 2.
	varying, fixedVal, primary, size := classify([][]int{{2, 0, 0}, {2, 0, 1}, {2, 0, 2}}, []int{5, 4, 6})
	if size != 6 || primary != 2 {
		t.Fatalf("size=%d primary=%d, want 6,2", size, primary)
	}
	if !reflect.DeepEqual(varying, []bool{false, false, true}) {
		t.Fatalf("varying = %v, want [f f t]", varying)
	}
	if fixedVal[0] != 2 || fixedVal[1] != 0 {
		t.Fatalf("fixedVal = %v, want axis0=2 axis1=0", fixedVal)
	}
}
