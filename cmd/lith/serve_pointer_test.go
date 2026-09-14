// SPDX-License-Identifier: Apache-2.0

package main

import "testing"

// TestGatewayRefreshRefused: only a version CHANGE is refused on the gateway;
// resolving to the same version is a legitimate no-op (not a refusal).
func TestGatewayRefreshRefused(t *testing.T) {
	cases := []struct {
		served, resolved string
		wantRefused      bool
	}{
		{"v1", "v1", false}, // same version -> no-op success
		{"v1", "v2", true},  // version change -> refuse
		{"v1", "", false},   // unresolved -> not a refusal (error handled separately)
	}
	for _, c := range cases {
		if got := gatewayRefreshRefused(c.served, c.resolved); got != c.wantRefused {
			t.Errorf("gatewayRefreshRefused(%q,%q)=%v, want %v", c.served, c.resolved, got, c.wantRefused)
		}
	}
}

// TestPointerRootID: stable for a version (a restart reproduces handles) and
// different across versions (adopting a new version is a clean STALE).
func TestPointerRootID(t *testing.T) {
	a := pointerRootID("b", "ds", "v1")
	if a != pointerRootID("b", "ds", "v1") {
		t.Error("root id not stable for the same version")
	}
	if a == pointerRootID("b", "ds", "v2") {
		t.Error("root id collides across versions (a swap would silently reinterpret handles)")
	}
	if a == pointerRootID("b", "other", "v1") {
		t.Error("root id collides across datasets")
	}
}
