// SPDX-License-Identifier: Apache-2.0

package fuse

import (
	"testing"
	"time"

	"github.com/scttfrdmn/lith/internal/blockstore"
	"github.com/scttfrdmn/lith/internal/prefetch"
)

// #312's instrument: the idle distribution over the handles in the readahead divisor.
//
// WHY lith_streaming_handles COULD NOT ANSWER THIS, which is the mistake this test exists to
// make unrepeatable: it counts an established stream whether it read a microsecond ago or a
// minute ago. Its gap against lith_open_handles answers "is #311's divisor input working" and
// says nothing about idleness, and both pre-registered rows offered on that issue were really
// about #311.
func TestStreamIdleBucketsMeasureTheDivisorsPopulation(t *testing.T) {
	const objBytes = int64(64) << 20
	raw, srv := newColdFS(t, objBytes)
	node, fh := openBig(t, raw)
	h := raw.handleOf(fh)

	// Before any read: nothing established, so there is nothing to be idle. A handle that has
	// never been read must NOT be counted, or the denominator includes descriptors #311
	// already excluded from the divisor and the instrument re-creates the confusion it exists
	// to remove.
	if _, _, _, n := raw.streamIdleBuckets(time.Now()); n != 0 {
		t.Fatalf("%d streams measured before any read; an unread handle is not in the divisor", n)
	}

	// Establish a stream: sequential 128 KiB reads until the detector says Sequential.
	const readLen = int64(128) << 10
	for off := int64(0); off < objBytes && !h.pf.isStreaming(); off += readLen {
		readAt(t, raw, node, fh, off, readLen)
	}
	quiesce(srv)
	if !h.pf.isStreaming() {
		t.Fatal("the fixture never established a stream, so this measures nothing")
	}

	now := time.Now()
	ge1, ge5, ge30, streams := raw.streamIdleBuckets(now)
	if streams != 1 {
		t.Fatalf("measured %d streams, want 1", streams)
	}
	// Just read, so it is in no idle bucket.
	if ge1 != 0 || ge5 != 0 || ge30 != 0 {
		t.Errorf("a stream read microseconds ago lands in buckets (1s=%d 5s=%d 30s=%d)",
			ge1, ge5, ge30)
	}

	// THE BUCKETS ARE CUMULATIVE, asserted by moving the clock rather than by sleeping: a
	// test that slept 30 s would be the slowest in the suite and would still only check one
	// row. streamIdleBuckets takes `now` for exactly this reason.
	for _, tc := range []struct {
		idle        time.Duration
		w1, w5, w30 int64
	}{
		{500 * time.Millisecond, 0, 0, 0},
		{2 * time.Second, 1, 0, 0},
		{10 * time.Second, 1, 1, 0},
		{60 * time.Second, 1, 1, 1},
	} {
		ge1, ge5, ge30, n := raw.streamIdleBuckets(now.Add(tc.idle))
		if n != 1 {
			t.Errorf("idle %v: measured %d streams, want 1", tc.idle, n)
		}
		if ge1 != tc.w1 || ge5 != tc.w5 || ge30 != tc.w30 {
			t.Errorf("idle %v: buckets (1s=%d 5s=%d 30s=%d), want (%d %d %d) — the buckets "+
				"must be CUMULATIVE, so a 60 s stream counts in all three",
				tc.idle, ge1, ge5, ge30, tc.w1, tc.w5, tc.w30)
		}
	}

	// And the denominator can never be smaller than a bucket it contains. That pair is the
	// thing a single-walk collector exists to guarantee; asserting it here means a future
	// change back to per-gauge walks has something to fail.
	for _, d := range []time.Duration{0, time.Second, 7 * time.Second, time.Minute} {
		ge1, ge5, ge30, n := raw.streamIdleBuckets(now.Add(d))
		if ge1 > n || ge5 > n || ge30 > n {
			t.Errorf("idle %v: a bucket exceeds the denominator (1s=%d 5s=%d 30s=%d, n=%d)",
				d, ge1, ge5, ge30, n)
		}
		if ge30 > ge5 || ge5 > ge1 {
			t.Errorf("idle %v: buckets are not monotone (1s=%d 5s=%d 30s=%d)", d, ge1, ge5, ge30)
		}
	}
}

// A handle that is open and read but NOT an established stream is not in the divisor, so it
// must not appear in the distribution — otherwise the instrument counts the population #311
// already removed and reproduces exactly the ambiguity it was built to resolve.
func TestStreamIdleIgnoresHandlesNotInTheDivisor(t *testing.T) {
	const objBytes = int64(64) << 20
	raw, srv := newByteExactFS(t, objBytes)
	node, fh := openBig(t, raw)
	h := raw.handleOf(fh)

	// Scattered small reads at a large byte gap: the coverage and byte-gap gates hold this
	// handle out of Sequential, so it is read but never established.
	blk := int64(blockstore.ChunkSize)
	for i := int64(0); i < 10; i++ {
		readAt(t, raw, node, fh, i*17*blk+4096, 4<<10)
	}
	quiesce(srv)

	if h.pf.state() == prefetch.Sequential {
		t.Skip("this fixture established a stream; it cannot test the not-in-divisor case")
	}
	if h.pf.isStreaming() {
		t.Fatalf("state %v is not Sequential but isStreaming() is true", h.pf.state())
	}
	if ge1, ge5, ge30, n := raw.streamIdleBuckets(time.Now().Add(time.Minute)); n != 0 ||
		ge1 != 0 || ge5 != 0 || ge30 != 0 {
		t.Errorf("a read-but-unestablished handle appears in the distribution "+
			"(1s=%d 5s=%d 30s=%d, n=%d): it is not in perHandleWindow's divisor, so its "+
			"idleness costs nobody anything", ge1, ge5, ge30, n)
	}
}
