// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"strconv"
	"strings"
)

// parseSize parses a byte size like "8MiB", "1GiB", "512KiB", or a plain byte
// count ("1048576"). Binary (KiB/MiB/GiB/TiB) and decimal (KB/MB/GB) suffixes
// are accepted; the comparison is case-insensitive.
func parseSize(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty size")
	}
	upper := strings.ToUpper(s)
	mult := int64(1)
	for _, u := range []struct {
		suffix string
		mult   int64
	}{
		{"TIB", 1 << 40}, {"GIB", 1 << 30}, {"MIB", 1 << 20}, {"KIB", 1 << 10},
		{"TB", 1e12}, {"GB", 1e9}, {"MB", 1e6}, {"KB", 1e3},
		{"T", 1 << 40}, {"G", 1 << 30}, {"M", 1 << 20}, {"K", 1 << 10}, {"B", 1},
	} {
		if strings.HasSuffix(upper, u.suffix) {
			mult = u.mult
			upper = strings.TrimSuffix(upper, u.suffix)
			break
		}
	}
	n, err := strconv.ParseFloat(strings.TrimSpace(upper), 64)
	if err != nil {
		return 0, fmt.Errorf("invalid size %q: %w", s, err)
	}
	return int64(n * float64(mult)), nil
}
