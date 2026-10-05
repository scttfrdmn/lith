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

// #313: a --prefetch-pressure-max above 1.0 cannot do what it is asked to do, and the mount
// must say so rather than letting the operator believe they are protected.
//
// It is arithmetic, not a measurement: a threshold of 1.3 admits 1.3 tiers' worth of unread
// bytes, so the tier fills and must evict one to take another. Measured on real S3 at 1.3 the
// gate fired 94-100 times and held peak pressure at exactly 1.300 as designed -- and still
// evicted 2051-2235 unread chunks. I picked 1.3 by fitting between a clean 1.22 observation
// and a collapsed 1.40 one, which is the mistake this warning exists to stop someone else
// repeating.
func TestPressureMaxWarning(t *testing.T) {
	for _, tc := range []struct {
		name string
		v    float64
		want bool
	}{
		{"off is silent", 0, false},
		{"negative is silent", -1, false},
		// The measured band.
		{"1.0 is usable", 1.0, false},
		{"0.85 is usable", 0.85, false},
		{"0.5 is the floor measured", 0.5, false},
		// Above 1.0 cannot work.
		{"1.3 warns", 1.3, true},
		{"just above 1 warns", 1.01, true},
		// Below the measured floor, warn about cost rather than correctness.
		{"0.2 warns about over-throttling", 0.2, true},
	} {
		got := pressureMaxWarning(tc.v)
		if (got != "") != tc.want {
			t.Errorf("%s: pressureMaxWarning(%v) = %q, want warning=%v",
				tc.name, tc.v, got, tc.want)
		}
	}

	// The above-1.0 warning must say it CANNOT work, not that it is merely suboptimal --
	// those are different claims and only one of them is true here.
	w := pressureMaxWarning(1.3)
	if !strings.Contains(w, "cannot work") {
		t.Errorf("the above-1.0 warning does not say it cannot work: %q", w)
	}
	// And it must carry the measured evidence, so an operator can tell this apart from a
	// style preference.
	for _, want := range []string{"0.85-1.0", "2100"} {
		if !strings.Contains(w, want) {
			t.Errorf("the warning omits %q: %q", want, w)
		}
	}
	// The below-band warning must NOT claim it cannot work, because 0.5 does work -- it
	// just costs wall clock. Conflating the two would make both less believable.
	if lo := pressureMaxWarning(0.2); strings.Contains(lo, "cannot work") {
		t.Errorf("the below-band warning claims it cannot work: %q", lo)
	}
}
