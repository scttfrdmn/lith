// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"math"
	"sort"
)

// The two statistics the pre-registered #256 verdict turns on. Both are
// rank-based, because per-handle byte follow-through is bounded in [0,1] with mass
// at both ends and the candidate features are heavy-tailed (byte gaps span six
// orders of magnitude) — a Pearson correlation on those raw values would report
// whatever the outliers felt like.

// ranks returns the 1-based ranks of xs, averaging ties. Ties matter here: many
// handles score exactly 0.0 or 1.0 follow-through, and breaking those ties
// arbitrarily would manufacture correlation that isn't there.
func ranks(xs []float64) []float64 {
	idx := make([]int, len(xs))
	for i := range idx {
		idx[i] = i
	}
	sort.SliceStable(idx, func(a, b int) bool { return xs[idx[a]] < xs[idx[b]] })
	out := make([]float64, len(xs))
	for i := 0; i < len(idx); {
		j := i
		for j+1 < len(idx) && xs[idx[j+1]] == xs[idx[i]] {
			j++
		}
		avg := float64(i+j)/2 + 1 // mean of 1-based ranks i+1..j+1
		for k := i; k <= j; k++ {
			out[idx[k]] = avg
		}
		i = j + 1
	}
	return out
}

// spearman returns the Spearman rank correlation of xs and ys, and false when it
// is undefined (fewer than 3 pairs, or either side constant — a constant feature
// correlates with nothing, and reporting 0 would read as "measured no relationship"
// rather than "carries no information").
func spearman(xs, ys []float64) (float64, bool) {
	if len(xs) != len(ys) || len(xs) < 3 {
		return 0, false
	}
	rx, ry := ranks(xs), ranks(ys)
	var mx, my float64
	for i := range rx {
		mx += rx[i]
		my += ry[i]
	}
	n := float64(len(rx))
	mx /= n
	my /= n
	var num, dx, dy float64
	for i := range rx {
		a, b := rx[i]-mx, ry[i]-my
		num += a * b
		dx += a * a
		dy += b * b
	}
	if dx == 0 || dy == 0 {
		return 0, false
	}
	return num / math.Sqrt(dx*dy), true
}

// aucMannWhitney returns the probability that a randomly chosen value from `a`
// exceeds one from `b`, ties counting half — the rank-sum AUC. 0.5 means the two
// distributions are indistinguishable by rank; the #256 rule calls < 0.7 an
// overlap. Reported as max(auc, 1-auc) by the caller so direction is explicit
// rather than implied by argument order.
func aucMannWhitney(a, b []float64) (float64, bool) {
	if len(a) == 0 || len(b) == 0 {
		return 0, false
	}
	all := append(append([]float64{}, a...), b...)
	r := ranks(all)
	var sumA float64
	for i := range a {
		sumA += r[i]
	}
	na, nb := float64(len(a)), float64(len(b))
	// U statistic for sample a, normalized to an AUC.
	u := sumA - na*(na+1)/2
	return u / (na * nb), true
}

// quantile returns the p-quantile of xs (0 <= p <= 1) by nearest-rank, which needs
// no interpolation assumption. xs is sorted in place.
func quantile(xs []float64, p float64) float64 {
	if len(xs) == 0 {
		return math.NaN()
	}
	sort.Float64s(xs)
	i := int(p*float64(len(xs)-1) + 0.5)
	if i < 0 {
		i = 0
	}
	if i >= len(xs) {
		i = len(xs) - 1
	}
	return xs[i]
}

// minDistinct is how many distinct values each axis needs before a correlation may
// influence the verdict. With three handles and a two-way tie on both axes, any
// monotone relation gives |rho| = 1 by construction — which is how an earlier build
// printed "VERDICT: SEPARATION" off one effective degree of freedom, on a feature
// that was really a restatement of which handle had enough rows to be scored at all.
const minDistinct = 4

// degenerate returns a short reason when a correlation must not count toward the
// verdict, or "" when it may. Checked per arm: replicating a degenerate fit on a
// replicate of the same workload adds no degree of freedom, so agreement across arms
// must not excuse it.
func degenerate(xs, ys []float64, minN int) string {
	if len(xs) < minN {
		return fmt.Sprintf("n=%d<%d", len(xs), minN)
	}
	if d := distinct(xs); d < minDistinct {
		return fmt.Sprintf("feature has %d distinct", d)
	}
	if d := distinct(ys); d < minDistinct {
		return fmt.Sprintf("outcome has %d distinct", d)
	}
	return ""
}

func distinct(xs []float64) int {
	seen := map[float64]bool{}
	for _, x := range xs {
		seen[x] = true
	}
	return len(seen)
}

func mean(xs []float64) float64 {
	if len(xs) == 0 {
		return math.NaN()
	}
	var s float64
	for _, x := range xs {
		s += x
	}
	return s / float64(len(xs))
}
