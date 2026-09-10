// SPDX-License-Identifier: Apache-2.0

package index

import (
	"errors"
	"strings"
	"testing"
)

// realIndexImage builds a small but structurally complete index (files, nested
// dirs) and returns its serialized image — a stand-in for a real RODA index for
// the corruption corpus.
func realIndexImage() []byte {
	return build("data/", "a", "b/c", "b/d", "b/sub/e", "f").Marshal()
}

// TestParseRejectsCorruptImages: truncations and bit-flips must return a clean
// error (ErrCorruptIndex, or the version/byte-order messages) or a value, and
// never panic. Scope is the deserializer: it bounds-checks every length/offset
// before slicing. (A bit-flip can still produce a semantically-inconsistent but
// structurally-valid image — e.g. an unsorted arena — so we do not run query
// paths over arbitrary corrupt images here; see TestQueryGoodImage.)
func TestParseRejectsCorruptImages(t *testing.T) {
	img := realIndexImage()

	if _, err := Unmarshal(img); err != nil {
		t.Fatalf("valid image: %v", err)
	}

	noPanic := func(what string, b []byte) {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("%s panicked: %v", what, r)
			}
		}()
		_, _ = Unmarshal(b)
	}

	for cut := 0; cut < len(img); cut++ {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("truncation to %d bytes panicked: %v", cut, r)
				}
			}()
			if _, err := Unmarshal(img[:cut]); err == nil {
				t.Fatalf("truncation to %d bytes parsed without error", cut)
			}
		}()
	}
	for pos := 0; pos < len(img); pos++ {
		c := append([]byte(nil), img...)
		c[pos] ^= 0xFF
		noPanic("bit-flip at "+itoa(pos), c)
	}
}

// TestQueryGoodImage: a freshly parsed valid image is fully queryable — this is
// where the arena-slicing paths (Key, Readdir, Stat) are exercised, on a
// known-sorted image, so validateOffsets' guarantee is checked end to end.
func TestQueryGoodImage(t *testing.T) {
	ix, err := Unmarshal(realIndexImage())
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < ix.Len(); i++ {
		_ = ix.key(i)
	}
	var cur uint64
	for {
		ents, next, err := ix.Readdir("/", cur, 256)
		if err != nil || len(ents) == 0 {
			break
		}
		for _, e := range ents {
			if _, err := ix.Stat("/" + e.Name); err != nil && !e.IsDir {
				t.Fatalf("Stat(%q): %v", e.Name, err)
			}
		}
		cur = next
	}
}

// FuzzParseIndex asserts the deserializer never panics on arbitrary bytes and,
// when it errors, the error is one of the defined kinds (not a wrapped panic).
// Seeded with a real index plus truncated and bit-flipped variants; the
// committed corpus runs as a regression suite under plain `go test`.
func FuzzParseIndex(f *testing.F) {
	img := realIndexImage()
	f.Add(img)
	for _, cut := range []int{0, 1, 8, 12, 16, 48, headerSize, headerSize + 4, len(img) / 2, len(img) - 1} {
		if cut >= 0 && cut <= len(img) {
			f.Add(append([]byte(nil), img[:cut]...))
		}
	}
	for _, pos := range []int{8, 12, 16, 24, 48, headerSize, headerSize + 4, len(img) / 2, len(img) - 2, len(img) - 1} {
		if pos >= 0 && pos < len(img) {
			c := append([]byte(nil), img...)
			c[pos] ^= 0xFF
			f.Add(c)
		}
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("parse panicked on a %d-byte image: %v", len(data), r)
			}
		}()
		_, err := Unmarshal(data)
		if err != nil &&
			!errors.Is(err, ErrCorruptIndex) &&
			!strings.Contains(err.Error(), "format version") &&
			!strings.Contains(err.Error(), "byte order") {
			t.Fatalf("unexpected error kind: %v", err)
		}
	})
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
