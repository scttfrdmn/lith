#!/usr/bin/env python3
# SPDX-License-Identifier: Apache-2.0
#
# Read a CloudTrail trail's delivered S3 data/management log files for a time
# window and produce a neutral operations ledger: counts by eventName
# (GetObject/HeadObject/ListObjects*/multipart), bytes transferred out, and the
# request-cost + optional instance-time cost. Data events are NOT in
# LookupEvents; this reads the gzip JSON log files under
# s3://<logbucket>/AWSLogs/<acct>/CloudTrail/<region>/YYYY/MM/DD/.
#
# Runs are attributed by exclusive time window (keep a >=60s gap between runs);
# an optional --principal substring filters to one caller (the box role).
#
# S3 request rates default to the published us-east-1 rates; pass --get-rate /
# --list-rate (per 1,000) to override with Price List values. In-region data
# transfer is $0.
import argparse, gzip, io, json, sys, time
from datetime import datetime, timezone
import boto3

def parse_t(s):
    return datetime.strptime(s, "%Y-%m-%dT%H:%M:%SZ").replace(tzinfo=timezone.utc)

def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--logbucket", required=True)
    ap.add_argument("--account", required=True)
    ap.add_argument("--region", default="us-east-1")
    ap.add_argument("--start", required=True)   # UTC ISO Z
    ap.add_argument("--end", required=True)
    ap.add_argument("--workload", default="")
    ap.add_argument("--tool", default="")
    ap.add_argument("--principal", default="")  # substring match on userIdentity
    ap.add_argument("--get-rate", type=float, default=0.0004)  # $/1000 GET/HEAD/SELECT (us-east-1)
    ap.add_argument("--list-rate", type=float, default=0.005)  # $/1000 PUT/COPY/POST/LIST (us-east-1)
    ap.add_argument("--instance-rate", type=float, default=0.0)  # $/hr
    ap.add_argument("--wall", type=float, default=0.0)  # seconds
    ap.add_argument("--csv", action="store_true")
    # Delivery-completeness guard (#225): CloudTrail delivers S3 data events in
    # batches minutes after the fact, so a parse run before the window's tail is
    # delivered silently under-counts. Before scanning, wait until a log file has
    # been delivered with LastModified >= end + --delivery-lag (proof CloudTrail
    # has flushed past the window). Fail loudly on timeout rather than report low.
    ap.add_argument("--delivery-lag", type=int, default=600)   # seconds past end that a log file must be dated
    ap.add_argument("--wait-timeout", type=int, default=1800)  # give up (exit 2) after this long
    ap.add_argument("--no-wait", action="store_true")          # skip the guard (e.g. re-parsing old windows)
    args = ap.parse_args()

    start, end = parse_t(args.start), parse_t(args.end)
    s3 = boto3.client("s3", region_name=args.region)
    days = sorted({start.strftime("%Y/%m/%d"), end.strftime("%Y/%m/%d")})

    # Guard: block until delivery is provably complete for this window.
    if not args.no_wait:
        need = end.timestamp() + args.delivery_lag
        deadline = time.time() + args.wait_timeout
        while True:
            newest = 0.0
            for day in days:
                prefix = f"AWSLogs/{args.account}/CloudTrail/{args.region}/{day}/"
                for page in s3.get_paginator("list_objects_v2").paginate(Bucket=args.logbucket, Prefix=prefix):
                    for obj in page.get("Contents", []):
                        newest = max(newest, obj["LastModified"].timestamp())
            if newest >= need:
                break
            if time.time() > deadline:
                sys.stderr.write(
                    f"ERROR: CloudTrail delivery incomplete for window ending {args.end}: "
                    f"newest log file is {int(need - newest)}s short of end+{args.delivery_lag}s "
                    f"after waiting {args.wait_timeout}s. Refusing to report a possibly-low count "
                    f"(#225). Re-run later, or pass --no-wait to override.\n")
                sys.exit(2)
            time.sleep(30)

    counts = {}   # eventName -> count (one per matched record)
    bytes_out = 0
    buckets = {}  # bucket -> matched records touching it (one per record, deduped)
    paginator = s3.get_paginator("list_objects_v2")
    for day in days:
        prefix = f"AWSLogs/{args.account}/CloudTrail/{args.region}/{day}/"
        for page in paginator.paginate(Bucket=args.logbucket, Prefix=prefix):
            for obj in page.get("Contents", []):
                body = s3.get_object(Bucket=args.logbucket, Key=obj["Key"])["Body"].read()
                recs = json.load(gzip.GzipFile(fileobj=io.BytesIO(body))).get("Records", [])
                for r in recs:
                    if r.get("eventSource") != "s3.amazonaws.com":
                        continue
                    et = r.get("eventTime", "")
                    try:
                        t = parse_t(et)
                    except ValueError:
                        continue
                    if not (start <= t <= end):
                        continue
                    if args.principal:
                        ui = json.dumps(r.get("userIdentity", {}))
                        if args.principal not in ui:
                            continue
                    name = r.get("eventName", "?")
                    counts[name] = counts.get(name, 0) + 1
                    aed = r.get("additionalEventData", {}) or {}
                    b = aed.get("bytesTransferredOut", 0) or 0
                    bytes_out += int(b)
                    # Count each bucket at most ONCE per record. A single S3 data
                    # event carries multiple resource ARNs (the object AND the
                    # bucket, sometimes an access point / KMS key), so a naive
                    # per-resource tally over-counts — it was the "buckets 15 vs
                    # GET 7" divergence in #220: 7 events, ~2 s3 resources each.
                    # Deduping makes the bucket tally == the event count, so it is
                    # a real cross-check of GET again.
                    seen = set()
                    for res in (r.get("resources") or []):
                        arn = res.get("ARN", "")
                        if ":s3:::" in arn:
                            bkt = arn.split(":::")[1].split("/")[0]
                            if bkt not in seen:
                                seen.add(bkt)
                                buckets[bkt] = buckets.get(bkt, 0) + 1

    gets = counts.get("GetObject", 0)
    heads = counts.get("HeadObject", 0)
    lists = counts.get("ListObjects", 0) + counts.get("ListObjectsV2", 0)
    mpu = sum(counts.get(n, 0) for n in ("CreateMultipartUpload", "UploadPart", "CompleteMultipartUpload", "UploadPartCopy"))
    req_cost = (gets + heads) * args.get_rate / 1000.0 + lists * args.list_rate / 1000.0
    inst_cost = args.instance_rate * args.wall / 3600.0
    total = req_cost + inst_cost

    if args.csv:
        # workload,tool,wall_s,get,head,list,multipart,bytes_out,request_usd,instance_usd,total_usd
        print(f"{args.workload},{args.tool},{args.wall:.2f},{gets},{heads},{lists},{mpu},{bytes_out},{req_cost:.6f},{inst_cost:.4f},{total:.4f}")
    else:
        print(f"[{args.workload}/{args.tool}] window {args.start}..{args.end}")
        print(f"  eventName counts: {dict(sorted(counts.items()))}")
        print(f"  GET={gets} HEAD={heads} LIST={lists} multipart={mpu} bytesOut={bytes_out}")
        print(f"  buckets touched: {dict(sorted(buckets.items()))}")
        print(f"  request$={req_cost:.6f} instance$={inst_cost:.4f} total$={total:.4f}")

if __name__ == "__main__":
    main()
