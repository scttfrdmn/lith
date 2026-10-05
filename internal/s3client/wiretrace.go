// SPDX-License-Identifier: Apache-2.0

package s3client

import (
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
type WireTraceFunc func(wire, roundTrip time.Duration)

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
	// firstByte is written on the transport's goroutine and read on this one after
	// RoundTrip returns, which is a happens-before edge only because RoundTrip has
	// returned; the atomic makes that explicit rather than relying on it.
	var firstByte atomic.Int64
	trace := &httptrace.ClientTrace{
		GotFirstResponseByte: func() { firstByte.Store(time.Now().UnixNano()) },
	}
	start := time.Now()
	resp, err := w.next.RoundTrip(req.WithContext(httptrace.WithClientTrace(req.Context(), trace)))
	rt := time.Since(start)
	if err != nil {
		return resp, err
	}
	// No first-byte callback means the response came from somewhere other than the wire
	// (a cached or synthetic response in a test). Reporting the round trip as the wire time
	// would understate nothing and overstate nothing, but it would be a different quantity,
	// so skip it instead.
	if ns := firstByte.Load(); ns > 0 {
		w.fn(time.Duration(ns-start.UnixNano()), rt)
	}
	return resp, err
}
