// SPDX-License-Identifier: Apache-2.0

package main

import (
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// TestSelectBandwidth exercises the pure selection logic against a fake
// DescribeInstanceTypes NetworkInfo (a burst-credit class where baseline < peak,
// and a multi-card sum).
func TestSelectBandwidth(t *testing.T) {
	// c8g.large-shaped: baseline well below peak.
	ni := &ec2types.NetworkInfo{
		NetworkCards: []ec2types.NetworkCardInfo{
			{BaselineBandwidthInGbps: aws.Float64(0.781), PeakBandwidthInGbps: aws.Float64(12.5)},
		},
	}
	base, peak := selectBandwidth(ni)
	if base != 0.781 || peak != 12.5 {
		t.Fatalf("single card = (%.3f, %.1f), want (0.781, 12.5)", base, peak)
	}

	// Two cards sum.
	ni2 := &ec2types.NetworkInfo{
		NetworkCards: []ec2types.NetworkCardInfo{
			{BaselineBandwidthInGbps: aws.Float64(25), PeakBandwidthInGbps: aws.Float64(50)},
			{BaselineBandwidthInGbps: aws.Float64(25), PeakBandwidthInGbps: aws.Float64(50)},
		},
	}
	if b, p := selectBandwidth(ni2); b != 50 || p != 100 {
		t.Fatalf("two cards = (%.0f, %.0f), want (50, 100)", b, p)
	}

	// Nil and empty are safe.
	if b, p := selectBandwidth(nil); b != 0 || p != 0 {
		t.Fatalf("nil = (%.0f, %.0f), want (0, 0)", b, p)
	}
	// A card missing baseline contributes 0 baseline but its peak.
	ni3 := &ec2types.NetworkInfo{NetworkCards: []ec2types.NetworkCardInfo{
		{PeakBandwidthInGbps: aws.Float64(10)},
	}}
	if b, p := selectBandwidth(ni3); b != 0 || p != 10 {
		t.Fatalf("missing baseline = (%.0f, %.0f), want (0, 10)", b, p)
	}
}

// TestNICCacheRoundTrip verifies nic.json write/read and that a zero-baseline
// (or absent) cache is rejected so a bad entry can't pin a wrong budget.
func TestNICCacheRoundTrip(t *testing.T) {
	dir := t.TempDir()
	if _, ok := readNICCache(dir); ok {
		t.Fatal("readNICCache found a file in an empty dir")
	}
	want := nicInfo{InstanceType: "c8g.2xlarge", BaselineGbps: 3.125, PeakGbps: 15, Source: "DescribeInstanceTypes"}
	writeNICCache(dir, want)
	got, ok := readNICCache(dir)
	if !ok {
		t.Fatal("readNICCache failed after write")
	}
	if got != want {
		t.Fatalf("round-trip = %+v, want %+v", got, want)
	}
	// A zero-baseline cache is treated as absent.
	writeNICCache(dir, nicInfo{InstanceType: "x", BaselineGbps: 0, PeakGbps: 5})
	if _, ok := readNICCache(dir); ok {
		t.Fatal("readNICCache accepted a zero-baseline entry")
	}
}

// TestComputeInflightFromBaseline: the in-flight budget is sized from baseline,
// and an unknown NIC (0) falls back to the fixed constant.
func TestComputeInflightFromBaseline(t *testing.T) {
	// 2 × 0.781 Gbps × 100 ms = 2 × 0.781e9/8 × 0.1 ≈ 19.5 MB — far under the
	// 512 MB fallback, so a burst-credit box no longer over-commits.
	n, desc := computeInflightBytes("", 0.781)
	if n >= defaultInflightFallback {
		t.Fatalf("baseline-sized budget %d (%s) should be well under the %d fallback", n, desc, defaultInflightFallback)
	}
	if got, _ := computeInflightBytes("", 0); got != defaultInflightFallback {
		t.Fatalf("unknown NIC budget = %d, want fallback %d", got, defaultInflightFallback)
	}
	// An explicit flag wins over any baseline.
	if got, _ := computeInflightBytes("256MiB", 0.781); got != 256<<20 {
		t.Fatalf("--inflight-bytes flag = %d, want %d", got, 256<<20)
	}
}
