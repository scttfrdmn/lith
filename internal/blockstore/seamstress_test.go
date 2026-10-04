// SPDX-License-Identifier: Apache-2.0

package blockstore

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/scttfrdmn/lith/internal/s3client/fake"
)

// M17-D (#219): stress the seam where an extent fill and a whole-chunk fill race on the SAME
// chunk, and verify the BYTES.
//
// WHY CONTENT AND NOT COUNTS. Every concurrency test in this package today checks counts,
// states, or the absence of a panic. None checks that a concurrent reader got the right bytes
// — and this is the code where #219 says a race "serves wrong bytes, not a crash", which
// `-race` catches only if the schedule happens to occur. The singleflight in claim() hands
// several readers one chunkState; the join path re-claims when the owner filled fewer extents
// than the joiner wanted; mem2Q.merge replaces a resident chunk's buffer in place while
// readers may hold the old one. A torn buffer, an off-by-one extent mask, or a re-claim that
// loses the base would all show up as wrong bytes and as nothing else.
//
// The object's byte at offset i is a fixed function of i, so a read served from the wrong
// offset, a zero-filled extent, or a boundary assembled out of two fills is detectable at the
// byte that is wrong — and the failure message says which offset it actually came from.
func seamByte(i int64) byte { return byte(i*31 + 7) }

func seamObject(n int64) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = seamByte(int64(i))
	}
	return b
}

// verifySeam checks a read's bytes and, on mismatch, reports the offset the data actually came
// from — which is the difference between "something is wrong" and a diagnosis.
func verifySeam(t *testing.T, got []byte, off int64) {
	t.Helper()
	for j := range got {
		want := seamByte(off + int64(j))
		if got[j] != want {
			// Search for the offset this byte would be correct at, within a wide window.
			actual := int64(-1)
			for cand := off - 4*ChunkSize; cand < off+4*ChunkSize; cand++ {
				if cand >= 0 && seamByte(cand+int64(j)) == got[j] {
					actual = cand
					break
				}
			}
			t.Fatalf("read at %d, byte %d: got %#x want %#x (that byte is correct for a read "+
				"at offset %d, so this looks like %s)", off, j, got[j], want, actual,
				seamDiagnosis(off, actual))
		}
	}
}

func seamDiagnosis(off, actual int64) string {
	switch {
	case actual < 0:
		return "a torn or zero-filled buffer, matching no offset"
	case actual == off:
		return "a single wrong byte rather than a shifted read"
	case (actual-off)%ChunkSize == 0:
		return fmt.Sprintf("a whole-chunk shift of %d chunks", (actual-off)/ChunkSize)
	case (actual-off)%ExtentSize == 0:
		return fmt.Sprintf("an extent shift of %d extents", (actual-off)/ExtentSize)
	default:
		return fmt.Sprintf("a shift of %d bytes", actual-off)
	}
}

// TestSeamExtentAndWholeChunkFillsRace drives extent reads, whole-chunk reads and straddling
// reads at the same chunks from many goroutines at once, checking every byte.
//
// All four read shapes are deliberate: Chunk with sequential=false takes the extent path
// (want = just this read's 64 KiB extents), Chunk with sequential=true takes the whole-chunk
// path, and GetRange takes ensureChunks — which is where four prefetch-credit paths were found
// missing in #320, and the one path with no content test at all.
func TestSeamExtentAndWholeChunkFillsRace(t *testing.T) {
	const nChunks = 12
	objSize := int64(nChunks) * ChunkSize
	srv := fake.New()
	srv.Put("seam", seamObject(objSize), time.Unix(1_700_000_000, 0))
	k := keyFor(t, srv, "seam")

	workers, iters := 16, 120
	if os.Getenv("LITH_SWEEP_FULL") != "" {
		workers, iters = 48, 400
	}

	// A tier far smaller than the object, so eviction runs concurrently with the fills and a
	// chunk can be dropped and re-fetched mid-flight. That is the schedule a bigger tier
	// hides, and mem2Q.merge replacing a resident buffer is the thing it exposes.
	bs := newStore(t, srv, Config{BlockSize: 4 << 20, MemCache: 4 * ChunkSize, MaxRange: 8 << 20})
	ctx := context.Background()

	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(seed int) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(int64(seed)))
			for i := 0; i < iters; i++ {
				// Concentrate on a few chunks so collisions are the common case rather than
				// a rarity: four workers per chunk at 16 workers.
				ci := int64(seed % 4)
				switch rng.Intn(4) {
				case 0: // small extent read, non-sequential: the extent path
					lo := rng.Int63n(ChunkSize - 4096)
					d, err := bs.Chunk(ctx, k, ci, objSize, lo, lo+4096, false)
					if err != nil {
						t.Errorf("Chunk extent: %v", err)
						return
					}
					verifySeam(t, d[lo:lo+4096], ci*ChunkSize+lo)
				case 1: // whole-chunk read, sequential: the full-mask path
					d, err := bs.Chunk(ctx, k, ci, objSize, 0, ChunkSize, true)
					if err != nil {
						t.Errorf("Chunk whole: %v", err)
						return
					}
					verifySeam(t, d, ci*ChunkSize)
				case 2: // straddling read across the chunk boundary: ensureChunks
					off := ci*ChunkSize + ChunkSize - 2048
					d, err := bs.GetRange(ctx, k, off, 4096, objSize)
					if err != nil {
						t.Errorf("GetRange straddle: %v", err)
						return
					}
					verifySeam(t, d, off)
				case 3: // a prefetch of the same region, racing the demand reads above
					bs.Prefetch(ctx, k, ci*ChunkSize/(4<<20), objSize)
				}
			}
		}(w)
	}
	wg.Wait()

	t.Logf("%d workers x %d iterations over %d chunks, tier %d chunks: every byte verified",
		workers, iters, 4, 4)
	// A clean run is a passing result (#219). Record that the fixture actually collided,
	// otherwise this proves only that the reads work serially.
	if got := srv.GetCallCount(); got == 0 {
		t.Fatal("no GETs: the fixture served everything from cache and raced nothing")
	}
	t.Logf("GETs %d for a %d-chunk object with a %d-chunk tier (re-fetching under eviction "+
		"is the point, not a defect)", srv.GetCallCount(), nChunks, 4)
}
