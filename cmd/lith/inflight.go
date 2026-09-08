// SPDX-License-Identifier: Apache-2.0

package main

import "fmt"

// defaultInflightFallback is used when the NIC bandwidth cannot be determined.
const defaultInflightFallback = 512 << 20

// computeInflightBytes resolves the bytes-in-flight budget. An explicit flag
// wins; otherwise it is 2 × (NIC bandwidth × 100 ms), or a 512 MiB fallback
// when the NIC speed is unknown. Returns the budget and a human description.
func computeInflightBytes(flag string) (int64, string) {
	if flag != "" {
		n, err := parseSize(flag)
		if err == nil && n > 0 {
			return n, fmt.Sprintf("%d bytes (from --inflight-bytes)", n)
		}
	}
	if gbps := nicGbps(); gbps > 0 {
		// 2 × bandwidth-delay product at 100 ms.
		n := int64(2 * gbps * 1e9 / 8 * 0.1)
		return n, fmt.Sprintf("%d bytes (2 × %.0f Gbps × 100 ms)", n, gbps)
	}
	return defaultInflightFallback, fmt.Sprintf("%d bytes (fallback; NIC speed unknown)", int64(defaultInflightFallback))
}

// effectiveReadahead resolves the per-handle readahead window in blocks. A
// positive user value wins; otherwise the default is the bandwidth-delay
// product (inflightBytes / block) so a single reader can hold enough in flight
// to fill a fat NIC on a cold read (#56), clamped to [8, 1024] blocks.
func effectiveReadahead(userBlocks, inflightBytes, blockSize int64) int64 {
	if userBlocks > 0 {
		return userBlocks
	}
	if blockSize <= 0 {
		blockSize = 8 << 20
	}
	n := inflightBytes / blockSize
	if n < 8 {
		n = 8
	}
	if n > 1024 {
		n = 1024
	}
	return n
}
