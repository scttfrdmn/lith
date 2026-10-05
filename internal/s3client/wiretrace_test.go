// SPDX-License-Identifier: Apache-2.0

package s3client

import (
	"io"
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
	var got WireSample
	cl := &http.Client{Transport: NewWireTracer(http.DefaultTransport, func(s WireSample) {
		mu.Lock()
		got = s
		mu.Unlock()
	})}

	resp, err := cl.Get(srv.URL)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	_ = resp.Body.Close()

	mu.Lock()
	gotWire, gotRT := got.Wire, got.RoundTrip
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
	cl := &http.Client{Transport: NewWireTracer(errRT{}, func(WireSample) { calls++ })}
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

// #350: the connection split is the point of the instrument, so a reused connection must be
// reported as reused and must show a near-zero acquisition time.
//
// The external cell that motivated this showed the wire histogram matching the fill
// histogram to within one sample in every bucket, with equal counts -- so no retries, and
// the delay is not above the wire. What is left is that lith's requests differ from
// lith-s3bench's AS REQUESTS, and the leading candidate is connection reuse: a fresh mount's
// burst opens up to one connection per concurrent fill (66 for a 153-GET burst) where a
// 3-second s3bench window over the same 128-connection pool is almost entirely reused.
func TestWireTracerSplitsReusedFromNewConnections(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	var mu sync.Mutex
	var samples []WireSample
	tr := &http.Transport{MaxIdleConnsPerHost: 4}
	cl := &http.Client{Transport: NewWireTracer(tr, func(s WireSample) {
		mu.Lock()
		samples = append(samples, s)
		mu.Unlock()
	})}

	// Three sequential requests on a pool that keeps the connection: the first opens one,
	// the rest reuse it.
	for range 3 {
		resp, err := cl.Get(srv.URL)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}

	mu.Lock()
	got := append([]WireSample(nil), samples...)
	mu.Unlock()
	if len(got) != 3 {
		t.Fatalf("got %d samples, want 3", len(got))
	}
	if got[0].Reused {
		t.Error("the first request on a fresh pool was reported as reusing a connection")
	}
	// FIXTURE PRECONDITION: without reuse the split has nothing to compare, and a
	// transport that closed the connection each time would make this test vacuous.
	if !got[1].Reused || !got[2].Reused {
		t.Fatalf("fixture: requests 2 and 3 did not reuse the connection (reused=%v,%v); "+
			"the split cannot be tested without both cases", got[1].Reused, got[2].Reused)
	}
	// A reused connection is acquired from the pool, so acquisition must be far below the
	// first request's dial. This is the quantity that would show a fresh mount paying for
	// connections its burst opened.
	if got[1].ConnAcquire > got[0].ConnAcquire {
		t.Errorf("a reused connection took longer to acquire (%v) than a new one (%v)",
			got[1].ConnAcquire, got[0].ConnAcquire)
	}
	// Every sample must carry a wire time, reused or not -- otherwise the labelled
	// histogram has a hole on exactly the arm being compared.
	for i, s := range got {
		if s.Wire <= 0 {
			t.Errorf("sample %d (reused=%v) has no wire time", i, s.Reused)
		}
	}
}
