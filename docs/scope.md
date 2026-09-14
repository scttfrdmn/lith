# What lith is for

lith presents an S3 bucket as a read-only filesystem in its native key layout.
Every other page here shows you how to use it; this one is so you can decide
whether to — before you spend an afternoon on it, and with enough of the edges
in view that a skeptical reader has nothing to catch lith hiding. The numbers
that back the claims live on the other pages; this page is the argument, and it
links to them.

## Read-only by definition — the reason lith exists

lith has no write path. Every mutating operation returns `EROFS`, and not
because writes are switched off — the code to perform them is absent. Read this
as the design, not a missing feature, because read-only is what buys everything
lith is good at:

- **Metadata is free.** The namespace cannot change under you, so an index built
  once — a single listing pass — answers every `readdir`, `lookup`, and
  `getattr` locally afterward, at zero S3 operations. 100,000 stats in a fraction
  of a second is not a cache trick; it is what "the listing is final" makes
  possible.
- **The namespace is a file.** An index is a file with a sha256. Two readers who
  have never spoken to each other, handed the same index, see byte-for-byte the
  same filesystem — no coordination, no metadata server, no agreement protocol,
  because there is nothing to agree about.
- **Handles are pinned at open.** A file opened through lith stays the file you
  opened, because nothing lith does can replace the bytes behind it.
- **A cluster cache needs no invalidation.** The [gateway](serving-a-cluster.md)
  caches with no coherence protocol at all — nothing can go stale within the
  index it is serving, so there is nothing to invalidate.

A tool with a write path cannot have these properties, no matter how well it is
engineered — the moment any client can change the namespace, the free metadata,
the shareable index, the pinned handle, and the invalidation-free cache each
need machinery to defend them, and you are building a different program. That is
the honest place for writes: not an extension of lith, but a separate thing.
lith is the read half, done properly.

## Cloud-native by thesis, portable by implementation — why it works

Read-only is *what* lith is. The cloud is *why it works*. lith borrows four
properties from object storage, none of which an on-prem object store
(MinIO, Ceph, and the like) has at a research group's scale:

- **Aggregate read bandwidth grows as you add clients.** S3's effective read
  ceiling is close to the sum of the readers' NICs, so a second node *adds*
  bandwidth rather than taking a share of it — which is why the
  [fan-out cost line stays flat](deadline.md). On a provisioned store, N readers
  divide one fixed pool.
- **The data is already there.** A Registry of Open Data dataset, a lab bucket,
  a published archive — nobody staged it for lith. On-prem, the object store is
  a *second* copy sitting next to the parallel filesystem that holds the first,
  and "don't stage a copy" quietly stops being true.
- **Compute is ephemeral and storage outlives it.** Every cost number lith
  reports is instance-hours plus requests, with nothing provisioned — which is
  what lets "cost to a result" mean something. Where the hardware is sunk
  capital, "cheaper" stops meaning much.
- **Durability is free.** Eleven nines is why an index can be a plain file and a
  published pointer can be a single atomic PUT, with no replication, quorum, or
  repair of lith's own to build.

lith *runs* against any S3-compatible endpoint — point `--endpoint` at a lab's
MinIO or a test server and it works, which is genuinely useful. What does not
travel is the economic argument: on hardware you already bought, the crossover
math that makes lith compelling is no longer the math you are doing.

> Lustre and GPFS are on-prem designs running in the cloud; lith is a design
> that could not have been built anywhere else.

## Where lith is the wrong tool

Better to read this here than discover it mid-project.

- **No writes, and nothing that depends on them** — no atomic rename, no
  `O_CREAT`, no locking, no POSIX write semantics that object storage cannot
  implement honestly. If your workflow writes back to the filesystem, lith is
  not in the running.
- **The index is point-in-time.** lith reads the object that existed when the
  index was built. If a key's ETag has changed since, that read returns `EIO` —
  lith gives you an error, never silently stale bytes — and you rebuild or
  `lith index refresh`. lith does not expose S3 object versions or mount a
  bucket as of a past time.
- **Linux only**, for releases. lith relies on FUSE; there are no macOS or
  Windows builds.
- **The gateway is a funnel.** [`lith serve nfs`](serving-a-cluster.md) shares
  one node's NIC across its clients, so the rule is: **distinct** data per node
  → mount per node; **shared** data across the cluster → one gateway. Point a
  gateway at a workload where every node reads something different and you have
  built a bottleneck.
- **The cache holds plaintext.** The memory and optional disk cache hold object
  bytes in the clear; don't point `--disk-cache` at shared or untrusted storage.

And lith still reads from S3 over a network, so it does not beat physics: the
first byte of each distinct object costs a round-trip (this is what makes
**many small objects in key order** — Zarr, sharded stores — the hard case, not
one big file); cold throughput is bounded by the NIC; a single connection tops
out well below the link, so lith fans reads across many; and when the job is
CPU-bound, the bytes arrive faster than the application consumes them and a
local copy on the same box ties. lith's win is removing the staging step and
moving only the bytes you touch — see
[Copy or mount?](copy-or-mount.md) and [Sizing the node](sizing.md) — not
outrunning the network.

**When a provisioned filesystem is the right answer, use one.** Checkpoint-heavy
MPI, anything that needs cross-node locking or read-write scratch, POSIX-complete
semantics — that is Lustre, FSx, or EFS, and lith does not pretend otherwise.
lith is for reading data that lives in S3, at the speed its native layout allows,
without copying it first.

## The boundary

lith holds nothing out of its future permanently except by definition. There are
exactly two kinds of "not in lith," and they are not the same kind.

### Out by definition

**Writes, and only writes.** This is not a decision waiting on evidence and no
signal reopens it — it is the thesis above. A write path is a separate program.
If you need one, don't wait for lith to grow it: that is the **write plane**,
tracked apart from lith and not a line on its roadmap. Knowing that now is a
better outcome than discovering it in a month.

### Not for 1.0 — each with the signal that would reopen it

Everything else that lith does not do today is deferred, not disowned. The
precedent is the NFS gateway: it sat as "someday" from the first design note and
shipped in v0.4.0 the moment the operations ledger gave it an argument. Each of
these is the same kind of door, with the trigger that opens it named:

- **Parquet tier-2 (byte-precise projection) as a default** — a clustered-column
  projection workload at scale. It already wins on bytes for clustered columns
  and stays experimental and off by default until that workload is the one being
  run; for a spread projection, streaming the file wins, which is why lith
  streams by default.
- **NFSv4 and Kerberos** — an environment that requires them.
- **Sharded / multi-export gateways** — a shared-data workload that outgrows one
  gateway's NIC.
- **HDF5 / NetCDF4, Cloud-Optimized GeoTIFF, record-sequential, ORC, Arrow
  tier-2** — measured demand that a native library on top of a lith mount does
  not already serve better. The Parquet arc is the lesson here: format-specific
  byte-precision is where lith is weakest against a purpose-built reader, so it
  is gated on evidence, not built on spec.
- **Windows and macOS release builds** — a user who needs one.
- **Cache encryption at rest** — a compliance requirement that asks for it.
- **Versioned-bucket time travel** — demand to mount a bucket as of a past time.
- **Incremental index refresh for mutating buckets** — a use case built on a
  bucket that changes under a long-lived mount.
- **Packaging beyond the container** (deb/rpm, Homebrew) — a distribution ask.

If you need one of these, say so on the tracker with the shape of your workload;
that is the signal, and it is how the gateway got built.

## What "feature-complete" means

lith 1.0 does what it set out to do: any POSIX tool reads any S3 bucket — its
native layout, or a CargoShip or published dataset — at local-metadata speed,
with format-aware I/O, on one node or across a cluster, and a stranger reaches a
working mount in five minutes. Past 1.0, work is extension, not completion; a
new format plane or the write plane makes lith do *more*, not make it *finished*,
because for its thesis it already is. The road there, and what is shipped, is the
[1.0 tracker](https://github.com/scttfrdmn/lith/issues/175).
