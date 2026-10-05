// SPDX-License-Identifier: Apache-2.0

package nfs

import (
	"bytes"
	"log/slog"
	"strings"
	"sync"
	"testing"
)

// #240, the gateway half: a prefix-scoped export that is silently short a key must say so.
//
// The FUSE path has logged this since the GCHP integration run; the gateway did not, and it
// reads the same index through its own code path — the gap that produced #343 and #346. On
// the reporting workload one missing date-pinned file surfaced 9 s into init as a Fortran
// "file not found" from deep in application code with nothing in the lith log, and cost a
// 20-minute hunt for a one-line cause.
func TestGatewayLogsALookupMissWithThePath(t *testing.T) {
	var buf bytes.Buffer
	var mu sync.Mutex
	h, _ := testGateway(t, map[string][]byte{"a/present.txt": []byte("hello")})
	h.fs.cfg.Logger = slog.New(slog.NewTextHandler(&lockedWriter{w: &buf, mu: &mu}, nil))

	read := func() string {
		mu.Lock()
		defer mu.Unlock()
		return buf.String()
	}

	// A path the export does not have. The miss must name it -- a bare "not found" is the
	// black box this issue is about.
	if _, err := h.fs.Stat("/a/absent.nc4"); err == nil {
		t.Fatal("stat of a missing path succeeded")
	}
	got := read()
	if !strings.Contains(got, "/a/absent.nc4") {
		t.Errorf("the miss log does not name the requested path: %q", got)
	}
	if !strings.Contains(got, "level=INFO") {
		t.Errorf("the miss was not logged at INFO: %q", got)
	}

	// A path that IS present must not log. Otherwise the breadcrumb is noise and an
	// operator cannot grep for it.
	before := len(read())
	if _, err := h.fs.Stat("/a/present.txt"); err != nil {
		t.Fatalf("stat of a present path failed: %v", err)
	}
	if len(read()) != before {
		t.Error("a successful lookup logged a miss")
	}

	// DEDUPED. Stat is called by GETATTR, LOOKUP and ACCESS alike, so a client probing one
	// missing path hits this repeatedly; without dedup a single absent file floods the log.
	for i := 0; i < 500; i++ {
		_, _ = h.fs.Stat("/a/absent.nc4")
	}
	if n := strings.Count(read(), "/a/absent.nc4"); n != 1 {
		t.Errorf("501 lookups of one missing path logged %d lines, want 1", n)
	}

	// Open must log too: a client that LOOKUPs through a cached handle and goes straight to
	// READ reaches OpenFile, not Stat.
	if _, err := h.fs.Open("/a/other-absent.bin"); err == nil {
		t.Fatal("open of a missing path succeeded")
	}
	if !strings.Contains(read(), "/a/other-absent.bin") {
		t.Error("a miss on the open path was not logged")
	}

	// Capped, so a probe-heavy client cannot grow the dedup map without bound.
	for i := range gatewayMissLogCap * 2 {
		_, _ = h.fs.Stat("/probe/" + string(rune('a'+i%26)) + string(rune('a'+(i/26)%26)) + string(rune('a'+(i/676)%26)))
	}
	h.fs.missMu.Lock()
	n := len(h.fs.missSeen)
	h.fs.missMu.Unlock()
	if n > gatewayMissLogCap {
		t.Errorf("dedup map grew to %d entries, past the %d cap", n, gatewayMissLogCap)
	}
}

// lockedWriter serializes writes so the -race detector has nothing to find when a test
// reads the buffer while a logger may still be writing to it.
type lockedWriter struct {
	w  *bytes.Buffer
	mu *sync.Mutex
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}
