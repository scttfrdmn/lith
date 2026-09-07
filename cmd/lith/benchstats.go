// SPDX-License-Identifier: Apache-2.0

package main

import (
	"sort"
	"time"
)

// percentile returns the p-th percentile (0..100) of durs (nearest-rank).
// durs need not be sorted.
func percentile(durs []time.Duration, p float64) time.Duration {
	if len(durs) == 0 {
		return 0
	}
	s := append([]time.Duration(nil), durs...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	idx := int(p / 100 * float64(len(s)))
	if idx >= len(s) {
		idx = len(s) - 1
	}
	return s[idx]
}

// medianFloat returns the median of vals (does not mutate the caller's slice).
func medianFloat(vals []float64) float64 {
	if len(vals) == 0 {
		return 0
	}
	s := append([]float64(nil), vals...)
	sort.Float64s(s)
	return s[len(s)/2]
}

func minFloat(vals []float64) float64 {
	if len(vals) == 0 {
		return 0
	}
	m := vals[0]
	for _, v := range vals[1:] {
		if v < m {
			m = v
		}
	}
	return m
}

func maxFloat(vals []float64) float64 {
	if len(vals) == 0 {
		return 0
	}
	m := vals[0]
	for _, v := range vals[1:] {
		if v > m {
			m = v
		}
	}
	return m
}
