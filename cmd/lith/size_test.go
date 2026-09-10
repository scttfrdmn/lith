// SPDX-License-Identifier: Apache-2.0

package main

import "testing"

func TestParseSizeValid(t *testing.T) {
	cases := []struct {
		in   string
		want int64
	}{
		{"8MiB", 8 << 20},
		{"1GiB", 1 << 30},
		{"1024", 1024},
		{"512KiB", 512 << 10},
		{"0", 0},
	}
	for _, c := range cases {
		got, err := parseSize(c.in)
		if err != nil {
			t.Errorf("parseSize(%q) unexpected error: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("parseSize(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestParseSizeRejects(t *testing.T) {
	bad := []string{
		"Inf", "+Inf", "-Inf", "NaN", "NaNMiB", "-5", "-1GiB",
		"1e400",      // overflows float64 -> +Inf
		"1e19",       // finite but exceeds int64 max bytes
		"9999999TiB", // multiplied overflow
	}
	for _, in := range bad {
		if got, err := parseSize(in); err == nil {
			t.Errorf("parseSize(%q) = %d, want error", in, got)
		}
	}
}
