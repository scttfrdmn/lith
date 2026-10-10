// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/feature/ec2/imds"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// nicInfo is the resolved NIC bandwidth for the running instance. Baseline and
// Peak differ on burst-credit classes; lith sizes its in-flight budget from
// Baseline so a sustained read does not over-commit once burst credits deplete
// (#79).
type nicInfo struct {
	InstanceType string  `json:"instance_type"`
	BaselineGbps float64 `json:"baseline_gbps"`
	PeakGbps     float64 `json:"peak_gbps"`
	Source       string  `json:"source"`

	// Tried records every source consulted before the winner, and why each one did not
	// answer (#317). The chain used to report only its winner, so a box whose
	// DescribeInstanceTypes call was IAM-denied looked identical to one that had never
	// tried: the log said `source=imds-estimate` and nothing said the precise path had
	// been refused. An operator who can see "denied" adds one IAM action; an operator who
	// cannot see it has no reason to think a better number exists.
	//
	// NEVER PERSISTED. The cache stores a resolved answer, and these reasons describe one
	// particular resolution on one particular box at one particular moment -- a cached
	// "DescribeInstanceTypes denied" would be reported on a later run that never called it.
	Tried []nicAttempt `json:"-"`
}

// nicAttempt is one consulted source and why it did not produce an answer.
type nicAttempt struct {
	Source string
	Reason string
}

// resolveNIC determines the NIC baseline and peak bandwidth for the running
// instance and records which source won. Order of preference:
//
//  1. --nic-gbps override (baseline = peak = the value),
//  2. ethtool link speed (a negotiated fixed link; baseline = peak),
//  3. a cached nic.json in the index directory (repeat mounts, and boxes with
//     no ec2:DescribeInstanceTypes permission, still get a real answer),
//  4. EC2 DescribeInstanceTypes for the IMDS-reported type (cached on success),
//  5. the size-keyed estimate, where it can be trusted,
//  6. the fixed fallback.
//
// EVERY STEP THAT DOES NOT ANSWER RECORDS WHY, in nicInfo.Tried (#317). The chain previously
// reported only its winner, which made the interesting case invisible: on a ParallelCluster
// head node the DescribeInstanceTypes call is refused (the default node roles do not grant
// ec2:DescribeInstanceTypes), lith fell through to the size estimate, and the log said
// `source=imds-estimate` with nothing anywhere saying the precise path had been DENIED. The
// reporter could not tell a box that had been refused from one that had never asked, so there
// was no reason to think a better number was one IAM action away.
func resolveNIC(ctx context.Context, indexDir string, overrideGbps float64) nicInfo {
	if overrideGbps > 0 {
		return nicInfo{BaselineGbps: overrideGbps, PeakGbps: overrideGbps, Source: "--nic-gbps"}
	}
	var tried []nicAttempt
	note := func(source, reason string) { tried = append(tried, nicAttempt{source, reason}) }

	g, why := ethtoolGbps()
	if g > 0 {
		return nicInfo{BaselineGbps: g, PeakGbps: g, Source: "ethtool", Tried: tried}
	}
	note("ethtool", why)

	itype := imdsInstanceType(ctx)
	if itype == "" {
		note("imds", "instance type unavailable: not on EC2, or IMDS is blocked or unreachable")
	}
	if indexDir != "" {
		ni, ok := readNICCache(indexDir)
		switch {
		case ok && (itype == "" || ni.InstanceType == itype):
			ni.Source = "cache(" + ni.Source + ")"
			ni.Tried = tried
			return ni
		case ok:
			note("cache", fmt.Sprintf("cached entry is for %s but this box is %s", ni.InstanceType, itype))
		default:
			note("cache", "no usable entry in "+nicCachePath(indexDir))
		}
	}
	if itype != "" {
		base, peak, err := describeInstanceBandwidth(ctx, itype)
		if err == nil {
			ni := nicInfo{InstanceType: itype, BaselineGbps: base, PeakGbps: peak, Source: "DescribeInstanceTypes"}
			if indexDir != "" {
				writeNICCache(indexDir, ni) // written WITHOUT Tried: the field is json:"-"
			}
			ni.Tried = tried
			return ni
		}
		note("DescribeInstanceTypes", describeFailureReason(err))
		// Denied or errored, but IMDS still gave us the instance type with no IAM. Estimate
		// the baseline from the size — enough to keep the device-derived knobs (parts-max,
		// coalesce-gap) sized sanely without a permission change (#237). --nic-gbps remains
		// the precise path.
		if g, ok := bandwidthFromType(itype); ok {
			return nicInfo{InstanceType: itype, BaselineGbps: g, PeakGbps: g,
				Source: "imds-estimate", Tried: tried}
		}
		note("imds-estimate", estimateRefusalReason(itype))
	}
	// Nothing detected (off-EC2, IMDS blocked, or an instance family the size-keyed estimate
	// cannot speak for). Fall back to an assumed bandwidth rather than letting a literal 0
	// propagate into the derivations — which silently clamped parts-max to its 4 MiB floor and
	// disabled the whole-file parts path for every 4–64 MiB file (#237). Callers should still
	// pass --nic-gbps; doctor WARNs on this source.
	return nicInfo{InstanceType: itype, BaselineGbps: defaultFallbackGbps,
		PeakGbps: defaultFallbackGbps, Source: "fallback", Tried: tried}
}

// describeFailureReason turns a DescribeInstanceTypes error into something an operator can
// act on. The denial is called out by name because it is both the common case on HPC images
// and the one with a one-line fix: ParallelCluster's default head-node and compute-node roles
// do not grant ec2:DescribeInstanceTypes, so this is the NORMAL path there, not an edge case.
func describeFailureReason(err error) string {
	if isAccessDenied(err) || strings.Contains(err.Error(), "UnauthorizedOperation") {
		return "ec2:DescribeInstanceTypes DENIED — grant that one action to this instance " +
			"profile for an exact baseline (ParallelCluster's default node roles do not " +
			"include it), or pass --nic-gbps"
	}
	return err.Error()
}

// defaultFallbackGbps is the assumed NIC bandwidth when detection fails entirely
// (no ethtool speed, no cache, no DescribeInstanceTypes, no IMDS type — e.g. an
// off-EC2 host). A 10 GbE-class assumption: conservative for the modern EC2/HPC
// nodes where detection actually fails, and enough that the parts path stays on
// for typical scientific inputs. --nic-gbps overrides it.
const defaultFallbackGbps = 10.0

// sizeBandwidthGbps maps an EC2 instance size suffix to an approximate baseline
// bandwidth (Gbps). Baseline scales with size across current-gen families, so a
// size-keyed estimate is family-agnostic and good enough to size the derived
// knobs when DescribeInstanceTypes is unavailable (#237). It is deliberately
// conservative (leans to baseline, not "up to" peak); --nic-gbps is the precise
// override and doctor labels this an estimate. Network-optimized ('n') and metal
// variants exceed these — again, --nic-gbps.
var sizeBandwidthGbps = map[string]float64{
	"large": 0.9, "xlarge": 1.9, "2xlarge": 3.75, "4xlarge": 7.5,
	"8xlarge": 15, "12xlarge": 22.5, "16xlarge": 30, "24xlarge": 37.5,
	"32xlarge": 50, "48xlarge": 50, "metal": 30,
}

// bandwidthFromType estimates a baseline Gbps from an instance type's size suffix
// (e.g. "c7g.4xlarge" -> "4xlarge" -> 7.5). ok is false for an unrecognized size, and false
// for a NETWORK-OPTIMIZED family, where the size tells you nothing useful (#317).
//
// WHY IT NOW REFUSES RATHER THAN GUESSING. The table is keyed on size alone, so
// `c8gn.48xlarge` -- rated 600 Gbps across two network cards -- resolved through
// "48xlarge" to 50, a confident number **12x low**, logged at INFO with no warning anywhere.
// The uncomfortable part is that the code already knew: the table's own comment said
// "Network-optimized ('n') and metal variants exceed these". It documented the blind spot and
// returned a number into it anyway.
//
// Returning not-ok drops the chain to the 10 Gbps fallback, which makes the figure LOWER and
// in ratio further from 600. That is deliberate and it is the whole point: `fallback` is a
// source `doctor` WARNs on and the mount reports as undetected, so the operator is told to
// pass --nic-gbps. A wrong number that presents as detected is acted on; an admitted unknown
// is asked about. It is the same call EvidenceRatioFor makes for an unknown region, for the
// same reason. Under-detection is also the safer direction here until the window is related
// to what has to hold the bytes -- at 64 concurrent readers the 50 Gbps sizing measured ~5x
// FASTER than the true 600 (#313's 5f-F).
func bandwidthFromType(itype string) (float64, bool) {
	i := strings.LastIndexByte(itype, '.')
	if i < 0 || i+1 >= len(itype) {
		return 0, false
	}
	if isNetworkOptimized(itype[:i]) {
		return 0, false
	}
	g, ok := sizeBandwidthGbps[itype[i+1:]]
	return g, ok
}

// isNetworkOptimized reports whether an instance family prefix (the part before the dot,
// e.g. "c8gn") carries the network-optimized attribute.
//
// EC2 type names are <family letters><generation digits><attribute letters>, and 'n' in the
// ATTRIBUTE position means network-optimized: c5n, c6in, c7gn, c8gn, m5n, m6idn, m8gn, r5n,
// i3en, im4gn, is4gen, d3en. Parsing after the generation digits is what makes this safe --
// "trn1" and "inf2" have an n BEFORE the digit, where it is part of the family name and says
// nothing about the network, and reading the whole string would misclassify them.
//
// WHAT THIS DOES NOT CATCH, named rather than quietly left: families whose network vastly
// exceeds the size table without carrying an 'n' -- hpc7g/hpc7a (EFA, 200+ Gbps), p4d/p5
// (400-3200 Gbps), trn1 (800 Gbps) -- and the "metal" size, which the table maps to a flat 30
// across generations whose real figures diverge. Each would need its own rule, none has been
// reported, and inventing rules for families nobody has run is how a table gets a second
// generation of wrong entries. --nic-gbps is the answer on those boxes, and `doctor` naming
// the estimate as an estimate is how an operator finds out.
func isNetworkOptimized(family string) bool {
	i := strings.IndexFunc(family, func(r rune) bool { return r >= '0' && r <= '9' })
	if i < 0 {
		return false // no generation digit: not an EC2-style family name
	}
	attrs := family[i:]
	// Skip the generation digits; what remains is the attribute letters.
	for len(attrs) > 0 && attrs[0] >= '0' && attrs[0] <= '9' {
		attrs = attrs[1:]
	}
	return strings.ContainsRune(attrs, 'n')
}

// estimateRefusalReason says why the size-keyed estimate declined, so the fallback's WARN can
// name a cause instead of just reporting that nothing was detected.
func estimateRefusalReason(itype string) string {
	if i := strings.LastIndexByte(itype, '.'); i > 0 && isNetworkOptimized(itype[:i]) {
		return itype + " is a network-optimized family, where the size-keyed estimate is far " +
			"too low (c8gn.48xlarge is 600 Gbps against the table's 50), so no estimate is " +
			"offered — pass --nic-gbps with the sustained baseline"
	}
	return "no estimate for size suffix of " + itype
}

// selectBandwidth sums the baseline and peak bandwidth (Gbps) across an
// instance type's network cards. Pure, so it is unit-tested with a fake
// DescribeInstanceTypes response.
func selectBandwidth(ni *ec2types.NetworkInfo) (baseline, peak float64) {
	if ni == nil {
		return 0, 0
	}
	for _, c := range ni.NetworkCards {
		if c.BaselineBandwidthInGbps != nil {
			baseline += *c.BaselineBandwidthInGbps
		}
		if c.PeakBandwidthInGbps != nil {
			peak += *c.PeakBandwidthInGbps
		}
	}
	return baseline, peak
}

// describeInstanceBandwidth calls EC2 DescribeInstanceTypes for one type and returns its
// baseline/peak Gbps. A non-nil error means the caller falls through to the estimate or the
// fallback.
//
// IT RETURNS THE ERROR RATHER THAN A BOOL (#317) because the distinction is the whole finding:
// "denied" is one IAM action away from an exact answer, "timed out" is a retry, and "returned
// no bandwidth" is a gap in the API's data for a new instance family. Collapsed to false, all
// three looked like "no answer" and the chain silently estimated instead.
func describeInstanceBandwidth(ctx context.Context, itype string) (baseline, peak float64, err error) {
	cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	// Resolve the region from IMDS: on a bare EC2 box there is no AWS_REGION env
	// or config file, and without it the EC2 endpoint can't be built.
	cfg, err := config.LoadDefaultConfig(cctx, config.WithEC2IMDSRegion())
	if err != nil {
		return 0, 0, fmt.Errorf("could not load AWS config (no region from IMDS?): %w", err)
	}
	out, err := ec2.NewFromConfig(cfg).DescribeInstanceTypes(cctx, &ec2.DescribeInstanceTypesInput{
		InstanceTypes: []ec2types.InstanceType{ec2types.InstanceType(itype)},
	})
	if err != nil {
		return 0, 0, err
	}
	if len(out.InstanceTypes) == 0 {
		return 0, 0, fmt.Errorf("DescribeInstanceTypes returned no entry for %s", itype)
	}
	b, p := selectBandwidth(out.InstanceTypes[0].NetworkInfo)
	if b <= 0 && p <= 0 {
		return 0, 0, fmt.Errorf("DescribeInstanceTypes reported no network bandwidth for %s", itype)
	}
	if b <= 0 { // no baseline reported: treat peak as sustained
		b = p
	}
	if p <= 0 {
		p = b
	}
	return b, p, nil
}

// imdsInstanceType reads the running instance's type from IMDSv2, or "" off-EC2.
func imdsInstanceType(ctx context.Context) string {
	cctx, cancel := context.WithTimeout(ctx, 1*time.Second)
	defer cancel()
	cfg, err := config.LoadDefaultConfig(cctx)
	if err != nil {
		return ""
	}
	out, err := imds.NewFromConfig(cfg).GetMetadata(cctx, &imds.GetMetadataInput{Path: "instance-type"})
	if err != nil {
		return ""
	}
	defer func() { _ = out.Content.Close() }()
	b, _ := io.ReadAll(out.Content)
	return strings.TrimSpace(string(b))
}

// nicCachePath is the per-uid cache file. Callers pass os.TempDir() when
// --index-file is unset (and bench always), so a shared name like nic.json in a
// world-writable /tmp would let another user pre-plant a symlink or a decoy
// (findings F2/F3). Scoping the name by euid — mirroring the daemon log in
// daemon.go — keeps each user's cache in a path only they write.
func nicCachePath(dir string) string {
	return filepath.Join(dir, fmt.Sprintf("nic-%d.json", os.Geteuid()))
}

// readNICCache reads a previously written per-uid cache, but only trusts it if
// it is a regular file (not a symlink or a directory) owned by the current euid.
// A planted symlink or a file owned by another user could otherwise steer our
// in-flight/readahead budget (F3). Any failure falls through to live detection.
func readNICCache(dir string) (nicInfo, bool) {
	path := nicCachePath(dir)
	fi, err := os.Lstat(path)
	if err != nil {
		return nicInfo{}, false
	}
	if fi.Mode()&os.ModeSymlink != 0 || !fi.Mode().IsRegular() {
		return nicInfo{}, false
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); !ok || st.Uid != uint32(os.Geteuid()) {
		return nicInfo{}, false
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nicInfo{}, false
	}
	var ni nicInfo
	if json.Unmarshal(b, &ni) != nil || ni.BaselineGbps <= 0 {
		return nicInfo{}, false
	}
	return ni, true
}

// writeNICCache persists the resolved NIC info to a per-uid file (best-effort).
// It never follows a symlink and never truncates a file it does not own: an
// attacker who pre-plants the cache path (as a symlink or a decoy owned by
// another user) must not be able to redirect our write to an arbitrary file or
// have us clobber it as root (F2). A stale cache we do own is refreshed via an
// Lstat-verified unlink-then-create; the create is O_EXCL|O_NOFOLLOW so a symlink
// raced in after the Lstat still makes us skip rather than follow it.
func writeNICCache(dir string, ni nicInfo) {
	b, err := json.Marshal(ni)
	if err != nil {
		return
	}
	_ = os.MkdirAll(dir, 0o755)
	path := nicCachePath(dir)
	if fi, err := os.Lstat(path); err == nil {
		if fi.Mode()&os.ModeSymlink != 0 || !fi.Mode().IsRegular() {
			return // symlink or non-regular: never touch it
		}
		if st, ok := fi.Sys().(*syscall.Stat_t); !ok || st.Uid != uint32(os.Geteuid()) {
			return // owned by someone else: never overwrite it
		}
		_ = os.Remove(path) // our own stale cache: safe to replace
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return // EEXIST (raced) / ELOOP (symlink) / other: skip, cache is best-effort
	}
	defer func() { _ = f.Close() }()
	_, _ = f.Write(b)
}

// logNICAttempts reports why each consulted source did not answer, so the chain's failures are
// visible rather than inferable from its winner (#317).
//
// The level is chosen by what the winner means. `fallback` and `imds-estimate` are the mount
// saying "I do not know" and "I guessed from the size" -- there, the reasons are the actionable
// content and go out at WARN, so an all-INFO startup does not bury them. When a precise source
// won, the reasons are history and go out at DEBUG.
func logNICAttempts(log *slog.Logger, nic nicInfo) {
	if len(nic.Tried) == 0 {
		return
	}
	level := slog.LevelDebug
	switch nic.Source {
	case "fallback", "imds-estimate":
		level = slog.LevelWarn
	}
	for _, a := range nic.Tried {
		if a.Reason == "" {
			continue
		}
		log.Log(context.Background(), level, "nic source did not answer",
			"source", a.Source, "reason", a.Reason, "winner", nic.Source)
	}
}

// addNICAttempts reports why each consulted source did not answer, one row per reason (#317).
//
// doctor used to report only the chain's winner, so a ParallelCluster node whose
// DescribeInstanceTypes call had been REFUSED looked exactly like one that never asked. When it
// happens, that denial is the most actionable line in the whole report -- one IAM action buys
// an exact baseline instead of a size guess.
//
// The level follows the winner, so a reason is never filtered out separately from the verdict
// it explains: `fallback` and `imds-estimate` both mean "this number is not measured", and
// there the reasons ARE the content, so they go out at WARN. Behind a precise source they are
// history, and go out at INFO.
//
// Separated from doctorChecks so it can be exercised against a synthesized nicInfo -- the live
// chain on any one machine reaches only one of its branches, so a test that ran the real
// resolution could not cover the denial case at all.
func addNICAttempts(d *doctor, nic nicInfo) {
	level := info
	if nic.Source == "fallback" || nic.Source == "imds-estimate" {
		level = warn
	}
	for _, a := range nic.Tried {
		if a.Reason == "" {
			continue
		}
		d.add(level, "nic:"+a.Source, a.Reason, "")
	}
}

// nicVerdict grades a resolved NIC figure: the status doctor should report it at, the detail
// line, and the fix when there is one (#317).
//
// THE GRADES ARE ABOUT WHETHER THE NUMBER WAS MEASURED, not about its size:
//
//	fallback      WARN  nothing answered; the figure is an assumption
//	imds-estimate WARN  guessed from the instance SIZE, ignoring family
//	anything else INFO  measured, cached, or stated by the operator
//
// `imds-estimate` WAS AN INFO AND THAT IS THE #317 BUG. On a c8gn.48xlarge -- rated 600 Gbps --
// this path reported 50, and an all-PASS doctor run said nothing about it, so there was no
// signal that the precise source had been refused or that the estimate ignores the one
// attribute that mattered. The estimate now declines outright for network-optimized families
// (see bandwidthFromType), but it still speaks for the rest of the fleet off a size-keyed table
// with no family dimension, so "this was guessed" has to be visible wherever it is used.
//
// Separated from doctorChecks so every branch is reachable in a test. The live chain on any one
// machine reaches exactly one.
func nicVerdict(nic nicInfo, detail string) (checkStatus, string, string) {
	switch nic.Source {
	case "fallback":
		// THE REASON IS NOT ALWAYS "NOTHING ANSWERED" ANY MORE. Before #317 this verdict was
		// reached only when IMDS gave no instance type, so the fixed parenthetical was true.
		// #317 added a second route -- IMDS names the type and the size-keyed estimate
		// DECLINES because the family is network-optimized -- and the message was not updated
		// for the path that change introduced. Reported from the box it was built for: it read
		// "no IMDS type" on a mount where IMDS had returned `c8gn.48xlarge` and the row
		// directly below said so by name. The #388 shape again, inside one report.
		why := "no ethtool speed, no IMDS type, no DescribeInstanceTypes"
		if nic.InstanceType != "" {
			why = fmt.Sprintf("IMDS type %s known, but no usable bandwidth for it — see the "+
				"nic: rows below", nic.InstanceType)
		}
		return warn, detail + " — NIC undetected (" + why + ")",
			"pass --nic-gbps <Gbps>; without it the device-derived knobs are guesses (e.g. parts-max)"
	case "imds-estimate":
		return warn, detail + fmt.Sprintf(" — ESTIMATED from the %s size, not measured; the "+
				"size-keyed table ignores family, so it can be badly low", nic.InstanceType),
			"pass --nic-gbps <sustained baseline>, or grant this instance profile " +
				"ec2:DescribeInstanceTypes for an exact figure"
	}
	return info, detail, ""
}

// nicOverrideWarning returns a warning when a --nic-gbps value looks like the instance's
// advertised PEAK rather than its sustained baseline, and "" when it does not (#239).
//
// WHY THIS EXISTS. --nic-gbps sizes --inflight-bytes, the readahead window, and the
// device-derived parts-max/coalesce-gap, and it wants the SUSTAINED BASELINE. The number AWS
// shows on the instance page is the PEAK -- "Up to 15 Gigabit" -- so the obvious value to copy
// is the wrong one by roughly 2x on burst-credit classes. Measured on a c7g.4xlarge, whose
// baseline is 7.5, with an A3dyn/U hyperslab over 3 reps:
//
//	--nic-gbps 15  (the peak, what was passed) : 654 MB fetched for 110 MB wanted = 6.0x
//	--nic-gbps 7.5 (the true baseline)         : 382 MB fetched for 110 MB wanted = 3.5x
//
// 42% fewer bytes and an indistinguishable wall clock (2.35-2.79 s against 2.38-3.31 s). The
// window scales with the stated baseline, so overstating it fetches 272 MB nobody reads and
// earns zero milliseconds. In-region that is request cost; cross-region it is billed egress.
//
// IT ONLY SPEAKS WHEN THE COMPARISON IS AGAINST A MEASURED FIGURE. That is the correction this
// function makes over the doctor-only version it replaces, and #317 is why: an `imds-estimate`
// baseline comes from a size-keyed table with no family dimension and was measured 12x LOW on
// a c8gn.48xlarge. Warning that a user's correct 600 is "well above the detected baseline 50"
// would be exactly backwards -- the user would be right and the estimate wrong. A `fallback`
// baseline is a flat 10 Gbps assumption and is no better. So both are excluded, and the only
// bases for comparison are ethtool, DescribeInstanceTypes, and a cache of one of those.
func nicOverrideWarning(override float64, det nicInfo) string {
	if override <= 0 || !nicSourceIsMeasured(det.Source) || det.BaselineGbps <= 0 {
		return ""
	}
	// THE SOUND CASE, and it needs no threshold: the value IS the advertised peak. Only
	// DescribeInstanceTypes reports a peak distinct from the baseline, so this fires exactly
	// where AWS itself has told us both numbers. The 0.95 is a float-equality tolerance, not
	// a judgement about how wrong is too wrong.
	if det.PeakGbps > det.BaselineGbps && override >= det.PeakGbps*0.95 {
		return fmt.Sprintf("--nic-gbps %.1f is this instance's PEAK, not its sustained "+
			"baseline (baseline %.1f, peak %.1f, source=%s). --nic-gbps wants the baseline: "+
			"the readahead window scales with it, so the peak fetches bytes nobody reads for "+
			"no speed gain — measured 654 MB against 382 MB for the same 110 MB of data, with "+
			"an indistinguishable wall clock. Pass --nic-gbps %.1f (#239)",
			override, det.BaselineGbps, det.PeakGbps, det.Source, det.BaselineGbps)
	}
	// THE HEURISTIC CASE: a value well above a measured baseline that is not the peak either
	// -- someone has typed a number from somewhere else. 1.5x is a judgement and the only
	// fitted constant here; it is set below the 2.0x of the reported case (15 against a 7.5
	// baseline) so that case is caught, and above 1.0 so a rounding difference or a genuine
	// small correction is not nagged about. A warning, never a clamp.
	if override >= det.BaselineGbps*overstatedBaselineRatio {
		return fmt.Sprintf("--nic-gbps %.1f is %.1fx the detected sustained baseline %.1f "+
			"(source=%s). --nic-gbps wants the baseline, not the advertised 'Up to N Gigabit' "+
			"peak; overstating it over-fetches for no speed gain (#239)",
			override, override/det.BaselineGbps, det.BaselineGbps, det.Source)
	}
	return ""
}

// overstatedBaselineRatio is how far above a MEASURED baseline a --nic-gbps value must sit
// before it is called out, when it is not an exact peak match. Below the 2.0x of the reported
// case so that case is caught; above 1.0 so a small deliberate correction is not nagged about.
const overstatedBaselineRatio = 1.5

// nicSourceIsMeasured reports whether a resolved NIC figure came from somewhere that actually
// knows, as opposed to an assumption.
//
// This is the same distinction nicVerdict grades on, and it is load-bearing in two places: a
// figure that was not measured must not be reported as fact (#317), and it must not be used as
// the basis for correcting an operator (#239). `imds-estimate` is excluded because the
// size-keyed table has no family dimension and was measured 12x low; `fallback` because it is
// a flat 10 Gbps assumption; `--nic-gbps` because it is the thing being checked.
func nicSourceIsMeasured(source string) bool {
	switch {
	case source == "ethtool", source == "DescribeInstanceTypes":
		return true
	case strings.HasPrefix(source, "cache("):
		// A cache entry is only ever written from a successful DescribeInstanceTypes
		// resolution, so it carries a measured figure.
		return true
	}
	return false
}

// nicDetailLine renders doctor's NIC fact line: which figure this is, the peak when there is a
// distinct measured one, the source, and the two device-derived knobs it feeds (#239).
//
// IT NAMES THE FIGURE because the unlabelled version is what made copying the wrong number
// easy. doctor printed "7.5 Gbps (source=...)" with nothing saying that --nic-gbps wants the
// SMALLER of the two values AWS publishes for a burst-credit instance, and the larger one is
// the one on the instance page.
//
// The peak is shown only when it is DISTINCT. ethtool reports a fixed negotiated link, the
// size estimate and the fallback are single assumptions, and all three set peak = baseline;
// printing "peak 10.0" there would invent a second fact that does not exist.
func nicDetailLine(nic nicInfo, partsMax, inflight int64) string {
	s := fmt.Sprintf("%.1f Gbps baseline", nic.BaselineGbps)
	if nic.PeakGbps > nic.BaselineGbps {
		s += fmt.Sprintf(" (peak %.1f — --nic-gbps wants the baseline, not this)", nic.PeakGbps)
	}
	return s + fmt.Sprintf(" (source=%s) → parts-max %d MiB, inflight %d MiB",
		nic.Source, partsMax/(1<<20), inflight/(1<<20))
}
