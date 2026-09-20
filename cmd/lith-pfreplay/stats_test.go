// SPDX-License-Identifier: Apache-2.0

package main

import (
	"math"
	"testing"
)

// These two statistics decide the pre-registered #256 verdict, so they are pinned
// against hand-checkable values. A scorer whose arithmetic nobody verified is worse
// than no scorer: it produces a confident answer.

func TestSpearmanKnownValues(t *testing.T) {
	// Perfectly monotone increasing -> +1; decreasing -> -1; regardless of scale,
	// which is the property we want (byte gaps span six orders of magnitude).
	inc := []float64{1, 2, 3, 4, 5}
	if r, ok := spearman(inc, []float64{10, 200, 3000, 40000, 500000}); !ok || math.Abs(r-1) > 1e-9 {
		t.Errorf("monotone increasing: rho=%v ok=%v, want +1", r, ok)
	}
	if r, ok := spearman(inc, []float64{5, 4, 3, 2, 1}); !ok || math.Abs(r+1) > 1e-9 {
		t.Errorf("monotone decreasing: rho=%v ok=%v, want -1", r, ok)
	}
	// A constant side carries no information: undefined, NOT 0. Reporting 0 would
	// read as "measured no relationship" rather than "cannot say".
	if _, ok := spearman(inc, []float64{7, 7, 7, 7, 7}); ok {
		t.Error("constant y should be undefined, not a correlation of 0")
	}
	// Too few pairs to judge.
	if _, ok := spearman([]float64{1, 2}, []float64{1, 2}); ok {
		t.Error("n=2 should be undefined")
	}
	// Hand-computed: x ranks 1..4, y = 1,3,2,4 -> d = 0,-1,1,0, sum d^2 = 2,
	// rho = 1 - 6*2/(4*15) = 0.8
	if r, ok := spearman([]float64{1, 2, 3, 4}, []float64{1, 3, 2, 4}); !ok || math.Abs(r-0.8) > 1e-9 {
		t.Errorf("rho=%v, want 0.8", r)
	}
}

func TestSpearmanAveragesTies(t *testing.T) {
	// Follow-through has heavy mass at exactly 0 and exactly 1; breaking those ties
	// arbitrarily would manufacture correlation. With y entirely tied the result must
	// be undefined, and with partial ties it must be finite and bounded.
	x := []float64{1, 2, 3, 4, 5, 6}
	y := []float64{0, 0, 0, 1, 1, 1}
	r, ok := spearman(x, y)
	if !ok {
		t.Fatal("partial ties should still be defined")
	}
	if r <= 0.8 || r > 1.0001 {
		t.Errorf("rho=%v, want a strong positive below or at 1", r)
	}
	rk := ranks([]float64{5, 5, 5})
	for _, v := range rk {
		if v != 2 {
			t.Errorf("all-tied ranks = %v, want every entry 2 (mean of 1,2,3)", rk)
			break
		}
	}
}

func TestAUCKnownValues(t *testing.T) {
	// Complete separation, a above b.
	if v, ok := aucMannWhitney([]float64{4, 5, 6}, []float64{1, 2, 3}); !ok || math.Abs(v-1) > 1e-9 {
		t.Errorf("disjoint high/low: auc=%v, want 1", v)
	}
	// Complete separation the other way.
	if v, ok := aucMannWhitney([]float64{1, 2, 3}, []float64{4, 5, 6}); !ok || math.Abs(v) > 1e-9 {
		t.Errorf("disjoint low/high: auc=%v, want 0", v)
	}
	// Identical distributions: indistinguishable by rank.
	if v, ok := aucMannWhitney([]float64{1, 2, 3}, []float64{1, 2, 3}); !ok || math.Abs(v-0.5) > 1e-9 {
		t.Errorf("identical: auc=%v, want 0.5", v)
	}
	// Empty input is not an answer.
	if _, ok := aucMannWhitney(nil, []float64{1}); ok {
		t.Error("empty sample should be undefined")
	}
	// The #256 rule calls AUC < 0.7 an overlap. Interleaved samples are the honest
	// "overlapping" case and sit below it (hand-checked: 0.625).
	v, _ := aucMannWhitney([]float64{1, 3, 5, 7}, []float64{2, 4, 6, 8})
	if got := math.Max(v, 1-v); math.Abs(got-0.625) > 1e-9 {
		t.Errorf("interleaved samples gave auc=%.3f, want 0.625", got)
	}
	// Worth pinning how undemanding the 0.7 bar is, since a verdict rides on it: a
	// uniform shift of one unit already clears it (hand-checked: 0.71875). So an
	// AUC just over 0.7 is weak evidence of separation, and the rule's real weight
	// sits on the Spearman half.
	v, _ = aucMannWhitney([]float64{1, 2, 3, 4}, []float64{2, 3, 4, 5})
	if got := math.Max(v, 1-v); math.Abs(got-0.71875) > 1e-9 {
		t.Errorf("shift-by-one gave auc=%.5f, want 0.71875", got)
	}
}

func TestQuantileAndMean(t *testing.T) {
	xs := []float64{1, 2, 3, 4, 5}
	if q := quantile(append([]float64{}, xs...), 0.5); q != 3 {
		t.Errorf("median=%v want 3", q)
	}
	if q := quantile(append([]float64{}, xs...), 0); q != 1 {
		t.Errorf("p0=%v want 1", q)
	}
	if q := quantile(append([]float64{}, xs...), 1); q != 5 {
		t.Errorf("p100=%v want 5", q)
	}
	if m := mean(xs); m != 3 {
		t.Errorf("mean=%v want 3", m)
	}
	if !math.IsNaN(quantile(nil, 0.5)) || !math.IsNaN(mean(nil)) {
		t.Error("empty input should be NaN, not 0")
	}
}

// TestDegenerateGuardsRejectOneDegreeOfFreedom pins the guard that stops a
// confident-but-empty verdict. The reported failure was n=3 with a two-way tie on
// both axes and one handle differing: monotone by construction, |rho| = 1.000,
// printed as "VERDICT: SEPARATION". The winning feature was really a restatement of
// which handle had enough rows to be scored at all.
func TestDegenerateGuardsRejectOneDegreeOfFreedom(t *testing.T) {
	// The exact reported shape.
	xs := []float64{0.000, 0.125, 0.125}
	ys := []float64{0.155852, 0.000000, 0.000000}
	if r, ok := spearman(xs, ys); !ok || math.Abs(r) < 0.99 {
		t.Fatalf("precondition: this shape is monotone by construction, rho=%v ok=%v", r, ok)
	}
	if why := degenerate(xs, ys, 8); why == "" {
		t.Error("n=3 with two-way ties must be rejected: it is one effective degree of freedom")
	}

	// Enough handles but a near-constant feature is still not evidence.
	xs2 := []float64{1, 1, 1, 1, 1, 1, 1, 2}
	ys2 := []float64{1, 2, 3, 4, 5, 6, 7, 8}
	if why := degenerate(xs2, ys2, 8); why == "" {
		t.Error("a feature with 2 distinct values across 8 handles must be rejected")
	}
	// ...and symmetric in the outcome, which is the case that matters here because
	// follow-through has real mass at exactly 0.
	if why := degenerate(ys2, xs2, 8); why == "" {
		t.Error("an outcome with 2 distinct values must be rejected")
	}

	// A genuine relationship with enough spread passes.
	var big, out []float64
	for i := 0; i < 20; i++ {
		big = append(big, float64(i))
		out = append(out, float64(i)*0.5+float64(i%3))
	}
	if why := degenerate(big, out, 8); why != "" {
		t.Errorf("a 20-handle spread relationship was rejected: %s", why)
	}
	if d := distinct([]float64{1, 1, 2, 2, 3}); d != 3 {
		t.Errorf("distinct = %d, want 3", d)
	}
}
