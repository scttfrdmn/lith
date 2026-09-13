// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/scttfrdmn/lith/internal/pointer"
	"github.com/scttfrdmn/lith/internal/s3client/fake"
)

const (
	ptrBucket  = "scttfrdmn-lith-bench"
	ptrDataset = "lith-bench/published/smoke"
)

// buildRealIndexImage builds a genuine cargoship-backed index image from the
// committed fixture manifest, so the resolver test exercises a real .lith image.
func buildRealIndexImage(t *testing.T, srv *fake.Server) []byte {
	t.Helper()
	srv.Put(fixtureManifestKey, readFixtureManifest(t), time.Now())
	srv.Put(fixtureChunkKey, []byte("chunk-bytes"), time.Now())
	ix, err := buildCargoshipIndex(context.Background(), srv, ptrBucket, fixtureManifestKey, false, discardLog())
	if err != nil {
		t.Fatalf("build fixture index: %v", err)
	}
	return ix.Marshal()
}

func validCurrent(t *testing.T, indexKey string, sha [32]byte) []byte {
	t.Helper()
	b, err := json.Marshal(pointer.Current{
		Schema: pointer.Schema, VersionID: "20260913T213513Z-13de9c",
		ManifestKey: ptrDataset + "/v/20260913T213513Z-13de9c/uploads/x/manifest.json.gz",
		IndexKey:    indexKey, IndexSHA256: hex.EncodeToString(sha[:]),
		Created: "2026-09-13T21:35:50Z", FileCount: 4, TotalBytes: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestResolvePointerCurrent(t *testing.T) {
	srv := fake.New()
	image := buildRealIndexImage(t, srv)
	indexKey := ptrDataset + "/v/20260913T213513Z-13de9c/index.lith"
	srv.Put(indexKey, image, time.Now())
	srv.Put(ptrDataset+"/CURRENT", validCurrent(t, indexKey, sha256.Sum256(image)), time.Now())

	ix, vid, err := resolvePointer(context.Background(), srv, ptrBucket, ptrDataset, "current")
	if err != nil {
		t.Fatalf("resolve @current: %v", err)
	}
	if ix.Len() == 0 {
		t.Fatal("resolved index is empty")
	}
	if vid != "20260913T213513Z-13de9c" {
		t.Errorf("version id = %q", vid)
	}
	if srv.ListCalls != 0 {
		t.Errorf("resolve issued %d ListObjectsV2 calls, want 0", srv.ListCalls)
	}
}

func TestResolvePointerPinnedVersion(t *testing.T) {
	srv := fake.New()
	image := buildRealIndexImage(t, srv)
	vid := "20260913T213513Z-13de9c"
	srv.Put(ptrDataset+"/v/"+vid+"/index.lith", image, time.Now())
	// No CURRENT — a pinned version resolves directly.
	ix, got, err := resolvePointer(context.Background(), srv, ptrBucket, ptrDataset, vid)
	if err != nil {
		t.Fatalf("resolve @%s: %v", vid, err)
	}
	if ix.Len() == 0 || got != vid {
		t.Errorf("pinned resolve: len=%d vid=%q", ix.Len(), got)
	}
	if srv.ListCalls != 0 {
		t.Errorf("pinned resolve issued %d lists, want 0", srv.ListCalls)
	}
}

// TestResolvePointerFailsClosed: every malformed/dangling case returns an error
// and issues ZERO ListObjectsV2 calls — a pointer failure never degrades into a
// whole-bucket listing (#141).
func TestResolvePointerFailsClosed(t *testing.T) {
	image := func(t *testing.T) ([]byte, string) {
		srv := fake.New()
		img := buildRealIndexImage(t, srv)
		return img, ptrDataset + "/v/v1/index.lith"
	}

	t.Run("missing CURRENT", func(t *testing.T) {
		srv := fake.New()
		_, _, err := resolvePointer(context.Background(), srv, ptrBucket, ptrDataset, "current")
		if err == nil || !strings.Contains(err.Error(), "CURRENT") {
			t.Fatalf("want CURRENT error, got %v", err)
		}
		if srv.ListCalls != 0 {
			t.Errorf("issued %d lists", srv.ListCalls)
		}
	})

	t.Run("malformed CURRENT", func(t *testing.T) {
		srv := fake.New()
		srv.Put(ptrDataset+"/CURRENT", []byte("}{ not json"), time.Now())
		_, _, err := resolvePointer(context.Background(), srv, ptrBucket, ptrDataset, "current")
		if err == nil {
			t.Fatal("malformed CURRENT must error")
		}
		if srv.ListCalls != 0 {
			t.Errorf("issued %d lists", srv.ListCalls)
		}
	})

	t.Run("dangling index_key", func(t *testing.T) {
		srv := fake.New()
		img, indexKey := image(t)
		srv.Put(ptrDataset+"/CURRENT", validCurrent(t, indexKey, sha256.Sum256(img)), time.Now())
		// index_key is NOT put → GET fails.
		_, _, err := resolvePointer(context.Background(), srv, ptrBucket, ptrDataset, "current")
		if err == nil || !strings.Contains(err.Error(), "index") {
			t.Fatalf("want dangling-index error, got %v", err)
		}
		if srv.ListCalls != 0 {
			t.Errorf("issued %d lists", srv.ListCalls)
		}
	})

	t.Run("sha mismatch", func(t *testing.T) {
		srv := fake.New()
		img, indexKey := image(t)
		srv.Put(indexKey, img, time.Now())
		var wrong [32]byte // all zero — will not match
		srv.Put(ptrDataset+"/CURRENT", validCurrent(t, indexKey, wrong), time.Now())
		_, _, err := resolvePointer(context.Background(), srv, ptrBucket, ptrDataset, "current")
		if err == nil || !strings.Contains(err.Error(), "sha256") {
			t.Fatalf("want sha mismatch error, got %v", err)
		}
	})
}

// TestChunklessManifestFailsClosed: an index built from a chunkless (direct-upload)
// manifest is rejected, naming the manifest, with zero ListObjectsV2 calls — the
// lith-side counterpart of the publish 0-chunk finding (same class as #141).
func TestChunklessManifestFailsClosed(t *testing.T) {
	srv := fake.New()
	chunkless := []byte(`{"version":"2.1","upload_id":"x","chunks":[]}`)
	manifestKey := "lith-bench/published/smoke/v/vX/uploads/x/manifest.json"
	srv.Put(manifestKey, chunkless, time.Now())
	_, err := buildCargoshipIndex(context.Background(), srv, ptrBucket, manifestKey, false, discardLog())
	if err == nil {
		t.Fatal("chunkless manifest must fail closed")
	}
	if !strings.Contains(err.Error(), manifestKey) {
		t.Errorf("error should name the manifest %q: %v", manifestKey, err)
	}
	if !strings.Contains(err.Error(), "no chunks") {
		t.Errorf("error should explain the chunkless cause: %v", err)
	}
	if srv.ListCalls != 0 {
		t.Errorf("chunkless build issued %d lists, want 0", srv.ListCalls)
	}
}

func TestSplitPointerRef(t *testing.T) {
	cases := []struct{ in, prefix, ref string }{
		{"dataset@current", "dataset", "current"},
		{"a/b/c@20260101T000000Z-abc", "a/b/c", "20260101T000000Z-abc"},
		{"plain/prefix", "plain/prefix", ""},
		{"", "", ""},
	}
	for _, c := range cases {
		p, r := splitPointerRef(c.in)
		if p != c.prefix || r != c.ref {
			t.Errorf("splitPointerRef(%q) = (%q,%q), want (%q,%q)", c.in, p, r, c.prefix, c.ref)
		}
	}
}
