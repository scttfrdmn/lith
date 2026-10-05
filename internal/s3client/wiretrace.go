// SPDX-License-Identifier: Apache-2.0

package s3client

import (
	"crypto/tls"
	"net/http"
	"net/http/httptrace"
	"sync/atomic"
	"time"
)

// WireTraceFunc is told, per HTTP attempt, how long elapsed between the request being handed
// to the transport and the FIRST RESPONSE BYTE arriving on the wire, and how long the whole
// RoundTrip took.
//
// The difference between the second and the first is time spent inside the transport after
// the bytes arrived; the difference between an outer caller's own measurement and `wire` is
// everything above the transport -- SDK middleware, response deserialization, and the delay
// before the calling goroutine is rescheduled.
type WireTraceFunc func(WireSample)

// WireSample is one HTTP attempt as the transport saw it (lith#350).
type WireSample struct {
	// Wire is request start -> first response byte. It INCLUDES connection acquisition,
	// which is the point: an external cell showed lith's first bytes arriving as late at
	// the transport as in the fill path (the two histograms differed by at most one sample
	// in any bucket, with equal counts so no retries), while lith-s3bench on the same box
	// and endpoint at the same moment got ~30 ms. So the delay is not above the wire, and
	// the remaining difference between the two is how they use connections.
	Wire time.Duration
	// ConnAcquire is request start -> a connection in hand (httptrace GetConn -> GotConn).
	// For a reused connection this is near zero; for a new one it is dial + TLS.
	ConnAcquire time.Duration
	// Reused reports whether the connection came from the idle pool. A fresh mount's burst
	// opens up to one connection per concurrent fill -- 66 for a 153-GET burst -- so a
	// large share of its requests are first-use, where a 3 s 600-request s3bench window
	// over the same 128-connection pool is almost entirely reused. That asymmetry is the
	// leading explanation for the gap and this field is what tests it.
	Reused bool
	// TLS is the TLS handshake alone, zero when the connection was reused or plaintext.
	TLS time.Duration
	// RoundTrip is the whole RoundTrip call, for reference.
	RoundTrip time.Duration
}

// WireTracer wraps a RoundTripper to report first-byte latency AS THE TRANSPORT SEES IT
// (lith#350).
//
// WHY THIS EXISTS. An external differential probe put a mount and lith-s3bench against the
// same bucket, box, endpoint and part size at the same moment: s3bench's first bytes came in
// at ~30 ms (p50) while the same mount's fills were mostly over 50 ms, with the box at
// 20-34% CPU. Same SDK call, same tuned transport -- so the gap is above the wire and inside
// lith, and two mechanisms were already refuted by measurement (request concurrency, and
// bytes in flight). Guessing a third would be the fourth wrong answer on that issue.
//
// GotFirstResponseByte is the only seam that can split "the endpoint was slow" from "we were
// slow to notice". It fires on the transport's own goroutine the moment the first byte is
// read off the connection, so it is unaffected by whether the caller is scheduled.
//
// One trace per attempt, including retries -- a retried request reports each attempt
// separately, which is deliberate: a silent retry is exactly the kind of thing that would
// show up as one slow fill and nothing else.
type WireTracer struct {
	next http.RoundTripper
	fn   WireTraceFunc
}

// NewWireTracer wraps next. A nil fn makes it a pass-through, so the wrapper can be installed
// unconditionally without costing anything when nothing is listening.
func NewWireTracer(next http.RoundTripper, fn WireTraceFunc) *WireTracer {
	return &WireTracer{next: next, fn: fn}
}

func (w *WireTracer) RoundTrip(req *http.Request) (*http.Response, error) {
	if w.fn == nil {
		return w.next.RoundTrip(req)
	}
	// All written on the transport's goroutine(s) and read here only after RoundTrip has
	// returned. The atomics make that edge explicit rather than relying on it.
	var firstByte, gotConn, tlsStart, tlsDone atomic.Int64
	var reused atomic.Bool
	trace := &httptrace.ClientTrace{
		GotFirstResponseByte: func() { firstByte.Store(time.Now().UnixNano()) },
		GotConn: func(info httptrace.GotConnInfo) {
			gotConn.Store(time.Now().UnixNano())
			reused.Store(info.Reused)
		},
		TLSHandshakeStart: func() { tlsStart.Store(time.Now().UnixNano()) },
		TLSHandshakeDone:  func(tls.ConnectionState, error) { tlsDone.Store(time.Now().UnixNano()) },
	}
	start := time.Now()
	resp, err := w.next.RoundTrip(req.WithContext(httptrace.WithClientTrace(req.Context(), trace)))
	rt := time.Since(start)
	if err != nil {
		return resp, err
	}
	// No first-byte callback means the response did not come off a wire (a cached or
	// synthetic response in a test). Reporting the round trip in its place would be a
	// different quantity, so skip the sample instead.
	fb := firstByte.Load()
	if fb == 0 {
		return resp, err
	}
	s := WireSample{
		Wire:      time.Duration(fb - start.UnixNano()),
		Reused:    reused.Load(),
		RoundTrip: rt,
	}
	if gc := gotConn.Load(); gc > 0 {
		s.ConnAcquire = time.Duration(gc - start.UnixNano())
	}
	if ts, td := tlsStart.Load(), tlsDone.Load(); ts > 0 && td > ts {
		s.TLS = time.Duration(td - ts)
	}
	w.fn(s)
	return resp, err
}
