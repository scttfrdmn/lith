// SPDX-License-Identifier: Apache-2.0

//go:build !linux

package fuse

func setReadAheadKB(string, int) error { return nil }
func raisePipeMaxSize(int) error       { return nil }
