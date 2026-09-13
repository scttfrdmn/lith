// SPDX-License-Identifier: Apache-2.0

package lithindex

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/scttfrdmn/lith/internal/index"
)

// fakeFetcher returns a canned ETag for any HEAD and records the keys it saw. The
// fixture has no incremental chain, so GetObject must never be called.
type fakeFetcher struct {
	heads   []string
	getCall bool
}

func (f *fakeFetcher) HeadETag(_ context.Context, key string) (string, error) {
	f.heads = append(f.heads, key)
	return `"etag-` + key + `"`, nil
}

func (f *fakeFetcher) GetObject(_ context.Context, _ string) (io.ReadCloser, error) {
	f.getCall = true
	return nil, errors.New("unexpected GetObject")
}

func fixtureManifest(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "internal", "cargoship", "testdata", "fixture", "manifest.json"))
	if err != nil {
		t.Fatalf("read fixture manifest: %v", err)
	}
	return b
}

func TestBuildIndexFromManifest(t *testing.T) {
	f := &fakeFetcher{}
	image, sha, err := BuildIndexFromManifest(context.Background(), f, fixtureManifest(t), Options{Bucket: "b"})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if len(image) == 0 {
		t.Fatal("empty index image")
	}
	if sha == [32]byte{} {
		t.Fatal("zero sha")
	}
	if f.getCall {
		t.Error("GetObject called for a single-manifest build (no incremental chain)")
	}
	if len(f.heads) == 0 {
		t.Error("no chunk HEADed — the ETag identity was not fetched")
	}
	// The image must be a valid lith index recording the cargoship source.
	ix, err := index.Unmarshal(image)
	if err != nil {
		t.Fatalf("unmarshal built image: %v", err)
	}
	if got := ix.Source(); got != "cargoship" {
		t.Errorf("index source = %q, want cargoship", got)
	}
	// Deterministic: same inputs, same image sha.
	_, sha2, err := BuildIndexFromManifest(context.Background(), &fakeFetcher{}, fixtureManifest(t), Options{Bucket: "b"})
	if err != nil {
		t.Fatalf("second build: %v", err)
	}
	if sha != sha2 {
		t.Errorf("non-deterministic image sha: %x vs %x", sha, sha2)
	}
}

func TestBuildIndexFromManifestErrors(t *testing.T) {
	if _, _, err := BuildIndexFromManifest(context.Background(), nil, []byte("{}"), Options{}); err == nil {
		t.Error("nil fetcher must error")
	}
	if _, _, err := BuildIndexFromManifest(context.Background(), &fakeFetcher{}, []byte("not json"), Options{}); err == nil {
		t.Error("malformed manifest must error")
	}
}
