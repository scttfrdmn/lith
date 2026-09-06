// SPDX-License-Identifier: Apache-2.0

package index

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/scttfrdmn/lith/internal/s3client"
)

// TestE2EBuildFromPublicBucket builds an index from a real public S3 prefix and
// asserts a known key resolves. It is skipped unless LITH_E2E_BUCKET is set.
//
// Verified default: LITH_E2E_BUCKET=1000genomes LITH_E2E_PREFIX=changelog_details
// which contains the stable key "changelog_details_20081219".
func TestE2EBuildFromPublicBucket(t *testing.T) {
	bucket := os.Getenv("LITH_E2E_BUCKET")
	if bucket == "" {
		t.Skip("set LITH_E2E_BUCKET to run the end-to-end test")
	}
	prefix := os.Getenv("LITH_E2E_PREFIX")
	if prefix == "" {
		prefix = "changelog_details"
	}
	wantKey := os.Getenv("LITH_E2E_KEY")
	if wantKey == "" {
		wantKey = "/changelog_details_20081219"
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	client, err := s3client.New(ctx, s3client.Config{
		Bucket:        bucket,
		NoSignRequest: true,
	})
	if err != nil {
		t.Fatalf("client: %v", err)
	}

	ix, err := BuildFromList(ctx, client, ListOptions{
		Options: Options{Bucket: bucket, Prefix: prefix},
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if ix.Len() == 0 {
		t.Fatal("index is empty; check the bucket/prefix")
	}
	fi, err := ix.Stat(wantKey)
	if err != nil {
		t.Fatalf("expected %q to resolve: %v", wantKey, err)
	}
	if fi.IsDir || fi.Size <= 0 {
		t.Errorf("%q resolved to unexpected info: %+v", wantKey, fi)
	}
	t.Logf("built %d keys from s3://%s/%s; %q size=%d", ix.Len(), bucket, prefix, wantKey, fi.Size)
}
