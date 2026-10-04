// SPDX-License-Identifier: Apache-2.0

package index

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/scttfrdmn/lith/internal/s3client/fake"
)

// M17-B case 5 (#217): a manifest with a chunk key that does not exist.
//
// Two distinct situations, with different verdicts, and the difference is worth recording:
//
//   - Building an index FROM a manifest (`--cargoship`) HEADs every chunk for its ETag, so a
//     missing chunk is DETECTED at mount, before anything is served.
//   - A PUBLISHED index (@current) is a prebuilt artifact, so no HEAD happens and the
//     dangling key surfaces on first read. That is fail-closed, not wrong — see
//     fuse.TestCargoMissingChunkFailsClosed for the bytes.
//
// A manifest file referencing a chunk that is not IN the manifest is a third case, rejected
// by Resolve ("references unknown chunk"), and pinned here too since all three look alike
// from outside.
func TestBuildFromManifestDetectsAMissingChunkObject(t *testing.T) {
	srv := fake.New()
	// A manifest naming two chunks; only the first exists in the bucket.
	srv.Put("p/uploads/u1/chunk-0.tar.zst", []byte("not really zstd, never read"), time.Unix(1_700_000_000, 0))

	man := `{"version":"2.1","upload_id":"u1","prefix":"p","source_path":"/src",
	  "files":[
	    {"path":"/src/a","size":100,"chunk_id":0,"s3_key":"uploads/u1/chunk-0.tar.zst","archive_offset":0},
	    {"path":"/src/b","size":100,"chunk_id":1,"s3_key":"uploads/u1/chunk-1.tar.zst","archive_offset":0}
	  ],
	  "chunks":[
	    {"id":0,"s3_key":"uploads/u1/chunk-0.tar.zst","uncompressed_size":4096,"compressed_size":1000,
	     "frames":[{"compressed_offset":0,"compressed_size":1000,"uncompressed_offset":0,"uncompressed_size":4096}]},
	    {"id":1,"s3_key":"uploads/u1/chunk-1.tar.zst","uncompressed_size":4096,"compressed_size":1000,
	     "frames":[{"compressed_offset":0,"compressed_size":1000,"uncompressed_offset":0,"uncompressed_size":4096}]}
	  ]}`

	_, err := BuildFromCargoshipManifest(context.Background(), srv, CargoshipOptions{
		Options:       Options{Bucket: "bkt", Prefix: ""},
		ManifestBytes: []byte(man),
	})
	if err == nil {
		t.Fatal("verdict SERVES WRONG: a manifest naming a chunk object that does not " +
			"exist built an index; every file in that chunk would EIO at read time with " +
			"nothing at mount to say why")
	}
	// The error must name the missing KEY. "HEAD failed" without it leaves an operator
	// with a dangling archive and no way to tell which chunk went missing.
	if !strings.Contains(err.Error(), "chunk-1.tar.zst") {
		t.Errorf("the error does not name the missing chunk key: %v", err)
	}

	// And with both chunks present it must build, or the check above is just a refusal.
	srv.Put("p/uploads/u1/chunk-1.tar.zst", []byte("also never read"), time.Unix(1_700_000_000, 0))
	ix, err := BuildFromCargoshipManifest(context.Background(), srv, CargoshipOptions{
		Options:       Options{Bucket: "bkt", Prefix: ""},
		ManifestBytes: []byte(man),
	})
	if err != nil {
		t.Fatalf("a manifest whose chunks all exist was rejected: %v", err)
	}
	if ix.Len() != 2 {
		t.Errorf("index has %d entries, want 2", ix.Len())
	}
}

// A manifest FILE referencing a chunk id/key that the manifest itself does not declare:
// detected by Resolve, before any S3 call. Pinned because it is indistinguishable from the
// case above in an operator's log unless the errors differ.
func TestManifestFileReferencingAnUndeclaredChunkIsDetectedWithoutS3(t *testing.T) {
	srv := fake.New()
	man := `{"version":"2.1","upload_id":"u1","prefix":"p","source_path":"/src",
	  "files":[{"path":"/src/a","size":100,"chunk_id":7,"s3_key":"uploads/u1/ghost.tar.zst","archive_offset":0}],
	  "chunks":[{"id":0,"s3_key":"uploads/u1/chunk-0.tar.zst","uncompressed_size":4096,"compressed_size":1000,
	     "frames":[{"compressed_offset":0,"compressed_size":1000,"uncompressed_offset":0,"uncompressed_size":4096}]}]}`

	_, err := BuildFromCargoshipManifest(context.Background(), srv, CargoshipOptions{
		Options:       Options{Bucket: "bkt", Prefix: ""},
		ManifestBytes: []byte(man),
	})
	if err == nil {
		t.Fatal("verdict SERVES WRONG: a file naming a chunk the manifest does not declare " +
			"resolved clean")
	}
	if !strings.Contains(err.Error(), "unknown chunk") {
		t.Errorf("rejected, but not as an unknown chunk reference: %v", err)
	}
	// No S3 call should have been needed: this is a self-consistency failure of the
	// manifest, and detecting it before any network I/O is what makes a dangling archive
	// cheap to diagnose.
	if n := srv.GetCallCount(); n != 0 {
		t.Errorf("the manifest's own inconsistency cost %d S3 GETs to detect", n)
	}
}
