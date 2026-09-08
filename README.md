# lith

Read-only, high-performance POSIX filesystem over an existing S3 bucket in its
native key layout.

`lith` presents any S3 bucket — a Registry of Open Data dataset, a lab bucket,
an instrument dump — as a read-only filesystem, using the bucket's key
namespace directly. It writes nothing to the bucket, needs no sidecar objects,
and serves all metadata (`readdir`, `lookup`, `getattr`, `open`) locally from an
index after a one-time build.

## What it is

- **Read-only.** There is no write path. Every mutating operation returns `EROFS`.
- **Native layout.** The bucket's keys *are* the filesystem. Any bucket, any producer.
- **Local metadata.** After an index build, listing and stat make zero S3 calls.
- **Fast.** Sequential throughput near NIC line rate; random reads bounded by
  cache hit rate, not S3 request latency.
- A single static Go binary.

## What it is not

- **Not writable.** No writes, ever — not "writes disabled", writes *absent*.
- **Not a re-layout.** It does not own or rewrite the bucket's object format.
- **Linux only** (for v0.x). It relies on FUSE; macOS/Windows are out of scope.

## Install

```
go install github.com/scttfrdmn/lith/cmd/lith@latest
```

Or download a static binary from the [releases](https://github.com/scttfrdmn/lith/releases) page.

## Commands

```
lith mount   s3://bucket[/prefix] /mnt/point [flags]   # mount as a filesystem (M2)
lith index   build|refresh|inspect s3://bucket[/prefix] --index-file F
lith bench   s3://bucket/key --pattern seq|rand4k|stride [--against PATH]  (M2)
lith version                                            # version, commit, build date
```

> Status: `mount` and `bench` are implemented in milestone M2 and currently
> report "not implemented in this build". `index` lands in M1.

## Example: a public Registry of Open Data bucket

The `1000genomes` bucket is public; use `--no-sign-request` to read it without
credentials:

```
lith index build s3://1000genomes/changelog_details --index-file /tmp/1kg.lithidx --no-sign-request
lith index inspect /tmp/1kg.lithidx
lith mount  s3://1000genomes/changelog_details /mnt/1kg --index-file /tmp/1kg.lithidx --no-sign-request
ls -l /mnt/1kg
cat /mnt/1kg/changelog_details_20081219
```

## Caching / No NVMe?

lith serves file reads from a two-tier cache: a bounded in-memory tier
(`--mem-cache`, default 25% of system memory) and an optional on-disk tier
(`--disk-cache`, default off). The disk tier is worth it only on **fast local
storage** — an instance-store NVMe volume.

If the machine has no local NVMe (most instances, laptops, cluster head
nodes), either:

- leave `--disk-cache` off and rely on the memory tier plus the kernel page
  cache (sequential and re-read workloads still benefit), or
- point `--disk-cache` at **`/dev/shm`** (tmpfs, i.e. RAM): `--disk-cache 8GiB
  --disk-path /dev/shm/lith`.

Do **not** put the disk cache on EBS, EFS, or NFS — caching a block there can
be slower than just re-fetching it from S3 in-region. `lith mount` prints a
warning if the `--disk-cache` path resolves onto the root filesystem or a
network volume.

## Sizing the node

For cold reads the network is the ceiling: lith streams from S3, so its
throughput is bounded by the instance's NIC, not by CPU or disk. Size the node
to the bandwidth you want.

- **Match the NIC baseline to your sustained demand.** Many instance classes
  advertise a burst bandwidth well above their baseline (e.g. `c8gd.4xlarge` is
  7.5 Gbps baseline, 15 Gbps burst). A short read stays in burst; a long or
  many-file read drains the burst credits and settles at the baseline. In
  measurement, a single cold sequential read of a >1 GiB object showed a
  post-first-window p99 of **~4.9 ms on a 30 Gbps `c8gd.16xlarge`** versus
  **~7.4 ms on a `c8gd.4xlarge`** once its burst credits depleted — same code,
  different NIC headroom.
- **For sustained or multi-reader workloads, pick a class whose *baseline*
  meets your target,** or a 16xlarge+ where the advertised bandwidth is
  sustained (no burst-credit model). A `c8gd.16xlarge` (30 Gbps) sustains
  multi-GB/s aggregate; a 4xlarge will settle at ~0.9 GB/s under a long
  many-reader run.
- **Local NVMe (`d` instance types) is worth it for re-read and random
  workloads** — point `--disk-cache` at the instance-store mount. Without it,
  see "No NVMe?" above.
- `lith` sizes its S3 concurrency to the bandwidth-delay product automatically
  (`--inflight-bytes`, default 2 × NIC × 100 ms via `ethtool`); override it if
  the NIC speed can't be detected.

## Expectations

Measured against `s3://1000genomes` in `us-east-1`, lith vs mountpoint-s3, all
MB/s. lith `--disk-cache 0`, `--mem-cache` 25% of RAM; single = a 14 GiB CRAM
read sequentially; 8-reader = eight 3.8–14 GiB CRAMs read concurrently; warm =
re-read from cache; metadata = 100k `stat`s from a prebuilt local index. Full
data and every per-run number are in [`bench/results/`](bench/results/); the S3
client ceiling can be reproduced independently with `cmd/lith-s3bench`.

| instance | RAM | NVMe | cold seq (lith / mnt-s3) | warm seq (lith) | 8-reader (lith / mnt-s3) | 100k stat | warm rand4k |
|---|---|---|---|---|---|---|---|
| c8g.2xlarge | 16 GiB | no | 1354 / 1527 | 1163 | **1550** / 1602 | 0.16 s, 0 S3 | 543k IOPS |
| c8gd.4xlarge | 32 GiB | yes | 1440 / 1547 | 1400 | **1688** / 1637 | 0.17 s, 0 S3 | — |
| c8gd.16xlarge | 128 GiB | yes | **2848** / 2613 | **21383** | **3385** / 3221 | 0.16 s, 0 S3 | 589k IOPS |
| c8gd.8xlarge † | 64 GiB | yes | 1361 / 1581 | 19214 | 1696 / 1686 | 0.16 s, 0 S3 | 547k IOPS |
| c8i.4xlarge (x86) † | 32 GiB | no | 1295 / 1545 | 1402 | 1137 / 1683 | 0.12 s, 0 S3 | 687k IOPS |
| m8g.4xlarge † | 64 GiB | no | 1398 / 1611 | 20408 | 1703 / 1704 | 0.16 s, 0 S3 | 518k IOPS |

† pre-fix (the earlier run); their multi-reader improves the same way the
re-run rows do — the 32 GiB x86 box would rise from 1137 toward mount-s3 like
the 32 GiB `c8gd.4xlarge` did (1095 → 1688).

**Reading guide: cold sequential is your NIC; warm and random are your cache;
metadata is free.** A single cold reader fills the instance's NIC (the readahead
window is sized to the bandwidth-delay product), reaching mount-s3 parity or
better. The **8-reader aggregate reaches mount-s3 parity across box sizes**,
from a 16 GiB `c8g.2xlarge` (1550, 97% of mount-s3) to a 128 GiB
`c8gd.16xlarge` (3385, 105%): aggregate readahead is bounded to the memory tier
and shared fairly across handles, so a small-RAM box no longer thrashes. With
RAM ≥ ~2× your working file, warm reads come from cache at ~20 GB/s. Metadata is
served entirely from the local index: 100k `stat`s in ~0.15 s with **zero** S3
requests on every class. (mount-s3 still edges ahead on cold *single*-reader on
the smallest burst-credit boxes, where it rides NIC burst harder.)

## Copy first, or mount?

Cost-to-result on a `c8gd.4xlarge`, in-region (transfer/egress $0). Each cell is
**wall-to-result / dollars / bytes pulled from S3** (full data in
[`bench/results/copy-vs-mount-v0.1.0.csv`](bench/results/)):

| access path | region query (1% of 14 GiB) | full read (3.8 GiB) | parallel batch (8 × = 60 GiB) |
|---|---|---|---|
| copy → gp3 EBS, then compute | 1.3 min / $0.019 / 14.1 GB | 0.95 min / $0.013 / 3.8 GB | 15.5 min / $0.21 / 60 GB |
| copy → local NVMe, then compute | 0.45 min / $0.006 / 14.1 GB | 0.90 min / $0.012 / 3.8 GB | 3.4 min / $0.05 / 60 GB |
| **mount with lith, compute in place** | **0.007 min / $0.0001 / 0.05 GB** | **0.79 min / $0.011 / 3.8 GB** | **3.1 min / $0.05 / 68 GB** |

Copying first pays off only when you re-read a dataset many times from fast local
storage and the copy amortizes — and a local-NVMe copy always beats an EBS copy
for later compute. For query-once or selective-access workloads, mounting is both
faster and cheaper: no staging wall-clock, and for a selective query lith moves a
tiny fraction of the bytes (a 1% region query pulled 49 MB instead of the whole
14 GiB object).

## Real workloads

Real applications on a `c8gd.4xlarge` in `us-east-1`, mounting the source
bucket with lith vs the substrates you'd otherwise stage to. Each cell is
**minutes-to-result / dollars / GB pulled from S3**; full data and rates in
[`bench/results/apps-v0.1.0.csv`](bench/results/apps-v0.1.0.csv). A copy path
must stage the whole object; lith fetches only what the app touches. (io2 is
never needed — every app's peak IOPS was far under gp3's 3000 baseline; FSx
Lustre and EFS are dominated by their minimums for these working sets.)

**Streaming** (read most of an object front to back):

| app | lith cold | lith warm | copy→gp3 | copy→NVMe |
|---|---|---|---|---|
| `samtools flagstat` (3.8 GB CRAM) | 0.79 / $0.011 / 3.6 | 0.78 / $0.010 / 3.6 | 1.26 / $0.016 / 3.6 | 0.84 / $0.011 / 3.6 |
| `fastp` (3.9 GB FASTQ pair) | 0.89 / $0.012 / 3.7 | 1.02 / $0.014 / 3.7 | 1.43 / $0.019 / 3.8 | 0.98 / $0.013 / 3.8 |
| `xarray` yearly mean (Zarr) | 0.67 / $0.009 / 0.8 | 0.09 / $0.001 / 0.8 | — impractical — | — impractical — |

*Streaming is CPU-bound at these sizes (samtools/fastp decode), so lith, which
overlaps its fetch with compute, matches or beats a stage-then-compute copy; a
local-NVMe copy ties it when the app is purely CPU-bound. For a chunked Zarr
store, copying the (multi-TB) store to read one year is infeasible — lith reads
just the year's chunks.*

**Random / selective** (touch a fraction, or seek):

| app | lith cold | lith warm | copy→gp3 | copy→NVMe |
|---|---|---|---|---|
| `tabix` 1000×10 kb regions (VCF) | 0.13 / $0.002 / 0.32 | 0.12 / $0.002 / 0.32 | 0.17 / $0.002 / 0.33 | 0.13 / $0.002 / 0.33 |
| `samtools view` 1000×1 Mb (14 GB CRAM) | 0.83 / $0.011 / **0.81** | 0.74 / $0.010 / 0.81 | 2.53 / $0.033 / **13.1** | 0.96 / $0.013 / 13.1 |
| `h5py` 500 hyperslabs (31 MB .nc) | 0.02 / $0.0002 / 0.03 | 0.004 / — / 0.03 | 0.01 / — / 0.03 | 0.004 / — / 0.03 |
| `pyarrow` predicate pushdown (2.5 %) | 0.04 / $0.0006 / 0.31 | 0.004 / — / 0.31 | 0.06 / $0.0007 / 0.39 | 0.01 / — / 0.39 |

*Selectivity is where mounting pays: reading 1000 regions of a 14 GB CRAM pulls
0.81 GB with lith vs 13.1 GB to copy the file first — 16× less data and 3× less
wall than a gp3 copy. When the object is small (a 31 MB granule) or the "random"
access actually touches most of it (a scattered but dense VCF scan), a fast
local copy ties lith on the first read; lith's warm read (near-instant, zero new
GETs) then wins every repeat query. The full-object-copy penalty and the
warm-repeat advantage are the two axes to reason about.*

Reproduce with the harness and dataset keys recorded in the
[application-benchmarks issue](https://github.com/scttfrdmn/lith/issues/61).

## Testing

Unit tests run with the race detector and touch no network:

```
make test
```

An optional end-to-end test runs only when `LITH_E2E_BUCKET` is set. It builds
an index from a real public prefix and asserts a known key resolves. It is
verified against `s3://1000genomes/changelog_details` (key
`changelog_details_20081219`):

```
LITH_E2E_BUCKET=1000genomes LITH_E2E_PREFIX=changelog_details go test ./internal/index/ -run E2E -v
```

## License

Apache-2.0. Copyright 2026 Scott A Friedman.
