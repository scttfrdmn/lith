// SPDX-License-Identifier: Apache-2.0

package fuse

import (
	"testing"

	"github.com/scttfrdmn/lith/internal/blockstore"
	"github.com/scttfrdmn/lith/internal/prefetch"
)

// #222: where a strided slice reader's over-fetch actually comes from.
//
// A FITS 2-D cutout walking row segments at a constant stride is classified Strided, and it
// over-fetches 4.76x. Two mechanisms exist to stop exactly that, and neither reaches Strided:
//
//	byte-exact extent lane (Read)  gates on  state() == prefetch.Random   -> Strided excluded
//	evidence gate (windowCap)      consulted only on the Sequential ramp  -> never consulted
//
// So extending the demand lane to Strided looks like the fix. IT IS NOT, and this test is the
// measurement that says so -- recorded because it is a plausible one-line change that someone
// will reach for again.
//
// The cost is the strided branch's PREDICTION, not the demand read. The branch dispatches one
// predicted block per read, that block is fetched WHOLE, and at a 4 KiB read that is 256x. The
// prediction is also correct about which block: at stride d the prediction is blk+d, which is
// exactly the next read's block. So the branch is right about where the reader is going and
// wrong about how much of it the reader wants.
//
// Measured when the demand lane was extended to Strided: bytes went UP by ~14.9 KB per read,
// because the demand extents were then fetched in addition to a prediction that already
// covered them. The fix has to bound the prediction; there is no byte-range prefetch entry
// point today (BlockStore.Prefetch takes a block index), which is why this is not a one-liner.
func TestStridedSliceReaderPaysAWholeBlockPerRead(t *testing.T) {
	const objBytes = int64(64) << 20
	const readLen = int64(4) << 10 // a row segment, not a block
	const strideBlocks = 3         // non-unit, so it is a stride and not sequential
	raw, srv := newByteExactFS(t, objBytes)
	node, fh := openBig(t, raw)
	h := raw.handleOf(fh)

	// strideRun (#328) needs three consecutive equal deltas to establish, so a two-read
	// fixture would never reach Strided -- the trap that made an earlier test in this package
	// prove nothing.
	blk := int64(blockstore.ChunkSize)
	reads := 0
	before := quiesce(srv)
	for i := int64(0); i*strideBlocks*blk+readLen < objBytes; i++ {
		readAt(t, raw, node, fh, i*strideBlocks*blk, readLen)
		reads++
	}
	got := quiesce(srv) - before
	perRead := got / int64(reads)

	t.Logf("%d strided reads of %d KiB at a %d-chunk stride: state=%v", reads, readLen>>10,
		strideBlocks, h.pf.state())
	t.Logf("  %d B per read = %.0fx the read  (one chunk is %d, one extent %d)",
		perRead, float64(perRead)/float64(readLen), int64(blockstore.ChunkSize),
		int64(blockstore.ExtentSize))

	// NOT VACUOUS: if the fixture never reaches Strided this measures a different branch.
	if h.pf.state() != prefetch.Strided {
		t.Fatalf("state = %v, want Strided — this fixture is not exercising #222's branch",
			h.pf.state())
	}

	// THE CHARACTERIZATION, pinned so a change to the strided branch has to confront it: one
	// whole block per read. If this drops, the prediction has been bounded and #222 has moved.
	if perRead < int64(blockstore.ChunkSize) {
		t.Errorf("a strided slice reader now pays %d B per read, below one %d B chunk — the "+
			"prediction has been bounded, which is #222's actual fix. Update this test with "+
			"the new figure rather than deleting it", perRead, int64(blockstore.ChunkSize))
	}
	// And it must not be worse than one block plus a straddle, which is what extending the
	// demand lane to Strided produced.
	if perRead > int64(blockstore.ChunkSize)+2*int64(blockstore.ExtentSize) {
		t.Errorf("a strided slice reader pays %d B per read, more than one chunk plus a "+
			"straddle — the demand read is now fetching on top of the prediction instead of "+
			"being covered by it", perRead)
	}
}

// The bound any fix to the above has to respect: a strided reader taking LARGE reads is served
// correctly today and must stay that way. Whatever bounds the prediction for a slice reader
// must not starve a strided bulk reader, whose next block genuinely is wanted whole.
func TestStridedBulkReaderKeepsWholeChunks(t *testing.T) {
	const objBytes = int64(64) << 20
	const readLen = int64(blockstore.ChunkSize)
	const strideBlocks = 3
	raw, srv := newByteExactFS(t, objBytes)
	node, fh := openBig(t, raw)
	h := raw.handleOf(fh)

	blk := int64(blockstore.ChunkSize)
	reads := 0
	before := quiesce(srv)
	for i := int64(0); i*strideBlocks*blk+readLen < objBytes; i++ {
		readAt(t, raw, node, fh, i*strideBlocks*blk, readLen)
		reads++
	}
	perRead := (quiesce(srv) - before) / int64(reads)

	if h.pf.state() != prefetch.Strided {
		t.Fatalf("state = %v, want Strided", h.pf.state())
	}
	t.Logf("%d strided reads of a whole chunk: %d B per read", reads, perRead)
	if perRead < int64(blockstore.ChunkSize) {
		t.Errorf("a whole-chunk read on a strided handle fetched only %d B — a fix aimed at "+
			"the slice case has starved the bulk case", perRead)
	}
}
