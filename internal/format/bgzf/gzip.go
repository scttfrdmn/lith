// SPDX-License-Identifier: Apache-2.0

package bgzf

import (
	"bytes"
	"compress/gzip"
	"io"
)

// gunzip decompresses raw (gzip or bgzf — bgzf is gzip-compatible) with a hard
// cap on the decompressed size (MaxIndexBytes), so a compression bomb in an
// attacker-supplied index cannot exhaust memory (#101). Returns (nil,false) on
// any error or if the output would exceed the cap.
func gunzip(raw []byte) ([]byte, bool) {
	if len(raw) == 0 || len(raw) > MaxIndexBytes {
		return nil, false
	}
	zr, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		return nil, false
	}
	defer func() { _ = zr.Close() }()
	zr.Multistream(true) // bgzf is many concatenated gzip members
	out, err := io.ReadAll(io.LimitReader(zr, MaxIndexBytes+1))
	if err != nil && err != io.ErrUnexpectedEOF {
		return nil, false
	}
	if len(out) > MaxIndexBytes {
		return nil, false // decompression-bomb guard
	}
	return out, true
}
