# R4 footer-family benchmark target (#108)

**Substitution note.** The session-27 prompt named Common Crawl's columnar index
(`s3://commoncrawl/cc-index/table/cc-main/warc/`). That bucket **denies
`ListObjectsV2`** (both anonymous and authenticated: `AWS Error ACCESS_DENIED`),
and lith's mount/index build requires a *listable* prefix. Anonymous listing
works on other public buckets (1000genomes lists fine). So R4 uses an equivalent
**anonymously-listable real-world columnar projection workload**: Overture Maps
`places`. The benchmark's essence is preserved — a selective projection of a few
small columns from a large, many-row-group Parquet whose bulk is other columns.

- Bucket/prefix (us-west-2, anonymous, listable):
  `s3://overturemaps-us-west-2/release/2026-08-19.0/theme=places/type=place/`
- 16 files, 10.5 GB total; each file **256 row groups**, ~4.6 M rows, ~650 MB.
  The box is us-east-1, so reads are cross-region — this inflates every wall
  equally (lith and pyarrow alike), so the relative comparison holds; byte/GET
  counts are region-independent.
- **Query (R4):** `pyarrow.dataset` projecting `id, confidence, basic_category`,
  filter `basic_category == 'restaurant'`. The projection is **20.2 %** of the
  file's compressed bytes (127 MB of 632 MB on part-00000) — the rest is
  `geometry`, `names`, `addresses`, `sources`, etc. `basic_category` is both the
  filter and a projected column (mirrors CC's `url_host_tld == 'edu'`).
- 8 files used for the 8-concurrent runs: `part-00000` … `part-00007`
  (634.9, 704.6, 701.5, 705.9, 623.9, 633.1, 692.2, 667.9 MB).

## In-region staging (session 29, #124 measurement)

CC `cc-index`/`nyc-tlc` now deny direct S3 (CDN-only), so R4 is measured on a
**staged in-region copy** of the Overture places partition:

- Source: `s3://overturemaps-us-west-2/release/2026-08-19.0/theme=places/type=place/`
  (8 files `part-00000`…`part-00007`).
- Dest (us-east-1, writable from the spored role): `s3://scttfrdmn-lith-bench/lith-bench/overture-places/`.
- Copied with `s5cmd` (download anon us-west-2 → upload us-east-1): **5,363,950,212 bytes** (5.36 GB), one-time cross-region egress ≈ $0.11; upload same-region (free).
- **7-day bucket lifecycle → expires ~2026-09-18.** The copy step (session-29 driver `bench/footer/`) is rerunnable to restage.
- All session-29 R4 numbers are **in-region** (box us-east-1 ↔ bucket us-east-1). Session-27/28 cross-region rows stay in `apps.csv` marked as such.
