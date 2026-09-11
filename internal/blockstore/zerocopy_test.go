// SPDX-License-Identifier: Apache-2.0

package blockstore

import (
	"context"
	"runtime"
	"testing"

	"github.com/scttfrdmn/lith/internal/s3client/fake"
)

// TestChunkZeroCopy: Chunk returns a sub-slice of the cached buffer (no copy),
// and a cache hit allocates nothing chunk-sized.
func TestChunkZeroCopy(t *testing.T) {
	srv := fake.New()
	makeObj(srv, "obj", 16)
	k := keyFor(t, srv, "obj")
	size := int64(16) * mib
	bs := newStore(t, srv, Config{BlockSize: 8 << 20, MaxRange: 64 << 20, MemCache: 64 << 20})

	ctx := context.Background()
	a, err := bs.Chunk(ctx, k, 5, size, 0, ChunkSize, true) // fill + cache
	if err != nil {
		t.Fatal(err)
	}
	if a[0] != 5 || int64(len(a)) != mib {
		t.Fatalf("chunk 5: byte=%d len=%d", a[0], len(a))
	}
	b, _ := bs.Chunk(ctx, k, 5, size, 0, ChunkSize, true) // cache hit
	if &a[0] != &b[0] {
		t.Error("Chunk copied the data; the returned slice should alias the cached buffer")
	}

	// A cache hit must not allocate a chunk-sized buffer (no memmove-class copy).
	runtime.GC()
	var m0, m1 runtime.MemStats
	runtime.ReadMemStats(&m0)
	const n = 2000
	for i := 0; i < n; i++ {
		_, _ = bs.Chunk(ctx, k, 5, size, 0, ChunkSize, true)
	}
	runtime.ReadMemStats(&m1)
	perCall := (m1.TotalAlloc - m0.TotalAlloc) / n
	if perCall > 4096 {
		t.Errorf("Chunk allocates %d bytes/call; expected no chunk-sized copy (<4 KiB)", perCall)
	}
}

// TestChunkSurvivesEviction: a slice returned by Chunk stays valid (correct
// bytes) after the chunk is evicted from the memory tier — the reply can safely
// reference it.
func TestChunkSurvivesEviction(t *testing.T) {
	srv := fake.New()
	makeObj(srv, "obj", 64)
	k := keyFor(t, srv, "obj")
	size := int64(64) * mib
	// Tiny mem so subsequent chunks evict chunk 3.
	bs := newStore(t, srv, Config{BlockSize: 1 << 20, MaxRange: 64 << 20, MemCache: 4 << 20})

	ctx := context.Background()
	held, err := bs.Chunk(ctx, k, 3, size, 0, ChunkSize, true)
	if err != nil {
		t.Fatal(err)
	}
	for ci := int64(0); ci < 40; ci++ { // evict chunk 3 from the 4 MiB mem tier
		_, _ = bs.Chunk(ctx, k, ci, size, 0, ChunkSize, true)
	}
	runtime.GC()
	for i, bv := range held {
		if bv != 3 {
			t.Fatalf("held chunk-3 slice corrupted at %d: %d", i, bv)
		}
	}
}

// TestStraddleReadCorrect: a read spanning a chunk boundary assembles correctly.
func TestStraddleReadCorrect(t *testing.T) {
	srv := fake.New()
	makeObj(srv, "obj", 4)
	k := keyFor(t, srv, "obj")
	size := int64(4) * mib
	bs := newStore(t, srv, Config{BlockSize: 8 << 20, MaxRange: 64 << 20, MemCache: 64 << 20})

	// 100 bytes at the end of chunk 0 + 100 at the start of chunk 1.
	off := mib - 100
	data, err := bs.GetRange(context.Background(), k, off, 200, size)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) != 200 {
		t.Fatalf("len=%d", len(data))
	}
	for i := 0; i < 100; i++ {
		if data[i] != 0 {
			t.Fatalf("byte %d (chunk 0) = %d, want 0", i, data[i])
		}
	}
	for i := 100; i < 200; i++ {
		if data[i] != 1 {
			t.Fatalf("byte %d (chunk 1) = %d, want 1", i, data[i])
		}
	}
}
