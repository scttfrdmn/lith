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
| `--parts-max` | `auto` | Files up to this are fetched whole as **concurrent block-sized range parts** — but only **once the access pattern proves it tiles** ([#229](https://github.com/scttfrdmn/lith/issues/229)): a sequential reader establishes within the first couple of reads and then gets the whole-file parts fetch (what lets a 31 MB granule beat `aws s3 cp`); a reader that only wants a slice (a hyperslab, a footer probe) never establishes and is served byte-exact instead of pulling the whole object. The size threshold is `auto` — derived from NIC baseline × first-byte latency, clamped to `[--small-file, 64MiB]` ([#220](https://github.com/scttfrdmn/lith/issues/220)): a fat pipe would fetch whole cheaply, a thin pipe keeps a sub-file read byte-exact. An explicit size overrides; `0` disables. |
| `--bgzf-whole-file-max` | `512MiB` | For a bgzf-family data file (BAM/CRAM/VCF.gz/BCF) that has a coordinate-index sibling (`.bai`/`.tbi`/`.csi`/`.crai`), files at or below this are fetched whole through the parts path when the index opens — a region-indexed scan (e.g. `tabix` over many regions) touches nearly every block, so the whole-file working set makes cold ≈ warm (v0.3, [#107](https://github.com/scttfrdmn/lith/issues/107)). Above it, tier-2 prefetches only the index-resolved slices/chunks. `0` disables whole-file prefetch. The handle keeps its adaptive readahead window either way — the plan only *adds* ranges. |

**Cold-start cost of "prove it first" ([#229](https://github.com/scttfrdmn/lith/issues/229)).** lith will not fetch
broadly (whole-file parts, or wide readahead) until a handle's reads show they tile — the first couple of reads are
served precise. This is what keeps a hyperslab or a footer probe from pulling a whole object. The cost lands on the
opposite case: a **cold sequential read of a mid-size file** (a `cat`/`cp` off the mount) pays roughly one extra
round-trip of latency for its first block, because that block is demand-fetched in chunks before the pattern
establishes and the whole-file fetch kicks in. It is cold-only (a warm re-read is unaffected), byte-identical, and
shrinks with file size (the first block is a smaller fraction of a larger file). The block-0 coalescing gap that
would erase even this is tracked in [#233](https://github.com/scttfrdmn/lith/issues/233).

## RAM vs re-fetch

How much lith keeps in memory (and optionally on disk) so a re-read is free
instead of another GET.

| flag | default | why |
|---|---|---|
| `--mem-cache` | 25 % of system RAM | The in-memory tier. 25 % leaves room for the app and the page cache; raise it for re-read-heavy work with spare RAM. **Per-daemon:** the default is 25 % of RAM *for each mount*, so several mounts on one host add up — five mounts default to a 125 % cap. If you run more than one mount on a box, set `--mem-cache` explicitly so they sum to a sane fraction ([#242](https://github.com/scttfrdmn/lith/issues/242)). |
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
| `--nic-gbps` | detect | Override the detected NIC bandwidth (Gbps) — it sizes `--inflight-bytes`, the readahead window, **and the device-derived `--parts-max`/`--coalesce-gap`**. Detection tries `ethtool` → cache → `ec2:DescribeInstanceTypes` → an IMDS size estimate → a 10 Gbps fallback. **On AWS ParallelCluster and other least-privilege HPC roles, pass this explicitly:** ENA reports no `ethtool` speed and the generated node role omits `ec2:DescribeInstanceTypes`, so lith falls to the IMDS size estimate (approximate) — `--nic-gbps` gives the exact baseline, and `lith doctor` WARNs when the NIC is undetected ([#79](https://github.com/scttfrdmn/lith/issues/79), [#237](https://github.com/scttfrdmn/lith/issues/237)). **Pass the sustained baseline, not the "Up to N Gigabit" figure on the instance page — that is the *peak*.** Overstating it (e.g. `15` for a c7g.4xlarge whose baseline is `7.5`) over-sizes the readahead window and fetches bytes that are never read, for no speed gain; `doctor` WARNs when `--nic-gbps` looks like the advertised peak ([#239](https://github.com/scttfrdmn/lith/issues/239)). |
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
| `lith index build --keys` | — | Build from an explicit key list (one key per line; optional `\t<size>\t<mtime>`), for buckets that are GET-public but **deny `ListObjectsV2`** (Common Crawl `cc-index`, `nyc-tlc`). Keys lacking size/mtime are `HeadObject`-ed. |
| `lith index build --keys-from-manifest` | — | Same, but the key list is fetched from an `s3://` URL or local path (`.gz` transparent). |
| `lith index build --keys-allow-missing` | off | Skip keys that `HeadObject` reports 403/404 instead of failing the build. |

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

## Experimental

Off by default; enable only if you have measured a win on your own workload.

| flag | default | why |
|---|---|---|
| `--footer-tier2` | `false` | **Experimental, clustered projections only.** Byte-precise projection fetch for footer-family containers (Parquet/ORC/Arrow/zip) — a touched Parquet row group's projected column chunks, or a read zip entry + the next few in directory order ([#108](https://github.com/scttfrdmn/lith/issues/108)). Measured (sessions 29–43): it **wins on bytes for a *clustered* projection** (adjacent columns — ~10× fewer bytes than whole-file streaming) but **loses on wall-clock for a *spread* projection**, where the achievable floor is the reader's own footprint (pyarrow fetches ~the same bytes) and a whole-file stream is faster on a fat pipe (a handful of GETs at line rate vs a round-trip per column region). So it is off by default and stays experimental. Leave off unless you have a clustered projection where bytes-saved is the goal. Tier 1 (footer + head prefetch on open) always runs regardless. |
| `--readahead-evidence-ratio` | `0` (off) | **Experimental ([#256](https://github.com/scttfrdmn/lith/issues/256)).** Bound a committed readahead window to this multiple of the bytes the handle has actually read. A handle establishes at its first **block crossing** — one block (8 MiB) of contiguous evidence — and that buys the *full* NIC-derived window, ~223 blocks (~1.8 GB) on a 50 Gbps node. With ratio `k` a handle prefetches ~`k`x what it has read, so a **sequential copy still earns the full window** (after consuming `max-readahead x block-size / k`) while a reader that tiles a slab and jumps does not. **Measured on a 48-rank GCHP fullchem run:** amplification 2.425x -> **1.956x** at `k=8`, within 1.5% of what pinning `--max-readahead 1` gives (1.925x) but without pinning a global window. It does **not** improve prefetch precision — the hit rate fell 16.7% -> 14.3%, because accrued evidence is uncorrelated with whether a prefetch is used. Use it where over-fetch costs money (cross-region, requester-pays) and you do not want to cap readahead for every reader; it is not a fix for prefetch accuracy. |
| `--coalesce-gap` | `0` (derived) | Largest gap between two projection/demand fill ranges still merged into one range GET (the byte-precise fill path; only active with `--footer-tier2`). `0` derives it from the device: **NIC baseline × measured first-byte latency ÷ usable concurrency**, clamped to `[256 KiB, 64 MiB]` — a round-trip's worth of bytes amortized across the concurrent requests in flight ([#124](https://github.com/scttfrdmn/lith/issues/124)/[#31](https://github.com/scttfrdmn/lith/issues/31)). Set a fixed size to override the derivation. |

## Archives and published datasets

Mount a [CargoShip](cargoship.md) archive, or a [published dataset](published-datasets.md) by name.

| flag | default | why |
|---|---|---|
| `--cargoship` | — | Mount/serve a CargoShip 2.1 archive: build the index in-process from this manifest `s3://` URL and present the archive's original file tree (reads fetch only the covering zstd frame(s)). Fails closed — a manifest that cannot be resolved is an error, never a fallback to listing. |
| `s3://bucket/dataset@current` / `@<version>` | — | A published-dataset pointer mount (no flag): resolve the atomic `CURRENT` pointer (or a pinned version) and mount its prebuilt index. See [Published datasets](published-datasets.md). |
| `lith refresh <mountpoint>` | — | Re-read `CURRENT` and hot-swap a FUSE mount to a new version (explicit; no polling). The gateway does not hot-swap — restart it to adopt a new version. |

## Gateway (`lith serve nfs`)

| flag | default | why |
|---|---|---|
| `--listen` | `:2049` | Listen address for the NFSv3 + embedded MOUNT service. The gateway never registers with portmap/rpcbind; clients mount with an explicit `port=`/`mountport=`. |
| `--client-idle` | `5m` | Release a mounted client's share of the read-ahead budget after it has been idle (since its last MOUNT) this long. |

## Operational and diagnostics

| flag / command | default | why |
|---|---|---|
| `--log-level` | `info` | Log verbosity: `debug`/`info`/`warn`/`error` (on `mount` and `serve nfs`). `debug` surfaces the format-plan diagnostics. |
| `--daemon` | off | `lith mount` forks into the background once the mount is ready. |
| `lith doctor [s3://…] --mountpoint` | — | Diagnose whether lith will work here — credentials, bucket access/region, LIST, FUSE, and the given `--mountpoint` (exists, a dir, owned by you, empty). Exits non-zero on any failure. See [the doctor](#the-doctor). |
| `lith mounts --prune` | off | List active mounts; `--prune` drops stale records whose process is gone. |
| `lith umount --all` / `--force` / `--timeout` | — | Unmount one, or `--all`; `--force` lazy-unmounts a busy mount; `--timeout` bounds the wait. |

### The doctor

`lith doctor` runs the checks above (add a bucket to include bucket/region/LIST/pointer checks, `--index-file` to validate an index) and prints each as `PASS`/`FAIL`/`N/A` with a one-line fix. Run it first on a new box — it catches the failures that otherwise surface as a cryptic mount error (wrong region, a root-owned mountpoint, a LIST-denied bucket, missing FUSE).

## Benchmark (`lith bench`)

| flag | default | why |
|---|---|---|
| `--against` | — | Comparison reader to run alongside (e.g. `mountpoint-s3`) for an apples-to-apples table. |
| `--pattern` | — | Access pattern to exercise (`seq`, random, projection, …). |
| `--ops` / `--runs` | — | Operations per run / number of runs. |
| `--readers` | `1` | Concurrent readers (multi-reader mode; requires `--objects`). |
| `--objects` | — | Comma-separated keys for multi-reader mode. |
| `--cache-dir` | `$TMPDIR` | Directory for the bench block cache. |
