// SPDX-License-Identifier: Apache-2.0

package pointer

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func realCurrent(tb testing.TB) []byte {
	tb.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "current_real.json"))
	if err != nil {
		tb.Fatalf("read seed: %v", err)
	}
	return b
}

func TestParseReal(t *testing.T) {
	c, err := Parse(realCurrent(t))
	if err != nil {
		t.Fatalf("parse real CURRENT: %v", err)
	}
	if c.Schema != Schema {
		t.Errorf("schema = %q", c.Schema)
	}
	if c.IndexKey == "" || c.ManifestKey == "" || c.VersionID == "" {
		t.Errorf("missing required field: %+v", c)
	}
	if len(c.IndexSHA256) != 64 {
		t.Errorf("index_sha256 len = %d", len(c.IndexSHA256))
	}
}

func TestParseRejects(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string // substring of the error
	}{
		{"empty", "", "empty"},
		{"not json", "not json", "not valid JSON"},
		{"wrong schema", `{"schema":"other/9","version_id":"v","manifest_key":"m","index_key":"i","index_sha256":"` + strings.Repeat("a", 64) + `"}`, "schema"},
		{"no version", `{"schema":"cargoship.current/1","manifest_key":"m","index_key":"i","index_sha256":"` + strings.Repeat("a", 64) + `"}`, "version_id"},
		{"no manifest", `{"schema":"cargoship.current/1","version_id":"v","index_key":"i","index_sha256":"` + strings.Repeat("a", 64) + `"}`, "manifest_key"},
		{"no index", `{"schema":"cargoship.current/1","version_id":"v","manifest_key":"m","index_sha256":"` + strings.Repeat("a", 64) + `"}`, "index_key"},
		{"short sha", `{"schema":"cargoship.current/1","version_id":"v","manifest_key":"m","index_key":"i","index_sha256":"abcd"}`, "64 hex"},
		{"non-hex sha", `{"schema":"cargoship.current/1","version_id":"v","manifest_key":"m","index_key":"i","index_sha256":"` + strings.Repeat("z", 64) + `"}`, "not hex"},
		{"negative files", `{"schema":"cargoship.current/1","version_id":"v","manifest_key":"m","index_key":"i","index_sha256":"` + strings.Repeat("a", 64) + `","file_count":-1}`, "file_count"},
		{"unknown field", `{"schema":"cargoship.current/1","version_id":"v","manifest_key":"m","index_key":"i","index_sha256":"` + strings.Repeat("a", 64) + `","evil":1}`, "not valid JSON"},
		{"trailing data", `{"schema":"cargoship.current/1","version_id":"v","manifest_key":"m","index_key":"i","index_sha256":"` + strings.Repeat("a", 64) + `"} garbage`, "trailing"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Parse([]byte(c.in))
			if err == nil {
				t.Fatalf("want error containing %q, got nil", c.want)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error %q does not contain %q", err.Error(), c.want)
			}
		})
	}
}

func TestParseOverCap(t *testing.T) {
	big := make([]byte, MaxPointerBytes+1)
	if _, err := Parse(big); err == nil || !strings.Contains(err.Error(), "cap") {
		t.Fatalf("over-cap input must be refused, got %v", err)
	}
}

// FuzzParse: no input may panic, and any error is a plain returned error. Seeded
// from the real published CURRENT plus truncations and bit-flips.
func FuzzParse(f *testing.F) {
	seed := realCurrent(f)
	f.Add(seed)
	f.Add([]byte(""))
	f.Add([]byte("{}"))
	for i := 0; i < len(seed); i += 7 {
		f.Add(seed[:i]) // truncations
	}
	flip := append([]byte(nil), seed...)
	for i := range flip {
		if i%13 == 0 {
			flip[i] ^= 0xff
		}
	}
	f.Add(flip)
	f.Fuzz(func(t *testing.T, b []byte) {
		c, err := Parse(b) // must never panic
		if err == nil && c == nil {
			t.Fatal("nil error but nil pointer")
		}
		if err == nil {
			// A successful parse must satisfy the invariants.
			if c.Schema != Schema || c.IndexKey == "" || c.ManifestKey == "" || c.VersionID == "" || len(c.IndexSHA256) != 64 {
				t.Errorf("parsed pointer violates invariants: %+v", c)
			}
		}
	})
}
