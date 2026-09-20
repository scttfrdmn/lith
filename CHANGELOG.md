# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- **`cmd/lith-pfreplay`: replay a `--pf-trace` and score per-handle byte
  follow-through offline** ([#256](https://github.com/scttfrdmn/lith/issues/256)).
  Four hypotheses for #256 each cost a ~35-minute 48-rank cluster job to refute.
  This replays a trace through the real `internal/prefetch` detector, recovers the
  blocks it dispatched, and scores what fraction of those **bytes** the same handle
  later read — so a candidate policy costs a replay instead of a run. Not a shipped
  binary (goreleaser builds only `./cmd/lith`).

  Dispatched blocks are **clamped at EOF** exactly as `store.Prefetch` clamps them:
  without that, a deep readahead window against a small object counts blocks that
  fetched nothing (measured at ~94% of the denominator in one case), dragging every
  score to ~0 and reading as "prefetch never pays off". The cold-start tax is
  measured **net** of bytes the same handle later reads, and first-run `cold` is
  split from re-entries — both corrections requested after the first version
  inflated a streaming arm's waste by hundreds of MiB.

  Fidelity is reported first and **labelled for what it does not prove**: the
  state-machine check compares `Observe` against a trace column that also came from
  `Observe`, so it cannot catch a byte-accounting error. A denominator sanity check
  and an optional `-issued` cross-check against the live counter are the independent
  ones. The trace gained a `size` column so the clamp is exact rather than estimated.
  The scoring rule is the one agreed and pre-registered with the reporting workload
  before either side had data — bytes not chunk touches, Spearman |ρ| ≥ 0.5 fit on
  one arm and tested on another, no-separation iff |ρ| < 0.5 **and** rank-sum
  AUC < 0.7 — so it cannot be tuned to the answer. It also counts the #256
  cold-start granularity tax directly from the trace.

- **`--pf-trace`: the access-pattern detector's decisions, recorded and replayable**
  ([#262](https://github.com/scttfrdmn/lith/issues/262)). The trace behind
  `LITH_PF_TRACE` has existed for a while but could not answer what it was needed
  for, because rows carried the object key and **not the file handle** — lith builds
  one prefetcher per `open`, so the handle is the unit that makes decisions, and
  concurrent handles on one object interleaved indistinguishably (dozens of them
  under a many-rank job through one daemon). Rows now carry `fh` and `pid`.

  It was also a **silently biased sample**: reads served while a whole-file parts
  fetch was in flight, or by a footer handle's byte-exact plan, were dropped with no
  marker — and those are a large, non-random share of the bytes wherever
  [#229](https://github.com/scttfrdmn/lith/issues/229) whole-fetches large objects.
  A test case here drops **72%** of its reads under the old behaviour. Every read is
  now recorded with a `path` column (`window` / `parts` / `footer`) saying which
  path served it.

  Rows gained `window` and `dispatched`, and the file gained a `#` header line
  recording the config that produced it (block size, max-readahead, parts-max,
  small-file, coverage gate, evidence ratio), so a trace is self-describing,
  comparable across runs, and an offline replay can be **validated for fidelity**
  before its verdict on a new policy is trusted. A failed trace-file create is now
  logged instead of silently yielding an empty trace and a successful-looking run.
  Documented in `docs/knobs.md`, with its cost stated: unbounded, and serialized
  under one mutex, so it adds a global lock to every read — characterization, not
  production.


- **`--readahead-evidence-ratio`, experimental and off by default**
  ([#256](https://github.com/scttfrdmn/lith/issues/256)). Bounds a committed
  readahead window to a multiple of the bytes a handle has actually read. A handle
  establishes at its first **block crossing** — one block (8 MiB) of contiguous
  evidence — and today that buys the full NIC-derived window, ~223 blocks
  (~1.8 GB) on a 50 Gbps node. With a ratio of `k` a handle prefetches ~`k`× what
  it has read, so a sequential copy still earns the full window (once it has
  consumed `max-readahead × block-size ÷ k`) while a reader that tiles a slab and
  jumps does not.

  **What it does and does not do, measured on the reporting workload** (GCHP
  fullchem, 48 ranks, PR binary with the flag unset as the control): it cuts
  amplification **2.425× → 1.956×** at `k=8`, which is within 1.5% of what pinning
  `--max-readahead 1` achieves (1.925×) — but it gets there *without* pinning a
  global window, so a sequential reader can still earn the full one. It does
  **not** improve prefetch precision: the hit rate went **16.7% → 14.3%**, because
  accrued evidence turns out to be uncorrelated with whether a prefetch is used.
  So this is an ergonomics and safety improvement over telling operators to pin
  `--max-readahead`, and **not** an answer to #256's precision question, which
  stays open. Off by default; with the flag unset the read path is unchanged
  (reproduced to 0.5% on the cluster).
- **`lith_prefetch_deestablished_total{size_class}`**
  ([#256](https://github.com/scttfrdmn/lith/issues/256)) counts how many times a
  handle lost an establishment it had. It is **not**
  `lith_prefetch_reset_random_total`, which counts collapses to the Random *state*
  and is 20–30× larger (307–332 per run against 10–17 on the same mount) — a
  distinction that matters, because conflating them made a re-establishment cap
  look worth building and it measured completely inert. Kept because the counter
  separates mounts in the *opposite* direction to the obvious guess: the mount with
  an 80% prefetch hit rate de-establishes **51** times (45% of its Random
  collapses) while the pathological one de-establishes **10** (3%). Frequent
  re-anchoring marks a reader whose prefetch works, not one whose prefetch is
  wasted.

- **`lith_prefetch_evidence_clamped_total{size_class}` and
  `lith_prefetch_evidence_withheld_blocks_total`** make the gate's action
  observable. Without them the only visible effect was that `prefetch_issued`
  fell, which cannot distinguish a window the gate refused from one the detector
  never wanted — and cannot answer whether the gate fires on the large objects or
  the small ones.

### Fixed

- **`--pf-trace` was accepted, documented, and silently ignored**
  ([#264](https://github.com/scttfrdmn/lith/issues/264)). The flag bound a field
  that never reached `fuse.Config`, so it produced no trace, no error, and exit 0 —
  and the docs guard passed because the flag *was* documented. Now wired, with a
  new guard (`TestEveryFlagBindingIsConsumed`) that walks the AST for flag
  registrations and fails when a bound field is never read anywhere else in the
  package. It catches exactly this: reverting the one-line fix makes it name
  `--pf-trace`.
- **`lith_prefetch_used_total` now says it is a chunk-touch count, not a byte
  count** ([#256](https://github.com/scttfrdmn/lith/issues/256)). A 64 KiB demand
  read marks the whole 1 MiB chunk *used*, so `used/issued` **overstates byte
  follow-through — and overstates it most for the scattered readers where prefetch
  is least useful.** A measured case reported **89%** by this ratio while at most
  **~25%** of the prefetched bytes were ever read. Every prefetch-efficiency number
  in the #256 campaign, including the ones used as kill conditions, was scored on
  this friendlier unit. The metric help and `docs/knobs.md` now say so, and point at
  `lith_fill_bytes_total{kind=...}` and `--pf-trace` for byte-level answers. No
  behaviour change — the counter is unchanged, its description was wrong.
- **Labelled prefetch counters now emit their series at zero**
  ([#256](https://github.com/scttfrdmn/lith/issues/256)).
  `lith_prefetch_evidence_clamped_total` was only created when it fired, so at zero
  it emitted **no series at all** — indistinguishable in a scrape from "the binary
  lacks the feature", "a different label value", or "the mount was never opened". A
  reporter had to infer a zero from an absent line and could only confirm it because
  another arm proved the same binary did emit the series when it fired. This is the
  same present-and-zero-versus-absent trap as
  [#253](https://github.com/scttfrdmn/lith/issues/253), in a metric added to *fix*
  an observability complaint, so it now has a test.


- **GitHub releases carry release notes again.** `.goreleaser.yaml` disables
  goreleaser's git-log changelog (this hand-written file is the source of truth)
  but nothing supplied notes in its place, so every release from **v0.5.0** on
  (`v0.5.0`, `v1.1.0`–`v1.1.3`) published with an **empty body**. The release
  workflow now extracts the tag's section from this file
  (`scripts/release-notes.sh`) and passes it to goreleaser as `--release-notes`,
  **fails the release** when the section is missing, and **asserts the published
  body is non-empty** afterwards. The five affected releases were backfilled from
  this file, so every published release now carries its notes.
- **Two stale instrumentation strings that misled a bug reporter**
  ([#256](https://github.com/scttfrdmn/lith/issues/256)). `--parts-max`'s help still
  said files are fetched whole **"on first read"**, which has been untrue since
  [#229](https://github.com/scttfrdmn/lith/issues/229) gated the parts fetch on the
  handle's reads proving they tile — it reads as contradicting the documented
  behaviour, and cost a reporter a round trip to resolve. And
  `lith_prefetch_issued_total`/`_used_total` described themselves as counting
  **blocks** when they are recorded per **1 MiB chunk**, so a reader converting the
  counter to bytes had to guess the unit. Both corrected; no behaviour change.

## [1.1.3] - 2026-09-17

A gateway-observability fix from the GCHP shared-gateway validation
([#244](https://github.com/scttfrdmn/lith/issues/244)): the gateway's
distinct-bytes counter now advances, so a shared gateway's byte-dedup
amplification is computable from its own metrics endpoint.

### Fixed

- **`lith_distinct_bytes_read` now advances in `serve nfs` mode**
  ([#253](https://github.com/scttfrdmn/lith/issues/253)). The gateway read path
  never called `MarkDistinctRead`, so the counter stayed 0 — and because it was
  present-and-zero, the gateway amplification ratio `s3_bytes / distinct_bytes`
  read as a *perfect* result for an unmeasured gateway rather than failing
  visibly. The NFS read path now accounts distinct object bytes the same way the
  FUSE path does, so gateway amplification is computable from the gateway's own
  metrics endpoint (the counter [#250](https://github.com/scttfrdmn/lith/issues/250)
  needs).

## [1.1.2] - 2026-09-17

The concurrency fix from the GCHP project's shared-gateway test
([#244](https://github.com/scttfrdmn/lith/issues/244)) — `serve nfs` no longer
returns spurious `NFS3ERR_STALE` under concurrent readers — plus the enhancements
and observability from its end-to-end integration run
([#210](https://github.com/scttfrdmn/lith/issues/210)), the first workload to run
entirely off lith-served input.

### Added

- **`lith` logs a lookup miss (ENOENT) with the path**, at INFO, once per distinct
  path and capped ([#240](https://github.com/scttfrdmn/lith/issues/240)). A
  prefix-scoped mount that is silently short a key now says which one, instead of
  leaving the application's "file not found" as the only clue.

### Changed

- **`lith doctor` warns when `--nic-gbps` looks like the advertised peak**
  ([#239](https://github.com/scttfrdmn/lith/issues/239)). `--nic-gbps` wants the
  sustained baseline; the "Up to N Gigabit" figure on the instance page is the
  peak, and passing it over-sizes readahead and fetches bytes that are never
  read. `doctor` re-detects the true baseline and flags a peak-valued override.
- **`docs/knobs.md`**: `--mem-cache` is per-daemon (N mounts on a host add up;
  set it explicitly), and `--nic-gbps` is the baseline, not the "Up to N" peak.
- **`lith_nfs_ops_total` now counts the true NFS procedure**
  ([#247](https://github.com/scttfrdmn/lith/issues/247)). It previously counted at
  the billy-filesystem layer, which cannot see the procedure — so `getattr`
  silently included LOOKUP and ACCESS (all call `Lstat`), `readdirplus` included
  READDIR, and FSSTAT was uncounted. The gateway now counts per procedure via
  go-nfs's request-trace seam, yielding distinct `getattr`, `lookup`, `access`,
  `read`, `readdir`, `readdirplus`, `fsstat`, `fsinfo`, `pathconf`, `commit`,
  `readlink` (and an `unparsed` fallback so a future log-format change is visible,
  not silent). go-nfs's own error/warn logs now route through lith's slog and
  respect `--log-level`. **Watch for label changes** if you dashboard these:
  `getattr` no longer includes lookups/accesses.

### Fixed

- **`lith serve nfs` returned `NFS3ERR_STALE` on GETATTR under concurrent
  readers** (clean to ~32, ~15% from 48 up), which could abort a tightly-coupled
  MPI job at file open even though the bytes were never wrong
  ([#244](https://github.com/scttfrdmn/lith/issues/244)). The root cause was
  upstream in `go-nfs-client`'s `xdr.ReadOpaque`, which issued a single `r.Read`
  and ignored short reads: a file handle that straddled a buffer/TCP-segment
  boundary under load decoded with a zeroed tail, so `FromHandle` saw a bad inode
  (or root id) and correctly returned STALE. It was GETATTR-specific because that
  request path decodes the handle through the hand-rolled `ReadOpaque`, whereas
  READ decodes via `io.ReadFull`-backed struct unmarshaling and was never
  affected. Fixed by bumping `go-nfs-client` to the version whose `ReadOpaque`
  uses `io.ReadFull`. lith's own handle logic was never the bug — it round-trips
  `-race`-clean at 96×200 concurrent handles (`TestHandleRoundTripConcurrent`).
  The categorized STALE logging and the concurrency-ceiling note added while
  diagnosing this ([#245](https://github.com/scttfrdmn/lith/issues/245)) stay in.

## [1.1.1] - 2026-09-17

A patch for a real deployment finding from the GCHP project running 1.1.0 on AWS
ParallelCluster ([#237](https://github.com/scttfrdmn/lith/issues/237)). On stock
ParallelCluster nodes **both** NIC-detection paths fail — ENA reports no
`ethtool` speed, and the generated node role omits `ec2:DescribeInstanceTypes` —
and the failure fed a literal `0` into the device-derived knobs. `--parts-max`
silently clamped to its 4 MiB floor (a 16× swing off the 64 MiB it should be),
disabling the whole-file parts path for every 4–64 MiB file — exactly the band
where per-variable scientific inputs live — and `doctor` reported it healthy.

### Fixed

- **NIC detection never propagates `0` into the derived knobs**
  ([#237](https://github.com/scttfrdmn/lith/issues/237)). When every detection
  path fails, `--parts-max`, `--coalesce-gap` and `--inflight-bytes` now derive
  from a single assumed bandwidth (10 Gbps) instead of clamping to their floors.
  The whole-file parts path stays on for mid-size files on nodes lith cannot
  probe.
- **`lith doctor` prints the device-derived values `mount` will actually use**
  (parts-max, inflight, NIC source) and **`WARN`s — not `INFO`s — when the NIC
  is undetected**, naming the consequence and pointing at `--nic-gbps`. It no
  longer reports a healthy `PASS` over a 16× misconfiguration, or an
  inflight-bytes figure that contradicts the mount.

### Added

- **IMDS instance-type NIC estimate** ([#237](https://github.com/scttfrdmn/lith/issues/237)).
  When `ec2:DescribeInstanceTypes` is denied but IMDS is reachable (the
  ParallelCluster case), lith estimates the baseline from the instance size — no
  IAM change required. `--nic-gbps` remains the precise override, and
  `DescribeInstanceTypes` the precise detected path when permitted.

## [1.1.0] - 2026-09-16

The read path learned to **not commit before it knows**. The headline is one
fetch policy ([#229](https://github.com/scttfrdmn/lith/issues/229)): lith no
longer fetches broadly — whole-file parts, wide readahead — until a handle's
reads prove they tile. A sequential reader establishes in the first couple of
reads and gets the whole-file fetch as before; a scattered or sub-file reader (an
HDF5 hyperslab, a COG window, a GRIB `.idx` field sweep) stays byte-exact and
stops pulling whole objects to answer a question about a slice. Plus the release
now ships packaged and signed, and the prefetch detector stopped mistaking
scattered metadata walks for streams.

### Added

- **deb and rpm packages, an SBOM, cosign signatures, and SLSA build
  provenance** on every release (M15,
  [#207](https://github.com/scttfrdmn/lith/issues/207)). Packages (via goreleaser
  `nfpms`) and a syft SBOM attach to the release; checksums are signed keyless
  with cosign (Sigstore bundle); binaries and the image carry
  `actions/attest-build-provenance` attestations. `cosign verify-blob` and
  `dpkg -i`/`rpm -i` on a box, verified on the v1.0.2-rc.
- **The release gate is now the full branch gate** — vet, golangci-lint,
  `-race`, govulncheck, fuzz, dual-arch build, docker build — via one reusable
  workflow, no longer a subset ([#199](https://github.com/scttfrdmn/lith/issues/199)).

### Fixed

- **Release workflow pins goreleaser to the triggering tag**
  (`GORELEASER_CURRENT_TAG`) instead of `git describe`. An `-rc` and its stable
  tag legitimately share a commit; `git describe` then resolved ambiguously,
  so the stable run rebuilt the prerelease. Caught cutting this very release —
  the rc-then-real-tag verification did its job.

### Changed

- **Fetch policy: no broad fetch until the access pattern establishes**
  ([#229](https://github.com/scttfrdmn/lith/issues/229), subsuming
  [#220](https://github.com/scttfrdmn/lith/issues/220)/[#221](https://github.com/scttfrdmn/lith/issues/221)/[#228](https://github.com/scttfrdmn/lith/issues/228)).
  One signal — coverage over a trailing read window — gates the open-time
  parts-fetch, the initial readahead ramp, and the post-seek re-anchor. Measured
  amplification on scattered/sub-file reads (cold, parts-fetch on): HDF5
  hyperslab **92.5× → 6.3×**, COG overview+window **22.1× → 1.27×**, GRIB `.idx`
  **8.1× → 3.35×**; streaming and the CargoShip tree walk stay byte-identical.
  Cost: a cold sequential copy of a mid-size file pays roughly one extra
  round-trip for its first block (byte-identical, cold-only, shrinks with size).
- **Prefetch detector no longer mistakes a scattered metadata walk for a stream**
  ([#210](https://github.com/scttfrdmn/lith/issues/210)/[#213](https://github.com/scttfrdmn/lith/issues/213)).
  Reads that land in adjacent blocks but jump in byte offset — an HDF5/netCDF-4
  metadata traversal — were classified Sequential and got whole-block readahead;
  gap-aware then coverage-gated classification cut over-read **72–92%** on the
  GEOS-Chem gcgrid workload, ledger-confirmed.
- **`--parts-max` default is now `auto`** — derived from NIC baseline ×
  first-byte latency, clamped to `[--small-file, 64MiB]`
  ([#220](https://github.com/scttfrdmn/lith/issues/220)).

### Known issues

- **`mmap` of a large object walked randomly is slower than v1.0.1**
  ([#232](https://github.com/scttfrdmn/lith/issues/232)). The byte-exact posture
  that wins on scattered *reads* maximizes round-trips on serial *page faults*: a
  3,000-fault synthetic over an 892 MB index measured ~4.7× v1.0.1's wall
  (real-workload magnitude, e.g. `bwa`, unmeasured). Each random fault is a
  synchronous S3 round-trip and no readahead helps. **Mitigation:** copy a
  randomly-`mmap`'d reference index to local NVMe, or use the gateway / EFS — see
  `docs/copy-or-mount.md`.

## [1.0.1] - 2026-09-14

Hardening release from an external review of the v1.0.0 tag. No features. The
headline is a correctness fix: **`lith refresh` could return stale data** — a
failing-first integration test through a real FUSE mount confirmed it before the
fix. If you use `lith refresh`, upgrade.

### Fixed

- **`lith refresh` did not invalidate the kernel cache, so a reopen after a
  refresh could return the OLD version's size and bytes**
  ([#193](https://github.com/scttfrdmn/lith/issues/193)). The swap replaced an
  in-memory pointer, but the mount's one-year attribute/entry timeouts and
  `FOPEN_KEEP_CACHE` meant the kernel served a reopen from its cached
  previous-version attrs and pages — userspace never saw the read. **A
  failing-first integration test through a real mount confirmed the bug** (a
  reopen returned 4 KiB of the old content when the new version was 8 KiB of
  new content). The fix drives `notifyInvalInode`/`notifyInvalEntry` from a diff
  of the old and new index on swap; files unchanged between versions stay
  cached. (Verified by the same test passing post-fix, on a real kernel.)
- **`lith doctor` bypassed the production S3 client**
  ([#194](https://github.com/scttfrdmn/lith/issues/194)): it could sign a
  request to a non-HTTPS `--endpoint` — the exact case the mount path refuses —
  and so could report success on a configuration the real client rejects. doctor
  now builds its client through the same factory `mount` uses: a signed
  non-HTTPS endpoint is a check failure with the factory's own message,
  `--requester-pays` reaches the bucket and LIST probes, and a LIST denial is a
  **failure** (N/A only when `--keys` / `--keys-from-manifest` / `--cargoship` /
  `@ref` declares an alternate source).
- **The `CURRENT` pointer read was not bounded at the network**
  ([#195](https://github.com/scttfrdmn/lith/issues/195)): it read the whole
  object into memory before the 64 KiB cap applied. It now requests
  `MaxPointerBytes+1` and refuses an oversized pointer, naming the key and the
  cap — bounded in transit, not after the fact.
- **NFS gateway handle identity**
  ([#197](https://github.com/scttfrdmn/lith/issues/197)): `--cargoship` and
  auto-list now derive the handle root id from the index's own content, like
  `@version` and `--index-file` already did, so a rebuilt namespace at the same
  location returns `NFS3ERR_STALE` instead of silently reinterpreting old handles.

### Changed

- **NFS gateway `seqState` is bounded** (LRU, 8192 entries) with a
  `lith_nfs_seq_states` gauge — it was an unbounded per-path map that leaked over
  a long-lived gateway ([#197](https://github.com/scttfrdmn/lith/issues/197)).
  Eviction only resets one file's readahead, never correctness.
- **The gateway readahead window is described honestly** — in code and docs — as
  a **mount-registration-based share** of the global budget, not a measured
  per-client fair share (go-nfs's read path carries no per-op client identity)
  ([#197](https://github.com/scttfrdmn/lith/issues/197)).
- **Removed the cosmetic `--no-portmap` flag** from `serve nfs`
  ([#197](https://github.com/scttfrdmn/lith/issues/197)): only its default
  behavior ever existed. The gateway never registers with rpcbind; clients mount
  with an explicit port. No behavior change.
- **The 1.x index compatibility contract is published** on the
  [Scope page](https://scttfrdmn.github.io/lith/scope/) and enforced by golden
  fixtures ([#196](https://github.com/scttfrdmn/lith/issues/196)): every lith 1.x
  reader reads every index produced by lith 1.0.x. Stale "no compatibility
  promise before v1" language removed from the source, CONTRIBUTING, and design.
- **Documentation sweep** ([#198](https://github.com/scttfrdmn/lith/issues/198)):
  the README lists `serve`/`refresh`/`doctor` and the cluster/container/published/
  CargoShip pages and drops "Linux only (for v0.x)"; SECURITY.md drops "pre-1.0".
  The drift guard is extended beyond flags: CI now also fails on a command
  missing from the README summary, pre-1.0 status language, or a stale pre-1.0
  image tag / URL in any user-facing doc.

### Security

- **`lith doctor` no longer signs requests to a non-HTTPS endpoint**
  ([#194](https://github.com/scttfrdmn/lith/issues/194)) — the diagnostic path
  now enforces the same cleartext-credential guard as the mount path.
- **The NFS gateway's security boundary is stated plainly**
  ([#197](https://github.com/scttfrdmn/lith/issues/197)): it is the network
  perimeter. The gateway advertises `AUTH_UNIX`/`AUTH_NULL` and does not verify
  client identity, so any host that can reach its port has read access — restrict
  it with a security group / trusted subnet. Now documented in SECURITY.md and
  the cluster page.

## [1.0.0] - 2026-09-14

**lith 1.0 — feature-complete for its thesis.** lith is read-only by definition
and cloud-native by thesis: it presents any S3 bucket as a read-only filesystem
in its native key layout, serving metadata locally at zero S3 operations, on one
node (FUSE) or across a cluster (the NFS gateway). With 1.0 the capabilities are
complete *for that thesis* — any POSIX tool reads any bucket, native layout or a
CargoShip/published dataset, with format-aware I/O, and a stranger reaches a
working mount in five minutes. Past 1.0, work is extension, not completion; what
lith does and deliberately does not do is published on the
**[Scope page](https://scttfrdmn.github.io/lith/scope/)**. This release is the
operability-and-honesty pass on top of the v0.5.0 feature set: first-run
diagnostics, health endpoints, a container image, and docs that provably match
the binary.

### Added

- **`lith doctor`** ([#176](https://github.com/scttfrdmn/lith/issues/176)):
  first-run diagnostics — eight checks (credentials, bucket + region, `LIST`
  permission, FUSE, mountpoint, NIC, the `@current` pointer, the index file),
  each `PASS`/`FAIL`/`N/A` with a one-line fix, non-zero exit on any failure,
  run through lith's own credential and endpoint resolution so "doctor OK" means
  "mount works." Verified by induction — every check broken on purpose and
  confirmed caught, which is how the `GetBucketRegion` error-wrapping
  misclassification was found and fixed before shipping.
- **Health endpoints — `/healthz` and `/readyz`**
  ([#177](https://github.com/scttfrdmn/lith/issues/177)): liveness and readiness
  on the metrics server. The gateway starts the health server *before* the index
  load, so `/readyz` returns `503` with a reason until it is serving and `200`
  after — wire both to an orchestrator.
- **Distroless container image — `ghcr.io/scttfrdmn/lith`**
  ([#178](https://github.com/scttfrdmn/lith/issues/178)): multi-arch
  (`linux/amd64` + `linux/arm64`), built by the release workflow from the *same*
  binaries as the tarballs and published to GHCR in the same gated run — no
  second build path. The image is for the gateway (`serve nfs` is a userspace
  server needing no privileges); `doctor` and `index build` run in it too. See
  [Running in a container](https://scttfrdmn.github.io/lith/running-in-a-container/).
- **Flag-documentation CI guard — `TestKnobsDocumentsEveryFlag`**
  ([#179](https://github.com/scttfrdmn/lith/issues/179)): fails CI if any command
  flag is missing from `docs/knobs.md`, so the documentation cannot drift from
  the binary.
- **[Scope page](https://scttfrdmn.github.io/lith/scope/)**
  ([#182](https://github.com/scttfrdmn/lith/issues/182)): what lith is for, the
  two-category boundary (writes are out *by definition*; everything else is
  deferred, each with the named signal that reopens it), and where lith is the
  wrong tool.

### Changed

- `docs/knobs.md` now documents every shipped flag; the `--footer-tier2` help
  is reconciled to the measured truth (experimental, clustered-projection only)
  ([#179](https://github.com/scttfrdmn/lith/issues/179)).
- The five-minute Start-here walk is re-verified end-to-end on a clean box and
  gains `lith doctor` as its pre-flight step
  ([#180](https://github.com/scttfrdmn/lith/issues/180)).
- `docs/not.md` is folded into the Scope page; the published `/not/` URL now
  redirects there.

### Removed

- Dead code ([#45](https://github.com/scttfrdmn/lith/issues/45)/[#46](https://github.com/scttfrdmn/lith/issues/46)):
  the unused `--disk-path` bench flag, a `benchBucket` global, an unused return
  value, a duplicate `*metrics.Metrics` handle, and Go-1.22 loop-variable copies.
  (`s3client.GetRange` was kept — it has production callers via pointer
  resolution — and documented, [#44](https://github.com/scttfrdmn/lith/issues/44).)

### Upgrade

- **No index rebuild required.** The index format is unchanged since v0.5.0;
  existing `--index-file` artifacts and published datasets mount as-is. lith
  reads **Linux only** — there are no macOS or Windows builds — and remains
  read-only: every mutating operation returns `EROFS`.

## [0.5.0] - 2026-09-14

**Published datasets: pack and publish once, mount it anywhere by name.** A
CargoShip-published dataset is a versioned prefix with an atomic `CURRENT`
pointer; `lith mount s3://bucket/dataset@current` resolves the pointer and mounts
the current immutable version — no index file to track, and two uncoordinated
readers of the same name serve byte-identical namespaces. This is the seam
between CargoShip and lith closed on lith's side.

### Added

- **Pointer-based mounts — `s3://bucket/dataset@current` / `@<version-id>`**
  ([#167](https://github.com/scttfrdmn/lith/issues/167)/[#169](https://github.com/scttfrdmn/lith/issues/169)):
  resolve the atomic `CURRENT` pointer to a published version's prebuilt index and
  mount it; a bare `s3://bucket/prefix` (no `@`) is unchanged. The `CURRENT` parser
  is hardened (#101 discipline: 64 KiB cap checked before decode, every field
  validated, unknown-fields/trailing rejected, never panics; fuzzed from a real
  pointer) and **fails closed** (#141): a missing, malformed, or dangling pointer —
  or one whose index came from a chunkless (unmountable) manifest — is a hard error
  naming the key, never a fallback to listing (0 `ListObjectsV2`).
- **`lith refresh <mountpoint>`**
  ([#170](https://github.com/scttfrdmn/lith/issues/170)): re-read `CURRENT` and, if
  a new version was published, **atomically hot-swap** the mount's index. Explicit —
  lith never polls. An open handle keeps the version it was opened against (versions
  are immutable, so its objects still exist), so an in-flight read stays consistent;
  only new lookups see the new version. Inodes are path-hashed, so a file in both
  versions keeps its inode across a swap — a cached `(dev, ino)` in `find`/`rsync`
  stays valid.
- **Pointer-based NFS gateway — `lith serve nfs s3://bucket/dataset@current`**
  ([#171](https://github.com/scttfrdmn/lith/issues/171)): serves a published dataset
  by name. The gateway does **not** hot-swap — a version change would `STALE` every
  NFS handle and NFSv3 is stateless — so a version-changing `refresh` is refused with
  an actionable message (restart the gateway to adopt); a same-version refresh is a
  no-op.
- **`pkg/lithindex`** ([#168](https://github.com/scttfrdmn/lith/issues/168)): a
  public, cross-repo package to build a lith index from a CargoShip 2.1 manifest —
  the producer-side library behind `cargoship publish` (cargoship#560). Its exported
  API is a compatibility surface, kept minimal.
- **`--log-level` on `mount` and `serve nfs`**
  ([#155](https://github.com/scttfrdmn/lith/issues/155)): `debug`/`info`/`warn`/`error`.

### Fixed

- **Byte-precise Parquet projection over-fetch**
  ([#125](https://github.com/scttfrdmn/lith/issues/125)): the demand path missed the
  `ProjectionCoalesceGap` cap that #153 wired to the plan path — on a fat NIC its
  concurrency-derived gap swept the non-projected columns between projected ones;
  now capped, and the fill metric splits `demand-batch` from `plan` so the two are no
  longer conflated ([#153](https://github.com/scttfrdmn/lith/issues/153)/[#156](https://github.com/scttfrdmn/lith/issues/156)).
  The projection learner now records columns by read-range overlap, not start offset,
  so `id`-at-offset-0 and coalesced reads are learned correctly
  ([#154](https://github.com/scttfrdmn/lith/issues/154)/[#157](https://github.com/scttfrdmn/lith/issues/157)).
  `--footer-tier2` stays **experimental, clustered-projections-only**: the measured
  floor for a spread projection is pyarrow's own bytes, and streaming wins on time.

### Docs

- **Published datasets** page
  ([#172](https://github.com/scttfrdmn/lith/issues/172)): what a published dataset
  is, publish/mount-by-name, pointer semantics, `refresh`, and the FUSE-mount-vs-NFS-
  gateway asymmetry; plus a "Copy or mount?" one-liner (*pack and publish once; mount
  it anywhere by name*).

### Known limits

`lith refresh` hot-swaps a single-client FUSE mount; the NFS gateway adopts a new
version by restart (NFSv3 statelessness makes a live swap unsafe). `--footer-tier2`
remains experimental (clustered projections only).

## [0.4.0] - 2026-09-13

**The NFS gateway: one node mounts, a cluster shares.** `lith serve nfs` exports a
lith mount over read-only NFSv3, so N compute nodes read a shared S3 dataset
through one node — fetched from S3 **once**, not once per node — with the second
job free. It replaces EFS/FSx-linked-to-S3 for read-only shared datasets: no
hydration, no filesystem minimum, no filesystem to create and delete.

### Added

- **`lith serve nfs s3://bucket[/prefix]`** ([#143](https://github.com/scttfrdmn/lith/issues/143)/[#144](https://github.com/scttfrdmn/lith/issues/144)):
  read-only NFSv3 gateway over `willscott/go-nfs`, serving the `Index` and
  `BlockStore` directly (no FUSE in the path). **Deterministic `(index-sha,
  inode)` file handles** — stable across a restart against the same index,
  `NFS3ERR_STALE` against a rebuilt one, no server-side handle table. Every
  mutating op is `NFS3ERR_ROFS`. **`READDIRPLUS`/`GETATTR`/`FSSTAT` served from
  the index** (0 S3). **Per-client fair-share** readahead window. `MOUNT` v3 on
  the same port (clients mount with an explicit port; no portmap). Metrics
  `lith_nfs_clients`, `lith_nfs_ops_total{op}`, `lith_nfs_read_bytes_total`.
- **`Index.ByInode(ino)`** ([#144](https://github.com/scttfrdmn/lith/issues/144)):
  reverse inode→path lookup for NFS handle resolution, through `View`; a sorted
  side array built lazily (~12 B/key).
- **`lith serve nfs --disk-cache/--disk-path`** ([#143](https://github.com/scttfrdmn/lith/issues/143)):
  a gateway sized to its working set serves a warm re-read and a restart from
  local disk, not S3.
- **`serve` flag parity with `mount`** ([#143](https://github.com/scttfrdmn/lith/issues/143)):
  the read-path/cache/S3 tuning flags (`--block-size`, `--max-range`,
  `--prefetch-budget`/`-concurrency`, `--inflight-bytes`, `--coalesce-gap`,
  `--s3-concurrency`, `--nic-gbps`, `--endpoint`, `--path-style`,
  `--auto-index-limit`) are on `serve`; FUSE-only/diagnostic flags are documented
  inapplicable. A test fails CI if any mount flag is silently missing.

### Changed

- **Gateway read path dispatches readahead concurrently** ([#143](https://github.com/scttfrdmn/lith/issues/143)):
  a cold single stream reads **1,463 MB/s** on loopback — *faster than the FUSE
  mount* (no FUSE hop); was 67 MB/s with the initial serial dispatch.

### Measured (N=8, CloudTrail-ledger-backed)

- **Shared dataset** (8 nodes read the same 8.88 GB bwa index): the gateway
  fetches it **once** — 1,139 GETs / 9.1 GB — where 8 independent mounts fetch it
  **8×** (8,561 GETs / 71.3 GB): **~1/8 the S3 traffic**. The second run issues
  **0 S3**. Cheaper than EFS (which needs ~4.5 min to hydrate 8.88 GB first) and
  FSx Lustre (1.2 TiB minimum).
- **Distinct data per node** (W3, 8 CRAMs): independent per-node `lith mount`
  wins (parallel across 8 NICs); the gateway is a one-NIC funnel. **The sizing
  rule: distinct data → per-node mounts; shared data → gateway.** See
  [Serving a cluster](https://github.com/scttfrdmn/lith/blob/main/docs/serving-a-cluster.md).

### Known limits

NFSv3 only (no NFSv4 state/delegations/ACLs); read-only; `AUTH_UNIX`/squash, no
Kerberos; per-**path** (not per-client) sequential state (go-nfs's stateless read
path); FSx Lustre client unavailable on arm64 Ubuntu (costed analytically).

## [0.3.2] - 2026-09-12

> Includes the [0.3.1] section below, which was never tagged — its CargoShip
> integration ships here (v0.3.1's tag was gated on an acknowledgment that the
> session never reached, and the next release picked up the slack). The compare
> link for this release points back to **v0.3.0** so the notes and `git log`
> agree.

CargoShip read efficiency and safety. The packed small-files win is now
**universal, not archive-specific**: a tree walk moves ~1× the archive's
compressed bytes regardless of the frame size (was up to ~3× at the 16 MiB
default). Recommended with cargoship **v0.24.5** and `--frame-size 4MiB`.

### Added

- **Decoded-frame cache** for CargoShip framed reads ([#137](https://github.com/scttfrdmn/lith/issues/137)).
  A fetched zstd frame is decoded **once** and served to every fill it covers — a
  byte-bounded LRU keyed by (chunk object, frame offset) with a **per-frame
  singleflight** so the concurrent prefetch fills of a large frame's many 1 MiB
  cache chunks don't each re-fetch it. A tree walk now fetches and decodes each
  frame exactly once at any frame size. New metric `lith_backing_frame_reuse_total`;
  `lith_backing_decompress_bytes_total` now counts each frame once. Budget
  defaults to 25% of the mem cache (min 64 MiB), allocated only for a CargoShip
  mount; a frame larger than the whole budget is served but not retained.
  Ledger-verified (CloudTrail S3 data events; lith counters match exactly), A1
  tree walk (1,985 files, ~13.1 MB compressed archive), cold:

  | frame size | before (main) | after (frame cache) |
  |---|---|---|
  | 16 MiB | 38.1 MB (2.9×), 23 GETs | **13.1 MB (1.0×), 11 GETs** |
  | 4 MiB | 19.7 MB (1.5×), 23 GETs | **13.1 MB (1.0×), 23 GETs** |

  A single 90 KB file's cold random read pulls only its one covering frame
  (1.1 MB at 16 MiB); a same-frame neighbour is 0 GETs. **Packed-Zarr (A2) drops
  2.66 GB → 0.76 GB (167 → 58 GETs)** as a side effect — Zarr chunks packed in
  key order share frames, so the frame cache serves neighbouring chunks without
  re-fetch; the CargoShip-plus-Zarr combination pays off twice. Frameless CRAM
  (A3) is unchanged (flagstat fetches the file size exactly, wall within 2% of
  native).

- **`lith mount --cargoship <manifest-url>`** ([#141](https://github.com/scttfrdmn/lith/issues/141)).
  Build the archive index in-process and mount it in one command. Mutually
  exclusive with `--index-file`; `lith mounts` shows `[cargoship:<key>]`.

### Fixed

- **`--cargoship` fails closed — it never falls back to listing the bucket**
  ([#141](https://github.com/scttfrdmn/lith/issues/141), P0). A manifest that
  cannot be resolved (missing, unparseable, unsupported version, encrypted) is a
  hard error naming the manifest key, for both `mount` and `index build` — with
  **zero `ListObjectsV2`** calls (regression-tested against a fake-S3 call log).
  Previously a failed `--cargoship` build left the index file absent, and a
  follow-up `lith mount --index-file` then **auto-listed the whole bucket**
  (session-36 benchmark artifact: 2,632 objects / 24 GB walked where one archive
  was intended). The catastrophic multi-chunk "thrash" once attributed to lith
  was this benchmark artifact, not a read-path bug; the real issue was the byte
  over-fetch fixed above.

### Docs

- CargoShip page: decoded-frame cache behaviour, one-command `mount --cargoship`,
  and "Choosing a frame size" (the tree walk is ~1× at every size; frame size now
  governs only random single-file over-fetch). Frame-size curve in
  [`bench/results/framesize-curve.csv`](https://github.com/scttfrdmn/lith/blob/main/bench/results/framesize-curve.csv).

## [0.3.1] - 2026-09-12

CargoShip archive integration: mount a [CargoShip](https://github.com/scttfrdmn/cargoship)
2.1 archive as its **original file tree** and serve reads from the packed chunks.
Packing many small objects once and mounting them turns the many-small-objects
case (Zarr, sharded datasets) into large-object streaming — lith's best shape.

### ⚠️ Upgrade — rebuild your index

On-disk **index format is now v5** (adds an optional CargoShip backing section).
A v4 index is rejected with `index: unsupported format version 4 (this build
writes v5); rebuild the index with lith index build`. Rebuild any saved index.

### Added

- **`lith index build --cargoship s3://…/manifest.json[.gz]`** ([#94](https://github.com/scttfrdmn/lith/issues/94)).
  Builds an index whose namespace is the archive's original tree (no
  `ListObjectsV2`). The new `internal/cargoship` parses and hardens the 2.1
  manifest (lith#101: size cap, every offset bounds-checked, fuzzed), handling
  dedup and split files; incremental chains (`previous_manifest_id`) fold to a
  union-of-latest. `lith index inspect` prints archive/chunk/frame provenance.
- **CargoShip backing read path** ([#94](https://github.com/scttfrdmn/lith/issues/94)).
  A read of a virtual file maps to its packed chunk. **Framed** `.tar.zst` chunks:
  the covering zstd frame(s) — one coalesced range GET, each verified by its
  sha256-of-compressed checksum and decoded, cached as 1 MiB uncompressed chunks
  so files sharing a chunk share the cache. **Frameless** plain-`.tar` chunks
  (already-compressed/small content): a direct range GET at `archive_offset`, no
  decode (requires cargoship **v0.24.3**). Mixed archives route per chunk. New
  metrics `lith_backing_frames_fetched_total`, `lith_backing_decompress_bytes_total`,
  `lith_backing_checksum_fail_total`.

### Measured

A1 tree walk (1985 small files): **0.64 s / 12 GETs vs native 14.05 s / 1998
GETs** (22× faster). A2 one-month packed-Zarr query: **9.9 s ≤ native 10.7 s ≤
xarray+s3fs 14.3 s**. A3 tabix on a frameless VCF: within noise of native.

### Known limits

- A large file that CargoShip framed as one giant zstd frame is not randomly
  readable (a zstd frame isn't seekable); lith caps decodable frame size and
  errors clearly. Store large already-compressed files frameless, or cut
  sub-frames ([cargoship#502](https://github.com/scttfrdmn/cargoship/issues/502)).
- Encrypted (KMS-envelope) manifests are rejected; 2.0 archives are unsupported.

### Upstream (CargoShip) follow-ups filed
- [cargoship#492](https://github.com/scttfrdmn/cargoship/issues/492) `archive_offset`
  for every file — **fixed in v0.24.3** (enables the frameless read path).
- [cargoship#493](https://github.com/scttfrdmn/cargoship/issues/493) manifest
  records intermediate staging snapshots (lith keeps the complete entry).
- [cargoship#494](https://github.com/scttfrdmn/cargoship/issues/494) `chunk_id`
  identity is per-shard (lith keys chunks by `s3_key`).
- [cargoship#502](https://github.com/scttfrdmn/cargoship/issues/502) file-boundary
  framing makes a large file one giant non-seekable frame.

## [0.3.0] - 2026-09-10

The **format-aware plan** release. lith learns the on-disk shape of the formats it
serves — Zarr chunk grids, bgzf coordinate indexes, columnar footers — and reads
along the app's access pattern instead of blindly. The measurement rule holds: I/O
metrics (bytes, GETs) are primary; wall-clock is reported under I/O-bound configs.

### Added

- **Zarr grid-plane prefetch — Format tier 2 ([#70](https://github.com/scttfrdmn/lith/issues/70)/[#109](https://github.com/scttfrdmn/lith/issues/109)).**
  A chunked-store read now prefetches the whole grid-plane selection an access
  implies, not just the next chunks in key order — closing the 2-D boundary gap
  where uncovered chunks each paid a cold GET on the app's bounded pool. On the
  1.4 TB `chrtout.zarr` the I/O tail is closed; the remaining difference from an
  async in-place reader is the app's compute floor (a documented tie).
- **bgzf family — index-aware readahead, tiers 1+2 ([#107](https://github.com/scttfrdmn/lith/issues/107)).**
  A BAM/CRAM/VCF.gz/BCF data file with a coordinate-index sibling
  (`.bai`/`.tbi`/`.csi`/`.crai`) gets header prefetch + random protection at open
  (tier 1); at or below `--bgzf-whole-file-max` (512 MiB) it is fetched whole
  through the parts path when the index opens, and above it tier 2 prefetches only
  the index-resolved slice ranges. **2.8× fewer bytes on a point query**; wall is
  unchanged (samtools is decode-bound), so the win is byte-selectivity.
- **Footer family — Parquet/zip tier-1 + tier-2 ([#108](https://github.com/scttfrdmn/lith/issues/108)).**
  A columnar/archive container (Parquet/ORC/Arrow/zip) gets its footer (tail) +
  head prefetched at open (tier 1, always on). An experimental tier 2
  (`--footer-tier2`, **off by default** — see Changed) fetches the index-resolved
  projection/entries byte-precise.
- **Sparse chunk fills + coalescing substrate ([#118](https://github.com/scttfrdmn/lith/issues/118)/[#124](https://github.com/scttfrdmn/lith/issues/124)/[#31](https://github.com/scttfrdmn/lith/issues/31)).**
  The 1 MiB cache chunk gains a 64 KiB-granularity filled-extent bitmap: a byte-
  exact plan range and a non-sequential point read fetch only the extents they
  cover, not the whole chunk; sequential reads still fill whole chunks. Fill
  batches coalesce extents across chunks into range GETs, merging gaps up to
  `--coalesce-gap` (default derived from NIC × first-byte latency ÷ usable
  concurrency, clamped [256 KiB, 64 MiB]), and dispatch their runs concurrently.
  The disk tier persists the bitmap. New metrics `lith_fill_partial_total`,
  `lith_fill_runs_total`, `lith_fill_bytes_total{kind=plan|demand|whole|gap}`,
  `lith_fill_gap_bytes_total`, `lith_fill_batch_size`, `lith_fill_inflight`,
  `lith_fill_inflight_peak`.
- **Read-path metrics ([#65](https://github.com/scttfrdmn/lith/issues/65)).** A
  read-size histogram (`lith_read_size_bytes`) and a distinct-object-bytes-read
  gauge (`lith_distinct_bytes_read`) from per-object 64 KiB touched-extent bitmaps.
- **`lith index build --keys <file>` / `--keys-from-manifest <url-or-key>`
  ([M6](https://github.com/scttfrdmn/lith/issues/106)).** Build an index from an
  explicit key list, for buckets that are GET-public but deny `ListObjectsV2`
  (Common Crawl `cc-index`, `nyc-tlc`). One key per line (`#` comments; optional
  `\t<size>\t<mtime>` columns); keys lacking size/mtime are `HeadObject`-ed with
  bounded concurrency (`--s3-concurrency`, `--no-sign-request` honored). A 403/404
  is counted and listed, failing the build unless `--keys-allow-missing`. A
  manifest is fetched (`.gz` transparently) then parsed the same way. The index
  records its build source and the key-file sha256; `lith index inspect` prints them.

### Changed

- **`--footer-tier2` defaults to off (experimental).** Byte-precise Parquet
  projection fetch is measured slower than default whole-file streaming on every
  tested instance class, in-region: a whole-file stream is a handful of coalesced
  GETs at line rate, while byte-precise fetch pays a round-trip per column chunk
  through a filesystem that sees reads one at a time. Criterion 3 is **met by
  streaming**; tier 2 ships as a correct, tested, opt-in substrate, and the
  precision work moves to [#125](https://github.com/scttfrdmn/lith/issues/125)
  (v0.4). With tier 2 off a footer handle streams exactly like a plain one (tier 1
  footer+head prefetch only).
- **`lith_distinct_bytes_read` now counts filled 64 KiB extents, not 1 MiB
  chunks** (it was chunk-rounded). Values are finer-grained (and smaller) than
  before for sub-chunk access ([#118](https://github.com/scttfrdmn/lith/issues/118)).
- On-disk index format bumped to **v4** (build-provenance trailer: source +
  key-file sha256); the disk block-cache format bumped to **v2** (per-chunk extent
  bitmap). Both are private — older files are ignored/rejected and rebuilt.

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

[Unreleased]: https://github.com/scttfrdmn/lith/compare/v1.0.1...HEAD
[1.0.1]: https://github.com/scttfrdmn/lith/compare/v1.0.0...v1.0.1
[1.0.0]: https://github.com/scttfrdmn/lith/compare/v0.5.0...v1.0.0
[0.5.0]: https://github.com/scttfrdmn/lith/compare/v0.4.0...v0.5.0
[0.4.0]: https://github.com/scttfrdmn/lith/compare/v0.3.2...v0.4.0
[0.3.2]: https://github.com/scttfrdmn/lith/compare/v0.3.0...v0.3.2
[0.3.1]: https://github.com/scttfrdmn/lith/compare/v0.3.0...v0.3.1
[0.3.0]: https://github.com/scttfrdmn/lith/compare/v0.2.2...v0.3.0
[0.2.2]: https://github.com/scttfrdmn/lith/compare/v0.2.1...v0.2.2
[0.2.1]: https://github.com/scttfrdmn/lith/compare/v0.2.0...v0.2.1
[0.2.0]: https://github.com/scttfrdmn/lith/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/scttfrdmn/lith/releases/tag/v0.1.0
