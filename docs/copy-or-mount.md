# Copy or mount?

Your dataset is in S3. You have a job to run against it, a box to run it on, a
bill to pay, and maybe a deadline. The reflex is to `aws s3 cp` it to a volume
first. Sometimes that is right. This page tells you when.

## The one number that decides it

Cost to result, in-region (transfer and egress are free):

```
cost = instance $/hr × wall-clock  +  staging storage  +  S3 requests
```

Everything below is about which term dominates for your access shape.

- **Copy** pays **staging time** — the instance sits idle burning $/hr while
  bytes land on a volume you had to size, attach, and will have to delete — and
  it stages the **whole object** whether your job reads all of it or 1%. Once
  staged, the job can run on a tiny box and re-read for free.
- **Mount** pays **nothing up front**: the job starts immediately and lith
  fetches bytes as the app reads them. In return it wants a **NIC and RAM**
  proportional to how fast the job reads. It never stages bytes you don't touch.

Staging storage and S3-request charges are rounding errors here (cents of gp3,
sub-cent of GETs); the fight is instance-$/hr × wall-clock, so **whichever path
finishes sooner usually also costs less.**

## By access shape

**Selective reads** — region queries, Parquet predicate pushdown, a few Zarr
chunks. **Mount, at any box size.** Copy must stage the whole file to answer a
question about a slice of it; lith moves only the slice.

**Full single-pass streams** — read an object front to back once (e.g.
`flagstat`). **Mount.** It is a *tie with a local-NVMe copy* when the app is
CPU-bound (both wait on the CPU, not the bytes) — but mount still wins by not
staging first, and an EBS copy loses outright to its own staging wall.

**Batches** — many objects, compute per object. **Mount.** lith overlaps each
object's fetch with the previous object's compute; the copy path serializes
staging in front of compute, and its compute then reads back from a
throughput-capped volume.

**Many-small-object stores** — Zarr, sharded datasets read in key order. **The
one shape where the answer is nuanced.** Reading these cold pays a serial S3
round-trip per object. As of v0.2, sibling readahead ([#63](https://github.com/scttfrdmn/lith/issues/63))
cut the Zarr yearly-mean cold read from **~40 s to ~24 s**, still above the
**~10 s** an in-place `xarray`+`s3fs` reader manages by fetching all chunks
concurrently; [#70](https://github.com/scttfrdmn/lith/issues/70) (chunk-grid–aware
prefetch) targets parity. If you read one chunked store repeatedly and can
afford the RAM, mount and let the warm cache serve reruns; if you read it once,
cold, an async in-place reader is currently faster. <!-- number: session 17 -->

## The crossover, measured

Cost to result, **copy → gp3 EBS then compute** vs **mount with lith**, across
the EBS-only Graviton4 ladder (no local NVMe — the case that most favors
copying). Each cell is **copy $ / lith $** (wall copy / wall lith); lower is
better. Full data: [`bench/results/crossover-v0.2.csv`](https://github.com/scttfrdmn/lith/blob/main/bench/results/crossover-v0.2.csv),
method in [#77](https://github.com/scttfrdmn/lith/issues/77).

| access shape | c8g.large (2 vCPU) | c8g.xlarge (4) | c8g.2xlarge (8) | c8gd.4xlarge (16) | lith margin |
|---|---|---|---|---|---|
| **selective** (14 GB CRAM, region set, ~0.8 GB touched) | $0.0035 / **$0.0013** | $0.0066 / **$0.0022** | $0.0123 / **$0.0043** | $0.033 / **$0.011** | 2.7–3.1× cheaper |
| **stream** (3.8 GB CRAM, `flagstat`) | $0.0020 / **$0.0015** | $0.0032 / **$0.0022** | $0.0054 / **$0.0042** | $0.0165 / **$0.0105** | 1.3–1.4× cheaper |
| **batch** (8 CRAMs, 60 GB, parallel = vCPU) | —¹ | $0.046 / **$0.011** | $0.080 / **$0.013** | $0.21 / **$0.046** | 3.9–5.6× cheaper |

**lith is cheaper on every box measured. The crossover is at or below the
smallest box (`c8g.large`) for all three shapes** — there is no instance in the
ladder where copying wins, because the copy path pays full-object staging that
lith never does.

¹ Batch was measured from `c8g.xlarge` up: 60 GB across two paths overran the
2-vCPU box's one-hour test budget. The crossover is ≤ large by extrapolation —
the copy path's cost is dominated by reading the staged files back from a
125 MB/s gp3 volume, which only worsens on a smaller box.

!!! note "Why warm reuse doesn't change this for CPU-bound jobs"
    On the 8-vCPU box, re-running the selective and stream jobs against a warm
    cache saved only **3–7 %** (region query 48.2 s → 44.6 s; `flagstat`
    47.9 s → 46.6 s). These jobs are **samtools-CPU-bound** — decode is the
    ~45 s floor — so lith's cold read is already ~compute-time, and there is
    little for a warm cache to shave. lith's win on these shapes is the
    **staging it avoids**, not cache hits. Warm reuse matters for *repeated
    selective queries* over a working set that fits RAM (see
    [Sizing the node](sizing.md)).

## The rule

**Pay for the bytes you touch, not the bytes you own.** Copy first only when you
will re-read a dataset many times from fast local storage and the staging cost
amortizes across those reads — and then copy to local NVMe, never EBS. For
query-once, selective, or batch work, mount.
