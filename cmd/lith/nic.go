// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
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
}

// resolveNIC determines the NIC baseline and peak bandwidth for the running
// instance and records which source won. Order of preference:
//
//  1. --nic-gbps override (baseline = peak = the value),
//  2. ethtool link speed (a negotiated fixed link; baseline = peak),
//  3. a cached nic.json in the index directory (repeat mounts, and boxes with
//     no ec2:DescribeInstanceTypes permission, still get a real answer),
//  4. EC2 DescribeInstanceTypes for the IMDS-reported type (cached on success),
//  5. nothing (0,0,"") — the caller then uses the fixed in-flight fallback.
func resolveNIC(ctx context.Context, indexDir string, overrideGbps float64) nicInfo {
	if overrideGbps > 0 {
		return nicInfo{BaselineGbps: overrideGbps, PeakGbps: overrideGbps, Source: "--nic-gbps"}
	}
	if g := ethtoolGbps(); g > 0 {
		return nicInfo{BaselineGbps: g, PeakGbps: g, Source: "ethtool"}
	}
	itype := imdsInstanceType(ctx)
	if indexDir != "" {
		if ni, ok := readNICCache(indexDir); ok && (itype == "" || ni.InstanceType == itype) {
			ni.Source = "cache(" + ni.Source + ")"
			return ni
		}
	}
	if itype != "" {
		if base, peak, ok := describeInstanceBandwidth(ctx, itype); ok {
			ni := nicInfo{InstanceType: itype, BaselineGbps: base, PeakGbps: peak, Source: "DescribeInstanceTypes"}
			if indexDir != "" {
				writeNICCache(indexDir, ni)
			}
			return ni
		}
	}
	return nicInfo{}
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

// describeInstanceBandwidth calls EC2 DescribeInstanceTypes for one type and
// returns its baseline/peak Gbps. ok is false on any error or missing data
// (e.g. no IAM permission), so the caller falls through to the cache/fallback.
func describeInstanceBandwidth(ctx context.Context, itype string) (baseline, peak float64, ok bool) {
	cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	// Resolve the region from IMDS: on a bare EC2 box there is no AWS_REGION env
	// or config file, and without it the EC2 endpoint can't be built.
	cfg, err := config.LoadDefaultConfig(cctx, config.WithEC2IMDSRegion())
	if err != nil {
		return 0, 0, false
	}
	out, err := ec2.NewFromConfig(cfg).DescribeInstanceTypes(cctx, &ec2.DescribeInstanceTypesInput{
		InstanceTypes: []ec2types.InstanceType{ec2types.InstanceType(itype)},
	})
	if err != nil || len(out.InstanceTypes) == 0 {
		return 0, 0, false
	}
	b, p := selectBandwidth(out.InstanceTypes[0].NetworkInfo)
	if b <= 0 && p <= 0 {
		return 0, 0, false
	}
	if b <= 0 { // no baseline reported: treat peak as sustained
		b = p
	}
	if p <= 0 {
		p = b
	}
	return b, p, true
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
