// SPDX-License-Identifier: Apache-2.0

package zarr

import (
	"reflect"
	"testing"
)

const sampleZmetadata = `{"zarr_consolidated_format":1,"metadata":{` +
	`"streamflow/.zarray":{"shape":[100,200],"chunks":[10,20],"dtype":"<i4"},` +
	`"streamflow/.zattrs":{"_ARRAY_DIMENSIONS":["time","feature"]},` +
	`"time/.zarray":{"shape":[100],"chunks":[10],"dtype":"<i8"},` +
	`".zgroup":{"zarr_format":2}}}`

func TestParseArray(t *testing.T) {
	g, ok := ParseArray([]byte(`{"shape":[367439,2776738],"chunks":[672,30000]}`))
	if !ok || !reflect.DeepEqual(g.Dims, []int{547, 93}) {
		t.Fatalf("ParseArray = %v,%v; want dims [547 93]", g, ok)
	}
	for _, bad := range []string{
		``, `not json`, `{}`, `{"shape":[10]}`,
		`{"shape":[10],"chunks":[0]}`,   // zero chunk
		`{"shape":[-1],"chunks":[1]}`,   // negative shape
		`{"shape":[10],"chunks":[1,2]}`, // length mismatch
	} {
		if g, ok := ParseArray([]byte(bad)); ok {
			t.Errorf("ParseArray(%q) = %v, want !ok", bad, g)
		}
	}
}

func TestParseConsolidated(t *testing.T) {
	m, ok := ParseConsolidated([]byte(sampleZmetadata))
	if !ok {
		t.Fatal("ParseConsolidated failed on a valid document")
	}
	if !reflect.DeepEqual(m["streamflow"].Dims, []int{10, 10}) {
		t.Errorf("streamflow dims = %v, want [10 10]", m["streamflow"].Dims)
	}
	if !reflect.DeepEqual(m["time"].Dims, []int{10}) {
		t.Errorf("time dims = %v, want [10]", m["time"].Dims)
	}
	for _, bad := range []string{``, `garbage`, `{}`, `{"metadata":{}}`, `{"metadata":{"a/.zattrs":{}}}`} {
		if m, ok := ParseConsolidated([]byte(bad)); ok {
			t.Errorf("ParseConsolidated(%q) = %v, want !ok", bad, m)
		}
	}
}

func TestParseConsolidatedSizeCap(t *testing.T) {
	if _, ok := ParseConsolidated(make([]byte, MaxConsolidatedBytes+1)); ok {
		t.Fatal("oversize .zmetadata accepted")
	}
}

func TestParseCoords(t *testing.T) {
	if c, ok := ParseCoords("0.12.5"); !ok || !reflect.DeepEqual(c, []int{0, 12, 5}) {
		t.Fatalf("ParseCoords = %v,%v", c, ok)
	}
	for _, bad := range []string{"", ".zarray", ".zgroup", "0.-1", "a.b", "0.x.2"} {
		if c, ok := ParseCoords(bad); ok {
			t.Errorf("ParseCoords(%q) = %v, want !ok", bad, c)
		}
	}
}

// FuzzParseConsolidated asserts the .zmetadata parser never panics on arbitrary
// (attacker-controlled) bytes, and that a parsed grid is internally consistent
// (#101 rule). Seeded with a real document plus truncations and bit-flips.
func FuzzParseConsolidated(f *testing.F) {
	seed := []byte(sampleZmetadata)
	f.Add(seed)
	f.Add([]byte(`{"metadata":{"a/.zarray":{"shape":[1],"chunks":[1]}}}`))
	for _, cut := range []int{0, 1, 10, len(seed) / 2, len(seed) - 1} {
		if cut >= 0 && cut < len(seed) {
			f.Add(append([]byte(nil), seed[:cut]...))
		}
	}
	for _, pos := range []int{5, 20, 40, len(seed) / 2, len(seed) - 2} {
		if pos >= 0 && pos < len(seed) {
			c := append([]byte(nil), seed...)
			c[pos] ^= 0xFF
			f.Add(c)
		}
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		m, ok := ParseConsolidated(data)
		if !ok {
			return
		}
		for dir, g := range m {
			if g == nil || len(g.Dims) == 0 || len(g.Dims) > maxDims {
				t.Fatalf("dir %q: bad grid %v", dir, g)
			}
			for _, d := range g.Dims {
				if d < 0 {
					t.Fatalf("dir %q: negative dim in %v", dir, g.Dims)
				}
			}
		}
	})
}
