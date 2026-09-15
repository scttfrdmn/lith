// SPDX-License-Identifier: Apache-2.0

//go:build fuse_integration

// Package fuse integration test for `lith refresh` (#193). This test mounts a
// real FUSE filesystem, so it requires /dev/fuse and fusermount3 and is gated
// behind the `fuse_integration` build tag — normal `go test` (and CI without
// FUSE) skips it. Run on the release box:
//
//	go test -tags fuse_integration -run TestRefresh ./internal/fuse/
//
// The rule it enforces (design #1): a FUSE behavior that depends on kernel
// caching must be tested THROUGH A REAL MOUNT. The swap is a userspace
// atomic.Pointer store; whether a reopen after refresh sees the new version is
// entirely a question of the kernel's entry/attr/page caches, which a userspace
// unit test cannot observe.
package fuse

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/scttfrdmn/lith/internal/blockstore"
	"github.com/scttfrdmn/lith/internal/index"
	"github.com/scttfrdmn/lith/internal/metrics"
	"github.com/scttfrdmn/lith/internal/s3client/fake"
)

// buildPrefixIndex builds an index rooted at prefix over the fake server, so the
// mounted path "x" backs onto "<prefix>x" — mirroring the immutable versioned
// prefixes a published dataset swaps between (distinct backing keys, so the
// block cache never collides across versions, exactly as in production).
func buildPrefixIndex(t *testing.T, srv *fake.Server, prefix string) index.Reader {
	t.Helper()
	ix, err := index.BuildFromList(context.Background(), srv, index.ListOptions{
		Options: index.Options{Bucket: "bkt", Prefix: prefix},
	})
	if err != nil {
		t.Fatalf("build index (%s): %v", prefix, err)
	}
	return ix
}

// TestRefreshInvalidatesKernelCache is the P0 (#193) test. v1 has x = 4 KiB of
// 'A'; after publishing v2 with x = 8 KiB of 'B' and swapping, a reopen through
// the same mount must see v2's size and bytes. Without kernel invalidation on
// swap, the reopen is served from the kernel's cached v1 attrs and pages.
func TestRefreshInvalidatesKernelCache(t *testing.T) {
	srv := fake.New()
	now := time.Unix(1_700_000_000, 0)
	v1 := strings.Repeat("A", 4096)
	v2 := strings.Repeat("B", 8192)
	srv.PutString("v1/x", v1, now)
	srv.PutString("v2/x", v2, now)

	ix1 := buildPrefixIndex(t, srv, "v1/")
	ix2 := buildPrefixIndex(t, srv, "v2/")

	bs, err := blockstore.New(srv, blockstore.Config{
		Bucket: "bkt", BlockSize: 1 << 20, MemCache: 1 << 24, MaxRange: 1 << 20,
	})
	if err != nil {
		t.Fatalf("blockstore: %v", err)
	}

	mnt := t.TempDir()
	server, swapper, err := Mount(mnt, Config{
		Index: ix1, Store: bs, Metrics: metrics.New(),
		UID: uint32(os.Getuid()), GID: uint32(os.Getgid()),
	}, MountOptions{FsName: "test"})
	if err != nil {
		t.Fatalf("mount: %v", err)
	}
	defer func() {
		if err := server.Unmount(); err != nil {
			t.Errorf("unmount: %v", err)
		}
	}()

	// Read v1 through the mount and close, so the kernel caches its attrs and
	// pages (one-year timeouts + FOPEN_KEEP_CACHE).
	got1, err := os.ReadFile(mnt + "/x")
	if err != nil {
		t.Fatalf("read v1: %v", err)
	}
	if len(got1) != 4096 || !bytes.Equal(got1, []byte(v1)) {
		t.Fatalf("v1 read: got %d bytes (want 4096 of 'A')", len(got1))
	}

	// Publish v2: swap the index, exactly as the SIGHUP refresh handler does.
	swapper.SwapIndex(ix2)

	// Reopen. stat must report the new size; a full read must be the new bytes.
	fi, err := os.Stat(mnt + "/x")
	if err != nil {
		t.Fatalf("stat after refresh: %v", err)
	}
	if fi.Size() != 8192 {
		t.Errorf("stat size after refresh = %d, want 8192 (stale attr cache)", fi.Size())
	}
	got2, err := os.ReadFile(mnt + "/x")
	if err != nil {
		t.Fatalf("read v2: %v", err)
	}
	if len(got2) != 8192 || !bytes.Equal(got2, []byte(v2)) {
		t.Errorf("v2 read: got %d bytes, first byte %q, want 8192 of 'B' (stale page cache)",
			len(got2), firstByte(got2))
	}
}

func firstByte(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	return string(b[:1])
}
