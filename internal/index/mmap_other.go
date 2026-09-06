// SPDX-License-Identifier: Apache-2.0

//go:build !unix

package index

import "os"

// Open reads the index file into memory on platforms without mmap support.
// The returned close function is a no-op. lith targets Linux; this fallback
// keeps the package buildable elsewhere for development.
func Open(path string) (*Index, func() error, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	ix, err := Unmarshal(b)
	if err != nil {
		return nil, nil, err
	}
	return ix, func() error { return nil }, nil
}
