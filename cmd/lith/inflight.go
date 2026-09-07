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
