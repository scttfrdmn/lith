// SPDX-License-Identifier: Apache-2.0

//go:build unix

package index

import (
	"fmt"
	"os"
	"syscall"
)

// Open memory-maps the index file at path and returns a queryable Index whose
// arrays point directly into the mapping (no copy), plus a close function that
// unmaps it. The Index must not be used after close is called. This gives
// millisecond loads for multi-gigabyte indexes.
func Open(path string) (*Index, func() error, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = f.Close() }()

	fi, err := f.Stat()
	if err != nil {
		return nil, nil, err
	}
	size := int(fi.Size())
	if size < headerSize {
		return nil, nil, fmt.Errorf("index: file too small: %s", path)
	}

	data, err := syscall.Mmap(int(f.Fd()), 0, size, syscall.PROT_READ, syscall.MAP_SHARED)
	if err != nil {
		return nil, nil, fmt.Errorf("index: mmap %s: %w", path, err)
	}

	ix, err := parse(data, false)
	if err != nil {
		_ = syscall.Munmap(data)
		return nil, nil, err
	}
	closeFn := func() error { return syscall.Munmap(data) }
	return ix, closeFn, nil
}
