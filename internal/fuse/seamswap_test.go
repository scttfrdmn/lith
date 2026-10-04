// SPDX-License-Identifier: Apache-2.0

package fuse

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/hanwen/go-fuse/v2/fuse"
	"github.com/scttfrdmn/lith/internal/blockstore"
	"github.com/scttfrdmn/lith/internal/index"
	"github.com/scttfrdmn/lith/internal/metrics"
	"github.com/scttfrdmn/lith/internal/s3client/fake"
)

// M17-D (#219): Open must build a handle from ONE index version, not a mix of two.
//
// #219 asks for "concurrent opens and closes during a refresh" and names "a stale read after
// SwapIndex completes" as a finding. This is the sharper version of that: not stale, but
// INCOHERENT.
//
// rawFS.Open loads the index THREE separate times — Stat for the size, ETagHashOf for the
// cache key, and BackingOf for the cargo parts. The index is an atomic pointer, so each load
// is individually safe, but a SwapIndex landing between two of them gives the handle a size
// from one version and a cache key from another. The handle then reads bytes bounded by one
// version's length under the other version's ETag, which is a wrong-bytes failure that
// announces itself as nothing at all.
//
// `liveIndex` is a struct behind one atomic pointer precisely so a single load yields a
// coherent view. Open takes three.
func TestOpenDoesNotTearAcrossAnIndexSwap(t *testing.T) {
	// Two versions of one path, differing in BOTH size and ETag, so a torn handle is
	// detectable as a mismatched pair rather than needing byte comparison.
	ix1 := buildVersionIndex(t, map[string]string{"shared.txt": "aaa"})
	ix2 := buildVersionIndex(t, map[string]string{"shared.txt": "bbbbbbbbbbbb"})

	want := map[int64]uint64{}
	for _, ix := range []*index.Index{ix1, ix2} {
		fi, err := ix.Stat("/shared.txt")
		if err != nil {
			t.Fatalf("stat: %v", err)
		}
		want[fi.Size] = ix.ETagHashOf("/shared.txt")
	}
	if len(want) != 2 {
		t.Fatalf("fixture: the two versions must differ in size, got %v", want)
	}
	for a := range want {
		for b := range want {
			if a != b && want[a] == want[b] {
				t.Fatalf("fixture: the two versions must differ in ETag too")
			}
		}
	}
	t.Logf("coherent pairs: %v", want)

	bs, err := blockstore.New(fake.New(), blockstore.Config{
		Bucket: "bkt", BlockSize: 4 << 20, MemCache: 1 << 20, MaxRange: 1 << 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(bs.Close)
	raw := NewRawFileSystem(Config{
		Index: ix1, Store: bs, Metrics: metrics.New(), UID: 1000, GID: 1000,
	}).(*rawFS)

	// Resolve the node once; Lookup is not what is under test.
	var eo fuse.EntryOut
	if s := raw.Lookup(nil, &fuse.InHeader{NodeId: fuse.FUSE_ROOT_ID}, "shared.txt", &eo); s != fuse.OK {
		t.Fatalf("lookup: %v", s)
	}

	var stop atomic.Bool
	var torn atomic.Int64
	var opens atomic.Int64
	var wg sync.WaitGroup

	// Swapper: flip versions as fast as possible, so the window between Open's index loads is
	// hit rather than hoped for.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; !stop.Load(); i++ {
			if i%2 == 0 {
				raw.SwapIndex(ix2)
			} else {
				raw.SwapIndex(ix1)
			}
		}
	}()

	// Openers: build handles and check each one's (size, etag) pair is from a single version.
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 4000; i++ {
				var oo fuse.OpenOut
				if s := raw.Open(nil, &fuse.OpenIn{InHeader: fuse.InHeader{NodeId: eo.NodeId}}, &oo); s != fuse.OK {
					continue // the path can be absent mid-swap; not what is under test
				}
				h := raw.handleOf(oo.Fh)
				if h != nil {
					opens.Add(1)
					if e, ok := want[h.size]; !ok || e != h.key.ETagHash {
						torn.Add(1)
					}
				}
				raw.Release(nil, &fuse.ReleaseIn{Fh: oo.Fh})
			}
		}()
	}

	// Let the openers finish, then stop the swapper.
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	for {
		if opens.Load() >= 8*4000-100 {
			break
		}
		select {
		case <-done:
			goto finished
		default:
		}
	}
finished:
	stop.Store(true)
	<-done

	t.Logf("%d opens during continuous SwapIndex: %d torn", opens.Load(), torn.Load())
	if torn.Load() > 0 {
		t.Errorf("%d of %d handles were built from a MIX of two index versions — a size from "+
			"one and an ETag from the other. Open loads the index three times (Stat, "+
			"ETagHashOf, BackingOf); it must load it once and use that snapshot, which is "+
			"what the liveIndex struct behind the atomic pointer is for.",
			torn.Load(), opens.Load())
	}
	if opens.Load() == 0 {
		t.Fatal("no handles were opened; the test exercised nothing")
	}
	_ = fmt.Sprint
}
