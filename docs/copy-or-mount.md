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
round-trip per object. v0.2 shipped sibling readahead
([#63](https://github.com/scttfrdmn/lith/issues/63)) and the first format-aware
plan — chunk-grid–aware Zarr prefetch ([#70](https://github.com/scttfrdmn/lith/issues/70)
tier 1, [#87](https://github.com/scttfrdmn/lith/issues/87)) that walks the grid
in access order instead of flat key order. On the 1.4 TB `chrtout.zarr`
yearly-mean the cold read is **~35 s**, versus **~25 s** for an in-place
`xarray`+`s3fs` reader on the same box, same day (c8gd.4xlarge, us-east-1;
`bench/results/apps.csv` @ `748b33d`). lith's prefetch runs deep and leads, but
the uncovered chunks at 2-D grid boundaries each still pay a full cold GET on
the app's bounded compute pool while s3fs fires the whole selection at once;
closing that gap is #70 tier 2 (whole-selection / grid-plane prefetch). If you
read one chunked store repeatedly and can afford the RAM, mount and let the warm
cache serve reruns (**~22 s** warm); if you read it once, cold, an async
in-place reader is currently a touch faster. <!-- number: apps.csv @748b33d (v0.2.0), sessions 19/21 -->

**Columnar files (Parquet).** For a projection query in-region, lith streams the
file and beats a byte-precise native reader (pyarrow) **~4× single-process**; on
small NICs it ties. This is counterintuitive — the native reader moves fewer
bytes (only the projected columns) — but on a fat in-region pipe a whole-file
stream is a handful of coalesced GETs at line rate, while byte-precise fetch pays
a round-trip per column chunk through a filesystem that sees the reads one at a
time. lith ships tier 1 (footer + head prefetch on open) on by default and
streams the rest; a byte-precise projection path exists behind
`--footer-tier2` but is **experimental and off** (slower on every box measured;
[#108](https://github.com/scttfrdmn/lith/issues/108),
[#122](https://github.com/scttfrdmn/lith/issues/122)). <!-- number: apps.csv in-region 4xl, sessions 29-32 -->

## The crossover, measured

Cost to result, **copy → compute** vs **mount with lith**, across the EBS-only
Graviton4 ladder (no local NVMe — the case that most favors copying). The copy
path is given its **best tool and tier**: **`s5cmd` tuned** (~1 GB/s, 3× faster
than `aws s3 cp`) staging to a **gp3 volume provisioned to 1000 MB/s**, which
un-caps both staging and the read-back the compute does ([#84](https://github.com/scttfrdmn/lith/issues/84)).
Each cell is **copy $ / lith $** (wall copy / wall lith); lower is better. Full
data: [`bench/results/crossover-v0.2.csv`](https://github.com/scttfrdmn/lith/blob/main/bench/results/crossover-v0.2.csv),
method in [#77](https://github.com/scttfrdmn/lith/issues/77).

| access shape | c8g.large (2 vCPU) | c8g.xlarge (4) | c8g.2xlarge (8) | c8gd.4xlarge (16) | lith margin |
|---|---|---|---|---|---|
| **selective** (14 GB CRAM, region set, ~0.8 GB touched) | $0.0064 / **$0.0012** | $0.0054 / **$0.0021** | $0.0065 / **$0.0042** | $0.0146 / **$0.0109** | 1.3–5× cheaper |
| **stream** (3.8 GB CRAM, `flagstat`) | $0.0036 / **$0.0014** | $0.0042 / **$0.0022** | $0.0069 / **$0.0043** | $0.0155 / **$0.0103** | 1.4–2.6× cheaper |
| **batch** (8 CRAMs, 60 GB, parallel = vCPU) | $0.044 / —² | $0.023 / **$0.0105** | $0.023 / **$0.0134** | $0.052 / **$0.040** | 1.2–2.2× cheaper |

**lith is still cheaper on every box — the crossover is at or below `c8g.large`
for all three shapes; nothing flips.** But given its best tool and tier the copy
path is far more competitive than the naive `aws s3 cp` → baseline-gp3 it
replaces: on the high-NIC boxes (2xlarge, 4xl) the margin **collapses to
~1.2–1.5×** (it was up to 5.6×). What remains of lith's edge is the staging it
never does — now only 5–60 s on a fast box — plus selectivity (a region query
still moves 0.8 GB, not 14). On the **smaller** boxes staging is NIC-bound, so
the best copier is no faster there and lith wins by more (2xlarge/4xl measured;
large/xlarge estimated).

Against **mountpoint-s3** (the other in-place reader), lith wins or ties **every**
app shape: selective (region query, tabix) and small-object (Zarr) it wins
clearly, and on the two large-file shapes it now matches — stream (S1) is a tie,
and large-file batch (W3) is **141 s vs mountpoint's 144 s** on a 4xl once the NIC
in-flight budget is sized to the detected baseline ([#79](https://github.com/scttfrdmn/lith/issues/79);
an earlier 184 s was pre-#79 and drove the now-closed [#85](https://github.com/scttfrdmn/lith/issues/85)).

² lith's batch cell on the 2-vCPU `c8g.large` wasn't run (60 GB × 8-way on 2
cores overran the test budget); the copy-best cost there (~$0.044) is shown for
scale. The crossover stays ≤ large — copy-best on a 2-vCPU box is dominated by
staging 60 GB over its small NIC and then `flagstat`-ing on 2 cores, both of
which favor the mount.

!!! note "Why warm reuse doesn't change this for CPU-bound jobs"
    On the 8-vCPU box, re-running the selective and stream jobs against a warm
    cache saved only **3–7 %** (region query 48.2 s → 44.6 s; `flagstat`
    47.9 s → 46.6 s). These jobs are **samtools-CPU-bound** — decode is the
    ~45 s floor — so lith's cold read is already ~compute-time, and there is
    little for a warm cache to shave. lith's win on these shapes is the
    **staging it avoids**, not cache hits. Warm reuse matters for *repeated
    selective queries* over a working set that fits RAM (see
    [Sizing the node](sizing.md)).

## Where the margin grows

The crossover table above uses a 14 GB object, so staging is a ~10-second
afterthought on a fast box and lith's edge looks modest (1.2–1.5×). **The margin
widens with:**

- **Object (or store) size.** Staging scales with bytes; lith's selective read
  does not. On the **1.4 TB `chrtout.zarr` store**, a one-month query touches
  **0.13 GB** — lith answers it **cold in 7.3 s** (mountpoint-s3 36 s), while
  copying the store with the best copier **did not finish in 20 minutes** (886 of
  1434 GB staged). At RODA scale, "stage first" isn't slower — it's *infeasible*.
- **Smaller NICs.** Staging is NIC-bound on small boxes, so the copy path is no
  faster there while lith still moves only the bytes touched — the margin is
  larger on a `c8g.large` than on a 4xl.
- **Repeated jobs.** A second query over the same working set is warm and free on
  lith; the copy path re-stages every fresh volume.
- **Fan-out.** N nodes each stage their shard; lith mounts the same index N times
  and moves no bulk data (see [Meet a deadline](deadline.md)).
- **Pack small files once.** Many small objects read cold pay a round-trip each; pack them with [CargoShip](cargoship.md) and mount the archive as a tree, and lith streams a few framed chunks instead — 600 small files walk in 0.34 s / 8 GETs vs 2.56 s / 604 GETs native (see [CargoShip archives](cargoship.md)).
- **Round-trips, not bytes.** In-region, round-trips cost time; bytes don't. A
  reader that saves bytes by fetching a precise slice can still lose to a
  whole-file stream if it pays more round-trips to do it (see Parquet, above).

## The rule

**Pay for the bytes you touch, not the bytes you own.** Copy first only when you
will re-read a dataset many times from fast local storage and the staging cost
amortizes across those reads — and then copy to local NVMe, never EBS. For
query-once, selective, or batch work, mount.
