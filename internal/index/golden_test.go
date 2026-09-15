// SPDX-License-Identifier: Apache-2.0

package index

import (
	"os"
	"path/filepath"
	"testing"
)

// TestGolden1xIndexesStillOpen is the mechanical guard for the 1.x index
// compatibility contract (docs/scope.md, #196): every lith 1.x reader must read
// every index produced by lith 1.0.x. The committed images under
// testdata/golden/ were produced by 1.0.x via the three production build paths
// (native, --keys, CargoShip-backed). If this test fails, a format change broke
// the promise — that is a 1.x compatibility event, not a fixture to regenerate
// casually (see testdata/golden/gen.go).
func TestGolden1xIndexesStillOpen(t *testing.T) {
	cases := []struct {
		file       string
		minLen     int
		wantSource string // "" = don't assert
		backed     bool   // CargoShip-backed: BackingOf must resolve
	}{
		{file: "native.lith", minLen: 1},
		{file: "keys.lith", minLen: 1},
		{file: "cargoship.lith", minLen: 1, wantSource: "cargoship", backed: true},
	}
	for _, c := range cases {
		t.Run(c.file, func(t *testing.T) {
			b, err := os.ReadFile(filepath.Join("testdata", "golden", c.file))
			if err != nil {
				t.Fatalf("read golden %s: %v (regenerate with testdata/golden/gen.go only for a deliberate format change)", c.file, err)
			}
			ix, err := Unmarshal(b)
			if err != nil {
				t.Fatalf("Unmarshal %s: %v — a lith 1.x reader must read every 1.0.x index", c.file, err)
			}
			if ix.Len() < c.minLen {
				t.Errorf("%s: Len()=%d, want >= %d", c.file, ix.Len(), c.minLen)
			}
			if c.wantSource != "" && ix.Source() != c.wantSource {
				t.Errorf("%s: Source()=%q, want %q", c.file, ix.Source(), c.wantSource)
			}
			if c.backed {
				// A CargoShip-backed index resolves a frame backing for its files.
				ents, _, err := ix.Readdir("", 0, 4096)
				if err != nil {
					t.Fatalf("%s: readdir root: %v", c.file, err)
				}
				found := false
				for _, e := range ents {
					if e.IsDir {
						continue
					}
					if _, ok := ix.BackingOf(e.Name); ok {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("%s: no file with a CargoShip backing at root", c.file)
				}
			}
		})
	}
}
