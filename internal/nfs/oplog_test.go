// SPDX-License-Identifier: Apache-2.0

package nfs

import (
	"sync"
	"testing"
)

// countingMetrics records NFSOp calls for the logger test.
type countingMetrics struct {
	mu  sync.Mutex
	ops map[string]int
}

func newCountingMetrics() *countingMetrics { return &countingMetrics{ops: map[string]int{}} }

func (m *countingMetrics) NFSClients(int)     {}
func (m *countingMetrics) NFSSeqStates(int)   {}
func (m *countingMetrics) NFSReadBytes(int64) {}
func (m *countingMetrics) NFSOp(op string) {
	m.mu.Lock()
	m.ops[op]++
	m.mu.Unlock()
}
func (m *countingMetrics) get(op string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.ops[op]
}

// TestNFSProcFromTrace covers the exact go-nfs trace format
// (conn.go: `Log.Tracef("request: %v", w.req)`, request.String() ->
// "RPC #N (nfs.Proc)") plus the shapes that must NOT parse to a procedure.
func TestNFSProcFromTrace(t *testing.T) {
	cases := []struct{ in, want string }{
		{"request: RPC #12 (nfs.GetAttr)", "getattr"},
		{"request: RPC #13 (nfs.Lookup)", "lookup"},
		{"request: RPC #14 (nfs.Access)", "access"},
		{"request: RPC #15 (nfs.ReadDirPlus)", "readdirplus"},
		{"request: RPC #16 (nfs.ReadDir)", "readdir"},
		{"request: RPC #17 (nfs.FSStat)", "fsstat"},
		{"request: RPC #18 (nfs.Read)", "read"},
		// MOUNT-service and unknown-program requests render without "(nfs.…".
		{"request: RPC #19 (100005.1)", ""},
		{"request: RPC #20 (100227.0)", ""},
		{"No handler for 100227.0", ""},
		{"(nfs.)", ""}, // empty proc name must not parse
	}
	for _, c := range cases {
		if got := nfsProcFromTrace(c.in); got != c.want {
			t.Errorf("nfsProcFromTrace(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestOpLoggerCounts verifies Tracef increments the right op, and that a
// request-shaped line with no nfs procedure lands in "unparsed" so a future
// format change is visible rather than silent.
func TestOpLoggerCounts(t *testing.T) {
	m := newCountingMetrics()
	l := newOpLogger(nil, m)

	// go-nfs calls Tracef("request: %v", req); simulate with a Stringer arg.
	l.Tracef("request: %v", traceStr("RPC #1 (nfs.GetAttr)"))
	l.Tracef("request: %v", traceStr("RPC #2 (nfs.ReadDirPlus)"))
	l.Tracef("request: %v", traceStr("RPC #3 (nfs.GetAttr)"))
	l.Tracef("request: %v", traceStr("RPC #4 (100005.1)")) // MOUNT service -> unparsed

	if got := m.get("getattr"); got != 2 {
		t.Errorf("getattr = %d, want 2", got)
	}
	if got := m.get("readdirplus"); got != 1 {
		t.Errorf("readdirplus = %d, want 1", got)
	}
	if got := m.get("unparsed"); got != 1 {
		t.Errorf("unparsed = %d, want 1", got)
	}
}

type traceStr string

func (s traceStr) String() string { return string(s) }
