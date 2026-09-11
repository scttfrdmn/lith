// SPDX-License-Identifier: Apache-2.0

package fuse

import (
	"testing"
	"time"

	"github.com/hanwen/go-fuse/v2/fuse"
	"github.com/scttfrdmn/lith/internal/metrics"
	"github.com/scttfrdmn/lith/internal/s3client/fake"
)

// TestReadRecordsReadMetrics: a Read through the mount observes its size in the
// read-size histogram and marks the touched object bytes as distinct (#65).
func TestReadRecordsReadMetrics(t *testing.T) {
	srv := fake.New()
	srv.Put("obj", make([]byte, 8<<20), time.Unix(1_700_000_000, 0))
	met := metrics.New()
	raw := mkFS(t, srv, Config{SmallFile: 1 << 20, Metrics: met})

	_, fh := openHandle(t, raw, "obj")
	buf := make([]byte, 4096)
	raw.Read(nil, &fuse.ReadIn{InHeader: fuse.InHeader{NodeId: 0}, Fh: fh, Offset: 0, Size: 4096}, buf)
	raw.Read(nil, &fuse.ReadIn{InHeader: fuse.InHeader{NodeId: 0}, Fh: fh, Offset: 5 << 20, Size: 4096}, buf)

	if n, sum := met.ReadSizeStats(); n != 2 || sum != 8192 {
		t.Fatalf("read-size stats = %d/%d, want 2/8192", n, sum)
	}
	// Two reads in two different 64 KiB extents -> 128 KiB distinct (#118).
	if got := met.DistinctBytesRead(); got != 2*64<<10 {
		t.Fatalf("distinct bytes = %d, want %d", got, 2*64<<10)
	}
}
