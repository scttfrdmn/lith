// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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
	// Written WITH attempt reasons attached, to prove they do not survive (#317).
	withTried := want
	withTried.Tried = []nicAttempt{{"ethtool", "no link speed"}, {"cache", "no usable entry"}}
	writeNICCache(dir, withTried)
	got, ok := readNICCache(dir)
	if !ok {
		t.Fatal("readNICCache failed after write")
	}
	if got.InstanceType != want.InstanceType || got.BaselineGbps != want.BaselineGbps ||
		got.PeakGbps != want.PeakGbps || got.Source != want.Source {
		t.Fatalf("round-trip = %+v, want %+v", got, want)
	}
	// THE REASONS MUST NOT BE PERSISTED. They describe one resolution on one box at one
	// moment; a cached "DescribeInstanceTypes denied" would be replayed on every later run,
	// including runs that never called it and runs on a box where the permission exists. The
	// `json:"-"` tag is the whole guard and nothing else would notice it being dropped.
	if len(got.Tried) != 0 {
		t.Errorf("attempt reasons survived the cache: %+v — a stale denial would be reported "+
			"as if it had just happened", got.Tried)
	}
	if b, err := os.ReadFile(nicCachePath(dir)); err == nil && strings.Contains(string(b), "ethtool") {
		t.Errorf("the cache file contains an attempt reason: %s", b)
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

// TestBandwidthFromType: the IMDS-estimate size table maps the size suffix to a
// baseline (#237) — the path stock ParallelCluster nodes take when
// DescribeInstanceTypes is denied but IMDS gives the type.
func TestBandwidthFromType(t *testing.T) {
	cases := map[string]float64{
		"c7g.4xlarge":    7.5,
		"m9g.48xlarge":   50,
		"c8g.large":      0.9,
		"r7g.16xlarge":   30,
		"m8g.metal-24xl": 0, // unrecognized size suffix after the dot
	}
	for itype, want := range cases {
		g, ok := bandwidthFromType(itype)
		if want == 0 {
			if ok {
				t.Errorf("%s: expected no estimate, got %g", itype, g)
			}
			continue
		}
		if !ok || g != want {
			t.Errorf("%s: got (%g,%v), want %g", itype, g, ok, want)
		}
	}
	// No dot at all → no estimate (not an EC2-style type).
	if _, ok := bandwidthFromType("notatype"); ok {
		t.Error("typeless string should yield no estimate")
	}
}

// TestFallbackNeverZero: the #237 fix — detection failing must not propagate a
// literal 0 into the device-derived knobs. defaultFallbackGbps is a real,
// nonzero assumption, and at it the derived parts-max lands well above its
// 4 MiB floor (the whole regression was parts-max collapsing to that floor).
func TestFallbackNeverZero(t *testing.T) {
	if defaultFallbackGbps <= 0 {
		t.Fatal("defaultFallbackGbps must be a positive assumption, not 0")
	}
	partsMax := int64(defaultFallbackGbps * 1e9 / 8 * 0.04) // NIC × TTFB(40ms)
	if partsMax <= 4<<20 {
		t.Fatalf("fallback parts-max %d must clear the 4 MiB floor (the #237 bug)", partsMax)
	}
}

// THE REPORTED CASE: c8gn.48xlarge is rated 600 Gbps across two network cards, and the
// size-keyed table resolved it through "48xlarge" to 50 -- a confident number 12x low, logged
// at INFO with nothing warning about it (#317). The estimate must now decline.
func TestEstimateRefusesNetworkOptimizedFamilies(t *testing.T) {
	if g, ok := bandwidthFromType("c8gn.48xlarge"); ok {
		t.Errorf("c8gn.48xlarge estimated at %g Gbps; it is rated 600, so the size-keyed "+
			"table must decline rather than return a 12x-low number", g)
	}
	// Every network-optimized family the table would otherwise answer for, across the three
	// shapes of EC2 attribute naming.
	for _, itype := range []string{
		"c8gn.48xlarge", "m8gn.24xlarge", "c7gn.16xlarge", // Graviton + network
		"c5n.18xlarge", "m5n.8xlarge", "r5n.4xlarge", // Intel + network
		"c6in.32xlarge", "m6idn.16xlarge", "r6idn.12xlarge", // + instance store
		"i3en.12xlarge", "im4gn.8xlarge", "is4gen.4xlarge", "d3en.6xlarge", // storage + network
	} {
		if g, ok := bandwidthFromType(itype); ok {
			t.Errorf("%s estimated at %g Gbps: network-optimized families exceed the "+
				"size-keyed table and must decline", itype, g)
		}
	}
	// AND THE TABLE MUST STILL ANSWER FOR EVERYTHING ELSE. Declining too broadly would undo
	// #237, whose whole point was that detection failure used to clamp parts-max to its
	// 4 MiB floor. These are the standard families, where 50 at 48xlarge is correct.
	for itype, want := range map[string]float64{
		"c7g.4xlarge":    7.5,
		"m9g.48xlarge":   50,
		"c6a.48xlarge":   50,
		"m7a.32xlarge":   50,
		"c8g.large":      0.9,
		"r7g.16xlarge":   30,
		"m7i.8xlarge":    15,
		"m7i-flex.large": 0.9, // a hyphenated attribute segment with no 'n'
	} {
		g, ok := bandwidthFromType(itype)
		if !ok || g != want {
			t.Errorf("%s: got (%g,%v), want %g — the estimate must still serve standard "+
				"families or #237's parts-max collapse returns", itype, g, ok, want)
		}
	}
}

// isNetworkOptimized reads the ATTRIBUTE position, which is what keeps families whose NAME
// contains an n -- trn1, inf2 -- from being misclassified. Parsing the whole string would.
func TestIsNetworkOptimized(t *testing.T) {
	for family, want := range map[string]bool{
		"c8gn": true, "c5n": true, "m6idn": true, "i3en": true,
		"im4gn": true, "is4gen": true, "d3en": true, "c7gn": true,
		// No 'n' after the generation digits.
		"c8g": false, "m7a": false, "r7g": false, "c6a": false,
		"m7i-flex": false, "hpc7g": false, "p4d": false, "x2iezn": true,
		// THE TRAP: the n is in the FAMILY name, before the generation digit, where it says
		// nothing about networking. trn1 is 800 Gbps and inf2 is not network-optimized; both
		// would be misread by a substring search over the whole string.
		"trn1": false, "inf2": false, "trn2": false,
		// Degenerate inputs must not panic or claim anything.
		"": false, "metal": false, "c": false,
	} {
		if got := isNetworkOptimized(family); got != want {
			t.Errorf("isNetworkOptimized(%q) = %v, want %v", family, got, want)
		}
	}
}

// The refusal must reach the chain's OUTPUT, not just the table: a network-optimized box has
// to end up on a source that doctor WARNs about, carrying a reason that names the cause.
func TestNetworkOptimizedFallsThroughToAnAdmittedUnknown(t *testing.T) {
	// estimateRefusalReason is what the fallback's attempt list carries.
	r := estimateRefusalReason("c8gn.48xlarge")
	for _, want := range []string{"c8gn.48xlarge", "network-optimized", "600", "--nic-gbps"} {
		if !strings.Contains(r, want) {
			t.Errorf("refusal reason omits %q, so an operator cannot act on it: %q", want, r)
		}
	}
	// An unrecognized size gets the other reason, not the network-optimized one.
	if r := estimateRefusalReason("m8g.metal-24xl"); strings.Contains(r, "network-optimized") {
		t.Errorf("a plain unrecognized size was blamed on network optimization: %q", r)
	}
}

// ethtoolGbps must say WHY, on both platforms, because a bare 0 made four different operator
// actions look identical (#317).
func TestEthtoolReportsAReason(t *testing.T) {
	g, why := ethtoolGbps()
	if g > 0 {
		if why != "" {
			t.Errorf("ethtool answered %g Gbps but also gave a reason %q", g, why)
		}
		return
	}
	if why == "" {
		t.Error("ethtool returned 0 with no reason: the four failure modes (no route, not " +
			"installed, ENA reports no speed, parse failure) call for different actions and " +
			"must be distinguishable")
	}
}

// resolveNIC must record the sources it consulted, and must not record one for the override
// (which short-circuits before consulting anything).
func TestResolveNICRecordsWhatItTried(t *testing.T) {
	ctx := context.Background()
	if ni := resolveNIC(ctx, "", 25); len(ni.Tried) != 0 {
		t.Errorf("--nic-gbps consulted sources: %+v (it short-circuits by design)", ni.Tried)
	} else if ni.Source != "--nic-gbps" || ni.BaselineGbps != 25 {
		t.Errorf("--nic-gbps 25 resolved to %+v", ni)
	}
	// With no override, at least the first source is consulted and explained. On this
	// machine ethtool is either absent (non-Linux) or ENA-like, so a reason is guaranteed;
	// if ethtool genuinely answers, the winner is ethtool and there is nothing to explain.
	ni := resolveNIC(ctx, t.TempDir(), 0)
	if ni.Source == "ethtool" {
		t.Skip("ethtool answered on this host, so no source needed explaining")
	}
	if len(ni.Tried) == 0 {
		t.Fatalf("resolveNIC fell through to %q without recording why any source declined",
			ni.Source)
	}
	for _, a := range ni.Tried {
		if a.Source == "" || a.Reason == "" {
			t.Errorf("empty attempt record %+v: a recorded attempt with no reason is the "+
				"thing this change existed to remove", a)
		}
	}
	// The fallback and the estimate are the two sources that mean "I do not know for sure",
	// so they must never arrive with an empty explanation.
	if ni.Source == "fallback" || ni.Source == "imds-estimate" {
		if len(ni.Tried) == 0 {
			t.Errorf("%s won with no attempt reasons recorded", ni.Source)
		}
	}
}

// doctor must RENDER the reasons, at a level that survives a skim. Exercised against a
// synthesized nicInfo because the live chain on any one machine reaches only one of its
// branches -- the denial case, which is the whole point of #317, cannot be produced on a
// laptop or on a box that actually has the IAM permission.
func TestDoctorRendersNICAttempts(t *testing.T) {
	denial := "ec2:DescribeInstanceTypes DENIED — grant that one action"
	ni := nicInfo{
		InstanceType: "c8gn.48xlarge",
		Source:       "fallback",
		Tried: []nicAttempt{
			{"ethtool", "ens68 reports no link speed"},
			{"DescribeInstanceTypes", denial},
			{"imds-estimate", "c8gn.48xlarge is a network-optimized family"},
			{"cache", ""}, // no reason: must not produce a row
		},
	}
	var buf bytes.Buffer
	addNICAttempts(&doctor{out: &buf}, ni)
	out := buf.String()

	// THE DENIAL MUST BE THERE AND MUST BE A WARN. On the reporting box this is the line that
	// turns "lith thinks my NIC is 50 Gbps" into "lith was refused the API that knows".
	if !strings.Contains(out, denial) {
		t.Errorf("doctor did not report the DescribeInstanceTypes denial:\n%s", out)
	}
	for _, want := range []string{"nic:ethtool", "nic:DescribeInstanceTypes", "nic:imds-estimate"} {
		if !strings.Contains(out, want) {
			t.Errorf("doctor omitted a row for %s:\n%s", want, out)
		}
	}
	if n := strings.Count(out, "[WARN]"); n != 3 {
		t.Errorf("got %d WARN rows, want 3 — behind a `fallback` winner the reasons ARE the "+
			"finding, and at INFO an all-PASS report buries them:\n%s", n, out)
	}
	// An attempt with no reason is not a row.
	if strings.Contains(out, "nic:cache") {
		t.Errorf("an empty reason produced a row:\n%s", out)
	}
	if lines := strings.Count(out, "\n"); lines != 3 {
		t.Errorf("got %d rows, want exactly 3 (one per non-empty reason):\n%s", lines, out)
	}

	// Behind a PRECISE winner the same reasons are history, not findings.
	ni.Source = "DescribeInstanceTypes"
	buf.Reset()
	addNICAttempts(&doctor{out: &buf}, ni)
	if n := strings.Count(buf.String(), "[WARN]"); n != 0 {
		t.Errorf("%d WARN rows behind an exact source; they should be INFO:\n%s", n, buf.String())
	}
	if n := strings.Count(buf.String(), "[INFO]"); n != 3 {
		t.Errorf("got %d INFO rows, want 3:\n%s", n, buf.String())
	}

	// And nothing at all when nothing declined.
	buf.Reset()
	addNICAttempts(&doctor{out: &buf}, nicInfo{Source: "ethtool"})
	if buf.Len() != 0 {
		t.Errorf("reported attempts for a chain that had none:\n%s", buf.String())
	}
}

// The verdict grades whether the number was MEASURED. imds-estimate being an INFO is the #317
// bug: 50 Gbps on a 600 Gbps box, inside an all-PASS report.
func TestNICVerdictGradesUnmeasuredFigures(t *testing.T) {
	for _, tc := range []struct {
		source   string
		want     checkStatus
		wantFix  bool
		contains string
	}{
		{"fallback", warn, true, "undetected"},
		{"imds-estimate", warn, true, "ESTIMATED"},
		{"DescribeInstanceTypes", info, false, ""},
		{"ethtool", info, false, ""},
		{"--nic-gbps", info, false, ""},
		{"cache(DescribeInstanceTypes)", info, false, ""},
	} {
		st, detail, fix := nicVerdict(nicInfo{InstanceType: "c8gn.48xlarge", Source: tc.source}, "D")
		if st != tc.want {
			t.Errorf("source=%s graded %s, want %s — the grade says whether the figure was "+
				"measured, and an unmeasured one inside an all-PASS report is #317",
				tc.source, st.tag(), tc.want.tag())
		}
		if (fix != "") != tc.wantFix {
			t.Errorf("source=%s fix=%q, wantFix=%v", tc.source, fix, tc.wantFix)
		}
		if tc.contains != "" && !strings.Contains(detail, tc.contains) {
			t.Errorf("source=%s detail %q omits %q", tc.source, detail, tc.contains)
		}
		if !strings.Contains(detail, "D") {
			t.Errorf("source=%s dropped the caller's detail: %q", tc.source, detail)
		}
	}
	// The estimate's fix must name BOTH escapes: the flag, and the one IAM action that makes
	// the exact path work. The reporter had the second one available and could not see it.
	_, _, fix := nicVerdict(nicInfo{InstanceType: "c8gn.48xlarge", Source: "imds-estimate"}, "D")
	for _, want := range []string{"--nic-gbps", "ec2:DescribeInstanceTypes"} {
		if !strings.Contains(fix, want) {
			t.Errorf("the estimate's fix omits %q: %q", want, fix)
		}
	}
}

// The MOUNT side of ask 1: the reasons must reach the mount log, at a level chosen by whether
// the winning figure was measured. Captured through a real slog handler, because "the mount
// logs it" is the half of #317 an operator actually reads at startup.
func TestLogNICAttemptsReachesTheMountLog(t *testing.T) {
	capture := func(ni nicInfo, level slog.Level) string {
		var buf bytes.Buffer
		log := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: level}))
		logNICAttempts(log, ni)
		return buf.String()
	}
	ni := nicInfo{
		InstanceType: "c8gn.48xlarge",
		Source:       "imds-estimate",
		Tried: []nicAttempt{
			{"ethtool", "ens68 reports no link speed"},
			{"DescribeInstanceTypes", "ec2:DescribeInstanceTypes DENIED"},
			{"cache", ""}, // no reason: must not produce a line
		},
	}
	// AT DEFAULT VERBOSITY. A denial logged at DEBUG is a denial nobody sees, and the whole
	// report was that this condition was invisible.
	out := capture(ni, slog.LevelInfo)
	if !strings.Contains(out, "DENIED") {
		t.Errorf("the DescribeInstanceTypes denial is not visible at default log level behind "+
			"an unmeasured figure:\n%s", out)
	}
	if n := strings.Count(out, `"level":"WARN"`); n != 2 {
		t.Errorf("got %d WARN lines, want 2 (one per non-empty reason):\n%s", n, out)
	}
	if strings.Contains(out, `"source":"cache"`) {
		t.Errorf("an attempt with no reason produced a log line:\n%s", out)
	}
	if n := strings.Count(out, "\n"); n != 2 {
		t.Errorf("got %d lines, want exactly 2:\n%s", n, out)
	}

	// Behind a MEASURED figure the same reasons are history: present at DEBUG, absent at INFO,
	// so a normal startup is not noisy.
	ni.Source = "DescribeInstanceTypes"
	if out := capture(ni, slog.LevelInfo); out != "" {
		t.Errorf("reasons logged at default level behind a measured figure:\n%s", out)
	}
	if out := capture(ni, slog.LevelDebug); !strings.Contains(out, "DENIED") {
		t.Errorf("reasons unavailable even at debug behind a measured figure:\n%s", out)
	}

	// Nothing consulted, nothing logged.
	if out := capture(nicInfo{Source: "ethtool"}, slog.LevelDebug); out != "" {
		t.Errorf("logged attempts for a chain that had none:\n%s", out)
	}
}
