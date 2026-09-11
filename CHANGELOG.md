# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- **Sparse chunk fills ([#118](https://github.com/scttfrdmn/lith/issues/118)).**
  The 1 MiB cache chunk gains a 64 KiB-granularity filled-extent bitmap. A
  format plan's byte-exact range (e.g. a Parquet column projection) and a
  non-sequential point read now fetch only the extents they cover, not the whole
  enclosing chunk; sequential/streaming reads still fill whole chunks. The disk
  tier persists the bitmap (partial chunks are valid). New metrics
  `lith_fill_partial_total` and `lith_fill_bytes_total{kind=plan|demand|whole}`.

### Changed

- **`lith_distinct_bytes_read` now counts filled 64 KiB extents, not 1 MiB
  chunks** (it was chunk-rounded). Values are finer-grained (and smaller) than
  before for sub-chunk access ([#118](https://github.com/scttfrdmn/lith/issues/118)).
- The disk block-cache format bumped to **v2** (per-chunk extent bitmap); it is a
  private cache — older ones are ignored and rebuilt.

## [0.2.2] - 2026-09-10

Security release: two internal audit passes, fully remediated. Defensive hardening
only — no on-disk format change, no new mechanisms.

### Security

Remediation of an internal security audit (defensive hardening; no format change).

- **`lith umount` no longer signals an unverified PID.** A mount record's PID is
  signalled only if it is genuinely among the processes holding the mount open
  (`fuser -m`), and mount records are read only from a runtime dir the current
  user owns; otherwise umount falls through to `fusermount3 -u`. This closes a
  local-multi-user vector where a planted record in a world-writable fallback dir
  could induce `lith umount` (esp. as root) to SIGTERM an arbitrary PID.
- **Runtime records and the disk cache are created private and ownership-checked.**
  The per-user runtime dir and the disk-cache dir are created `0700` and refused
  if they are a symlink or not owned by the current user; mount records are
  written `0600`; the daemon log is per-uid, `0600`, and opened `O_NOFOLLOW`.
- **`--endpoint` may not receive signed requests over plaintext.** A non-`https`
  endpoint is rejected unless `--no-sign-request` is set, so SigV4-signed requests
  (and the STS session token) are never sent in cleartext to an arbitrary host.
- **Ranged GETs are validated.** A response that ignores the requested byte range
  (returns the whole object) is rejected via its `Content-Range`, so offset-shifted
  data can never be served as the requested range.
- **`lith mounts`/`umount --all` match the exact `fuse.lith` fstype**, no longer a
  substring, so unrelated FUSE mounts (`fuse.monolith`, …) are never touched.
- **System helpers are resolved from a fixed trusted path** (`/usr/bin`, `/bin`,
  `/usr/sbin`, `/sbin`) rather than `$PATH`.
- **Keys containing C0 control bytes are rejected** at index build.
- **The index is memory-mapped `MAP_PRIVATE`**, immune to post-validation mutation
  of the backing file.
- **The HTTPS-endpoint guard now covers the environment**, not just `--endpoint`:
  a non-`https` endpoint resolved from `AWS_ENDPOINT_URL`/`AWS_ENDPOINT_URL_S3`/
  shared-config `endpoint_url` is also rejected when signing, so credentials can't
  reach a cleartext endpoint via the standard SDK config path.
- **`--metrics` no longer serves pprof.** `net/http/pprof` (which exposes the process
  argv and an on-demand CPU/goroutine profiling DoS) moved to a separate opt-in
  `--pprof <addr>` flag (off by default; bind to localhost). `--metrics` serves only
  `/metrics`, and block/mutex profiling is enabled only when `--pprof` is set.
- **The NIC-bandwidth cache (`nic.json`) is written safely.** It is now a per-uid
  file created with `O_EXCL|O_NOFOLLOW` (`0600`) and read only when it is a regular
  file owned by the current user — closing a symlink/pre-created-file overwrite in
  the predictable `$TMPDIR` path and a cross-user sizing-poisoning read.
- **`ethtool` and the bench `ss` helper are resolved from the fixed trusted path**
  (as `fusermount3`/`fuser` already were).
- **Supply chain:** CI now runs `govulncheck`; all GitHub Actions are pinned to
  commit SHAs and `goreleaser-action` to a fixed version; `nic*.json` is gitignored.

### Fixed

- **Truncated S3 responses are no longer cached or served as valid zeros.** A short
  chunk read (dropped/partial transfer) now fails the fill instead of completing a
  zero-padded buffer that was persisted to the memory and disk tiers and served as
  authoritative on every later read.
- **A crafted directory offset can no longer crash the mount.** `Readdir` clamps an
  out-of-range cursor and the FUSE readdirplus path no longer underflows, closing a
  local denial-of-service (out-of-bounds panic). FUSE query handlers also recover a
  panic into `EIO` rather than tearing down the mount for all users.
- **The S3 Inventory parser is bounded.** Manifest, per-file decompression, and total
  entry count are capped, so a hostile or malformed inventory (gzip bomb, giant
  manifest) errors instead of exhausting memory. Inventory keys are unescaped with
  `PathUnescape` (a literal `+` is preserved) and a negative object size is rejected.
- **`--timeline-csv` output is injection-safe.** The diagnostic CSV is now written with
  `encoding/csv` and cells beginning with `= + - @` (or tab/CR) are prefixed with `'`,
  so an S3 key name cannot corrupt the CSV structure or become a live spreadsheet formula.
- **`parseSize` rejects `Inf`/`NaN`/negative/overflowing values** instead of silently
  yielding a garbage size.
- **The daemon log is only appended to a file the current user owns** (an attacker
  pre-creating a regular file at the predictable path is now refused), and the mount-record
  write closes a `/tmp`-fallback symlink race (`Mkdir`+re-check, `O_NOFOLLOW`).

## [0.2.1] - 2026-09-10

Hardening and docs currency from an external review of v0.2.0. No new mechanisms.

### Fixed

- **The release workflow no longer publishes on a red commit** (#98): the tag
  build now depends on a `gate` job that reruns the full CI suite (vet, lint,
  `go test -race`) for the tagged SHA; goreleaser runs only if it passes. A tag
  push does not itself trigger CI, so the gate lives in the release workflow.
- **Flaky prefetch test** (#98): `TestPrefetchAheadCoversDemand` and
  `TestMidFillUnblock` synchronized on the fake S3 signalling a GET in flight
  (new `OnGetStart` hook) instead of sleeping; `-race -count=20` clean.
- **`View.TotalSize` data race** (#102): the per-mount-root size is summed once
  at `Root()` and read as an immutable field, so concurrent `StatFs` on a
  prefix view is race-free (was a lazily-set field with no lock).
- **The index deserializer now validates every length and arena offset against
  the image before slicing** (#101): a malformed image returns `ErrCorruptIndex`
  naming the byte offset instead of panicking. A fuzz test over the parser
  (truncated + bit-flipped corpus) runs as a regression corpus in CI.
- **Install one-liner** (#99): releases now also publish stable-name raw
  binaries (`lith_linux_amd64`, `lith_linux_arm64`) alongside the versioned
  `.tar.gz` archives, so `curl -L …/releases/latest/download/lith_linux_amd64`
  works across releases.

### Changed

- **Index format → v3** (#101): the arena offset arrays (`offs`/`dirOffs`) widen
  from `uint32` to `uint64`, lifting the ~4 GiB pathname-arena cap (measured
  cost: +4.0 bytes/key, ~4.6%; the zero-copy mmap path is unchanged). v2 index
  files are rejected on load with the rebuild message, as v1 → v2.
- **Docs currency** (#100): Zarr numbers reconciled to the shipped tier-1
  figures (~35 s cold vs ~25 s in-place s3fs, same box; grid-aware prefetch is
  shipped, not future work), README command table and feature list updated to
  v0.2, a "Why 'lith'" note added to the README and docs, and every page walked
  against `--help` and the CHANGELOG (`mkdocs build --strict`).

### Security

- **SECURITY.md** (#100): dropped the `security@example.com` placeholder;
  vulnerability reports go through GitHub private vulnerability reporting.

## [0.2.0] - 2026-09-10

### Added

- **Mount root at a prefix** (#90): `lith mount s3://bucket/some/prefix /mnt`
  roots the filesystem at `some/prefix/`, showing only what lives under it with
  the prefix stripped from every path. An index built at that prefix — or any
  parent of it, including a whole-bucket index — serves the mount with no
  rebuild, so **one index can back many prefix mounts concurrently**. A prefix
  the index cannot cover, or one with no keys under it, is rejected rather than
  mounted empty. `lith index inspect` now prints the index `root:`.
- **`lith umount` and `lith mounts`** (#91): `lith umount <mountpoint>` signals
  the mount process and waits for it to leave the mount table, falling back to
  `fusermount3 -u` (and `-uz` lazy only with `--force`); a busy mount is
  reported with the holding pids and refused without `--force`. `--all`
  unmounts every lith mount for the user. `lith mounts` lists live mounts
  (cross-checked against `/proc/mounts`) and stale records, with `--prune`.
- **Sibling readahead for chunked stores** (#63): when successive opens in a
  directory are close in index order (a detected directory walk), lith
  prefetches the next N siblings whole — turning many-small-object access (Zarr
  chunks, WebDataset shards, per-chromosome BAM) into large-object streaming,
  lith's best shape. New `--sibling-window` (default 4, the max index-position
  gap that still counts as walking) and `--sibling-readahead` (default 16;
  0 disables).
- **Parallel parts for mid-size files** (#69): a file at or below `--parts-max`
  (default 64 MiB) is fetched on first read as concurrent block-sized range GETs
  instead of one serial stream, so a mid-size file reaches full bandwidth
  without waiting on a single sequential fill.
- **Zarr grid-aware readahead — the first format-aware access plan** (#70,
  tier 1): on a `.zarray`/`.zmetadata`-shaped store, sibling readahead follows
  the chunk grid along the walked axis instead of flat key order, so
  grid-boundary jumps no longer reset the walk. On the NWM `chrtout.zarr` year
  read this halved the uncovered misses (114 → 64) with 100%-used prefetch.
- `lith mount --timeline-csv <path>`: an **opt-in** per-chunk diagnostic
  timeline (per demanded chunk: join-wait, in-flight fill depth, and the lag
  from a covering prefetch's dispatch to the app's open) for prefetch
  investigations. No effect on the read path when unset (#95, #70).
- `--nic-gbps` on `lith mount` overrides the detected NIC bandwidth (which sizes
  `--inflight-bytes` and the readahead window) (#79).
- **Documentation site** (mkdocs-material, #80): Start here, Copy or mount?,
  Sizing, Deadline, Knobs, and What lith is not.

### Changed

- **The in-flight budget is now sized from NIC baseline bandwidth** (#79):
  NIC detection is ethtool → EC2 `DescribeInstanceTypes` baseline → a fixed
  fallback (was ethtool → 512 MiB), and both the `--inflight-bytes` default and
  the readahead window derive from the baseline. This fixes burst-credit
  instance classes (e.g. `c8g`) the old bandwidth table missed and silently
  defaulted to 512 MiB in-flight. The NIC result is cached in `nic.json` next to
  the index so repeat mounts and boxes without `ec2:DescribeInstanceTypes` still
  get a real answer.
- **Unified prefetch limits** (#64): per-handle readahead, sibling readahead,
  and the parts path now draw on one shared prefetch budget and query the index
  for neighborhoods through a single policy, so aggregate readahead stays
  bounded by the memory tier across all three paths (extends #55).

## [0.1.0] - 2026-09-08

### Changed

- The block cache is now **chunk-granular**: a fixed 1 MiB chunk is the cache
  unit, and `--block-size` (default 8 MiB) is the fill/readahead unit (a run of
  chunks coalesced into one range GET). The on-disk cache layout is versioned
  (`chunkv1`); caches written by earlier builds are ignored, not misread.
- `lith bench` now reads through an actual lith mount via `pread`, so the FUSE
  per-handle prefetcher is exercised (it previously read the block store
  directly and never prefetched).
- `--mem-cache` now defaults to **25% of system memory** (from `/proc/meminfo`,
  1 GiB fallback), replacing the fixed 1 GiB default; `--disk-cache` stays off
  by default. `lith mount` warns if the `--disk-cache` path resolves onto the
  root filesystem or a network volume. README gains a "No NVMe?" section.
- `lith bench` gains `--runs N` (N cold runs; min/median/max reported, then a
  warm run), `--readers N --objects k1,k2,...` (concurrent multi-object
  readers; aggregate MB/s and S3 request count), per-read latency percentiles
  split at the first readahead window, time-to-first-byte, and `--cache-dir`.
- Default `--s3-concurrency` raised to **128** and `--max-readahead` to **64**
  (from 64 and 32). On a c8gd.4xlarge in-region against `s3://1000genomes`,
  128/64 won on both cold sequential (1248 MB/s vs 1126 at 64/32) and cold
  stride, so it wins on both workloads the tuning grid measured.
- Index file format bumped to **v2**: the directory table now stores a
  per-directory inode and mtime. v1 index files are rejected on load with a
  message to rebuild.

### Changed

- A read contained within one chunk returns a sub-slice of the (immutable,
  cached) chunk buffer directly to the kernel — no allocation and no copy on a
  cache hit — and go-fuse splices the reply zero-copy. Boundary-straddling
  reads still assemble and are counted in `lith_read_straddle_total` (≈0 for
  sequential reads, which fall entirely within a chunk).
- The memory tier is **sharded** into 64 independently-locked 2Q shards, keyed
  by chunk hash. Under concurrent readers this removes the single memory-tier
  mutex as a serialization point (profiling showed ~all mutex delay there,
  held during an O(n) eviction scan).
- New `--inflight-bytes` budget bounds total bytes in flight to S3 (default
  2 × NIC bandwidth × 100 ms, detected via `ethtool`; 512 MiB fallback).
  `--s3-concurrency` remains a request-count hard cap.
- The disk cache tier is now **write-behind**: a fill completes its readers as
  soon as the bytes are in the memory tier, and the disk write is handed to a
  bounded pool (`--disk-writers`, default 4) off the fill path. Memory chunks
  with a pending disk write are pinned so they are not evicted before the write
  lands. The metrics endpoint (`--metrics`) also serves Go `pprof`.

### Fixed

- **Aggregate readahead is now bounded by the memory tier**, so concurrent
  readers no longer thrash a small cache (#55). Each open handle's readahead
  window is capped at `prefetch-budget / open-handles` (floor 2, capped by
  `--max-readahead`), and memory-tier eviction prefers already-read chunks over
  prefetched-but-unread ones. On a 16 GiB `c8g.2xlarge`, the cold 8-reader
  aggregate went from 180 MB/s (prefetch thrash: prefetched chunks evicted
  before use and re-fetched) to **1550 MB/s, 97% of mountpoint-s3**; a 32 GiB
  `c8gd.4xlarge` went 1095 → 1688 (65% → 103%). New `--prefetch-budget`
  (default 50% of `--mem-cache`) and metric `lith_prefetch_evicted_unread_total`
  (the thrash signal, ~0 after the fix).
- **Single-reader cold sequential now fills a fat NIC.** `--max-readahead`
  defaults to 1.5× the bandwidth-delay product (`inflight-bytes / block`) instead
  of a fixed 64 blocks, so one reader holds enough in flight to saturate the
  link. On a 30 Gbps `c8gd.16xlarge`, cold single-reader went 1737 → ~2850 MB/s
  (67% → ~109% of mountpoint-s3) (#56). The **1.5× multiplier is empirical, not
  theory** — derived from measurement on that box: a raw-BDP window (~89 blocks)
  reached only 88% of mountpoint-s3 because completed chunks sit cached-unread
  ahead of the cursor, so the bytes actually in flight are less than the window;
  1.5× closes the gap. Re-derive if the workload or instance profile changes.

- The per-handle prefetcher is now **tolerant of out-of-order reads**. The kernel
  issues a single file handle's readahead concurrently, so reads can reach the
  FUSE layer out of order even for a strictly sequential file; the old detector
  treated any non-unit delta as a pattern break and collapsed the readahead
  window to zero, then re-ramped from 2. Under 8 concurrent readers this left the
  last, largest, slowest-draining file with an oscillating shallow window — the
  bimodal ~180 MB/s cold tail (#49). The detector now treats a read within the
  reorder band `[cursor-window, frontier+window]` as sequential progress; a
  genuine out-of-band seek **halves** the window (floor 2) and re-anchors rather
  than resetting to zero, and only two seeks with no progress between them fall
  to random. New metrics `lith_prefetch_window_halved_total` and
  `lith_prefetch_reset_random_total`. See #49, #40.
- Prefetch now dispatches the readahead window **ahead of the demand cursor** —
  on `open` (initial 2-block window) and on every window advance — instead of
  reactively on the read that reveals a gap. A demand read that finds its chunk
  neither cached nor in flight is counted as `lith_prefetch_uncovered_total`.
- Cold read throughput: a single chunk singleflight keyed by
  `(key, etagHash, chunkIdx)` with in-flight join replaces the range-keyed
  singleflight. A demand read for a chunk a prefetch is already fetching now
  joins that fetch (completing as the chunk's bytes arrive) instead of issuing a
  duplicate GET — eliminating the ~11× GET amplification observed in session 2.
- Random 4 KiB reads now fetch a single 1 MiB chunk instead of a whole 8 MiB
  block.
- Directory and file inodes now share a single build-time collision namespace,
  so a directory inode can no longer silently collide with a file inode;
  colliding directory inodes take the sequential fallback.
- Directory mtime is the maximum `LastModified` over the directory's entire
  subtree (falling back to the index build time when no descendant is dated),
  replacing the previous zero value.

### Added

- `--prefetch-concurrency` on `lith mount` and `lith bench` caps concurrent
  prefetch fills; it defaults to `--s3-concurrency` (previously the prefetch
  sub-limit was hardwired to half of `--s3-concurrency`, capping concurrent
  fills at 64 at the default and holding the 8-reader aggregate near the
  ~64-connection ceiling). Isolating this one change lifted the cold 8-reader
  aggregate on a c8gd.16xlarge from ~2300 to ~3000 MB/s. See #40.
- `--max-range` on `lith bench` (already present on `lith mount`): caps the
  coalesced range-GET size; the bench previously coalesced at most one block.
- `cmd/lith-s3bench`: a standalone diagnostic that isolates the S3
  client/transport (N workers, back-to-back ranged GETs, no cache/prefetch/
  FUSE). It established that the Go client sustains ~3.5 GB/s at 128 concurrent
  GETs on a 30 Gbps box — above mountpoint-s3 — so the multi-reader ceiling is
  in lith, not the transport. Tooling; not part of the shipped filesystem.
- `lith mount s3://bucket[/prefix] /mnt/point`: a read-only FUSE mount served
  from the local index and a tiered block cache. Flags include `--index-file`
  (auto-built under `--auto-index-limit` when absent), `--mem-cache`,
  `--disk-cache`/`--disk-path`, `--block-size`, `--max-range`,
  `--s3-concurrency`, `--max-readahead`, `--small-file`, `--metrics`,
  `--allow-other`, `--uid`/`--gid`, `--exec`, and `--daemon`. Mutating
  operations return `EROFS`; clean unmount on SIGINT/SIGTERM.
- `lith bench s3://bucket/key --pattern seq|rand4k|stride [--against PATH]`:
  measures MB/s, IOPS, p50/p99, S3 requests and bytes, and an estimated cost,
  cold then warm, optionally alongside another mount (e.g. mountpoint-s3).
- Read path: a 2Q memory + NVMe disk block cache with range coalescing,
  singleflight, a demand-favoring worker pool, a per-handle prefetcher
  (sequential/strided/random), and ETag-mismatch detection (`EIO`).
- Prometheus metrics endpoint (`--metrics`): cache hits by tier, S3 bytes and
  requests, in-flight gauge, prefetch accuracy, stale objects, and FUSE op
  latency histograms.
- Repository scaffold: Go module `github.com/scttfrdmn/lith`, internal package
  layout (`index`, `s3client`, `blockstore`, `prefetch`, `fuse`, `version`),
  and a `Makefile` with `build`, `test`, `lint`, `bench`, and `cover` targets.
- `lith version` command printing the version, commit, and build date, injected
  at build time via `-ldflags`.
- CLI skeleton (`cobra`): `mount`, `index`, and `bench` subcommands are present
  and report "not implemented in this build" pending M1/M2.
- Continuous integration (`go vet`, `golangci-lint`, `go test -race`, and
  `linux/amd64` + `linux/arm64` builds) and a tag-triggered goreleaser release
  workflow producing static binaries and a checksums file.
- Project documentation: `README`, `CONTRIBUTING`, `SECURITY`,
  `CODE_OF_CONDUCT`, issue templates, and a pull-request template.
- Dependabot configuration for Go modules and GitHub Actions.
- Namespace index: an immutable, sorted-arena snapshot of a bucket with local
  `Lookup`, `Stat`, and constant-memory paginated `Readdir`; directory
  structure is derived from sorted keys rather than stored. Includes POSIX key
  sanitization, directory-shadows-object resolution, xxh3 inode assignment with
  collision fallback, and a versioned, memory-mappable on-disk format.
- Index builders: from a paginated `ListObjectsV2` pass (with optional
  concurrent prefix sharding) and from an S3 Inventory manifest (CSV).
- Tuned aws-sdk-go-v2 S3 client: region resolved from the bucket, HTTP/1.1 with
  a large keep-alive connection pool, and `--no-sign-request`,
  `--requester-pays`, `--endpoint`, and `--path-style` options.
- `lith index build|refresh|inspect` commands.
- In-process fake S3 (ListObjectsV2/HeadObject/GetObject with Range) backing all
  unit tests, which run with the race detector and touch no network.

[Unreleased]: https://github.com/scttfrdmn/lith/compare/v0.2.2...HEAD
[0.2.2]: https://github.com/scttfrdmn/lith/compare/v0.2.1...v0.2.2
[0.2.1]: https://github.com/scttfrdmn/lith/compare/v0.2.0...v0.2.1
[0.2.0]: https://github.com/scttfrdmn/lith/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/scttfrdmn/lith/releases/tag/v0.1.0
