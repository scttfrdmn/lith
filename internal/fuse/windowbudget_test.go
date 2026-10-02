// SPDX-License-Identifier: Apache-2.0

package fuse

import "testing"

// #301: the readahead window is clamped by what the budget could hold, and nothing else.
func TestWindowForBudget(t *testing.T) {
	cases := []struct {
		name                string
		maxRA, budgetBlocks int64
		want                int64
	}{
		// The regime the divisor was wrong about: plenty of budget, so the configured
		// window stands. Under the divisor with 256 open descriptors this was 2.
		{"budget has room", 223, 492, 223},
		// The budget is a ceiling: don't hand out a window it could never hold.
		{"budget is the ceiling", 223, 60, 60},
		// The floor every handle gets regardless.
		{"floor", 223, 1, 2},
		{"floor from maxRA", 1, 492, 2},
		// No budget configured must not collapse the window to the floor.
		{"no budget", 223, 0, 223},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := windowForBudget(c.maxRA, c.budgetBlocks); got != c.want {
				t.Errorf("windowForBudget(%d, %d) = %d, want %d",
					c.maxRA, c.budgetBlocks, got, c.want)
			}
		})
	}
}
