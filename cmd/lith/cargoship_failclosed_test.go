// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/scttfrdmn/lith/internal/s3client/fake"
)

// discardLog is a logger that writes nowhere, for tests.
func discardLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

const (
	fixtureManifestKey = "lith-bench/cargoship/fixture-fix/uploads/20260911-f7fef25e/manifest.json"
	fixtureChunkKey    = "lith-bench/cargoship/fixture-fix/uploads/20260911-f7fef25e/shard-0/chunk-0.tar.zst"
)

func readFixtureManifest(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "internal", "cargoship", "testdata", "fixture", "manifest.json"))
	if err != nil {
		t.Fatalf("read fixture manifest: %v", err)
	}
	return b
}

// TestCargoshipBuildFailsClosed asserts that buildCargoshipIndex — the shared
// helper behind `index build --cargoship` and `mount --cargoship` — resolves a
// valid manifest, and that every manifest-resolution failure returns an error
// while issuing ZERO ListObjectsV2 calls. This is the guard against the
// session-36 auto-list footgun: a failed --cargoship resolution must never fall
// back to listing the bucket (#137).
func TestCargoshipBuildFailsClosed(t *testing.T) {
	valid := readFixtureManifest(t)

	t.Run("valid manifest builds with zero lists", func(t *testing.T) {
		srv := fake.New()
		srv.Put(fixtureManifestKey, valid, time.Now())
		srv.Put(fixtureChunkKey, []byte("chunk-bytes"), time.Now()) // HEAD target
		ix, err := buildCargoshipIndex(context.Background(), srv, "scttfrdmn-lith-bench", fixtureManifestKey, false, discardLog())
		if err != nil {
			t.Fatalf("valid manifest: %v", err)
		}
		if !ix.IsCargoship() {
			t.Fatalf("index is not cargoship")
		}
		if srv.ListCalls != 0 {
			t.Errorf("valid build issued %d ListObjectsV2 calls, want 0", srv.ListCalls)
		}
	})

	// Each failure mode: the manifest cannot be resolved, so the build must
	// return an error and must NOT list.
	cases := []struct {
		name    string
		body    []byte // manifest bytes to Put; nil => do not Put (missing)
		wantErr string // substring the error must contain
	}{
		{"missing", nil, "fetch cargoship manifest"},
		{"unparseable", []byte("this is not json {{"), "cargoship manifest"},
		{"wrong version", []byte(strings.Replace(string(valid), `"version": "2.1"`, `"version": "2.0"`, 1)), "version"},
		{"encrypted", []byte(strings.Replace(string(valid), `"format_features"`, `"encryption":{"enabled":true},"format_features"`, 1)), "encrypted"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := fake.New()
			if tc.body != nil {
				srv.Put(fixtureManifestKey, tc.body, time.Now())
			}
			_, err := buildCargoshipIndex(context.Background(), srv, "scttfrdmn-lith-bench", fixtureManifestKey, false, discardLog())
			if err == nil {
				t.Fatalf("%s: expected an error, got nil", tc.name)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("%s: error %q does not contain %q", tc.name, err.Error(), tc.wantErr)
			}
			if !strings.Contains(err.Error(), fixtureManifestKey) {
				t.Errorf("%s: error %q does not name the manifest key", tc.name, err.Error())
			}
			if srv.ListCalls != 0 {
				t.Errorf("%s: build issued %d ListObjectsV2 calls, want 0 (must fail closed, never list)", tc.name, srv.ListCalls)
			}
		})
	}

	t.Run("empty manifest key", func(t *testing.T) {
		srv := fake.New()
		_, err := buildCargoshipIndex(context.Background(), srv, "b", "", false, discardLog())
		if err == nil {
			t.Fatal("expected error for empty manifest key")
		}
		if srv.ListCalls != 0 {
			t.Errorf("empty key issued %d ListObjectsV2 calls, want 0", srv.ListCalls)
		}
	})
}

func TestParseCargoshipManifestArg(t *testing.T) {
	cases := []struct {
		arg, mountBucket, wantBucket, wantKey string
		wantErr                               bool
	}{
		{"s3://b/prefix/uploads/id/manifest.json.gz", "b", "b", "prefix/uploads/id/manifest.json.gz", false},
		{"prefix/uploads/id/manifest.json.gz", "b", "b", "prefix/uploads/id/manifest.json.gz", false},
		{"/prefix/manifest.json", "b", "b", "prefix/manifest.json", false},
		{"s3://other/key", "b", "other", "key", false}, // bucket mismatch is caught by the caller, not here
		{"s3://b", "b", "", "", true},                  // no key
		{"", "b", "", "", true},                        // empty
		{"not-an-s3-url-but-a-key", "b", "b", "not-an-s3-url-but-a-key", false},
	}
	for _, tc := range cases {
		gotB, gotK, err := parseCargoshipManifestArg(tc.arg, tc.mountBucket)
		if tc.wantErr {
			if err == nil {
				t.Errorf("%q: expected error", tc.arg)
			}
			continue
		}
		if err != nil {
			t.Errorf("%q: unexpected error %v", tc.arg, err)
			continue
		}
		if gotB != tc.wantBucket || gotK != tc.wantKey {
			t.Errorf("%q: got (%q,%q), want (%q,%q)", tc.arg, gotB, gotK, tc.wantBucket, tc.wantKey)
		}
	}
}
