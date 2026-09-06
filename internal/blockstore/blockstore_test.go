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

// keyFor returns a Key with the correct ETag hash for a stored object.
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
		cfg.BlockSize = 4
	}
	bs, err := New(srv, cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return bs
}

func TestGetRangeCoalescesContiguousMisses(t *testing.T) {
	srv := fake.New()
	srv.PutString("obj", "0123456789", time.Unix(1, 0)) // 10 bytes
	k := keyFor(t, srv, "obj")
	bs := newStore(t, srv, Config{MemCache: 1 << 20, MaxRange: 1024})

	data, err := bs.GetRange(context.Background(), k, 0, 10, 10)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "0123456789" {
		t.Fatalf("data = %q", data)
	}
	// 3 blocks (4+4+2), all contiguous misses, one coalesced GET.
	if srv.GetCalls != 1 {
		t.Errorf("GetCalls = %d, want 1 (coalesced)", srv.GetCalls)
	}

	// Second read is fully cached: no new S3 calls.
	if _, err := bs.GetRange(context.Background(), k, 2, 6, 10); err != nil {
		t.Fatal(err)
	}
	if srv.GetCalls != 1 {
		t.Errorf("GetCalls after warm read = %d, want 1", srv.GetCalls)
	}
}

func TestMaxRangeBoundsCoalescing(t *testing.T) {
	srv := fake.New()
	srv.PutString("obj", "0123456789", time.Unix(1, 0))
	k := keyFor(t, srv, "obj")
	// MaxRange = 2 blocks, so blocks 0,1,2 fetch as runs [0,1] and [2].
	bs := newStore(t, srv, Config{MemCache: 1 << 20, BlockSize: 4, MaxRange: 8})

	if _, err := bs.GetRange(context.Background(), k, 0, 10, 10); err != nil {
		t.Fatal(err)
	}
	if srv.GetCalls != 2 {
		t.Errorf("GetCalls = %d, want 2 (maxRange caps the run at 2 blocks)", srv.GetCalls)
	}
}

func TestDiskTierServesAfterMemoryDisabled(t *testing.T) {
	srv := fake.New()
	srv.PutString("obj", "0123456789", time.Unix(1, 0))
	k := keyFor(t, srv, "obj")
	bs := newStore(t, srv, Config{MemCache: 0, DiskCache: 1 << 20, DiskPath: t.TempDir(), MaxRange: 1024})

	if _, err := bs.Get(context.Background(), k, 0, 10); err != nil {
		t.Fatal(err)
	}
	if srv.GetCalls != 1 {
		t.Fatalf("GetCalls = %d, want 1", srv.GetCalls)
	}
	// Memory is disabled, so this must come from disk, not S3.
	if _, err := bs.Get(context.Background(), k, 0, 10); err != nil {
		t.Fatal(err)
	}
	if srv.GetCalls != 1 {
		t.Errorf("GetCalls after disk hit = %d, want 1", srv.GetCalls)
	}
}

func TestETagMismatchIsStaleNotServed(t *testing.T) {
	srv := fake.New()
	srv.PutString("obj", "0123456789", time.Unix(1, 0))
	// Wrong ETag hash: simulates the object changing since index build.
	k := Key{Key: "obj", ETagHash: 0xdeadbeef}
	bs := newStore(t, srv, Config{MemCache: 1 << 20, MaxRange: 1024})

	_, err := bs.GetRange(context.Background(), k, 0, 4, 10)
	if err != ErrStale {
		t.Fatalf("err = %v, want ErrStale", err)
	}
	stale := bs.StaleKeys()
	if len(stale) != 1 || stale[0] != "obj" {
		t.Errorf("StaleKeys = %v, want [obj]", stale)
	}
	// Nothing stale was cached.
	if _, ok := bs.peekCached(k, 0); ok {
		t.Error("a stale block must not be cached")
	}
}

func TestSingleflightJoinsConcurrentGets(t *testing.T) {
	srv := fake.New()
	srv.PutString("obj", "0123456789", time.Unix(1, 0))
	srv.GetDelay = 30 * time.Millisecond // widen the in-flight window
	k := keyFor(t, srv, "obj")
	bs := newStore(t, srv, Config{MemCache: 1 << 20, MaxRange: 1024})

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := bs.Get(context.Background(), k, 0, 10); err != nil {
				t.Errorf("Get: %v", err)
			}
		}()
	}
	wg.Wait()
	if srv.GetCalls != 1 {
		t.Errorf("GetCalls = %d, want 1 (singleflight joined 16 concurrent gets)", srv.GetCalls)
	}
}
