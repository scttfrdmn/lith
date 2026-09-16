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
