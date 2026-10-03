// SPDX-License-Identifier: Apache-2.0

package fuse

import (
	"testing"

	"github.com/scttfrdmn/lith/internal/blockstore"
)

// #232: the FUSE transport constants must match the go-fuse defaults lith relies on, because
// the mount logs them and nothing reads them back from the server.
//
// go-fuse exposes only the kernel's InitIn; MaxWrite and MaxBackground are lith's side of the
// negotiation and are never returned. So these are a transcription, and a transcription that
// drifts is worse than no log line at all -- it would report a read size the kernel is not
// using. If a go-fuse upgrade changes either default, or lith starts setting them explicitly,
// this is the test that should fail.
func TestNegotiatedTransportMatchesWhatTheMountReliesOn(t *testing.T) {
	// go-fuse's defaultMaxWrite, and the value it also passes as max_read.
	if got := NegotiatedMaxWrite(); got != 128<<10 {
		t.Errorf("NegotiatedMaxWrite = %d, want %d (go-fuse defaultMaxWrite)", got, 128<<10)
	}
	// go-fuse's _DEFAULT_BACKGROUND_TASKS.
	if got := NegotiatedMaxBackground(); got != 12 {
		t.Errorf("NegotiatedMaxBackground = %d, want 12 (go-fuse _DEFAULT_BACKGROUND_TASKS)", got)
	}

	// THE CONSEQUENCE the log line exists to surface: a 1 MiB chunk is delivered to the
	// application in eight FUSE round trips, seven of which are cache hits returning a
	// sub-slice. That is the number a reader of the log is meant to notice.
	if got := int64(blockstore.ChunkSize) / NegotiatedMaxWrite(); got != 8 {
		t.Errorf("reads per chunk = %d, want 8 — ChunkSize or the negotiated read size moved, "+
			"and the mount log's reads_per_chunk is now wrong", got)
	}
	// Each read must still land within one chunk, which is what keeps the zero-copy
	// single-chunk path in Read reachable. A read size above ChunkSize would straddle.
	if NegotiatedMaxWrite() > int64(blockstore.ChunkSize) {
		t.Errorf("negotiated read size %d exceeds ChunkSize %d: reads would straddle chunks "+
			"and lose the zero-copy path", NegotiatedMaxWrite(), blockstore.ChunkSize)
	}
}
