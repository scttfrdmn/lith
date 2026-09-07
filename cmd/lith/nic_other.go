// SPDX-License-Identifier: Apache-2.0

//go:build !linux

package main

// nicGbps is unknown on non-Linux platforms.
func nicGbps() float64 { return 0 }
