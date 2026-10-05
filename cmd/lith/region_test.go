// SPDX-License-Identifier: Apache-2.0

package main

import (
	"strings"
	"testing"

	"github.com/scttfrdmn/lith/internal/s3client"
)

// #362: a cross-region mount must announce itself.
//
// The reported case cost two instances. The same TB-scale sequential read ran at 1.52 MiB/s
// cross-region against 107-146 MB/s in-region -- ~70-96x -- and the only symptom was
// slowness, which is indistinguishable from every other cause. lith knew both regions the
// whole time: the client resolves the bucket's region in order to sign at all.
func TestCrossRegionWarning(t *testing.T) {
	for _, tc := range []struct {
		name             string
		instance, bucket string
		want             bool
	}{
		{"mismatch is the whole point", "us-west-1", "us-west-2", true},
		{"same region is silent", "us-west-2", "us-west-2", false},
		// Silent when it cannot tell. A warning that fires on "I could not determine
		// this" is noise, and this has to stay greppable to be worth anything: IMDS is
		// blocked on plenty of hardened images, and a custom --endpoint has no AWS
		// region at all.
		{"unknown instance region is silent", "", "us-west-2", false},
		{"unknown bucket region is silent", "us-west-1", "", false},
		{"both unknown is silent", "", "", false},
	} {
		got := crossRegionWarning(tc.instance, tc.bucket, "mybucket")
		if (got != "") != tc.want {
			t.Errorf("%s: warning=%q, want fired=%v", tc.name, got, tc.want)
		}
		if !tc.want {
			continue
		}
		// It must name BOTH regions and the bucket. "cross-region" alone sends the
		// operator back to the CLI to find out which way round it is -- the warning
		// exists to end the investigation, not to start one.
		for _, want := range []string{tc.instance, tc.bucket, "mybucket"} {
			if !strings.Contains(got, want) {
				t.Errorf("%s: warning does not name %q: %q", tc.name, want, got)
			}
		}
		// And it must say what to do about it.
		if !strings.Contains(got, "--no-region-check") {
			t.Errorf("%s: warning does not say how to silence it: %q", tc.name, got)
		}
	}
}

// regionOf reads the region off a client that already resolved it, and returns "" for a
// client that carries none rather than panicking. The optional-method shape is what keeps
// every other s3client.API implementer (including the test fakes) unchanged.
func TestRegionOfIsOptional(t *testing.T) {
	if got := regionOf(noRegionClient{}); got != "" {
		t.Errorf("a client without Region() returned %q, want empty", got)
	}
	if got := regionOf(regionClient{region: "eu-central-1"}); got != "eu-central-1" {
		t.Errorf("Region() = %q, want eu-central-1", got)
	}
	// Whitespace from a metadata read must not produce a spurious mismatch: " us-west-2"
	// against "us-west-2" would warn on an in-region mount, which is the one outcome
	// that would make an operator stop trusting the warning.
	if got := regionOf(regionClient{region: "  us-west-2\n"}); got != "us-west-2" {
		t.Errorf("Region() = %q, want it trimmed", got)
	}
	if w := crossRegionWarning("us-west-2", regionOf(regionClient{region: " us-west-2 "}), "b"); w != "" {
		t.Errorf("whitespace produced a spurious cross-region warning: %q", w)
	}
}

type noRegionClient struct{ s3client.API }

type regionClient struct {
	s3client.API
	region string
}

func (c regionClient) Region() string { return c.region }

// #349: the evidence gate's input must be a function of configuration, and regionPair is
// where that is established. known=false must never read as "near".
func TestRegionPair(t *testing.T) {
	for _, tc := range []struct {
		name         string
		instance     string
		clientRegion string
		wantNear     bool
		wantKnown    bool
	}{
		{"same region", "us-east-1", "us-east-1", true, true},
		{"different region", "us-west-1", "us-west-2", false, true},
		// A client with no region at all -- a custom --endpoint, or a fake. Must come back
		// not-known, which the policy treats as not-near.
		{"no bucket region", "us-east-1", "", false, false},
	} {
		near, known := regionPairFrom(tc.instance, tc.clientRegion)
		if near != tc.wantNear || known != tc.wantKnown {
			t.Errorf("%s: near=%v known=%v, want near=%v known=%v",
				tc.name, near, known, tc.wantNear, tc.wantKnown)
		}
		// THE INVARIANT THAT MATTERS: not-known must never be near. A caller that forgot
		// to check the second return would otherwise engage the gate at distance, which is
		// the measured 1.96x.
		if !known && near {
			t.Errorf("%s: near=true with known=false", tc.name)
		}
	}
}
