// SPDX-License-Identifier: Apache-2.0

package fuse

import (
	"testing"

	"github.com/scttfrdmn/lith/internal/blockstore"
	"github.com/scttfrdmn/lith/internal/prefetch"
)

// #222: a strided slice reader fetches its extents, and WHY BOTH HALVES WERE NEEDED.
//
// THE 2x2, measured on this fixture, steady-state bytes per 4 KiB read. It is the whole
// argument for the shape of the fix and it is why two previous attempts failed:
//
//	demand lane     prediction      B/read      vs main
//	Random only     whole block       990,321   baseline (what shipped)
//	+ Strided       whole block     1,001,244   +1.1% WORSE
//	Random only     bounded         1,044,935   +5.5% WORSE
//	+ Strided       bounded            61,895   16.0x BETTER
//
// EITHER HALF ALONE MAKES IT WORSE. The two paths are over-determined -- remove one and the
// other supplies the same bytes, plus its own:
//
//   - extending the demand lane alone: the prediction still fetches the whole chunk, so the
//     byte-exact demand read lands on top of it rather than inside it
//   - bounding the prediction alone: the prediction is for the block the NEXT read demands,
//     so a whole-chunk demand read asks for everything the bounded prefetch skipped, and the
//     per-chunk singleflight joins the two into one whole-chunk GET anyway
//
// Only moving both leaves the reader fetching its extents. The steady state lands at 0.94
// extents per read -- under one extent, because the prediction for read i+1 and the demand
// read at i+1 want the same extent and one fetch serves both.
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
func TestStridedSliceReaderFetchesItsExtents(t *testing.T) {
	const objBytes = int64(64) << 20
	const readLen = int64(4) << 10 // a row segment, not a block
	const strideBlocks = 3         // non-unit, so it is a stride and not sequential
	raw, srv := newByteExactFS(t, objBytes)
	node, fh := openBig(t, raw)
	h := raw.handleOf(fh)

	// strideRun (#328) needs three consecutive equal deltas to establish, so a two-read
	// fixture would never reach Strided -- the trap that made an earlier test in this package
	// prove nothing. Those establishing reads are whole-chunk by design and are measured
	// SEPARATELY below, because amortising them over the run hides the steady-state figure
	// this issue is about.
	blk := int64(blockstore.ChunkSize)

	// The establishment boundary is DETECTED, not hard-coded: the loop notes the byte count
	// at the read where the handle first reaches Strided. strideRun is unexported and has
	// already changed once (#328 raised it from one repeat to two), so a literal here would
	// silently drift and start charging steady-state reads to the establishment prefix.
	var atEstablish, total int64
	establishReads := 0
	reads := 0
	before := quiesce(srv)
	for i := int64(0); i*strideBlocks*blk+readLen < objBytes; i++ {
		readAt(t, raw, node, fh, i*strideBlocks*blk, readLen)
		reads++
		if establishReads == 0 && h.pf.state() == prefetch.Strided {
			establishReads = reads
			atEstablish = quiesce(srv) - before
		}
	}
	total = quiesce(srv) - before

	if establishReads == 0 {
		t.Fatalf("the handle never reached Strided in %d reads; this fixture is not "+
			"exercising #222's branch", reads)
	}
	steadyReads := int64(reads - establishReads)
	if steadyReads < 5 {
		t.Fatalf("only %d reads after establishment; too few to measure a steady state",
			steadyReads)
	}
	steady := (total - atEstablish) / steadyReads
	perRead := total / int64(reads)

	t.Logf("%d strided reads of %d KiB at a %d-chunk stride: state=%v", reads, readLen>>10,
		strideBlocks, h.pf.state())
	t.Logf("  amortised  %7d B per read = %5.1fx the read", perRead, float64(perRead)/float64(readLen))
	t.Logf("  STEADY     %7d B per read = %5.1fx the read  (after the stride is confirmed)",
		steady, float64(steady)/float64(readLen))
	t.Logf("  establish  %7d B over the first %d reads  (whole-chunk by design, #233)",
		atEstablish, establishReads)
	t.Logf("  one chunk is %d, one extent %d", int64(blockstore.ChunkSize),
		int64(blockstore.ExtentSize))

	// NOT VACUOUS: if the fixture never reaches Strided this measures a different branch.
	if h.pf.state() != prefetch.Strided {
		t.Fatalf("state = %v, want Strided — this fixture is not exercising #222's branch",
			h.pf.state())
	}

	// THE FIX, pinned on the STEADY-STATE figure. A confirmed strided slice reader fetches
	// its extents and nothing else, so the cost is a small multiple of one extent -- not the
	// whole 1 MiB chunk it used to pay. Both halves of the fix are required to reach this and
	// each is inert without the other; see the comment at the demand lane in fs.go.
	if steady > 3*int64(blockstore.ExtentSize) {
		t.Errorf("steady-state cost is %d B per read, above three %d B extents — a strided "+
			"slice reader is fetching more than its extents again", steady,
			int64(blockstore.ExtentSize))
	}
	// AND A FLOOR, so a fixture serving everything from cache cannot pass quietly. Set at
	// HALF an extent, not one: the measured steady state is 0.94 extents per read, because
	// the prediction for read i+1 and the demand read at i+1 want the same extent and the
	// per-chunk singleflight joins them into one fetch -- so ~one extent per read is the
	// floor the mechanism allows, and boundary effects (a prediction past EOF, one landing in
	// an already-fetched chunk) pull the average just under it. A floor of one whole extent
	// fails on the correct behaviour, which is how this assertion was first written.
	if steady < int64(blockstore.ExtentSize)/2 {
		t.Errorf("steady-state cost is %d B per read, under half a %d B extent — the fixture "+
			"is serving from cache rather than fetching, so this measures nothing", steady,
			int64(blockstore.ExtentSize))
	}
	// The amortised figure must still beat one whole chunk per read, which is what shipped
	// before. Stated as the headline number because it is what a run actually pays.
	if perRead >= int64(blockstore.ChunkSize) {
		t.Errorf("amortised cost is %d B per read, still at or above one %d B chunk — the fix "+
			"is not reaching a real run", perRead, int64(blockstore.ChunkSize))
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
