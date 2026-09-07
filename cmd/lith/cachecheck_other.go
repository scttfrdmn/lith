// SPDX-License-Identifier: Apache-2.0

//go:build !linux

package main

// diskCacheWarning is a no-op on non-Linux platforms (lith mounts only on
// Linux; this keeps the command buildable elsewhere for development).
func diskCacheWarning(string) string { return "" }
