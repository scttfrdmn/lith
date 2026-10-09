// SPDX-License-Identifier: Apache-2.0

//go:build !linux

package main

import "runtime"

// ethtoolGbps is unavailable off Linux; resolveNIC falls through to IMDS +
// DescribeInstanceTypes (both cross-platform) or the fixed fallback. The reason names the
// platform rather than saying nothing, because an operator reading a mount log on a Mac
// should not have to guess why the first source was skipped (#317).
func ethtoolGbps() (float64, string) {
	return 0, "not available on " + runtime.GOOS + " (ethtool is Linux-only)"
}
