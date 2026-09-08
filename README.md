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
| c8g.2xlarge | 16 GiB | no | 1321 / 1584 | 1274 | **180** / 1657 ⚠ | 0.16 s, 0 S3 | 543k IOPS |
| c8gd.4xlarge | 32 GiB | yes | 1406 / 1569 | 1428 | 1095 / 1683 | 0.17 s, 0 S3 | — |
| c8gd.8xlarge | 64 GiB | yes | 1361 / 1581 | **19214** | 1696 / 1686 | 0.16 s, 0 S3 | 547k IOPS |
| c8gd.16xlarge | 128 GiB | yes | 1970 / 2652 | **20845** | 3245 / 3270 | 0.16 s, 0 S3 | 617k IOPS |
| c8i.4xlarge (x86) | 32 GiB | no | 1295 / 1545 | 1402 | 1137 / 1683 | 0.12 s, 0 S3 | 687k IOPS |
| m8g.4xlarge | 64 GiB | no | 1398 / 1611 | **20408** | 1703 / 1704 | 0.16 s, 0 S3 | 518k IOPS |

**Reading guide: cold sequential is your NIC; warm and random are your cache;
metadata is free.** With RAM ≥ ~2× your working file, warm reads come from cache
at 19–21 GB/s and the 8-reader aggregate reaches mountpoint-s3 parity (it is
**RAM**, not local NVMe, that determines multi-reader health — the one
pathological cell, c8g.2xlarge at 180 MB/s, is a 16 GiB box whose cache is
smaller than eight readers' combined prefetch window, with no disk tier to spill
to). Metadata is served entirely from the local index: 100k `stat`s in ~0.15 s
with **zero** S3 requests on every class. Cold sequential tracks the instance
NIC baseline; mountpoint-s3 rides burst credits higher on the smaller boxes and
converges with lith on the sustained-bandwidth 16xlarge and on multi-reader.

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
