// SPDX-License-Identifier: Apache-2.0

package main

import (
	"testing"
	"time"
)

func TestPercentile(t *testing.T) {
	d := []time.Duration{5, 1, 3, 2, 4} // ms-agnostic units
	for _, tc := range []struct {
		p    float64
		want time.Duration
	}{
		{0, 1}, {50, 3}, {99, 5}, {100, 5},
	} {
		if got := percentile(d, tc.p); got != tc.want {
			t.Errorf("percentile(%v) = %v, want %v", tc.p, got, tc.want)
		}
	}
	if percentile(nil, 50) != 0 {
		t.Error("percentile(nil) should be 0")
	}
}

func TestMedianMinMaxFloat(t *testing.T) {
	v := []float64{3, 1, 2}
	if medianFloat(v) != 2 || minFloat(v) != 1 || maxFloat(v) != 3 {
		t.Errorf("median/min/max = %v/%v/%v", medianFloat(v), minFloat(v), maxFloat(v))
	}
	// input not mutated
	if v[0] != 3 {
		t.Error("medianFloat mutated its input")
	}
}
