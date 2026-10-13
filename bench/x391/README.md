# #391 cross-region fairness cell

**Question.** On a mount where the evidence gate is off — cross-region, by lith's region-pair
default — N concurrent readers finish far apart (#391: spread 3.08–3.38 measured in-region with the
gate forced off). Does a per-handle rule, the evidence gate at some ratio, restore fairness without
the 1.96× single-reader ramp penalty that is the reason the gate is off cross-region?

**Setup.** One `c8gd.4xlarge` in **us-west-2** reading `s3://scttfrdmn-lith-bench` in **us-east-1**
(~65 ms round trip). lith **v1.18.0**, the published artifact, hash-checked on the box. Every cell
on a fresh mount with the page cache dropped, so every read is cold. Arms: `def` (gate off, the
shipped cross-region default, confirmed `lith_readahead_evidence_ratio 0` from the running mount)
and `--readahead-evidence-ratio` 2 / 4 / 16 / 64. Arm order rotated per rep.

| file | run |
|---|---|
| `run.sh`, `results.csv` | default tier (8 GiB), 2 reps: single / n4 / d16 |
| `run-pressure.sh`, `results-pressure.csv` | **`--mem-cache 1GiB`**, 3 reps: single / d16 |

## Results

**Run 1 did not test the premise, and that is the first finding.** 16 × 512 MiB is 8 GiB of demand
against an 8 GiB tier: the working set FIT, `pressure_held` was 0 everywhere, every arm finished in
~15 s, and gate-off D16 spread was 1.27–1.32. #391's unfairness needs the tier to be contended —
GCHP's cell was 60 GB of demand against an 8.26 GB tier, with ~3000 evictions.

**Run 2, demand 8× the tier — the unfairness reproduces, and any evidence ratio removes it:**

| D16 | spread, 3 reps | median | last reader | pressure held |
|---|---|---|---|---|
| **gate off** | 3.18 / 2.70 / 2.89 | **2.89** | 17.7–22.4 s | 76–82 |
| ratio 2 | 1.17 / 1.29 / 1.39 | 1.29 | 15.2–16.5 s | 0 |
| ratio 4 | 1.37 / 1.35 / 1.34 | 1.35 | 15.8–15.9 s | 0 |
| ratio 16 | 1.63 / 1.33 / 1.17 | 1.33 | 15.6–15.8 s | 0 |
| ratio 64 | 1.26 / 1.26 / 1.20 | 1.26 | 15.4–16.2 s | 0 |

Bytes identical across arms (8.6 GB). The job also finishes **15–30% sooner** with the gate on,
because the last reader is no longer starved — fairness here is not cosmetic.

**Single reader, 2 GiB, across both runs (5 reps):** ratio 64 was ~3× slower in 3 of 5. Ratios
2–16 show no penalty by median (ratio 16: 0.94–1.00× the gate-off wall), but each arm, `def`
included, had one rep 3–5× slow — cross-region variance that 2–3 reps cannot separate from an effect.

## Why the default does not change

**This box may not be able to see the penalty.** A single cross-region stream here peaks at
~375 MB/s, bounded by latency, not by the NIC. The 1.96× was measured on a fast whole-object
consumer, and a ramp costs more the deeper the window the link needs — so a faster box is where the
penalty lives. A null from an instrument that cannot reach the mechanism is not a refutation.

So the remedy is **an operator setting**, documented in `docs/knobs.md`: a cross-region mount with
several concurrent readers and a tier small relative to what they read should set
`--readahead-evidence-ratio` (2–16; **not 64**).

## Cost

Two boxes, 13.0 + 11.1 min at $0.78384/hr = **$0.31** (Price List API). Inter-region transfer
290.5 GB read + 10.7 GB of uploads = 301 GB at **$0.02/GB** (Price List API, both directions) =
**$6.02**. **Total $6.33.** Test objects deleted afterwards; both boxes verified terminated.
