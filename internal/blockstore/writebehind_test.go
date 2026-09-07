// SPDX-License-Identifier: Apache-2.0

package blockstore

import (
	"context"
	"testing"
	"time"

	"github.com/scttfrdmn/lith/internal/s3client/fake"
)

// TestWriteBehindFillLatency: a fill returns as soon as bytes are in memory;
// the (slow) disk write happens off the fill path.
func TestWriteBehindFillLatency(t *testing.T) {
	srv := fake.New()
	makeObj(srv, "obj", 8)
	k := keyFor(t, srv, "obj")
	size := int64(8) * mib
	bs := newStore(t, srv, Config{
		BlockSize: 1 << 20, MaxRange: 64 << 20, MemCache: 64 << 20,
		DiskCache: 64 << 20, DiskPath: t.TempDir(), DiskWriters: 2,
	})
	bs.disk.putDelay = 200 * time.Millisecond // slow disk

	t0 := time.Now()
	if _, err := bs.GetRange(context.Background(), k, 0, 4096, size); err != nil {
		t.Fatal(err)
	}
	fill := time.Since(t0)
	if fill > 80*time.Millisecond {
		t.Errorf("fill latency %v depends on the disk write (200ms); should be write-behind", fill)
	}

	flushStart := time.Now()
	bs.Flush()
	if time.Since(flushStart) < 100*time.Millisecond {
		t.Errorf("Flush returned in %v; the slow disk write should still have been pending", time.Since(flushStart))
	}
}
