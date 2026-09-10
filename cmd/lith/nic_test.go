// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
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

// TestWriteNICCachePerUID0600 verifies the cache is written to a per-uid file
// with 0600 perms (F2 hardening).
func TestWriteNICCachePerUID0600(t *testing.T) {
	dir := t.TempDir()
	writeNICCache(dir, nicInfo{InstanceType: "c8g.large", BaselineGbps: 0.781, PeakGbps: 12.5, Source: "test"})
	path := nicCachePath(dir)
	if want := "nic-" + strconv.Itoa(os.Geteuid()) + ".json"; filepath.Base(path) != want {
		t.Fatalf("cache basename = %q, want %q", filepath.Base(path), want)
	}
	fi, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("cache not written: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("cache mode = %o, want 600", fi.Mode().Perm())
	}
}

// TestWriteNICCacheRefusesSymlink plants a symlink at the cache path (as an
// attacker in a shared /tmp would) and asserts the write neither follows nor
// overwrites the symlink target, and the planted link is not trusted on read
// (F2/F3).
func TestWriteNICCacheRefusesSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "victim")
	if err := os.WriteFile(target, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, nicCachePath(dir)); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	writeNICCache(dir, nicInfo{InstanceType: "attack", BaselineGbps: 9, PeakGbps: 9, Source: "attack"})
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "original" {
		t.Fatalf("victim overwritten through symlink: %q", got)
	}
	if _, ok := readNICCache(dir); ok {
		t.Fatal("readNICCache trusted a symlinked cache path")
	}
}

// TestReadNICCacheRejectsUntrusted asserts readNICCache rejects a symlink and a
// non-regular file (a directory), and accepts a valid owned regular file (F3).
func TestReadNICCacheRejectsUntrusted(t *testing.T) {
	// A directory at the cache path is not a regular file → rejected.
	dirD := t.TempDir()
	if err := os.Mkdir(nicCachePath(dirD), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, ok := readNICCache(dirD); ok {
		t.Fatal("readNICCache accepted a directory at the cache path")
	}

	// A symlink to an otherwise-valid cache is rejected without following it.
	dirS := t.TempDir()
	real := filepath.Join(dirS, "real.json")
	if err := os.WriteFile(real, []byte(fmt.Sprintf(`{"baseline_gbps":%f,"peak_gbps":%f}`, 5.0, 10.0)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, nicCachePath(dirS)); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	if _, ok := readNICCache(dirS); ok {
		t.Fatal("readNICCache trusted a symlink to a valid file")
	}

	// A valid owned regular file (written by writeNICCache) is accepted.
	dirV := t.TempDir()
	writeNICCache(dirV, nicInfo{InstanceType: "c8g.large", BaselineGbps: 0.781, PeakGbps: 12.5, Source: "test"})
	if _, ok := readNICCache(dirV); !ok {
		t.Fatal("readNICCache rejected a valid owned regular file")
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
