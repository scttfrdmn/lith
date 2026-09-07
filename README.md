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
