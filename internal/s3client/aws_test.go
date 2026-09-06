// SPDX-License-Identifier: Apache-2.0

package s3client

import "testing"

func TestBuildTransportTuning(t *testing.T) {
	tr := buildTransport(128)
	if tr.ForceAttemptHTTP2 {
		t.Error("expected HTTP/1.1 (ForceAttemptHTTP2 false)")
	}
	if tr.MaxIdleConnsPerHost < 128 {
		t.Errorf("MaxIdleConnsPerHost = %d, want >= concurrency (128)", tr.MaxIdleConnsPerHost)
	}
	if tr.IdleConnTimeout == 0 {
		t.Error("expected a non-zero IdleConnTimeout (keep-alives)")
	}
	if tr.DialContext == nil {
		t.Error("expected a DialContext with keep-alive")
	}
}

func TestBuildTransportDefaultConcurrency(t *testing.T) {
	tr := buildTransport(0)
	if tr.MaxIdleConnsPerHost != 64 {
		t.Errorf("default MaxIdleConnsPerHost = %d, want 64", tr.MaxIdleConnsPerHost)
	}
}

func TestPayerHeader(t *testing.T) {
	if got := (&Client{}).payer(); got != "" {
		t.Errorf("default payer = %q, want empty", got)
	}
	if got := (&Client{requesterPays: true}).payer(); got == "" {
		t.Error("requester-pays client should set a request payer")
	}
}

func TestPtrOrNil(t *testing.T) {
	if ptrOrNil("") != nil {
		t.Error("empty string should map to nil pointer")
	}
	if p := ptrOrNil("x"); p == nil || *p != "x" {
		t.Error("non-empty string should map to a pointer to it")
	}
}
