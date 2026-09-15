//go:build ignore

// Command gen writes the golden 1.0.x index images used by
// TestGolden1xIndexesStillOpen. Regenerate deliberately (a format change is a
// 1.x compatibility event, per docs/scope.md) with:
//
//	cd internal/index/testdata/golden && go run gen.go
//
// The three images exercise the three production build paths at format v5:
// native (BuildFromList), --keys (BuildFromKeys), and CargoShip-backed
// (pkg/lithindex from the committed manifest fixture).
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/scttfrdmn/lith/internal/index"
	"github.com/scttfrdmn/lith/internal/s3client/fake"
	"github.com/scttfrdmn/lith/pkg/lithindex"
)

type headOnlyFetcher struct{}

func (headOnlyFetcher) HeadETag(_ context.Context, key string) (string, error) {
	return `"etag-` + key + `"`, nil
}
func (headOnlyFetcher) GetObject(_ context.Context, _ string) (io.ReadCloser, error) {
	return nil, errors.New("gen: unexpected GetObject")
}

func main() {
	ctx := context.Background()
	now := time.Unix(1_700_000_000, 0)

	// native: object-backed index built by listing.
	srv := fake.New()
	srv.PutString("a/1.txt", "hello", now)
	srv.PutString("a/2.txt", "world!!", now)
	srv.PutString("b/3.bin", "0123456789", now)
	nat, err := index.BuildFromList(ctx, srv, index.ListOptions{Options: index.Options{Bucket: "golden"}})
	must(err)
	write("native.lith", nat.Marshal())

	// --keys: object-backed index built from a key list (HasMeta ⇒ no HEAD).
	keys := []index.KeyEntry{
		{Key: "a/1.txt", Size: 5, MTime: now.UnixNano(), HasMeta: true},
		{Key: "a/2.txt", Size: 7, MTime: now.UnixNano(), HasMeta: true},
	}
	kx, _, err := index.BuildFromKeys(ctx, srv, index.KeysOptions{Options: index.Options{Bucket: "golden"}, Keys: keys})
	must(err)
	write("keys.lith", kx.Marshal())

	// CargoShip-backed: virtual files with a frame backing, from the manifest.
	man, err := os.ReadFile(filepath.FromSlash("../../../cargoship/testdata/fixture/manifest.json"))
	must(err)
	img, _, err := lithindex.BuildIndexFromManifest(ctx, headOnlyFetcher{}, man, lithindex.Options{Bucket: "golden"})
	must(err)
	write("cargoship.lith", img)

	fmt.Println("wrote native.lith, keys.lith, cargoship.lith")
}

func write(name string, b []byte) {
	if err := os.WriteFile(name, b, 0o644); err != nil {
		panic(err)
	}
}
func must(err error) {
	if err != nil {
		panic(err)
	}
}
