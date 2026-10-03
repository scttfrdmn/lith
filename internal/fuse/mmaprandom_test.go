// SPDX-License-Identifier: Apache-2.0

package fuse

import (
	"fmt"
	"math/rand"
	"os"
	"sort"
	"testing"

	"github.com/scttfrdmn/lith/internal/blockstore"
	"github.com/scttfrdmn/lith/internal/prefetch"
)

// #232: characterize the mmap random-access stall OFFLINE.
//
// Reported: a program that mmaps an 892 MB object on a lith mount and touches ~3000 random
// 4 KiB pages took 326 s and pulled 676 MB. The issue says characterize first and declines to
// guess, which is right -- but it assumed characterization needs a mount, and most of it does
// not.
//
// THE ARITHMETIC THAT NARROWS IT. 676 MB is almost exactly the coupon-collector count of
// distinct 1 MiB chunks for 3000 random faults spread over ~685 MiB
// (685 x (1 - e^(-3000/685)) = 676). So lith fetched a WHOLE 1 MiB CHUNK per distinct chunk
// touched, and the 64 KiB byte-exact extent lane (#118/#210) did not engage for most reads.
// Against the 11.7 MiB the program actually wanted, that is 58x.
//
// WHICH IS SURPRISING, because the lane exists and TestDemandByteExactOnRandomHandle proves it
// works. Its gate (fs.go, in Read) requires `h.pf.state() == prefetch.Random` EXACTLY -- and a
// handle with no pattern is not always Random. The contiguous-but-low-coverage branch
// (prefetch.go, the one #319 added coverageHeld to) sets a handle to COLD, and a Cold handle
// buys a whole 1 MiB chunk for a 4 KiB read.
//
// So: does a sustained random walk stay Random, or does it oscillate into Cold and pay whole
// chunks? TestDemandByteExactOnRandomHandle measures ONE read after reaching Random; #232 is
// three thousand, and whether the posture holds across them is the entire question.
//
// WHAT THIS CANNOT SETTLE, and what the issue still needs a Linux/FUSE box for: per-fault
// latency and kernel fault serialization. 326 s over 3000 faults is 109 ms each, and nothing
// here measures time.
func TestMmapRandomWalkFetchGranularity(t *testing.T) {
	// 64 MiB, because newByteExactFS allocates the object in RAM via bytes.Repeat and CI has
	// been OOM-killed on this package twice (exit 143) by a 512 MB fixture. The faults-per-
	// chunk ratio is what matters for the coupon-collector shape, and it is preserved: the
	// report is ~3000 faults over ~685 chunks, so ~4.4 per chunk.
	objMiB := int64(64)
	if os.Getenv("LITH_SWEEP_FULL") != "" {
		objMiB = 256
	}
	objBytes := objMiB << 20
	reads := int(objMiB * 44 / 10)

	type arm = struct {
		name     string
		readSize int64
	}
	// One variable apart. A 4 KiB page fault is what the issue describes; 128 KiB is what the
	// kernel actually hands FUSE, which #316 established independently by measuring the holes
	// a shared page cache punches (every one a multiple of 131072).
	arms := []arm{
		{"fault-4KiB", 4 << 10},
		{"kreadahead-128KiB", 128 << 10},
	}

	type result struct {
		arm        string
		s3Bytes    int64
		useful     int64
		wholeChunk int
		byteExact  int
		noFetch    int
		states     map[prefetch.State]int
		covHeld    int64
		lowCov     int64
	}
	var table []result

	for _, a := range arms {
		raw, srv := newByteExactFS(t, objBytes)
		node, fh := openBig(t, raw)
		h := raw.handleOf(fh)

		// Deterministic walk, so a failure is reproducible.
		rng := rand.New(rand.NewSource(42))
		res := result{arm: a.name, states: map[prefetch.State]int{}}
		before := quiesce(srv)

		for i := 0; i < reads; i++ {
			off := rng.Int63n(objBytes-a.readSize) & ^(a.readSize - 1)
			res.states[h.pf.state()]++
			b0 := srv.GetByteCount()
			readAt(t, raw, node, fh, off, a.readSize)
			// The read is synchronous for its own bytes; readahead a Random handle dispatches
			// is nothing, which is itself part of what this checks.
			switch d := srv.GetByteCount() - b0; {
			case d == 0:
				res.noFetch++
			case d >= blockstore.ChunkSize:
				res.wholeChunk++
			default:
				res.byteExact++
			}
			res.useful += a.readSize
		}
		res.s3Bytes = quiesce(srv) - before
		res.covHeld, res.lowCov = h.pf.pf.CoverageHeld(), h.pf.pf.LowCoverage()
		table = append(table, res)
	}

	t.Logf("object %d MiB, %d reads per arm (~%.1f per 1 MiB chunk)",
		objMiB, reads, float64(reads)/float64(objMiB))
	t.Logf("%-20s %12s %10s %8s %11s %10s %9s %9s %s",
		"arm", "s3 bytes", "useful", "amp", "wholeChunk", "byteExact", "noFetch", "covHeld", "states")
	for _, r := range table {
		t.Logf("%-20s %12d %10d %7.1fx %11d %10d %9d %9d %s",
			r.arm, r.s3Bytes, r.useful, float64(r.s3Bytes)/float64(r.useful),
			r.wholeChunk, r.byteExact, r.noFetch, r.covHeld, stateHist(r.states))
	}

	for _, r := range table {
		// NOT VACUOUS: the walk must actually have driven the detector off Sequential, or this
		// measures nothing. Two tests this week passed while proving nothing -- one skipped
		// silently, one landed in Strided -- so the fixture states its own precondition.
		if r.states[prefetch.Random] < reads/2 {
			t.Fatalf("%s: only %d of %d reads saw Random (%s) — this fixture is not a "+
				"sustained random walk and measures nothing",
				r.arm, r.states[prefetch.Random], reads, stateHist(r.states))
		}
		if r.wholeChunk+r.byteExact == 0 {
			t.Fatalf("%s: no read fetched anything; the fixture is serving everything from "+
				"cache", r.arm)
		}

		// FINDING 1: the posture HOLDS. I predicted the walk would oscillate into Cold --
		// where the lane's `state() == Random` gate fails and a 4 KiB read buys a whole 1 MiB
		// chunk -- and it does not. Pinned so a change to the coverage gate cannot
		// reintroduce it silently, because that is the one way lith's own granularity could
		// become #232's cause.
		if r.wholeChunk > r.byteExact {
			t.Errorf("%s: %d reads fetched a WHOLE CHUNK against %d served byte-exact — the "+
				"#210 extent lane stopped holding across a sustained random walk (states: %s, "+
				"coverage_held=%d)", r.arm, r.wholeChunk, r.byteExact, stateHist(r.states), r.covHeld)
		}

		// FINDING 2: lith is FAITHFUL to the read it is given, to within extent rounding. Its
		// floor is one 64 KiB extent per read, so the per-read cost must not exceed the read
		// size rounded up to extents, plus one for a straddle. This is the bound that says the
		// over-fetch is NOT lith choosing to fetch more than it was asked for.
		fetches := int64(r.wholeChunk + r.byteExact)
		if fetches > 0 {
			perFetch := r.s3Bytes / fetches
			maxPerFetch := roundUpExtents(readSizeOf(r.arm, arms)) + int64(blockstore.ExtentSize)
			if perFetch > maxPerFetch {
				t.Errorf("%s: %d B per fetch against a %d B bound (the read rounded up to "+
					"extents, plus one for a straddle) — lith is fetching more than it was "+
					"asked for", r.arm, perFetch, maxPerFetch)
			}
		}
	}

	// THE CHAIN, logged rather than asserted, because the kernel's half is not measurable
	// here. #232's 55x against program intent decomposes as
	//
	//	(what the kernel asks for / what the program touched) x (extent rounding)
	//
	// and the second factor is all lith controls. At a 4 KiB read that factor is 16x and it is
	// the whole of the amplification; at the kernel's real ~128-220 KiB read it is ~1x. So on
	// the reported run -- 220 KiB fetched per 4 KiB fault -- roughly 3.4x was extent rounding
	// and the remaining ~16x was the kernel asking for 220 KiB to satisfy a 4 KiB touch.
	for _, r := range table {
		rs := readSizeOf(r.arm, arms)
		t.Logf("%-20s read=%6d B  bytes/read=%7d B  amp-vs-read=%4.2fx  "+
			"amp-vs-a-4KiB-touch=%5.1fx",
			r.arm, rs, r.s3Bytes/int64(reads),
			float64(r.s3Bytes)/float64(r.useful),
			float64(r.s3Bytes)/float64(int64(reads)*4096))
	}
}

// roundUpExtents is the smallest whole number of 64 KiB extents covering n bytes.
func roundUpExtents(n int64) int64 {
	e := int64(blockstore.ExtentSize)
	return ((n + e - 1) / e) * e
}

func readSizeOf(name string, arms []struct {
	name     string
	readSize int64
}) int64 {
	for _, a := range arms {
		if a.name == name {
			return a.readSize
		}
	}
	return 0
}

func stateHist(m map[prefetch.State]int) string {
	keys := make([]int, 0, len(m))
	for k := range m {
		keys = append(keys, int(k))
	}
	sort.Ints(keys)
	s := ""
	for _, k := range keys {
		if s != "" {
			s += " "
		}
		s += fmt.Sprintf("%v=%d", prefetch.State(k), m[prefetch.State(k)])
	}
	return s
}
