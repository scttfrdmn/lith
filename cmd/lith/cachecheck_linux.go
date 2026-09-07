// SPDX-License-Identifier: Apache-2.0

//go:build linux

package main

import (
	"os"
	"path/filepath"
	"syscall"
)

// diskCacheWarning returns a non-empty warning if the disk-cache directory sits
// on a filesystem where caching is a poor idea (network volume or the root
// filesystem). It stats the nearest existing ancestor of path.
func diskCacheWarning(path string) string {
	target := nearestExisting(path)

	var st syscall.Statfs_t
	if err := syscall.Statfs(target, &st); err != nil {
		return ""
	}

	onRoot := false
	var rootStat, pathStat syscall.Stat_t
	if syscall.Stat("/", &rootStat) == nil && syscall.Stat(target, &pathStat) == nil {
		onRoot = rootStat.Dev == pathStat.Dev
	}
	return classifyDiskCache(int64(st.Type), onRoot)
}

// nearestExisting walks up path until it finds a directory that exists.
func nearestExisting(path string) string {
	for path != "" && path != "/" && path != "." {
		if _, err := os.Stat(path); err == nil {
			return path
		}
		path = filepath.Dir(path)
	}
	return "/"
}
