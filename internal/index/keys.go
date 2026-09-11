// SPDX-License-Identifier: Apache-2.0

package index

import (
	"bufio"
	"compress/gzip"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// KeyEntry is one line of a --keys file, already parsed. Key is relative to the
// build's Prefix (it is joined with the normalized prefix to form the object
// key HeadObject is issued against). HasMeta is true only when the line carried
// both a size and an mtime, in which case no HeadObject is needed.
type KeyEntry struct {
	Key     string
	Size    int64
	MTime   int64 // Unix nanoseconds
	HasMeta bool
}

// Size/count ceilings that bound a key-list parse. A --keys file (or a Common
// Crawl manifest) is attacker-influenced in lith's threat model, so both the
// byte size and the entry count are capped to stay bounded. Declared as vars so
// tests can lower them; production never mutates them.
var (
	// maxKeyListBytes caps the (post-gunzip) key-list bytes read.
	maxKeyListBytes int64 = 8 << 30 // 8 GiB
	// maxKeyEntries caps the number of parsed key lines.
	maxKeyEntries = 500_000_000
	// maxKeyLineBytes caps a single line; S3 keys are <= 1024 bytes, so 1 MiB
	// is a generous ceiling that still refuses a hostile unbounded line.
	maxKeyLineBytes = 1 << 20
)

// ErrTooManyKeyEntries is returned when a key list exceeds maxKeyEntries.
var ErrTooManyKeyEntries = fmt.Errorf("index: key list has too many entries (limit %d)", maxKeyEntries)

// MaybeGunzip peeks the first two bytes of r and, if they are the gzip magic
// (0x1f 0x8b), returns a gzip.Reader wrapping the stream; otherwise it returns
// the (buffered) stream unchanged. It lets a caller accept either a plain or a
// gzipped key list / manifest without knowing which.
func MaybeGunzip(r io.Reader) (io.Reader, error) {
	br := bufio.NewReader(r)
	magic, err := br.Peek(2)
	if err != nil {
		// Fewer than 2 bytes (empty or tiny input): not gzip, hand back as-is.
		if err == io.EOF {
			return br, nil
		}
		return nil, err
	}
	if magic[0] == 0x1f && magic[1] == 0x8b {
		return gzip.NewReader(br)
	}
	return br, nil
}

// ParseKeyList reads one key per line: "key", "key\t<size>", or
// "key\t<size>\t<mtime-unix-nanos>". Lines beginning with '#' and blank lines
// are skipped; leading/trailing whitespace is trimmed. HasMeta is set only when
// both size and mtime parse. A malformed size/mtime is an error naming the line
// number. The reader is bounded by maxKeyListBytes and the entry count by
// maxKeyEntries so a hostile list cannot exhaust memory.
func ParseKeyList(r io.Reader) ([]KeyEntry, error) {
	capped := &cappedReader{
		r:   r,
		n:   maxKeyListBytes + 1,
		err: fmt.Errorf("index: key list exceeds the size limit (%d bytes) — refusing to parse a possibly-hostile list", maxKeyListBytes),
	}
	sc := bufio.NewScanner(capped)
	sc.Buffer(make([]byte, 0, 64*1024), maxKeyLineBytes)

	var out []KeyEntry
	line := 0
	for sc.Scan() {
		line++
		raw := strings.TrimSpace(sc.Text())
		if raw == "" || strings.HasPrefix(raw, "#") {
			continue
		}
		if len(out) >= maxKeyEntries {
			return nil, ErrTooManyKeyEntries
		}
		fields := strings.Split(raw, "\t")
		ke := KeyEntry{Key: strings.TrimSpace(fields[0])}
		if ke.Key == "" {
			continue
		}
		switch {
		case len(fields) >= 3:
			sz, err := strconv.ParseInt(strings.TrimSpace(fields[1]), 10, 64)
			if err != nil || sz < 0 {
				return nil, fmt.Errorf("index: key list line %d: bad size %q", line, fields[1])
			}
			mt, err := strconv.ParseInt(strings.TrimSpace(fields[2]), 10, 64)
			if err != nil {
				return nil, fmt.Errorf("index: key list line %d: bad mtime %q", line, fields[2])
			}
			ke.Size = sz
			ke.MTime = mt
			ke.HasMeta = true
		case len(fields) == 2:
			// Size given but no mtime: still needs a HeadObject for the mtime and
			// ETag, so HasMeta stays false. Validate the size for a clear error.
			sz, err := strconv.ParseInt(strings.TrimSpace(fields[1]), 10, 64)
			if err != nil || sz < 0 {
				return nil, fmt.Errorf("index: key list line %d: bad size %q", line, fields[1])
			}
			ke.Size = sz
		}
		out = append(out, ke)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
