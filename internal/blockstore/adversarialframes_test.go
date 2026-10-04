// SPDX-License-Identifier: Apache-2.0

package blockstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/scttfrdmn/lith/internal/cargoship"
	"github.com/scttfrdmn/lith/internal/s3client/fake"
)

// makeStampedChunk builds a framed .tar.zst chunk whose frames are DISTINGUISHABLE: every
// byte of frame f is byte(f+1). makeFramedChunk's data varies every 4 KiB on (i/4096)&0xff,
// which repeats every 4 MiB — so at a 4 MiB frame size two different frames hold identical
// bytes, and a test that swapped them would pass. This fixture is the point of the test.
func makeStampedChunk(t *testing.T, srv *fake.Server, key string, nFrames int, frameU int64) (Key, []byte) {
	t.Helper()
	master := make([]byte, int64(nFrames)*frameU)
	for f := range nFrames {
		for i := int64(0); i < frameU; i++ {
			master[int64(f)*frameU+i] = byte(f + 1)
		}
	}
	enc, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatalf("zstd writer: %v", err)
	}
	defer func() { _ = enc.Close() }()

	var comp []byte
	frames := make([]cargoship.Frame, nFrames)
	for f := range nFrames {
		uoff := int64(f) * frameU
		cb := enc.EncodeAll(master[uoff:uoff+frameU], nil)
		frames[f] = cargoship.Frame{
			CompOff: int64(len(comp)), CompLen: int64(len(cb)),
			UncompOff: uoff, UncompLen: frameU,
			Sum: sha256.Sum256(cb), HasSum: true,
		}
		comp = append(comp, cb...)
	}
	srv.Put(key, comp, time.Unix(1_700_000_000, 0))
	k := keyFor(t, srv, key)
	k.Cargo = &CargoChunk{Frames: frames, UncompTotal: int64(nFrames) * frameU}
	return k, master
}

// M17-B case 2 (#217), the bytes: a frame table whose COMPRESSED spans overlap serves
// ANOTHER FRAME'S CONTENT, successfully, with a valid checksum.
//
// Point frame 1's compressed span at frame 0's bytes. Every other field stays consistent —
// uncompressed spans still tile the chunk, sizes are positive, spans are inside the object —
// so nothing structural is wrong. The read path then:
//
//   - fetches frame 1's declared compressed range, which is frame 0's bytes;
//   - verifies the per-frame sha256, WHICH PASSES, because that checksum covers the
//     compressed bytes and they are exactly the bytes fetched (an attacker writing the
//     manifest computes it over what they point at);
//   - decodes to exactly UncompLen, because frame 0 and frame 1 are the same length;
//   - lands the result at frame 1's uncompressed offset.
//
// Result: a read at frame 1's offset returns frame 0's content. No error, no short read, no
// checksum failure. That is a SERVES WRONG verdict, the one #217 is hunting, and the reason
// it is now rejected at parse (cargoship.Parse, compressed-span overlap) rather than caught
// here: by the time the read path has the frame table, every cross-check it could make has
// already been satisfied by construction.
func TestOverlappingCompressedFramesServeAnotherFramesBytes(t *testing.T) {
	srv := fake.New()
	const nFrames = 3
	const frameU = int64(1) << 20
	k, master := makeStampedChunk(t, srv, "chunk.tar.zst", nFrames, frameU)

	// Sanity: the honest table serves the right bytes, or nothing below means anything.
	bs := newStore(t, srv, Config{BlockSize: 1 << 20, MaxRange: 64 << 20, Recorder: &backRec{}})
	got, err := bs.GetRange(context.Background(), k, frameU, 4096, int64(nFrames)*frameU)
	if err != nil {
		t.Fatalf("honest read of frame 1: %v", err)
	}
	if !bytes.Equal(got, master[frameU:frameU+4096]) {
		t.Fatalf("fixture: the honest table served frame %d's stamp at frame 1's offset", got[0])
	}

	// THE LIE, one field pair: frame 1's compressed span is frame 0's. The checksum is
	// recomputed the way whoever wrote the manifest would — over the bytes it points at.
	lying := *k.Cargo
	lying.Frames = append([]cargoship.Frame(nil), k.Cargo.Frames...)
	lying.Frames[1].CompOff = lying.Frames[0].CompOff
	lying.Frames[1].CompLen = lying.Frames[0].CompLen
	lying.Frames[1].Sum = lying.Frames[0].Sum
	bad := k
	bad.Cargo = &lying

	// A FRESH store, so nothing below is served from the honest store's frame cache or
	// chunk tier -- the object and the key are identical, only the table lies.
	bs2 := newStore(t, srv, Config{BlockSize: 1 << 20, MaxRange: 64 << 20, Recorder: &backRec{}})
	got, err = bs2.GetRange(context.Background(), bad, frameU, 4096, int64(nFrames)*frameU)

	switch {
	case err != nil:
		// fail-closed: acceptable, and record which check caught it.
		t.Logf("verdict FAIL-CLOSED at the read path: %v", err)
	case bytes.Equal(got, master[frameU:frameU+4096]):
		t.Fatal("fixture: the aliased frame returned frame 1's own bytes, which is " +
			"impossible — the lie did not take effect")
	default:
		// This is what actually happens, and why the fix is at parse time. Asserted
		// explicitly rather than tolerated, so if the read path ever grows a cross-check
		// that catches it, this test says so instead of passing quietly.
		if got[0] != 1 {
			t.Fatalf("expected frame 0's stamp (1), got %d", got[0])
		}
		t.Logf("verdict SERVES WRONG: a read at frame 1's offset returned frame 0's "+
			"content (stamp %d), with no error and a passing per-frame checksum", got[0])
	}

	// The gate that now stops it is at the only layer that can see it: the manifest parser.
	// See TestFrameTableRejectsOverlappingCompressedSpans in internal/cargoship -- a read
	// path handed this table has already satisfied every cross-check available to it.
}
