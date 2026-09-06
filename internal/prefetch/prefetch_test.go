// SPDX-License-Identifier: Apache-2.0

package prefetch

import (
	"reflect"
	"testing"
)

// run feeds a block sequence and returns the prefetch set emitted at each step
// plus the final state.
func run(maxRA int64, seq []int64) ([][]int64, State) {
	p := New(maxRA)
	out := make([][]int64, len(seq))
	for i, b := range seq {
		out[i] = p.Observe(b)
	}
	return out, p.State()
}

func TestSequentialGrowsGeometrically(t *testing.T) {
	got, state := run(8, []int64{0, 1, 2, 3, 4})
	want := [][]int64{
		nil,                         // cold, first read
		{2, 3},                      // window 2
		{3, 4, 5, 6},                // window 4
		{4, 5, 6, 7, 8, 9, 10, 11},  // window 8 (== max)
		{5, 6, 7, 8, 9, 10, 11, 12}, // window capped at max 8
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("sequential prefetch sets:\n got %v\nwant %v", got, want)
	}
	if state != Sequential {
		t.Errorf("state = %v, want Sequential", state)
	}
}

func TestStridedPredictsNextOnly(t *testing.T) {
	// Constant delta 5: strided is confirmed on the second delta and predicts
	// exactly the next block each step.
	got, state := run(32, []int64{0, 5, 10, 15})
	want := [][]int64{
		nil,  // cold
		nil,  // first delta 5 observed, not yet confirmed
		{15}, // delta 5 confirmed -> predict 10+5
		{20}, // predict 15+5
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("strided prefetch sets:\n got %v\nwant %v", got, want)
	}
	if state != Strided {
		t.Errorf("state = %v, want Strided", state)
	}
}

func TestRandomPrefetchesNothing(t *testing.T) {
	got, state := run(32, []int64{0, 7, 2, 9, 1})
	for i, s := range got {
		if len(s) != 0 {
			t.Errorf("step %d emitted %v, want nothing for random access", i, s)
		}
	}
	if state != Random {
		t.Errorf("state = %v, want Random", state)
	}
}

func TestSequentialThenRandomResets(t *testing.T) {
	// Sequential, then a jump: should stop prefetching until a pattern reforms.
	got, _ := run(8, []int64{0, 1, 2, 50})
	if len(got[3]) != 0 {
		t.Errorf("after a jump out of sequential, emitted %v, want nothing", got[3])
	}
}

func TestReReadSameBlockNoop(t *testing.T) {
	p := New(8)
	p.Observe(3)
	if got := p.Observe(3); got != nil {
		t.Errorf("re-read same block emitted %v, want nil", got)
	}
}
