# Sizing the node

lith turns S3 into a filesystem; what it asks of the box is a NIC and some RAM.
Size for the reads you'll actually do.

## The NIC is the ceiling for cold reads

A cold read streams from S3, so its throughput is bounded by the instance's
network, not its CPU or disk. Pick the NIC first.

Watch **burst-credit** classes. Many instances advertise a burst bandwidth well
above their sustained baseline; a short read rides burst, a long or many-file
read drains the credits and settles at baseline. In measurement, a single cold
sequential read of a >1 GiB object showed a post-first-window p99 latency of
**~4.9 ms on a 30 Gbps `c8gd.16xlarge`** (no burst model — advertised bandwidth
is sustained) versus **~7.4 ms on a `c8gd.4xlarge`** once its burst credits
depleted — same code, different NIC headroom.

For sustained or multi-reader work, pick a class whose **baseline** meets your
target, or a 16xlarge-and-up where the advertised bandwidth is sustained.

## RAM sets the cache and the prefetch budget

lith serves reads from a bounded in-memory tier, default **25 % of system
memory** (`--mem-cache`), and lets prefetch hold up to **50 % of that**
un-demanded (`--prefetch-budget`). Two consequences:

- **Warm working set** — a re-read is served from RAM only if it still fits the
  memory tier. With RAM ≳ 2× your working file, warm reads come from cache at
  ~20 GB/s.
- **Prefetch depth** — the readahead window is bounded by the prefetch budget
  and shared fairly across open files, so a small-RAM box won't thrash under
  many concurrent readers.

!!! warning "RAM floor: below your working set, warm equals cold"
    A 14 GiB object never warms on a 4–8 GiB box — the memory tier there is only
    ~1.9 GiB (4 vCPU) or ~3.9 GiB (8 vCPU), far under the file. On those boxes
    the measured *warm* sequential rate equaled *cold* (~1.0–1.3 GB/s both),
    because every read missed the cache and refetched. Warm reuse needs
    **RAM ≥ working set**; plan for it or expect cold economics on every pass.

## Sharing the box with a memory-heavy job

lith sizes itself from the **whole box** (or its cgroup), not from what your job leaves free.
Uncapped, its footprint is about **twice `--mem-cache`**, because the tier is Go heap and the
collector targets ~2× the live set. `--memory-limit auto` engages only when the tier is at least
half the budget, so on a large box running a large job it stays off. Every start logs a
`memory footprint` line with the projected figure and what it rests on.

When the job itself uses most of the RAM, **cap lith explicitly**: a small `--mem-cache`, and
`--memory-limit` set to what the host can spare. The limit is soft, so leave headroom beyond it.

Measured ([#452](https://github.com/scttfrdmn/lith/issues/452)): DeepVariant packed three genomes
deep on a `c8a.16xlarge` (123 GiB). Sixty-four make_examples processes do indexed region reads of
one 32 GB BAM through lith while earlier genomes' call_variants run, and the job itself peaks at
~112 GiB.

| lith cap (`--mem-cache` / `--memory-limit`) | Outcome | lith peak RSS | min MemAvailable | S3 bytes vs distinct | Box wall, 3 genomes |
|---|---|---|---|---|---|
| 12 GiB / none | OOM when genome 3 started, twice | — | ~1.9 GiB before the kill | — | — |
| 3 GiB / 6 GiB, 2 reps | all done, gVCFs identical | 6.0 GiB | 5.3–5.7 GiB | 1.001× | 2,689 s, 3,216 s |
| 4 GiB / 7 GiB, 2 reps | all done, gVCFs identical | 7.0–7.1 GiB | 4.7 GiB | 1.0014× | 2,678 s, 2,682 s |
| BAM copied to EBS, no lith | all done | — | 11.7 GiB | — | 2,723 s |

The small tier cost nothing measurable: nothing was evicted unread, and make_examples matched
the EBS copy within run-to-run noise (the slower 3 GiB rep was ~20% slower on all three genomes,
on a different box). Region readers consume prefetch promptly, so they need the prefetch window,
not a large cache. With a limit in force, peak RSS sat at the limit at 6 and 7 GiB, but reached
**6.29 GiB against a 5 GiB limit**.

**For many region readers of one BGZF object**, a few GiB of tier is enough. Set `--memory-limit`
to what the host can spare, and keep the job's own peak plus that limit plus ~1 GiB under RAM. A
streaming or re-reading workload is different: size its tier from the working set
([`--mem-cache` in the knobs reference](knobs.md)).

## lith's own CPU cost is small

Serving bytes costs lith about **1.3 CPU-seconds per GB**, roughly constant
across box sizes (measured 1.2–1.6 for region, stream, and batch workloads,
[#77](https://github.com/scttfrdmn/lith/issues/77)). At a ~100 MB/s cold fetch
that's ≈**13 % of one core** — so even on a 2-vCPU box lith does not starve the
application; the rest of the CPU goes to your code.

## What it looks like, by class

Measured against `s3://1000genomes` in `us-east-1`, lith vs mountpoint-s3, all
MB/s. Fair config: lith `--disk-cache 0`, `--mem-cache` 25 % of RAM; single = a
14 GiB CRAM read sequentially; 8-reader = eight 3.8–14 GiB CRAMs concurrently;
warm = re-read from cache; metadata = 100k `stat`s from a prebuilt index. Full
per-run data in [`bench/results/`](https://github.com/scttfrdmn/lith/tree/main/bench/results),
method in [#52](https://github.com/scttfrdmn/lith/issues/52).

| instance | RAM | NVMe | cold seq (lith / mnt-s3) | warm seq | 8-reader (lith / mnt-s3) | 100k stat | warm rand4k |
|---|---|---|---|---|---|---|---|
| c8g.2xlarge | 16 GiB | no | 1354 / 1527 | 1163 | **1550** / 1602 | 0.16 s, 0 S3 | 543k IOPS |
| c8gd.4xlarge | 32 GiB | yes | 1440 / 1547 | 1400 | **1688** / 1637 | 0.17 s, 0 S3 | — |
| c8gd.16xlarge | 128 GiB | yes | **2848** / 2613 | **21383** | **3385** / 3221 | 0.16 s, 0 S3 | 589k IOPS |

**Reading guide: cold sequential is your NIC; warm and random are your cache;
metadata is free.** A single cold reader fills the NIC (readahead is sized to
the bandwidth-delay product), reaching mount-s3 parity or better; the 8-reader
aggregate reaches parity from a 16 GiB box up; warm reads come from RAM at
~20 GB/s once the working set fits; 100k `stat`s take ~0.15 s with **zero** S3
requests. mount-s3 still edges cold *single*-reader on the smallest burst-credit
boxes, where it rides NIC burst harder.

## No NVMe?

Local instance-store NVMe (`d` classes) only helps when your working set exceeds
RAM **and** you re-read it — point `--disk-cache` at the instance-store mount
then. Otherwise leave `--disk-cache` off (the default) and rely on the memory
tier plus the kernel page cache, or point `--disk-cache` at `/dev/shm` (tmpfs).
Never put the disk cache on EBS, EFS, or NFS — caching a block there can be
slower than refetching it from S3 in-region.

## The smallest box that works

The [crossover ladder](copy-or-mount.md) found `c8g.large` (2 vCPU, EBS-only)
runs selective, stream, and batch jobs **cheaper than copying** — so small is
viable. But its cold sequential rate **collapsed toward ~50 MB/s under sustained
load** once its burst credits were spent (a fresh box rides burst at ~1 GB/s).

So: **a burst-credit class is fine for selective and short jobs and wrong for
sustained streaming.** For a long single-pass or many-reader run, pick a class
whose baseline meets your demand. lith currently can't read the link speed on
the `c8g` family, so it falls back to a conservative in-flight budget rather than
sizing to the NIC — tracked in
[#79](https://github.com/scttfrdmn/lith/issues/79), the pending mitigation.

## How lith sizes itself

lith derives its S3 concurrency from the bandwidth-delay product automatically
(`--inflight-bytes`, default 2 × NIC bandwidth × 100 ms via `ethtool`). When the
NIC speed can't be detected it uses a fixed fallback; set `--inflight-bytes`
explicitly on those classes. The full set of levers is on the
[Knobs](knobs.md) page.
