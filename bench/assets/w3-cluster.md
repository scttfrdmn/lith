# W3 cluster campaign assets (session 41, #143)

Staged **same-account** so the CloudTrail ledger captures reads (cross-account
RODA reads are not captured; session 35). Source: `s3://1000genomes/phase3/data`.

- **Location:** `s3://scttfrdmn-lith-bench/lith-bench/w3/`
- **Set:** 8 heterogeneous low-coverage CRAMs + `.crai` — HG00096, HG00097,
  HG00099, HG00100 (~14 GB), HG00101, HG00102, HG00103, HG00105 (~60 GB total).
- **Index:** `s3://scttfrdmn-lith-bench/lith-bench/w3/w3.idx` (built once, fetched
  by every configuration via `--index-file`).
- **Lifecycle:** the bench bucket's 7-day lifecycle expires these; re-stage with
  `bench/ops/` + `bench/cluster/` if a re-run is needed after expiry.
