// SPDX-License-Identifier: Apache-2.0

package blockstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"sort"
	"time"

	"github.com/klauspost/compress/zstd"
)

// backingRecorder is an optional Recorder extension for the CargoShip backing
// metrics (#94): frames fetched, bytes decompressed, and per-frame checksum
// failures. nil-safe via recordBacking.
type backingRecorder interface {
	BackingFramesFetched(n int64)
	BackingDecompressBytes(n int64)
	BackingChecksumFail()
}

func (bs *BlockStore) recordBacking(f func(backingRecorder)) {
	if bs.backing != nil {
		f(bs.backing)
	}
}

func (bs *BlockStore) decoder() *zstd.Decoder {
	bs.zdecOnce.Do(func() {
		// DecodeAll is safe for concurrent use; one shared decoder serves all fills.
		bs.zdec, _ = zstd.NewReader(nil, zstd.WithDecoderConcurrency(0))
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

// cargoFetch decodes [off,off+length) of a CargoShip chunk's uncompressed tar
// stream. The covering frames are contiguous in both spaces, so a single range
// GET fetches all their compressed bytes; each frame is checksum-verified
// (sha256 of the compressed bytes, #439) and zstd-decoded, and the requested
// slice is assembled. A checksum mismatch is ErrStale (never a silent bad read).
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
	compStart := frames[lo].CompOff
	compEnd := frames[hi].CompOff + frames[hi].CompLen
	compLen := compEnd - compStart
	if compLen <= 0 {
		return nil, "", fmt.Errorf("cargoship: empty compressed run for [%d,%d)", off, end)
	}

	bs.record(func(r Recorder) { r.StartInflight() })
	t0 := time.Now()
	body, etag, err := bs.src.GetRangeReader(ctx, k.Key, compStart, compLen)
	bs.record(func(r Recorder) { r.S3Get(compLen, err != nil); r.EndInflight() })
	if err != nil {
		return nil, "", err
	}
	bs.recordTTFB(time.Since(t0))
	comp, rerr := io.ReadAll(body)
	_ = body.Close()
	if rerr != nil {
		return nil, "", rerr
	}
	if int64(len(comp)) != compLen {
		return nil, "", io.ErrUnexpectedEOF
	}

	out := make([]byte, length)
	dec := bs.decoder()
	var decBytes int64
	for i := lo; i <= hi; i++ {
		f := frames[i]
		cs := f.CompOff - compStart
		if cs < 0 || cs+f.CompLen > int64(len(comp)) {
			return nil, "", fmt.Errorf("cargoship: frame %d compressed span out of run", i)
		}
		cb := comp[cs : cs+f.CompLen]
		if f.HasSum && sha256.Sum256(cb) != f.Sum {
			bs.recordBacking(func(r backingRecorder) { r.BackingChecksumFail() })
			bs.markStale(k.Key)
			return nil, "", ErrStale
		}
		ub, derr := dec.DecodeAll(cb, nil)
		if derr != nil {
			return nil, "", fmt.Errorf("cargoship: decode frame %d: %w", i, derr)
		}
		if int64(len(ub)) != f.UncompLen {
			return nil, "", fmt.Errorf("cargoship: frame %d decoded to %d bytes, want %d", i, len(ub), f.UncompLen)
		}
		decBytes += int64(len(ub))
		// Copy the overlap of this frame with [off,end) into out.
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
	nframes := int64(hi - lo + 1)
	bs.recordBacking(func(r backingRecorder) {
		r.BackingFramesFetched(nframes)
		r.BackingDecompressBytes(decBytes)
	})
	return io.NopCloser(bytes.NewReader(out)), etag, nil
}
