// SPDX-License-Identifier: Apache-2.0

//go:build !linux

package main

// ethtoolGbps is unavailable off Linux; resolveNIC falls through to IMDS +
// DescribeInstanceTypes (both cross-platform) or the fixed fallback.
func ethtoolGbps() float64 { return 0 }
