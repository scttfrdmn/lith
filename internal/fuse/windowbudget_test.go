// SPDX-License-Identifier: Apache-2.0

package fuse

import "testing"

// #301: one handle's share of the prefetch budget, and whether the floor is what bound it.
//
// The floor flag is the part that matters operationally: a mount sitting at 2 blocks reads
// at a fraction of its link, and the workload that found #301 had both production mounts
// there with nothing in the log saying so.
func TestShareClamp(t *testing.T) {
	cases := []struct {
		name                string
		share, maxReadahead int64
		want                int64
		wantFloor           bool
	}{
		// Budget has room: the share stands as computed.
		{"share fits", 60, 223, 60, false},
		// maxReadahead is the ceiling -- the #298 case where a configured depth is reduced.
		{"capped by maxReadahead", 492, 223, 223, false},
		// The floor, both ways in. A share of exactly 2 IS the floor: integer division
		// reaches it one descriptor before the clamp starts applying, and an earlier
		// version of this tested share < 2 and so reported the crossing one count late.
		{"share exactly two", 2, 223, 2, true},
		{"share below the floor", 1, 223, 2, true},
		{"share of zero", 0, 223, 2, true},
		// A maxReadahead below the floor still yields the floor, and counts as being at
		// it: the mount cannot go shallower whatever is configured.
		{"maxReadahead below the floor", 492, 1, 2, true},
		{"both below the floor", 1, 1, 2, true},
		// Exactly at the ceiling is not the floor.
		{"share equals maxReadahead", 223, 223, 223, false},
		// Three is the smallest share that is neither floored nor capped.
		{"smallest unbound share", 3, 223, 3, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, atFloor := shareClamp(c.share, c.maxReadahead)
			if got != c.want || atFloor != c.wantFloor {
				t.Errorf("shareClamp(%d, %d) = (%d, %v), want (%d, %v)",
					c.share, c.maxReadahead, got, atFloor, c.want, c.wantFloor)
			}
		})
	}
}
