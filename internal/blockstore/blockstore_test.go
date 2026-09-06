// SPDX-License-Identifier: Apache-2.0

package blockstore

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/scttfrdmn/lith/internal/s3client/fake"
	"github.com/zeebo/xxh3"
)

const mib = int64(1 << 20)

// makeObj stores an object of nChunks 1 MiB chunks; every byte in chunk ci is
// byte(ci), so a read can verify which chunk it landed in.
func makeObj(srv *fake.Server, key string, nChunks int) {
	data := make([]byte, int64(nChunks)*mib)
	for ci := 0; ci < nChunks; ci++ {
		lo := int64(ci) * mib
		for j := lo; j < lo+mib; j++ {
			data[j] = byte(ci)
		}
	}
	srv.Put(key, data, time.Unix(1, 0))
}

func keyFor(t *testing.T, srv *fake.Server, key string) Key {
	t.Helper()
	o, err := srv.HeadObject(context.Background(), key)
	if err != nil {
		t.Fatalf("head %q: %v", key, err)
	}
	return Key{Key: key, ETagHash: xxh3.HashString(o.ETag)}
}

func newStore(t *testing.T, srv *fake.Server, cfg Config) *BlockStore {
	t.Helper()
	cfg.Bucket = "bkt"
	if cfg.BlockSize == 0 {
		cfg.BlockSize = 8 << 20
	}
	if cfg.MemCache == 0 {
		cfg.MemCache = 256 << 20
	}
	bs, err := New(srv, cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return bs
}

// TestInFlightJoinOneGET: prefetching chunks 0–31 as one run, then demand reads
// of chunks 3, 7, 12 issues exactly one GET total (#35).
func TestInFlightJoinOneGET(t *testing.T) {
	srv := fake.New()
	makeObj(srv, "obj", 40)
	k := keyFor(t, srv, "obj")
	size := int64(40) * mib
	bs := newStore(t, srv, Config{BlockSize: 32 << 20, MaxRange: 32 << 20})

	bs.Prefetch(context.Background(), k, 0, size) // fills chunks 0..31 in one GET
	if srv.GetCalls != 1 {
		t.Fatalf("after prefetch GetCalls=%d, want 1", srv.GetCalls)
	}
	for _, ci := range []int64{3, 7, 12} {
		data, err := bs.GetRange(context.Background(), k, ci*mib, 4096, size)
		if err != nil {
			t.Fatal(err)
		}
		if data[0] != byte(ci) {
			t.Errorf("chunk %d: got byte %d", ci, data[0])
		}
	}
	if srv.GetCalls != 1 {
		t.Errorf("GetCalls=%d, want 1 (demand joined the prefetched run, no new GETs)", srv.GetCalls)
	}
}

// TestMidFillUnblock: a demand read for chunk 20 unblocks when chunk 20 lands,
// not when the last chunk (31) does (#35).
func TestMidFillUnblock(t *testing.T) {
	srv := fake.New()
	makeObj(srv, "obj", 32)
	srv.StreamChunkDelay = 12 * time.Millisecond
	srv.StreamChunkBytes = mib
	k := keyFor(t, srv, "obj")
	size := int64(32) * mib
	bs := newStore(t, srv, Config{BlockSize: 32 << 20, MaxRange: 32 << 20})

	go bs.Prefetch(context.Background(), k, 0, size) // slow fill of chunks 0..31
	time.Sleep(40 * time.Millisecond)                // let the prefetch claim all chunks

	start := time.Now()
	data, err := bs.GetRange(context.Background(), k, 20*mib, 4096, size)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	if data[0] != 20 {
		t.Errorf("chunk 20 data = %d", data[0])
	}
	// Chunk 20 lands ~20*12ms=240ms; the full run finishes ~31*12ms=372ms. The
	// demand must return well before the run completes.
	if elapsed > 26*12*time.Millisecond {
		t.Errorf("demand for chunk 20 took %v; should unblock before the run finishes (~%v)", elapsed, 31*12*time.Millisecond)
	}
	if srv.GetCalls != 1 {
		t.Errorf("GetCalls=%d, want 1 (demand joined the in-flight fill)", srv.GetCalls)
	}
}

// TestRandomReadFetchesOneChunk: a small demand read fetches ≤ 1 MiB (#37).
func TestRandomReadFetchesOneChunk(t *testing.T) {
	srv := fake.New()
	makeObj(srv, "obj", 40)
	k := keyFor(t, srv, "obj")
	size := int64(40) * mib
	bs := newStore(t, srv, Config{BlockSize: 8 << 20, MaxRange: 64 << 20})

	if _, err := bs.GetRange(context.Background(), k, 12*mib+123, 4096, size); err != nil {
		t.Fatal(err)
	}
	if srv.GetBytes != mib {
		t.Errorf("random 4KiB read fetched %d bytes, want %d (one chunk)", srv.GetBytes, mib)
	}
	if srv.GetCalls != 1 {
		t.Errorf("GetCalls=%d, want 1", srv.GetCalls)
	}
}

// TestSeqBlockFill: a prefetch fills a full block in one ≥ 8 MiB GET (#37).
func TestSeqBlockFill(t *testing.T) {
	srv := fake.New()
	makeObj(srv, "obj", 40)
	k := keyFor(t, srv, "obj")
	size := int64(40) * mib
	bs := newStore(t, srv, Config{BlockSize: 8 << 20, MaxRange: 64 << 20})

	bs.Prefetch(context.Background(), k, 0, size)
	if srv.GetBytes != 8*mib || srv.GetCalls != 1 {
		t.Errorf("block prefetch fetched %d bytes in %d GETs, want %d in 1", srv.GetBytes, srv.GetCalls, 8*mib)
	}
}

// TestDemandCoalesces: a large demand read coalesces its chunks into one GET.
func TestDemandCoalesces(t *testing.T) {
	srv := fake.New()
	makeObj(srv, "obj", 40)
	k := keyFor(t, srv, "obj")
	size := int64(40) * mib
	bs := newStore(t, srv, Config{BlockSize: 8 << 20, MaxRange: 64 << 20})

	data, err := bs.GetRange(context.Background(), k, 0, 8*mib, size)
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(data)) != 8*mib {
		t.Fatalf("read %d bytes, want %d", len(data), 8*mib)
	}
	if srv.GetCalls != 1 {
		t.Errorf("GetCalls=%d, want 1 (coalesced)", srv.GetCalls)
	}
}

func TestETagMismatchStale(t *testing.T) {
	srv := fake.New()
	makeObj(srv, "obj", 4)
	k := Key{Key: "obj", ETagHash: 0xdeadbeef} // wrong
	size := int64(4) * mib
	bs := newStore(t, srv, Config{BlockSize: 8 << 20, MaxRange: 64 << 20})

	if _, err := bs.GetRange(context.Background(), k, 0, 4096, size); err != ErrStale {
		t.Fatalf("err = %v, want ErrStale", err)
	}
	if s := bs.StaleKeys(); len(s) != 1 || s[0] != "obj" {
		t.Errorf("StaleKeys = %v, want [obj]", s)
	}
	if _, tier := bs.lookup(k, 0); tier != "" {
		t.Error("a stale chunk must not be cached")
	}
}

func TestDiskTierServesWithMemoryDisabled(t *testing.T) {
	srv := fake.New()
	makeObj(srv, "obj", 4)
	k := keyFor(t, srv, "obj")
	size := int64(4) * mib
	bs := newStore(t, srv, Config{BlockSize: 8 << 20, MaxRange: 64 << 20, MemCache: 1, DiskCache: 64 << 20, DiskPath: t.TempDir()})

	if _, err := bs.GetRange(context.Background(), k, 0, 4096, size); err != nil {
		t.Fatal(err)
	}
	first := srv.GetCalls
	if _, err := bs.GetRange(context.Background(), k, 0, 4096, size); err != nil {
		t.Fatal(err)
	}
	if srv.GetCalls != first {
		t.Errorf("second read hit S3 (%d GETs) instead of the disk tier", srv.GetCalls-first)
	}
}

func TestSingleflightJoinsConcurrentReads(t *testing.T) {
	srv := fake.New()
	makeObj(srv, "obj", 4)
	srv.GetDelay = 40 * time.Millisecond // widen the in-flight window
	k := keyFor(t, srv, "obj")
	size := int64(4) * mib
	bs := newStore(t, srv, Config{BlockSize: 8 << 20, MaxRange: 64 << 20})

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := bs.GetRange(context.Background(), k, mib, 4096, size); err != nil {
				t.Errorf("GetRange: %v", err)
			}
		}()
	}
	wg.Wait()
	if srv.GetCalls != 1 {
		t.Errorf("GetCalls=%d, want 1 (16 concurrent reads of one chunk joined)", srv.GetCalls)
	}
}
