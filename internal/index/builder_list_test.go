// SPDX-License-Identifier: Apache-2.0

package index

import (
	"context"
	"testing"
	"time"

	"github.com/scttfrdmn/lith/internal/s3client/fake"
)

func TestBuildFromListStripsPrefix(t *testing.T) {
	srv := fake.New()
	now := time.Unix(1_700_000_000, 0)
	srv.PutString("data/a", "AAA", now)
	srv.PutString("data/b/c", "CC", now)
	srv.PutString("data/b/d", "D", now)
	srv.PutString("other/x", "ignored", now) // outside the prefix

	ix, err := BuildFromList(context.Background(), srv, ListOptions{
		Options:  Options{Bucket: "bkt", Prefix: "data"},
		PageSize: 2, // force pagination
	})
	if err != nil {
		t.Fatalf("BuildFromList: %v", err)
	}
	if ix.Len() != 3 {
		t.Fatalf("Len = %d, want 3", ix.Len())
	}
	if fi, err := ix.Stat("/a"); err != nil || fi.Size != 3 {
		t.Errorf("stat /a: fi=%+v err=%v", fi, err)
	}
	got := names(readAll(t, ix, "/b"))
	if !equalStrings(got, []string{"c", "d"}) {
		t.Errorf("readdir /b = %v, want [c d]", got)
	}

	// Building consumed list calls; querying the built index consumes none.
	before := srv.ListCalls
	if before == 0 {
		t.Fatal("expected ListObjectsV2 calls during build")
	}
	_, _, _ = ix.Readdir("/", 0, 100)
	_, _ = ix.Stat("/a")
	if srv.ListCalls != before {
		t.Errorf("querying the index made %d extra S3 list calls", srv.ListCalls-before)
	}
}

func TestBuildFromListSharded(t *testing.T) {
	srv := fake.New()
	now := time.Unix(1_700_000_000, 0)
	for _, k := range []string{"p/a/1", "p/a/2", "p/b/1", "p/c/1"} {
		srv.PutString(k, "x", now)
	}
	plain, err := BuildFromList(context.Background(), srv, ListOptions{
		Options: Options{Bucket: "b", Prefix: "p"},
	})
	if err != nil {
		t.Fatal(err)
	}
	sharded, err := BuildFromList(context.Background(), srv, ListOptions{
		Options: Options{Bucket: "b", Prefix: "p"},
		Shards:  []string{"a/", "b/", "c/"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if plain.Len() != sharded.Len() || plain.Len() != 4 {
		t.Fatalf("plain=%d sharded=%d, want 4", plain.Len(), sharded.Len())
	}
	// The two builds are byte-identical.
	pb, sb := plain.Marshal(), sharded.Marshal()
	if len(pb) != len(sb) {
		t.Fatalf("sharded build differs in size: %d vs %d", len(pb), len(sb))
	}
	for i := range pb {
		if pb[i] != sb[i] {
			t.Fatalf("sharded build differs at byte %d", i)
		}
	}
}
