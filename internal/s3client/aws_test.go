// SPDX-License-Identifier: Apache-2.0

package s3client

import (
	"context"
	"strings"
	"testing"
)

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

// TestNewRejectsNonHTTPSSignedEndpoint covers M3: signed requests must not be
// sent to a non-HTTPS endpoint (SSRF + cleartext credential exposure), while
// anonymous mode may still use http.
func TestNewRejectsNonHTTPSSignedEndpoint(t *testing.T) {
	cases := []struct {
		name          string
		endpoint      string
		noSign        bool
		wantSchemeErr bool
	}{
		{"http signed", "http://attacker.host", false, true},
		{"no scheme signed", "attacker.host", false, true},
		{"https signed", "https://s3.example.com", false, false},
		{"http anonymous", "http://public.example.com", true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := New(context.Background(), Config{
				Bucket:        "b",
				Region:        "us-east-1", // avoid network region resolution
				Endpoint:      tc.endpoint,
				NoSignRequest: tc.noSign,
			})
			if tc.wantSchemeErr {
				if err == nil {
					t.Fatalf("New(%q, noSign=%v): want scheme error, got nil", tc.endpoint, tc.noSign)
				}
				if !strings.Contains(err.Error(), "non-HTTPS scheme") {
					t.Fatalf("New(%q): error %q does not name the non-HTTPS cause", tc.endpoint, err)
				}
				return
			}
			// The scheme guard must not fire; New itself may still succeed since
			// an endpoint override skips network region resolution.
			if err != nil && strings.Contains(err.Error(), "non-HTTPS scheme") {
				t.Fatalf("New(%q, noSign=%v): unexpected scheme rejection: %v", tc.endpoint, tc.noSign, err)
			}
		})
	}
}

// TestNewRejectsNonHTTPSSignedEnvEndpoint covers M3 via the SDK endpoint
// sources (AWS_ENDPOINT_URL / shared-config endpoint_url): the guard must
// validate the effective endpoint the SDK resolves into awsCfg.BaseEndpoint,
// not only the --endpoint flag. Region is set so no network resolution occurs.
func TestNewRejectsNonHTTPSSignedEnvEndpoint(t *testing.T) {
	cases := []struct {
		name          string
		envEndpoint   string
		noSign        bool
		wantSchemeErr bool
	}{
		{"env http signed", "http://attacker.host", false, true},
		{"env https signed", "https://s3.example.com", false, false},
		{"env http anonymous", "http://public.example.com", true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("AWS_ENDPOINT_URL", tc.envEndpoint)
			_, err := New(context.Background(), Config{
				Bucket:        "b",
				Region:        "us-east-1", // avoid network region resolution
				Endpoint:      "",          // no flag: exercise the env/shared-config path
				NoSignRequest: tc.noSign,
			})
			if tc.wantSchemeErr {
				if err == nil {
					t.Fatalf("New(env=%q, noSign=%v): want scheme error, got nil", tc.envEndpoint, tc.noSign)
				}
				if !strings.Contains(err.Error(), "non-HTTPS scheme") {
					t.Fatalf("New(env=%q): error %q does not name the non-HTTPS cause", tc.envEndpoint, err)
				}
				return
			}
			if err != nil && strings.Contains(err.Error(), "non-HTTPS scheme") {
				t.Fatalf("New(env=%q, noSign=%v): unexpected scheme rejection: %v", tc.envEndpoint, tc.noSign, err)
			}
		})
	}
}

// TestValidateContentRange covers L2: a ranged GET must return partial content
// starting at the requested offset. This validates the SDK Content-Range at
// the response layer; the fake bypasses the SDK and cannot exercise it.
func TestValidateContentRange(t *testing.T) {
	cases := []struct {
		name    string
		cr      string
		off     int64
		wantErr bool
	}{
		{"exact start zero", "bytes 0-99/1000", 0, false},
		{"exact start offset", "bytes 500-599/1000", 500, false},
		{"unknown total", "bytes 500-599/*", 500, false},
		{"empty (whole object, 200)", "", 500, true},
		{"wrong start", "bytes 0-99/1000", 500, true},
		{"missing bytes prefix", "500-599/1000", 500, true},
		{"garbage", "not-a-range", 500, true},
		{"non-numeric start", "bytes x-99/1000", 500, true},
		{"no dash", "bytes 500", 500, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateContentRange(tc.cr, tc.off)
			if tc.wantErr != (err != nil) {
				t.Fatalf("validateContentRange(%q, %d) err = %v, wantErr = %v", tc.cr, tc.off, err, tc.wantErr)
			}
		})
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
