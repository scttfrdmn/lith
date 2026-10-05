// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/feature/ec2/imds"

	"github.com/scttfrdmn/lith/internal/s3client"
)

// instanceRegion reads the running instance's own region from IMDSv2, or "" when that is
// not available — off EC2, or IMDS blocked, which is routine on hardened cluster images.
//
// This is NOT the client's configured region. By convention `--region` names the BUCKET's
// region (lith doctor fails a mount where the two disagree), so it says nothing about where
// the compute is. Only IMDS answers "where am I".
func instanceRegion(ctx context.Context) string {
	cctx, cancel := context.WithTimeout(ctx, 1*time.Second)
	defer cancel()
	cfg, err := config.LoadDefaultConfig(cctx)
	if err != nil {
		return ""
	}
	out, err := imds.NewFromConfig(cfg).GetRegion(cctx, &imds.GetRegionInput{})
	if err != nil || out == nil {
		return ""
	}
	return strings.TrimSpace(out.Region)
}

// regionOf reads the bucket's region off a client that has already resolved it. FREE: the
// client resolved it in s3client.New to sign requests at all, so this costs no API call.
// Empty when a region or a custom endpoint was configured instead, or for a client that
// does not carry one (a test fake).
func regionOf(cl s3client.API) string {
	if r, ok := cl.(interface{ Region() string }); ok {
		return strings.TrimSpace(r.Region())
	}
	return ""
}

// crossRegionWarning returns a warning line when the mount is reading a bucket in a
// different region from the instance doing the reading, and "" when it is not or cannot
// tell (#362).
//
// WHY THIS IS A WARNING AND NOT A NOTE. Cross-region, the reported workload pulled bytes at
// 1.52 MiB/s steady against 107–146 MB/s in-region on the same object and the same code — a
// ~70–96× difference. Two instances and most of a day went into chasing that as a
// performance bug before anyone thought to run `get-bucket-location`, because **the only
// symptom was slowness**, which is indistinguishable from every other reason something is
// slow. lith knew both regions the whole time and said nothing.
//
// It compounds with the worst case rather than the average one: a scattered random-fault
// stream already pays one round trip per miss (#232, no lever), so multiplying the round
// trip multiplies the cost of exactly the pattern lith is already worst at.
//
// Silent when either region is unknown. IMDS is blocked on plenty of hardened images and a
// custom --endpoint has no AWS region at all; a warning that fires on "I could not tell"
// would be noise, and this has to stay greppable to be worth anything.
func crossRegionWarning(instance, bucket, bucketName string) string {
	if instance == "" || bucket == "" || instance == bucket {
		return ""
	}
	return "bucket " + bucketName + " is in " + bucket + " but this instance is in " +
		instance + " — every read crosses regions, which costs egress and multiplies " +
		"per-request latency (measured ~70-96x slower on a sequential TB-scale read, and " +
		"worse on a random-fault workload). Run the compute in " + bucket +
		", or pass --no-region-check to silence this"
}

// warnCrossRegion logs the warning at mount, if there is one to log. Separated from
// crossRegionWarning (which is pure) so the message is testable without IMDS.
func warnCrossRegion(ctx context.Context, log *slog.Logger, cl s3client.API, bucket string, skip bool) {
	if skip {
		return
	}
	if w := crossRegionWarning(instanceRegion(ctx), regionOf(cl), bucket); w != "" {
		log.Warn("cross-region mount", "warning", w)
	}
}
