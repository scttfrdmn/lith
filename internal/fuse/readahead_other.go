// SPDX-License-Identifier: Apache-2.0

//go:build !linux

package fuse

// setReadAheadKB is a no-op on non-Linux platforms.
func setReadAheadKB(string, int) error { return nil }
