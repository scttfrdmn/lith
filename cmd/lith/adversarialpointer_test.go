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

	"github.com/scttfrdmn/lith/internal/index"
	"github.com/scttfrdmn/lith/internal/pointer"
	"github.com/scttfrdmn/lith/internal/s3client/fake"
)

// M17-B cases 2 and 4 (#217): a CURRENT naming an index built for a DIFFERENT BUCKET OR
// PREFIX, and an index whose root does not match the prefix it is mounted at.
//
// Both are the same question — does lith compare the index's recorded provenance against the
// thing it is being mounted as? The index carries both (`Bucket()`, `Prefix()`), so the
// check is available; the only question is whether it is made.

// publishDataset writes a published dataset: an index object built for (bucket, prefix), and
// a CURRENT naming it with the correct sha256. Everything is internally consistent; only the
// index's own provenance is a parameter.
func publishDataset(t *testing.T, srv *fake.Server, dataset, idxBucket, idxPrefix string, keys ...string) {
	t.Helper()
	entries := make([]index.Entry, 0, len(keys))
	for _, k := range keys {
		entries = append(entries, index.Entry{Key: k, Size: 1024, MTime: time.Now().UnixNano()})
	}
	ix := index.Build(entries, index.Options{Bucket: idxBucket, Prefix: idxPrefix})
	img := ix.Marshal()
	indexKey := dataset + "/v/1/index.lith"
	srv.Put(indexKey, img, time.Unix(1_700_000_000, 0))

	sum := sha256.Sum256(img)
	cur, err := json.Marshal(pointer.Current{
		Schema: pointer.Schema, VersionID: "1",
		ManifestKey: dataset + "/v/1/manifest.json", IndexKey: indexKey,
		IndexSHA256: hex.EncodeToString(sum[:]),
		FileCount:   int64(len(keys)), TotalBytes: int64(1024 * len(keys)),
	})
	if err != nil {
		t.Fatal(err)
	}
	srv.Put(dataset+"/CURRENT", cur, time.Unix(1_700_000_000, 0))
}

// CASE 2: a CURRENT naming an index built for a different BUCKET.
//
// The index records the bucket it was built from, and that record is what makes this
// checkable: an index built against bucket "elsewhere" says so. Mounted against "bkt", every
// key it names resolves against "bkt" instead — so the mount serves whatever happens to live
// at those keys in the bucket it was NOT built from, under the dataset's name, with
// correct-looking metadata. Sizes and mtimes come from the index, so nothing looks wrong
// until a checksum fails.
//
// The pointer's own sha256 does not help: it binds the index's BYTES, not its meaning.
func TestPointerRejectsAnIndexBuiltForADifferentBucket(t *testing.T) {
	srv := fake.New()
	publishDataset(t, srv, "ds", "elsewhere", "", "a.txt", "b.txt")

	_, _, err := resolvePointer(context.Background(), srv, "bkt", "ds", "current")
	if err == nil {
		t.Fatal("verdict SERVES WRONG: a CURRENT naming an index built for bucket " +
			"\"elsewhere\" resolved clean against bucket \"bkt\"; every read would go to " +
			"the wrong bucket's keys under this dataset's name")
	}
	if !strings.Contains(err.Error(), "elsewhere") || !strings.Contains(err.Error(), "bkt") {
		t.Errorf("the error must name both buckets so the mismatch is actionable: %v", err)
	}

	// The matching case must resolve, or the check is just a refusal to mount.
	srv2 := fake.New()
	publishDataset(t, srv2, "ds", "bkt", "", "a.txt", "b.txt")
	if _, _, err := resolvePointer(context.Background(), srv2, "bkt", "ds", "current"); err != nil {
		t.Fatalf("a correctly published dataset was rejected: %v", err)
	}

	// An index with NO recorded bucket must still mount: `index.Build` leaves it empty
	// unless the builder sets it, and refusing those would break every index built before
	// the field was populated. Absence of provenance is not a mismatch.
	srv3 := fake.New()
	publishDataset(t, srv3, "ds", "", "", "a.txt")
	if _, _, err := resolvePointer(context.Background(), srv3, "bkt", "ds", "current"); err != nil {
		t.Errorf("an index with no recorded bucket was rejected: %v", err)
	}
}

// CASE 2, the prefix half, and CASE 4: an index whose root is not the root it is mounted at.
//
// A published dataset is mounted at the archive's own tree (root ""), so an index built at a
// non-empty prefix is already refused by Root("") via ErrRootOutside. This pins that, because
// it is the reason the prefix half needs no new check — and because it is an accident of
// `rootPrefix = ""` rather than an explicit decision, so it is worth having a test that
// fails if that line ever changes.
func TestPointerRejectsAnIndexBuiltForADifferentPrefix(t *testing.T) {
	srv := fake.New()
	publishDataset(t, srv, "ds", "bkt", "other/", "other/a.txt", "other/b.txt")

	ix, _, err := resolvePointer(context.Background(), srv, "bkt", "ds", "current")
	if err != nil {
		// Detected at resolve: the better outcome, and what the bucket check above does.
		if !strings.Contains(err.Error(), "other") {
			t.Errorf("rejected, but the error does not name the index's root: %v", err)
		}
		return
	}

	// Otherwise the mount-root check must catch it. A published dataset mounts at root "",
	// and an index built at "other/" cannot serve that root.
	if _, rerr := ix.Root(""); rerr == nil {
		t.Error("verdict SERVES WRONG: an index built at prefix \"other/\" both resolved " +
			"and rooted at \"\", so the dataset would serve keys under a prefix it was " +
			"not published at")
	} else if !strings.Contains(rerr.Error(), "root") {
		t.Errorf("rooting failed, but not with a root mismatch: %v", rerr)
	}
}

// CASE 4 proper, for a PLAIN (non-pointer) mount: `--index-file` plus `--prefix`.
//
// This is the sub-root feature working as designed (#90) — one whole-bucket index backing
// many prefix mounts — so the test's job is to pin the boundary between a legal sub-root and
// an incompatible one, both directions.
func TestIndexRootAgainstMountPrefix(t *testing.T) {
	// Index keys are stored RELATIVE to the build prefix, which is what Root compares
	// against: it trims the index's root off the mount root and searches for the remainder.
	mk := func(idxPrefix string, keys ...string) *index.Index {
		entries := make([]index.Entry, 0, len(keys))
		for _, k := range keys {
			entries = append(entries, index.Entry{Key: k, Size: 10, MTime: 1})
		}
		return index.Build(entries, index.Options{Bucket: "bkt", Prefix: idxPrefix})
	}

	// Legal: a whole-bucket index rooted at a sub-prefix it contains.
	if _, err := mk("", "data/a", "data/b", "logs/c").Root("data/"); err != nil {
		t.Errorf("a whole-bucket index rooted at a contained prefix was rejected: %v", err)
	}
	// Legal: an index built at "data/" rooted deeper.
	if _, err := mk("data/", "sub/a", "sub/b").Root("data/sub/"); err != nil {
		t.Errorf("an index rooted deeper within its own root was rejected: %v", err)
	}
	// Legal: the pass-through, root == the index's own root.
	if _, err := mk("data/", "a").Root("data/"); err != nil {
		t.Errorf("the pass-through root was rejected: %v", err)
	}

	// DETECTED: the mount root is outside the index's root entirely. Serving this would
	// mean resolving "logs/..." keys from an index that only knows "data/...".
	if _, err := mk("data/", "a").Root("logs/"); err == nil {
		t.Error("verdict SERVES WRONG: an index built at \"data/\" rooted at \"logs/\"")
	} else if !strings.Contains(err.Error(), "root") {
		t.Errorf("rejected, but not as a root mismatch: %v", err)
	}
	// DETECTED: inside the index's root, but empty. An empty namespace mounts as a
	// directory that exists and has nothing in it, which is a wrong answer rather than an
	// error — the same class as resolvePointer's chunkless-manifest guard.
	if _, err := mk("data/", "a").Root("data/nothing/"); err == nil {
		t.Error("verdict SERVES WRONG: a mount root with no objects under it was accepted, " +
			"and would present an empty directory as the dataset")
	}
	// DETECTED: a root that is a string prefix of a KEY but not a directory. "data/ab"
	// shares a prefix with "data/abc" without being its parent, so a naive lowerBound
	// range would find entries and mount a tree that does not exist.
	if v, err := mk("", "data/abc").Root("data/ab/"); err == nil {
		t.Errorf("verdict SERVES WRONG: \"data/ab/\" matched the key \"data/abc\" and "+
			"mounted %d entries", v.Len())
	}
}
