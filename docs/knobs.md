# Knobs

Every flag, grouped by the trade it makes. Defaults are chosen to be right for
most workloads; each line says what the default is and why. The canonical,
never-drifting list is always `lith mount --help`, `lith index build --help`,
and `lith bench --help` — this page groups those and explains them.

## Bytes vs latency

How much lith fetches per miss. Bigger reads amortize round-trips but waste
bandwidth on bytes you won't use.

| flag | default | why |
|---|---|---|
| `--block-size` | `8MiB` | The fill/readahead unit — a run of 1 MiB cache chunks coalesced into one range GET. 8 MiB balances round-trip amortization against over-fetch. |
| `--max-range` | `64MiB` | Caps how many contiguous blocks coalesce into a single GET, so one fill can't monopolize a connection. |
| `--small-file` | `4MiB` | Files at or below this are fetched whole on first read — one GET beats seeking within a tiny object. |
| `--parts-max` | `64MiB` | Files up to this are fetched whole as **concurrent block-sized range parts** on first read (v0.2, [#69](https://github.com/scttfrdmn/lith/issues/69)); below one block it's a single GET. This is what lets a 31 MB granule beat `aws s3 cp`. `0` disables. |

## RAM vs re-fetch

How much lith keeps in memory (and optionally on disk) so a re-read is free
instead of another GET.

| flag | default | why |
|---|---|---|
| `--mem-cache` | 25 % of system RAM | The in-memory tier. 25 % leaves room for the app and the page cache; raise it for re-read-heavy work with spare RAM. |
| `--prefetch-budget` | 50 % of `--mem-cache` | Bytes prefetch may hold un-demanded. Bounding it to half the tier stopped concurrent readers thrashing a small cache ([#55](https://github.com/scttfrdmn/lith/issues/55)). |
| `--disk-cache` | `0` (off) | An on-disk second tier. Worth it only on **fast local NVMe** for working sets larger than RAM that you re-read; never on EBS/EFS/NFS. |
| `--disk-path` | `$TMPDIR/lith-cache` | Where the disk tier lives. Point it at your instance-store mount or `/dev/shm`. |
| `--disk-writers` | `4` | Write-behind workers that persist chunks off the read path, so disk writes never stall a reader. |

## Prefetch depth vs burst credits

How aggressively lith reads ahead. Deeper prefetch fills a fat NIC but can
drain a burst-credit box's credits faster than it helps.

| flag | default | why |
|---|---|---|
| `--max-readahead` | 1.5 × the bandwidth-delay product | Sequential readahead window in blocks. The 1.5× is **empirical**, measured on a `c8gd.16xlarge` ([#56](https://github.com/scttfrdmn/lith/issues/56)); a raw-BDP window left the NIC underfed. |
| `--inflight-bytes` | 2 × NIC baseline × 100 ms | Total bytes in flight to S3. NIC bandwidth is detected `ethtool` → EC2 `DescribeInstanceTypes` baseline → a fixed fallback, so burst-credit classes like `c8g` are now sized from their baseline automatically ([#79](https://github.com/scttfrdmn/lith/issues/79)); the result is cached in `nic.json` next to the index. |
| `--nic-gbps` | detect | Override the detected NIC bandwidth (Gbps) — it sizes `--inflight-bytes` and the readahead window. Use on boxes without `ec2:DescribeInstanceTypes` or a readable `ethtool` speed ([#79](https://github.com/scttfrdmn/lith/issues/79)). |
| `--s3-concurrency` | `128` | Hard cap on concurrent S3 requests. 128 won on both cold sequential and stride in the tuning grid ([#40](https://github.com/scttfrdmn/lith/issues/40)). |
| `--prefetch-concurrency` | `--s3-concurrency` | Sub-cap on concurrent prefetch fills; lower it to reserve request slots for demand reads under heavy prefetch. |
| `--sibling-readahead` | `16` | On a directory walked in key order, prefetch this many following small siblings whole (v0.2, [#63](https://github.com/scttfrdmn/lith/issues/63)) — the chunked-store path. `0` disables. |
| `--sibling-window` | `4` | How close in index order two successive opens must be to count as "walking" the directory. |

## Index

The namespace snapshot that makes metadata free.

The `s3://bucket/prefix` you pass to `mount` is the **root** of the filesystem:
the mount shows only what lives under that prefix, with the prefix stripped from
every path. An index whose own build root is that prefix — **or any parent of
it** — can serve the mount, so one wide index backs many prefix mounts
concurrently, with no rebuild. A prefix the index cannot cover (narrower than,
or disjoint from, its root), or one with no keys under it, is rejected rather
than mounted empty. `lith index inspect` prints the index's `root:`.

| flag | default | why |
|---|---|---|
| `--index-file` | (auto-build) | Load a prebuilt index. Without it, `lith mount` auto-builds from the bucket, bounded by `--auto-index-limit`. Prebuild for large buckets and reuse across nodes — one whole-bucket or parent-prefix index can back many prefix mounts at once. |
| `--auto-index-limit` | `5000000` | Max keys `lith mount` will auto-index; above this, build explicitly with `lith index build` first. |
| `lith index build --inventory` | — | Build from an S3 Inventory manifest instead of a `ListObjectsV2` pass — far cheaper for very large buckets. |
| `lith index build --shard` | — | List sub-prefixes concurrently (repeatable) to speed a build over a wide namespace. |
| `lith index build --page-size` | `1000` | `ListObjectsV2` page size. |
| `lith index refresh` | — | Re-list the bucket and rewrite the index when keys have changed (lith never mutates the bucket; the index is a private cache). |

## Access

Authentication, endpoint, and how files present to the OS.

| flag | default | why |
|---|---|---|
| `--no-sign-request` | off | Anonymous requests, for public buckets (RODA). |
| `--requester-pays` | off | Add the requester-pays header when the bucket requires it. |
| `--endpoint` | (AWS) | Point at an S3-compatible store. |
| `--path-style` | off | Path-style addressing for stores that need it. |
| `--region` | (resolved) | Resolved from the bucket if empty. |
| `--allow-other` | off | Let other users access the mount (needs `user_allow_other` in `/etc/fuse.conf`). **Security:** files are world-readable (`0444`/`0555`) with no per-object access control, so this exposes the entire mounted subtree to every local user regardless of the bucket's S3 ACLs — enable it only where that is acceptable. |
| `--uid` / `--gid` | your uid/gid | Owner reported for every file. |
| `--exec` | off | Report files as mode `0555` instead of `0444` (executables on the mount). |

## Observability

| flag | default | why |
|---|---|---|
| `--metrics` | off | Serve **Prometheus metrics only** (`/metrics`) on an address (e.g. `:9101`): cache hits by tier, S3 bytes/requests, prefetch accuracy, uncovered misses, FUSE op latency. No pprof (see `--pprof`). |
| `--pprof` | off | Serve Go `net/http/pprof` handlers on an address (e.g. `127.0.0.1:6060`). **Security:** pprof exposes the process argv (`/cmdline`) and an on-demand CPU/goroutine profiling DoS (`/profile`, `/trace`) with no auth — **bind it to localhost and never expose it to an untrusted network.** Enabling it also turns on block/mutex profiling. |
| `--timeline-csv` | off | Diagnostic ([#70](https://github.com/scttfrdmn/lith/issues/70)/[#95](https://github.com/scttfrdmn/lith/issues/95)): write a per-chunk demand-read timeline — join-wait, in-flight fill depth, and prefetch-dispatch→open lag — to this CSV on unmount. Opt-in; no effect on the read path when unset. |
