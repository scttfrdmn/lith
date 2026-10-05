// SPDX-License-Identifier: Apache-2.0

package s3client

import (
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"sync"
	"testing"
	"time"
)

// #350: the wire tracer must report first-byte latency as the TRANSPORT sees it, so a slow
// endpoint can be told apart from a slow caller.
func TestWireTracerReportsFirstByteFromTheWire(t *testing.T) {
	const serverDelay = 60 * time.Millisecond
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Nothing is written for serverDelay, so the first response byte cannot arrive
		// before it. Then the body dribbles out, so the round trip is longer still.
		time.Sleep(serverDelay)
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		time.Sleep(40 * time.Millisecond)
		_, _ = w.Write([]byte("tail"))
	}))
	defer srv.Close()

	var mu sync.Mutex
	var wire, rt time.Duration
	cl := &http.Client{Transport: NewWireTracer(http.DefaultTransport, func(w, r time.Duration) {
		mu.Lock()
		wire, rt = w, r
		mu.Unlock()
	})}

	resp, err := cl.Get(srv.URL)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	_ = resp.Body.Close()

	mu.Lock()
	gotWire, gotRT := wire, rt
	mu.Unlock()

	if gotWire < serverDelay {
		t.Errorf("wire %v is below the server's %v delay; the first byte cannot have "+
			"arrived sooner than the server sent it", gotWire, serverDelay)
	}
	// The endpoint is the only thing slow here, so the wire time must be close to the
	// server's delay and NOT inflated by the body transfer that follows it. A generous
	// bound, because a loopback httptest server plus scheduling is not a precise clock --
	// the point is that it measures first byte and not completion.
	if gotWire > serverDelay+30*time.Millisecond {
		t.Errorf("wire %v against a %v first-byte delay: this is not a first-byte "+
			"measurement", gotWire, serverDelay)
	}
	// And the round trip must exceed it, or the two quantities are the same number and
	// the split buys nothing.
	if gotRT <= gotWire {
		t.Errorf("roundTrip %v <= wire %v: the split is not separating anything",
			gotRT, gotWire)
	}
}

// A nil callback is a pass-through: the wrapper can be installed unconditionally without
// attaching a trace to every request when nothing is listening.
func TestWireTracerIsFreeWithNoCallback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// If a ClientTrace were attached, the transport would have populated it; what we
		// can check from here is that the request still works and carries no trace.
		if httptraceAttached(r) {
			t.Error("a ClientTrace was attached despite a nil callback")
		}
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	cl := &http.Client{Transport: NewWireTracer(http.DefaultTransport, nil)}
	resp, err := cl.Get(srv.URL)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	_ = resp.Body.Close()
}

// An error response must not report a sample. A failed attempt has no meaningful first-byte
// latency, and counting one would drag the distribution the policy-adjacent gauges read.
func TestWireTracerDoesNotReportOnError(t *testing.T) {
	var calls int
	cl := &http.Client{Transport: NewWireTracer(errRT{}, func(time.Duration, time.Duration) {
		calls++
	})}
	if _, err := cl.Get("http://example.invalid/x"); err == nil {
		t.Fatal("expected an error")
	}
	if calls != 0 {
		t.Errorf("reported %d samples for a failed attempt, want 0", calls)
	}
}

type errRT struct{}

func (errRT) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, http.ErrNotSupported
}

// httptraceAttached reports whether the request carries a ClientTrace with the hook the
// tracer installs.
func httptraceAttached(r *http.Request) bool {
	t := httptrace.ContextClientTrace(r.Context())
	return t != nil && t.GotFirstResponseByte != nil
}
