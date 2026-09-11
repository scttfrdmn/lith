# lith

Read-only, high-performance POSIX filesystem over an existing S3 bucket in its
native key layout.

`lith` presents any S3 bucket — a Registry of Open Data dataset, a lab bucket,
an instrument dump — as a read-only filesystem, using the bucket's key
namespace directly. It writes nothing to the bucket, needs no sidecar objects,
and serves all metadata (`readdir`, `lookup`, `getattr`, `open`) locally from an
index after a one-time build.

**Why "lith"?** As in *lithic* / *lithology* — rock strata. lith serves a
bucket's objects as read-only strata, exactly as they were laid down: the
bucket's native key layout is the stratum, and lith never rewrites it.

## What it is

- **Read-only.** There is no write path. Every mutating operation returns `EROFS`.
- **Native layout.** The bucket's keys *are* the filesystem. Any bucket, any producer.
- **Local metadata.** After an index build, listing and stat make zero S3 calls.
- **Mount any prefix.** Root the filesystem at any prefix below the bucket; one
  prebuilt index backs many prefix mounts at once (`lith umount`/`lith mounts`
  manage them).
- **Prefetch that knows its limits.** A single memory-tier-bounded budget feeds
  per-handle readahead, sibling readahead for chunked stores, a format-aware
  plan (Zarr chunk grid), and parallel parts for mid-size files; the in-flight
  window is sized from the NIC's baseline bandwidth.
- **Fast.** Sequential throughput near NIC line rate; random reads bounded by
  cache hit rate, not S3 request latency.
- A single static Go binary.

## What it is not

- **Not writable.** No writes, ever — not "writes disabled", writes *absent*.
- **Not a re-layout.** It does not own or rewrite the bucket's object format.
- **Linux only** (for v0.x). It relies on FUSE; macOS/Windows are out of scope.

## Install

Download the static binary for your architecture (a stable name that always
points at the latest release):

```
# arm64 (Graviton, Apple-on-Linux VMs, …)
curl -L https://github.com/scttfrdmn/lith/releases/latest/download/lith_linux_arm64 -o lith && chmod +x lith
# or x86-64
curl -L https://github.com/scttfrdmn/lith/releases/latest/download/lith_linux_amd64 -o lith && chmod +x lith
sudo mv lith /usr/local/bin/
```

Versioned `.tar.gz` archives are on the [releases](https://github.com/scttfrdmn/lith/releases)
page. Or, with Go: `go install github.com/scttfrdmn/lith/cmd/lith@latest`.

## Commands

```
lith mount   s3://bucket[/prefix] /mnt/point [flags]   # mount as a read-only filesystem
lith umount  /mnt/point [--all] [--force]              # unmount (SIGTERM, then fusermount fallback)
lith mounts                                            # list live lith mounts (and stale records)
lith index   build|refresh|inspect s3://bucket[/prefix] --index-file F
lith bench   s3://bucket/key --pattern seq|rand4k|stride [--against PATH]
lith version                                            # version, commit, build date
```

## Quick start: a public Registry of Open Data bucket

The `1000genomes` bucket is public; use `--no-sign-request` to read it without
credentials:

```
lith index build s3://1000genomes/changelog_details --index-file /tmp/1kg.lithidx --no-sign-request
lith index inspect /tmp/1kg.lithidx
sudo mkdir -p /mnt/1kg && sudo chown "$USER" /mnt/1kg   # you must own the mountpoint
lith mount  s3://1000genomes/changelog_details /mnt/1kg --index-file /tmp/1kg.lithidx --no-sign-request
ls -l /mnt/1kg
cat /mnt/1kg/changelog_details_20081219
lith umount /mnt/1kg
```

## Documentation

Full docs — how to use lith, when to use it, and when not to — are at
**[scttfrdmn.github.io/lith](https://scttfrdmn.github.io/lith/)**:

- **[Start here](https://scttfrdmn.github.io/lith/)** — five minutes to a first result on a public bucket.
- **[Copy or mount?](https://scttfrdmn.github.io/lith/copy-or-mount/)** — the decision, with the measured crossover by access shape.
- **[Sizing the node](https://scttfrdmn.github.io/lith/sizing/)** — NIC, RAM, NVMe, and lith's own CPU cost; the per-class matrix.
- **[Meet a deadline](https://scttfrdmn.github.io/lith/deadline/)** — fan-out: wider is sooner *and* cheaper with a mount.
- **[Knobs](https://scttfrdmn.github.io/lith/knobs/)** — every flag, grouped by the trade it makes.
- **[What lith is not](https://scttfrdmn.github.io/lith/not/)** — the edges and the physical limits.

Benchmark data lives in [`bench/results/`](bench/results/); the S3 client
ceiling can be reproduced independently with `cmd/lith-s3bench`.

## Real workloads

Real applications on a `c8gd.4xlarge` in `us-east-1`, mounting the source
bucket with lith vs the substrates you'd otherwise stage to. Each cell is
**minutes-to-result / dollars / GB pulled from S3**; full data and rates in
[`bench/results/apps-v0.1.0.csv`](bench/results/apps-v0.1.0.csv). A copy path
must stage the whole object; lith fetches only what the app touches. (io2 is
never needed — every app's peak IOPS was far under gp3's 3000 baseline; FSx
Lustre and EFS are dominated by their minimums for these working sets.)

**Streaming** (read most of an object front to back):

| app | lith cold | lith warm | in-place lib | copy→gp3 | copy→NVMe |
|---|---|---|---|---|---|
| `samtools flagstat` (3.8 GB CRAM) | 0.79 / $0.011 / 3.6 | 0.78 / $0.010 / 3.6 | — | 1.26 / $0.016 / 3.6 | 0.84 / $0.011 / 3.6 |
| `fastp` (3.9 GB FASTQ pair) | 0.89 / $0.012 / 3.7 | 1.02 / $0.014 / 3.7 | — | 1.43 / $0.019 / 3.8 | 0.98 / $0.013 / 3.8 |
| `xarray` yearly mean (Zarr) | 0.67 / $0.009 / 0.8 | **0.09 / $0.001 / 0.8** | **0.17 / $0.002 / 0.8** (s3fs) | — copy = whole store — | — |

*Streaming is CPU-bound at these sizes (samtools/fastp decode), so lith, which
overlaps its fetch with compute, matches or beats a stage-then-compute copy; a
local-NVMe copy ties it when the app is purely CPU-bound. For a chunked Zarr
store, staging the (multi-TB) store to read one year is infeasible, so the real
comparison is the in-place library: **`xarray`+`s3fs` reads the year's chunks in
9.9 s cold — ~4× faster than lith's 40.1 s cold**, because it fetches the many
small chunk objects concurrently while lith's per-file prefetcher pays a serial
cold time-to-first-byte per object (see [#63](https://github.com/scttfrdmn/lith/issues/63)).
lith wins the **warm** repeat (5.4 s vs `s3fs`'s ~8.6 s — `s3fs` re-fetches, lith
serves from cache). Sibling readahead (#63) targets closing lith's cold gap.*

**Random / selective** (touch a fraction, or seek):

| app | lith cold | lith warm | in-place lib | copy→gp3 | copy→NVMe |
|---|---|---|---|---|---|
| `tabix` 1000×10 kb regions (VCF) | 0.13 / $0.002 / 0.32 | 0.12 / $0.002 / 0.32 | — | 0.17 / $0.002 / 0.33 | 0.13 / $0.002 / 0.33 |
| `samtools view` 1000×1 Mb (14 GB CRAM) | 0.83 / $0.011 / **0.81** | 0.74 / $0.010 / 0.81 | — | 2.53 / $0.033 / **13.1** | 0.96 / $0.013 / 13.1 |
| `h5py` 500 hyperslabs (31 MB .nc) | 0.02 / $0.0002 / 0.03 | 0.004 / — / 0.03 | 0.02 / — / 0.03 (s3fs) | 0.01 / — / 0.03 | 0.004 / — / 0.03 |
| `pyarrow` predicate pushdown (2.5 %) | 0.04 / $0.0006 / 0.31 | 0.004 / — / 0.31 | see note | 0.06 / $0.0007 / 0.39 | 0.01 / — / 0.39 |

*Selectivity is where mounting pays: reading 1000 regions of a 14 GB CRAM pulls
0.81 GB with lith vs 13.1 GB to copy the file first — 16× less data and 3× less
wall than a gp3 copy. For the 31 MB HDF5 granule everything is sub-second;
`h5py`-over-`s3fs` in-place reads it in 1.3 s cold / 0.6 s warm, lith 0.9 s /
0.2 s — a wash on a small file. When the object is small, or the "random" access
actually touches most of it (a scattered but dense VCF scan), a fast local copy
ties lith on the first read; lith's warm read (near-instant, zero new GETs) then
wins every repeat query. The full-object-copy penalty and the warm-repeat
advantage are the two axes to reason about.*

> **Notes.** HDF5 `ros3` was not usable here — the conda-forge HDF5 1.14 build
> rejects anonymous access to the public bucket ("unauthorized"), so the in-place
> HDF5 row uses `h5py` over an `s3fs` file object. The `pyarrow` predicate-pushdown
> row is demand-only: no anonymous-listable us-east-1 Parquet dataset was found
> (nyc-tlc is us-east-1 but blocks anonymous `LIST`, so lith can't index it;
> ookla is us-west-2), and its demand is region-independent — an in-region
> in-place `pyarrow`-S3 comparison is a gap, tracked for a future run.

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
