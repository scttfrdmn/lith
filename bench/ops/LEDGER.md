# The ops ledger: how to run it (and why the bench box needs no IAM change)

The neutral S3-operation ledger (`trail-up.sh` / `ops-report.py` / `trail-down.sh`)
is a **CloudTrail S3 data-event trail**, which is **account-level, passive
infrastructure**. It records every S3 `GetObject`/`ListObjects` in the account —
including a bench box's — **regardless of the box's IAM role**. So:

**Run `trail-up.sh` with account-owner (or CloudTrail-admin) credentials, not on
the bench box.** The box's `spored-instance-role` (a spore.host resource) needs
**no CloudTrail permissions and no widening** — it only needs to read the objects
under test, which it already can. This is the least-privilege answer: the box
generates S3 traffic, the account trail captures it, and you parse the delivered
logs from wherever you hold owner creds.

The earlier attempt (#214) failed only because it ran `trail-up.sh` *on the box*,
where the instance role lacks `cloudtrail:*` / `s3:CreateBucket`. Don't do that —
provision centrally.

## Recipe (M16 1b used this)

1. **Provision (once, owner creds):** `bash trail-up.sh` — creates the log bucket
   `scttfrdmn-lith-bench-trail` (7-day lifecycle) and the trail `lith-bench-trail`
   with all-bucket data-event + read-management selectors.
2. **Stage same-account:** cross-account RODA data events are *not* captured by
   your trail, so copy the objects under test into a bucket you own (e.g.
   `scttfrdmn-lith-bench`) and mount that.
3. **Measure in exclusive windows:** run each tool in its own ≥60 s-gapped window
   and record the UTC `start`/`end`. For a *streaming* workload, add a settle
   (~25 s) before closing the window — a large file's readahead GETs trail the
   read, and a short window truncates the ledger count (seen in 1b: a 1 GB stream
   undercounted until the window was widened).
4. **Parse (owner creds):** `ops-report.py --logbucket scttfrdmn-lith-bench-trail
   --account <acct> --start <s> --end <e> --principal spored-instance-role`
   filters to the box role. Widen `--end` by ~20 s past the run to catch trailing
   async prefetch, staying inside the inter-run gap.
5. **Tear down:** `bash trail-down.sh` (owner creds); the log bucket self-expires.

## Counter-vs-ledger: what to expect

lith's `lith_s3_requests_total{op="get"}` is **one-to-one with CloudTrail
`GetObject`** where the window fully bounds the run (M16 1b: exact on A1 and
ALD2). Two known, benign deltas:

- **Bytes off by ~1.2 KB:** CloudTrail `bytesTransferredOut` includes the
  `ListObjects` response; lith's `s3_bytes` counts object GET bytes only.
- **CT GETs a few percent high on long runs:** the widened window catches
  async-prefetch GETs issued during the run but after lith's metric snapshot
  (taken at read-return). A snapshot-timing artifact, not a counter defect.

## Two hazards the tool now guards against (#225)

Both were mis-diagnosed as a "principal-filter divergence" in #220; the fix
made the report trustworthy for load-bearing measurements.

- **The bucket tally is now deduped per event.** A single S3 data event carries
  **multiple resource ARNs** — the object *and* the bucket (confirmed on a real
  event: `AWS::S3::Bucket` + `AWS::S3::Object`, both matching `:s3:::`). The old
  per-resource tally therefore double-counted (7 GETs showed as ~15 "buckets" —
  the #220 "divergence"). `ops-report.py` now counts each bucket at most once per
  record, so the bucket tally equals the GET count again and is a real cross-check.
- **Delivery-completeness guard.** CloudTrail delivers S3 data events in batches
  minutes after the fact, so a parse run before the window's tail is delivered
  **silently under-counts** (a fresh 4-GET window read GET=0 immediately, GET=4
  once delivered). `ops-report.py` now blocks until a log file is delivered dated
  past `end + --delivery-lag` (default 600 s) and **fails loudly (exit 2)** on
  timeout rather than report low. Pass `--no-wait` only to re-parse old windows.
  This is the settle-window lesson generalized: delivery timing is where this
  instrument is fragile, so make completeness a precondition, not a hope.
