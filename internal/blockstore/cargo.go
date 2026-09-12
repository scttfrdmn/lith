// SPDX-License-Identifier: Apache-2.0

package blockstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"sort"
	"strconv"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/scttfrdmn/lith/internal/cargoship"
)

// backingRecorder is an optional Recorder extension for the CargoShip backing
// metrics (#94): frames fetched, bytes decompressed, and per-frame checksum
// failures. nil-safe via recordBacking.
type backingRecorder interface {
	BackingFramesFetched(n int64)
	BackingFrameReuse(n int64)
	BackingDecompressBytes(n int64)
	BackingChecksumFail()
}

func (bs *BlockStore) recordBacking(f func(backingRecorder)) {
	if bs.backing != nil {
		f(bs.backing)
	}
}

// maxFrameBytes bounds a single zstd frame lith will decode into memory. Normal
// frames are ~ the archive's frame size (e.g. 16 MiB). CargoShip frames only at
// file boundaries, so a large file becomes one giant frame; a zstd frame is not
// randomly seekable, so serving a small read from a multi-GB frame would decode
// the whole thing (and per-chunk re-decodes are quadratic). Such a file belongs
// in a frameless (plain .tar) chunk — lith reads that as a direct range GET. We
// cap the decoder here so a giant frame is a clear error, never an OOM.
const maxFrameBytes = 512 << 20

func (bs *BlockStore) decoder() *zstd.Decoder {
	bs.zdecOnce.Do(func() {
		// DecodeAll is safe for concurrent use; one shared decoder serves all fills.
		bs.zdec, _ = zstd.NewReader(nil, zstd.WithDecoderConcurrency(0),
			zstd.WithDecoderMaxMemory(maxFrameBytes))
	})
	return bs.zdec
}

// fetchReader returns a reader over [off,off+length) of k's logical byte space
// and the backing object's ETag, recording S3 GET accounting (actual bytes
// fetched) and first-byte latency. For an ordinary key the logical space is the
// object itself (one range GET). For a CargoShip chunk it is the chunk's
// uncompressed tar stream, served by decoding the covering zstd frames.
func (bs *BlockStore) fetchReader(ctx context.Context, k Key, off, length int64) (io.ReadCloser, string, error) {
	// A frameless (plain .tar) CargoShip chunk reads like an ordinary object: the
	// requested offset is already a byte offset in the uncompressed tar object
	// (the FUSE layer added the file's archive_offset), so it is a direct range
	// GET — no decode, no per-frame checksum (#94, cargoship v0.24.3).
	if k.Cargo == nil || len(k.Cargo.Frames) == 0 {
		bs.record(func(r Recorder) { r.StartInflight() })
		t0 := time.Now()
		body, etag, err := bs.src.GetRangeReader(ctx, k.Key, off, length)
		bs.record(func(r Recorder) { r.S3Get(length, err != nil); r.EndInflight() })
		if err == nil {
			bs.recordTTFB(time.Since(t0))
		}
		return body, etag, err
	}
	return bs.cargoFetch(ctx, k, off, length)
}

// frameKey identifies a decoded frame in the frame cache: the chunk object key
// plus the frame's compressed offset (unique per frame within the object).
func frameKey(chunkKey string, compOff int64) string {
	return chunkKey + "@" + strconv.FormatInt(compOff, 10)
}

// cargoFetch decodes [off,off+length) of a CargoShip chunk's uncompressed tar
// stream. Each covering frame is served from the decoded-frame cache when
// present (no GET, no decode); the frames that miss are fetched — coalescing
// maximal runs of consecutive misses into one range GET — checksum-verified
// (sha256 of the compressed bytes, #439), zstd-decoded, cached, and the
// requested slice assembled. A tree walk thus fetches and decodes each frame
// exactly once regardless of how many files it covers (#137). A checksum
// mismatch is ErrStale (never a silent bad read).
func (bs *BlockStore) cargoFetch(ctx context.Context, k Key, off, length int64) (io.ReadCloser, string, error) {
	frames := k.Cargo.Frames
	end := off + length
	if off < 0 || length <= 0 {
		return nil, "", fmt.Errorf("cargoship: invalid read [%d,%d)", off, end)
	}
	lo := sort.Search(len(frames), func(i int) bool {
		return frames[i].UncompOff+frames[i].UncompLen > off
	})
	if lo >= len(frames) || frames[lo].UncompOff >= end {
		return nil, "", fmt.Errorf("cargoship: read [%d,%d) has no covering frame", off, end)
	}
	hi := lo
	for hi+1 < len(frames) && frames[hi].UncompOff+frames[hi].UncompLen < end {
		hi++
	}
	// Reject a frame too large to decode into memory (see maxFrameBytes): a large
	// file that CargoShip framed as one giant frame is not randomly readable. It
	// should be stored in a frameless (plain .tar) chunk, or cut into sub-frames.
	for i := lo; i <= hi; i++ {
		if frames[i].UncompLen > maxFrameBytes {
			return nil, "", fmt.Errorf("cargoship: frame %d is %d bytes uncompressed (> %d cap) — a large file was framed as one giant zstd frame, which is not randomly readable; store it frameless or cut sub-frames (cargoship framing)", i, frames[i].UncompLen, maxFrameBytes)
		}
	}

	out := make([]byte, length)
	// copySlice copies the overlap of frame f's decoded bytes ub with [off,end).
	copySlice := func(f cargoship.Frame, ub []byte) {
		s := off
		if f.UncompOff > s {
			s = f.UncompOff
		}
		e := end
		if fe := f.UncompOff + f.UncompLen; fe < e {
			e = fe
		}
		if e > s {
			copy(out[s-off:e-off], ub[s-f.UncompOff:e-f.UncompOff])
		}
	}

	var etag string
	var fetched, reused, decBytes int64
	for i := lo; i <= hi; {
		// Cache hit: serve this frame with no GET and no decode.
		if ub, et, ok := bs.frameCache.get(frameKey(k.Key, frames[i].CompOff)); ok {
			reused++
			if etag == "" {
				etag = et
			}
			copySlice(frames[i], ub)
			i++
			continue
		}
		// Gather a maximal run of consecutive missing frames [i,j], fetched in one
		// range GET (they are contiguous in compressed space).
		j := i
		for j+1 <= hi && !bs.frameCache.has(frameKey(k.Key, frames[j+1].CompOff)) {
			j++
		}
		et, db, err := bs.fetchFrameRun(ctx, k, frames, i, j, copySlice)
		if err != nil {
			return nil, "", err
		}
		etag = et
		fetched += int64(j - i + 1)
		decBytes += db
		i = j + 1
	}

	bs.recordBacking(func(r backingRecorder) {
		r.BackingFramesFetched(fetched)
		r.BackingFrameReuse(reused)
		r.BackingDecompressBytes(decBytes)
	})
	return io.NopCloser(bytes.NewReader(out)), etag, nil
}

// fetchFrameRun GETs the compressed span of frames [lo,hi] (contiguous in
// compressed space) in one request, verifies each frame's checksum, decodes it,
// caches the decoded bytes, and copies each frame's overlap into out via copy.
// It returns the chunk object's ETag and the total bytes decoded.
func (bs *BlockStore) fetchFrameRun(ctx context.Context, k Key, frames []cargoship.Frame, lo, hi int, emit func(cargoship.Frame, []byte)) (string, int64, error) {
	compStart := frames[lo].CompOff
	compEnd := frames[hi].CompOff + frames[hi].CompLen
	compLen := compEnd - compStart
	if compLen <= 0 {
		return "", 0, fmt.Errorf("cargoship: empty compressed run for frames [%d,%d]", lo, hi)
	}

	bs.record(func(r Recorder) { r.StartInflight() })
	t0 := time.Now()
	body, etag, err := bs.src.GetRangeReader(ctx, k.Key, compStart, compLen)
	bs.record(func(r Recorder) { r.S3Get(compLen, err != nil); r.EndInflight() })
	if err != nil {
		return "", 0, err
	}
	bs.recordTTFB(time.Since(t0))
	comp, rerr := io.ReadAll(body)
	_ = body.Close()
	if rerr != nil {
		return "", 0, rerr
	}
	if int64(len(comp)) != compLen {
		return "", 0, io.ErrUnexpectedEOF
	}

	dec := bs.decoder()
	var decBytes int64
	for i := lo; i <= hi; i++ {
		f := frames[i]
		cs := f.CompOff - compStart
		if cs < 0 || cs+f.CompLen > int64(len(comp)) {
			return "", 0, fmt.Errorf("cargoship: frame %d compressed span out of run", i)
		}
		cb := comp[cs : cs+f.CompLen]
		if f.HasSum && sha256.Sum256(cb) != f.Sum {
			bs.recordBacking(func(r backingRecorder) { r.BackingChecksumFail() })
			bs.markStale(k.Key)
			return "", 0, ErrStale
		}
		ub, derr := dec.DecodeAll(cb, nil)
		if derr != nil {
			return "", 0, fmt.Errorf("cargoship: decode frame %d: %w", i, derr)
		}
		if int64(len(ub)) != f.UncompLen {
			return "", 0, fmt.Errorf("cargoship: frame %d decoded to %d bytes, want %d", i, len(ub), f.UncompLen)
		}
		decBytes += int64(len(ub))
		bs.frameCache.put(frameKey(k.Key, f.CompOff), ub, etag)
		emit(f, ub)
	}
	return etag, decBytes, nil
}
