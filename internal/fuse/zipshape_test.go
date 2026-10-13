// SPDX-License-Identifier: Apache-2.0

package fuse

import (
	"math/rand"
	"testing"

	"github.com/scttfrdmn/lith/internal/blockstore"
)

// ★ #408: a ZIP read — central directory at the end, then random entries — is served its extents
// instead of the whole archive.
//
// THE EXTERNAL FINDING. Python `zipfile` on a 10.3 MB archive of 500 × 20 KiB stored entries:
// read the central directory, then read() 20 random entries. lith fetched the WHOLE ARCHIVE,
// confirmed against a CloudTrail ledger at 12 GETs and 10,291,022 bytes in all four runs, with
// `fill_whole` 0 and prefetch issued 0 — every byte came from demand fills.
//
// THE DIAGNOSIS, which corrected the one on the issue. The report concluded this was not fixable
// by prefetch policy, which is right, and that it was therefore a demand-chunk-size floor, which
// is not: lith has a byte-exact demand lane and these reads are well under its threshold
// (32 KiB against 1 MiB). What blocked it was the lane's predicate, `Random || Strided` — states
// denominated in BLOCKS. A 10 MB object over 8 MiB blocks is 1.25 blocks, and measured, 20 of 22
// reads land in the SAME block with 1 adjacent and 0 further: block-space deltas carry no
// information, so the handle sits in Cold forever and the lane never engages. Meanwhile its
// trailing coverage is 0.056 against the 0.50 threshold with the window full — the byte-space
// signal is emphatic.
//
// So the fix is a third qualifying claim, prefetch.NotTiling, and it is byte-denominated so
// object size cannot defeat it.
func TestZipShapeIsServedItsExtents(t *testing.T) {
	const objBytes = int64(10) << 20
	const blockSz = int64(8) << 20
	const read = int64(32) << 10
	const entry = int64(20) << 10
	const entries = 20

	raw, srv, rec := newWalkFS(t, objBytes, blockSz, 2.0, false, false, true)
	node, fh := openBig(t, raw)
	h := raw.handleOf(fh)
	before := quiesce(srv)

	var toolBytes int64
	for off := objBytes - (64 << 10); off < objBytes; off += read { // central directory
		readAt(t, raw, node, fh, off, read)
		toolBytes += read
	}
	rng := rand.New(rand.NewSource(42))
	for i := 0; i < entries; i++ { // 20 random entries, seeded
		off := rng.Int63n(objBytes - entry)
		for o := off; o < off+entry; o += read {
			n := read
			if o+n > off+entry {
				n = off + entry - o
			}
			readAt(t, raw, node, fh, o, n)
			toolBytes += n
		}
	}
	got := quiesce(srv) - before
	_, _, issued := rec.coverage()

	t.Logf("ZIP shape: fetched %d B for %d B of tool reads = %.2fx (archive is %d B)",
		got, toolBytes, float64(got)/float64(toolBytes), objBytes)
	t.Logf("  state=%v established=%v notTiling=%v prefetchIssued=%d fills=%v",
		h.pf.state(), h.pf.isEstablished(), h.pf.notTiling(), issued, rec.counts())

	// PRECONDITIONS. Each one is a step in the diagnosis above, so a fixture that drifts out of
	// the regime fails loudly instead of scoring a different mechanism.
	if issued != 0 {
		t.Errorf("prefetch issued %d; the external trace reports zero, and the safety of serving "+
			"this handle precisely rests on nothing being prefetched for it", issued)
	}
	if h.pf.isEstablished() {
		t.Error("the handle established; this shape is supposed to stay provisional, and an " +
			"established handle has a window that a precise read would stack on top of")
	}
	if !h.pf.notTiling() {
		t.Fatal("the coverage signal did not judge this access as not-tiling, so the demand " +
			"lane's third predicate is not what is being exercised here")
	}

	// THE RESULT: not the whole archive. That is the entire finding.
	if got >= objBytes {
		t.Errorf("fetched %d B of a %d B archive for %d B of tool reads — the mount is still "+
			"reading the whole file, which is #408 as reported", got, objBytes, toolBytes)
	}
	// AND A FLOOR: the reader's own bytes still have to be fetched, at extent granularity.
	if got < toolBytes {
		t.Errorf("fetched %d B for %d B of reads — below what was demanded, so this measures "+
			"cache hits rather than fetches", got, toolBytes)
	}
	t.Logf("  extent size %d B; %d reads at %d B would cost %d B if each took one extent",
		int64(blockstore.ExtentSize), toolBytes/read, read,
		(toolBytes/read)*int64(blockstore.ExtentSize))
}
