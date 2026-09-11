// SPDX-License-Identifier: Apache-2.0

package index

import (
	"context"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/scttfrdmn/lith/internal/s3client/fake"
)

// TestBuildFromKeysListDenied builds an index from an explicit key list against
// a fake whose LIST is denied but HEAD is allowed (Common Crawl cc-index /
// nyc-tlc shape), and asserts the result matches a plain Build over the same
// entries.
func TestBuildFromKeysListDenied(t *testing.T) {
	srv := fake.New()
	srv.DenyList = true
	now := time.Unix(1_700_000_000, 0)
	srv.PutString("data/a", "AAA", now)
	srv.PutString("data/b/c", "CC", now)
	srv.PutString("data/b/d", "D", now)

	// Sanity: LIST really is denied, so listing cannot build this index.
	if _, err := BuildFromList(context.Background(), srv, ListOptions{
		Options: Options{Bucket: "bkt", Prefix: "data"},
	}); err == nil {
		t.Fatal("expected BuildFromList to fail against a LIST-denied bucket")
	}

	keys := []KeyEntry{{Key: "a"}, {Key: "b/c"}, {Key: "b/d"}}
	ix, res, err := BuildFromKeys(context.Background(), srv, KeysOptions{
		Options: Options{Bucket: "bkt", Prefix: "data"},
		Keys:    keys,
	})
	if err != nil {
		t.Fatalf("BuildFromKeys: %v", err)
	}
	if res.Headed != 3 || res.Missing != 0 {
		t.Errorf("result = %+v, want headed 3 missing 0", res)
	}
	if ix.Len() != 3 {
		t.Fatalf("Len = %d, want 3", ix.Len())
	}
	if ix.Source() != "keys" {
		t.Errorf("Source = %q, want keys", ix.Source())
	}
	if fi, err := ix.Stat("/a"); err != nil || fi.Size != 3 {
		t.Errorf("stat /a: fi=%+v err=%v", fi, err)
	}
	if got := names(readAll(t, ix, "/b")); !equalStrings(got, []string{"c", "d"}) {
		t.Errorf("readdir /b = %v, want [c d]", got)
	}
}

// TestBuildFromKeysMissing covers a 404 key with and without AllowMissing.
func TestBuildFromKeysMissing(t *testing.T) {
	srv := fake.New()
	now := time.Unix(1_700_000_000, 0)
	srv.PutString("a", "AAA", now)
	// "ghost" is not stored -> HeadObject 404.
	keys := []KeyEntry{{Key: "a"}, {Key: "ghost"}}

	// Without AllowMissing: error naming the missing key.
	if _, res, err := BuildFromKeys(context.Background(), srv, KeysOptions{
		Options: Options{Bucket: "bkt"},
		Keys:    keys,
	}); err == nil {
		t.Fatal("expected error for a missing key")
	} else {
		if !strings.Contains(err.Error(), "ghost") {
			t.Errorf("error should name the missing key: %v", err)
		}
		if res.Missing != 1 {
			t.Errorf("res.Missing = %d, want 1", res.Missing)
		}
	}

	// With AllowMissing: skipped and counted, build succeeds without it.
	ix, res, err := BuildFromKeys(context.Background(), srv, KeysOptions{
		Options:      Options{Bucket: "bkt"},
		Keys:         keys,
		AllowMissing: true,
	})
	if err != nil {
		t.Fatalf("BuildFromKeys(AllowMissing): %v", err)
	}
	if res.Missing != 1 || res.Headed != 1 || ix.Len() != 1 {
		t.Errorf("res=%+v len=%d, want missing 1 headed 1 len 1", res, ix.Len())
	}
	if _, err := ix.Stat("/ghost"); err == nil {
		t.Error("missing key must not be in the index")
	}
}

// TestBuildFromKeysMetaSkipsHead asserts a key carrying size+mtime issues no
// HeadObject, while a bare key does.
func TestBuildFromKeysMetaSkipsHead(t *testing.T) {
	srv := fake.New()
	now := time.Unix(1_700_000_000, 0)
	srv.PutString("bare", "hello", now)
	srv.PutString("withmeta", "ignored-size", now)

	keys := []KeyEntry{
		{Key: "bare"}, // needs HEAD
		{Key: "withmeta", Size: 999, MTime: 42, HasMeta: true}, // no HEAD
	}
	ix, res, err := BuildFromKeys(context.Background(), srv, KeysOptions{
		Options: Options{Bucket: "bkt"},
		Keys:    keys,
	})
	if err != nil {
		t.Fatalf("BuildFromKeys: %v", err)
	}
	if srv.HeadCalls != 1 {
		t.Errorf("HeadCalls = %d, want 1 (only the bare key)", srv.HeadCalls)
	}
	if res.Headed != 1 {
		t.Errorf("res.Headed = %d, want 1", res.Headed)
	}
	// The meta key keeps the file-supplied size and an unknown (0) ETag hash.
	if fi, err := ix.Stat("/withmeta"); err != nil || fi.Size != 999 {
		t.Errorf("stat /withmeta: fi=%+v err=%v (want size 999)", fi, err)
	}
	if ix.ETagHashOf("/withmeta") != 0 {
		t.Error("meta key ETagHash should be 0 (no HEAD)")
	}
	if fi, _ := ix.Stat("/bare"); fi.Size != 5 {
		t.Errorf("stat /bare size = %d, want 5 (from HEAD)", fi.Size)
	}
}

// TestBuildFromKeysDirsMatchListing asserts the directory structure derived
// from a key list is identical to that derived from a listing of the same keys.
func TestBuildFromKeysDirsMatchListing(t *testing.T) {
	srv := fake.New()
	now := time.Unix(1_700_000_000, 0)
	rel := []string{"a", "b/c", "b/d", "b/sub/e", "f/g/h"}
	for _, k := range rel {
		srv.PutString("root/"+k, "x", now)
	}

	fromList, err := BuildFromList(context.Background(), srv, ListOptions{
		Options: Options{Bucket: "bkt", Prefix: "root"},
	})
	if err != nil {
		t.Fatalf("BuildFromList: %v", err)
	}

	keys := make([]KeyEntry, len(rel))
	for i, k := range rel {
		keys[i] = KeyEntry{Key: k}
	}
	fromKeys, _, err := BuildFromKeys(context.Background(), srv, KeysOptions{
		Options: Options{Bucket: "bkt", Prefix: "root"},
		Keys:    keys,
	})
	if err != nil {
		t.Fatalf("BuildFromKeys: %v", err)
	}

	// Compare the full directory set and the key set.
	if got, want := walkDirs(fromKeys), walkDirs(fromList); !equalStrings(got, want) {
		t.Errorf("dir sets differ:\n keys=%v\n list=%v", got, want)
	}
	if got, want := allKeys(fromKeys), allKeys(fromList); !equalStrings(got, want) {
		t.Errorf("key sets differ:\n keys=%v\n list=%v", got, want)
	}
}

// TestFormatV4Provenance round-trips Source and KeysSHAHex through Marshal /
// Unmarshal.
func TestFormatV4Provenance(t *testing.T) {
	var sha [32]byte
	for i := range sha {
		sha[i] = byte(i + 1)
	}
	ix := Build([]Entry{{Key: "a"}, {Key: "b/c"}}, Options{
		Bucket:  "bkt",
		Source:  "manifest",
		KeysSHA: sha,
	})
	if ix.Source() != "manifest" {
		t.Fatalf("built Source = %q", ix.Source())
	}
	want := ix.KeysSHAHex()
	if len(want) != 64 {
		t.Fatalf("KeysSHAHex len = %d, want 64", len(want))
	}

	got, err := Unmarshal(ix.Marshal())
	if err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got.Source() != "manifest" {
		t.Errorf("round-trip Source = %q, want manifest", got.Source())
	}
	if got.KeysSHAHex() != want {
		t.Errorf("round-trip KeysSHAHex = %q, want %q", got.KeysSHAHex(), want)
	}

	// A default (list) build carries no keys sha.
	def := Build([]Entry{{Key: "a"}}, Options{Bucket: "b"})
	rt, err := Unmarshal(def.Marshal())
	if err != nil {
		t.Fatal(err)
	}
	if rt.Source() != "list" || rt.KeysSHAHex() != "" {
		t.Errorf("default build: Source=%q sha=%q, want list/empty", rt.Source(), rt.KeysSHAHex())
	}
}

// walkDirs returns every directory path (relative, root as "") in the index.
func walkDirs(ix *Index) []string {
	out := make([]string, 0, ix.dirCount())
	for i := 0; i < ix.dirCount(); i++ {
		out = append(out, ix.dirPath(i))
	}
	sort.Strings(out)
	return out
}

// allKeys returns every stored relative key, sorted.
func allKeys(ix *Index) []string {
	out := make([]string, 0, ix.Len())
	for i := 0; i < ix.Len(); i++ {
		out = append(out, ix.key(i))
	}
	sort.Strings(out)
	return out
}
