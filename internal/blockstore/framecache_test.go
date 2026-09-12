// SPDX-License-Identifier: Apache-2.0

package blockstore

import (
	"context"
	"crypto/sha256"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/scttfrdmn/lith/internal/cargoship"
	"github.com/scttfrdmn/lith/internal/s3client/fake"
)

// backRec is a Recorder that also implements backingRecorder, tallying the
// CargoShip frame-cache metrics (#137).
type backRec struct {
	get    int64
	frames int64
	reuse  int64
	decomp int64
	ckFail int64
}

func (r *backRec) MemHit()                        {}
func (r *backRec) DiskHit()                       {}
func (r *backRec) Miss()                          {}
func (r *backRec) S3Get(n int64, isErr bool)      { atomic.AddInt64(&r.get, 1) }
func (r *backRec) StartInflight()                 {}
func (r *backRec) EndInflight()                   {}
func (r *backRec) StaleKey(string)                {}
func (r *backRec) PrefetchIssued()                {}
func (r *backRec) PrefetchHit()                   {}
func (r *backRec) UncoveredMiss()                 {}
func (r *backRec) BackingFramesFetched(n int64)   { atomic.AddInt64(&r.frames, n) }
func (r *backRec) BackingFrameReuse(n int64)      { atomic.AddInt64(&r.reuse, n) }
func (r *backRec) BackingDecompressBytes(n int64) { atomic.AddInt64(&r.decomp, n) }
func (r *backRec) BackingChecksumFail()           { atomic.AddInt64(&r.ckFail, 1) }

// makeFramedChunk builds a synthetic framed .tar.zst chunk of nFrames frames,
// each frameU uncompressed bytes, and stores it in srv at key. Each frame is an
// independent zstd frame with a sha256-of-compressed checksum, so the layout
// matches a real CargoShip framed chunk. It returns the Cargo-backed Key and the
// master uncompressed bytes (for read verification). Data varies every 4 KiB so
// reads are verifiable while staying compressible.
func makeFramedChunk(t *testing.T, srv *fake.Server, key string, nFrames int, frameU int64) (Key, []byte) {
	t.Helper()
	total := int64(nFrames) * frameU
	master := make([]byte, total)
	for i := range master {
		master[i] = byte((i / 4096) & 0xff)
	}
	enc, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatalf("zstd writer: %v", err)
	}
	defer func() { _ = enc.Close() }()

	var comp []byte
	frames := make([]cargoship.Frame, nFrames)
	for f := 0; f < nFrames; f++ {
		uoff := int64(f) * frameU
		cb := enc.EncodeAll(master[uoff:uoff+frameU], nil)
		frames[f] = cargoship.Frame{
			CompOff:   int64(len(comp)),
			CompLen:   int64(len(cb)),
			UncompOff: uoff,
			UncompLen: frameU,
			Sum:       sha256.Sum256(cb),
			HasSum:    true,
		}
		comp = append(comp, cb...)
	}
	srv.Put(key, comp, time.Unix(1_700_000_000, 0))
	k := keyFor(t, srv, key)
	k.Cargo = &CargoChunk{Frames: frames, UncompTotal: total}
	return k, master
}

// TestFrameCacheWalkDecodesEachFrameOnce: a sequential walk over a chunk whose
// frames each span several 1 MiB cache chunks fetches and decodes each frame
// exactly once — decompress_bytes == the uncompressed size (1×), not a multiple
// of it. This is the #137 fix: without the frame cache each 1 MiB fill re-fetched
// and re-decoded its whole covering frame (~4× over-fetch at a 4 MiB frame).
func TestFrameCacheWalkDecodesEachFrameOnce(t *testing.T) {
	srv := fake.New()
	const nFrames = 3
	const frameU = int64(4) << 20 // 4 MiB frame spans 4 one-MiB chunks
	rec := &backRec{}
	k, master := makeFramedChunk(t, srv, "chunk.tar.zst", nFrames, frameU)
	bs := newStore(t, srv, Config{BlockSize: 1 << 20, MaxRange: 64 << 20, Recorder: rec})
	total := int64(nFrames) * frameU

	// Walk the whole stream in 1 MiB reads (as a tree walk touches successive
	// files chunk by chunk).
	for off := int64(0); off < total; off += mib {
		got, err := bs.GetRange(context.Background(), k, off, mib, total)
		if err != nil {
			t.Fatalf("read at %d: %v", off, err)
		}
		if string(got) != string(master[off:off+mib]) {
			t.Fatalf("read at %d: bytes mismatch", off)
		}
	}
	if got := atomic.LoadInt64(&rec.frames); got != nFrames {
		t.Errorf("frames fetched = %d, want %d (each frame once)", got, nFrames)
	}
	if got := atomic.LoadInt64(&rec.decomp); got != total {
		t.Errorf("decompress bytes = %d, want %d (1×, not a multiple)", got, total)
	}
	if got := atomic.LoadInt64(&rec.reuse); got != total/mib-nFrames {
		t.Errorf("frame reuse = %d, want %d", got, total/mib-nFrames)
	}
	if got := atomic.LoadInt64(&rec.get); got != nFrames {
		t.Errorf("S3 GETs = %d, want %d (one per frame)", got, nFrames)
	}
}

// TestFrameCacheOneFrameManyFilesOneGET: a frame covering many files serves them
// all from one GET and one decode.
func TestFrameCacheOneFrameManyFilesOneGET(t *testing.T) {
	srv := fake.New()
	const frameU = int64(1) << 20 // one frame == one 1 MiB chunk, holds 16 "files"
	rec := &backRec{}
	k, master := makeFramedChunk(t, srv, "one.tar.zst", 1, frameU)
	bs := newStore(t, srv, Config{BlockSize: 1 << 20, MaxRange: 64 << 20, Recorder: rec})

	// 16 files of 64 KiB each within the single frame.
	const nFiles = 16
	fsz := frameU / nFiles
	for i := int64(0); i < nFiles; i++ {
		off := i * fsz
		got, err := bs.GetRange(context.Background(), k, off, fsz, frameU)
		if err != nil {
			t.Fatalf("file %d: %v", i, err)
		}
		if string(got) != string(master[off:off+fsz]) {
			t.Fatalf("file %d: bytes mismatch", i)
		}
	}
	if got := atomic.LoadInt64(&rec.get); got != 1 {
		t.Errorf("S3 GETs = %d, want 1 (one frame serves all files)", got)
	}
	if got := atomic.LoadInt64(&rec.frames); got != 1 {
		t.Errorf("frames fetched = %d, want 1", got)
	}
	if got := atomic.LoadInt64(&rec.decomp); got != frameU {
		t.Errorf("decompress bytes = %d, want %d (one decode)", got, frameU)
	}
}

// TestFrameCacheRandomSingleFileDecodesOneFrame: a random single-file read
// decodes only its covering frame, and a second read of a neighbour in the same
// frame (a different 1 MiB chunk) is served with no new GET.
func TestFrameCacheRandomSingleFileDecodesOneFrame(t *testing.T) {
	srv := fake.New()
	const nFrames = 4
	const frameU = int64(4) << 20
	rec := &backRec{}
	k, master := makeFramedChunk(t, srv, "rand.tar.zst", nFrames, frameU)
	bs := newStore(t, srv, Config{BlockSize: 1 << 20, MaxRange: 64 << 20, Recorder: rec})
	total := int64(nFrames) * frameU

	// Read inside frame 2 ([8MiB,12MiB)) at 9 MiB.
	off1 := int64(9) << 20
	got, err := bs.GetRange(context.Background(), k, off1, 4096, total)
	if err != nil || string(got) != string(master[off1:off1+4096]) {
		t.Fatalf("first read: err=%v", err)
	}
	if g := atomic.LoadInt64(&rec.frames); g != 1 {
		t.Errorf("frames fetched = %d, want 1 (only the covering frame)", g)
	}
	if d := atomic.LoadInt64(&rec.decomp); d != frameU {
		t.Errorf("decompress = %d, want %d (one frame)", d, frameU)
	}
	getsAfterFirst := atomic.LoadInt64(&rec.get)

	// Neighbour in the same frame but a different 1 MiB chunk (10 MiB): a frame-
	// cache hit, no new GET.
	off2 := int64(10) << 20
	got, err = bs.GetRange(context.Background(), k, off2, 4096, total)
	if err != nil || string(got) != string(master[off2:off2+4096]) {
		t.Fatalf("neighbour read: err=%v", err)
	}
	if g := atomic.LoadInt64(&rec.get); g != getsAfterFirst {
		t.Errorf("neighbour read issued %d new GET(s), want 0", g-getsAfterFirst)
	}
	if r := atomic.LoadInt64(&rec.reuse); r < 1 {
		t.Errorf("frame reuse = %d, want >=1", r)
	}
}

// TestFrameCacheBudgetOversizedFrameNotCached: a frame larger than the whole
// frame-cache budget is served correctly but not cached, so it cannot pin the
// budget — a later read re-fetches it (no reuse), and bytes stay correct.
func TestFrameCacheBudgetOversizedFrameNotCached(t *testing.T) {
	srv := fake.New()
	const frameU = int64(4) << 20
	rec := &backRec{}
	k, master := makeFramedChunk(t, srv, "big.tar.zst", 1, frameU)
	// Budget below one frame: the frame is never cached.
	bs := newStore(t, srv, Config{BlockSize: 1 << 20, MaxRange: 64 << 20, FrameCache: 1 << 20, Recorder: rec})

	read := func(off int64) {
		got, err := bs.GetRange(context.Background(), k, off, 4096, frameU)
		if err != nil || string(got) != string(master[off:off+4096]) {
			t.Fatalf("read at %d: err=%v", off, err)
		}
	}
	read(0)              // chunk 0 → GET+decode frame
	read(int64(2) << 20) // chunk 2, same frame, but not cached → GET+decode again
	if r := atomic.LoadInt64(&rec.reuse); r != 0 {
		t.Errorf("frame reuse = %d, want 0 (oversized frame not cached)", r)
	}
	if g := atomic.LoadInt64(&rec.get); g < 2 {
		t.Errorf("S3 GETs = %d, want >=2 (frame re-fetched, not cached)", g)
	}
}

// TestFrameCacheSingleflightConcurrent: many concurrent reads of DIFFERENT 1 MiB
// chunks all covered by ONE frame collapse to a single GET and a single decode —
// the singleflight that the block store's per-chunk singleflight cannot provide
// (a big frame spans chunks filled by separate concurrent operations). Without
// it, each concurrent fill would re-fetch and re-decode the same frame.
func TestFrameCacheSingleflightConcurrent(t *testing.T) {
	srv := fake.New()
	const frameU = int64(8) << 20 // one frame spanning 8 one-MiB chunks
	srv.GetDelay = 30 * time.Millisecond
	rec := &backRec{}
	k, master := makeFramedChunk(t, srv, "sf.tar.zst", 1, frameU)
	bs := newStore(t, srv, Config{BlockSize: 1 << 20, MaxRange: 64 << 20, Recorder: rec})

	var wg sync.WaitGroup
	for c := int64(0); c < 8; c++ { // one read in each distinct 1 MiB chunk
		off := c * mib
		wg.Add(1)
		go func(off int64) {
			defer wg.Done()
			got, err := bs.GetRange(context.Background(), k, off, 4096, frameU)
			if err != nil || string(got) != string(master[off:off+4096]) {
				t.Errorf("read at %d: err=%v", off, err)
			}
		}(off)
	}
	wg.Wait()
	if g := atomic.LoadInt64(&rec.get); g != 1 {
		t.Errorf("S3 GETs = %d, want 1 (singleflight dedups concurrent same-frame fills)", g)
	}
	if f := atomic.LoadInt64(&rec.frames); f != 1 {
		t.Errorf("frames fetched = %d, want 1", f)
	}
	if d := atomic.LoadInt64(&rec.decomp); d != frameU {
		t.Errorf("decompress = %d, want %d (one decode)", d, frameU)
	}
}

// TestFrameCacheConcurrentRace exercises the frame cache under concurrent reads
// sharing frames (run with -race).
func TestFrameCacheConcurrentRace(t *testing.T) {
	srv := fake.New()
	const nFrames = 4
	const frameU = int64(2) << 20
	k, master := makeFramedChunk(t, srv, "race.tar.zst", nFrames, frameU)
	bs := newStore(t, srv, Config{BlockSize: 1 << 20, MaxRange: 64 << 20, Recorder: &backRec{}})
	total := int64(nFrames) * frameU

	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			off := (int64(i) * mib) % (total - 4096)
			got, err := bs.GetRange(context.Background(), k, off, 4096, total)
			if err != nil {
				t.Errorf("read at %d: %v", off, err)
				return
			}
			if string(got) != string(master[off:off+4096]) {
				t.Errorf("read at %d: bytes mismatch", off)
			}
		}(i)
	}
	wg.Wait()
}
