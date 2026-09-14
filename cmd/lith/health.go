// SPDX-License-Identifier: Apache-2.0

package main

import (
	"net/http"
	"sync"
)

// readiness is the state behind /readyz: not ready (with a one-line reason) until
// the index is loaded and the mount/gateway is serving. Guarded for concurrent
// reads by the metrics server and the one write at the serving point.
type readiness struct {
	mu     sync.RWMutex
	ready  bool
	reason string
}

func newReadiness(reason string) *readiness { return &readiness{reason: reason} }

func (r *readiness) set(ok bool, reason string) {
	r.mu.Lock()
	r.ready, r.reason = ok, reason
	r.mu.Unlock()
}

func (r *readiness) status() (bool, string) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.ready, r.reason
}

// addHealth wires /healthz (liveness — 200 once the process serves HTTP) and
// /readyz (readiness — 200 when serving, else 503 with a one-line reason) onto
// mux. Both are cheap and make no S3 calls, so a container/systemd probe never
// touches the network or the block store.
func addHealth(mux *http.ServeMux, ready *readiness) {
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		if ok, reason := ready.status(); ok {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("ready\n"))
		} else {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(reason + "\n"))
		}
	})
}
