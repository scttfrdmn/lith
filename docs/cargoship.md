# CargoShip archives

[CargoShip](https://github.com/scttfrdmn/cargoship) packs a directory tree into a
few compressed `.tar.zst` **chunks** for cheap, fast uploads. lith mounts such an
archive as its **original file tree** — build the index from the archive's
manifest, and every file appears at its original path even though its bytes live
inside a packed chunk. No `ListObjectsV2`, no unpacking.

## Why pack, then mount

Reading many small objects cold pays one S3 round-trip *per object* — the
Zarr/WebDataset/shard problem. If you pack them once with CargoShip, lith turns
that into **large-object streaming**: the files in a chunk were written in tree
order, so a directory-order walk reads the chunk sequentially, and lith fetches
only the covering zstd **frames** (each ~16 MiB, independently decodable, with a
per-frame checksum). Files that share a chunk share lith's cache, so the whole
walk is a handful of coalesced range GETs.

**Pack small files with CargoShip once; mount them as a tree forever.**

## Build the index and mount

```
# One command — build the index in-process from the manifest and mount it:
lith mount s3://bucket /mnt/archive \
  --cargoship s3://bucket/prefix/uploads/<id>/manifest.json.gz

# Or build a reusable index file first, then mount it:
lith index build --cargoship s3://bucket/prefix/uploads/<id>/manifest.json.gz \
  --index-file archive.idx
lith mount s3://bucket /mnt/archive --index-file archive.idx
```

`lith index inspect archive.idx` prints the archive id, format/features, and the
chunk and frame counts. The index records the manifest's sha256 as provenance.
`--cargoship` **fails closed**: a manifest that cannot be resolved (missing,
unparseable, wrong version, encrypted) is a hard error — it never falls back to
listing the bucket. `lith mounts` shows a CargoShip mount as `[cargoship:<key>]`.

## What the frame path costs

A read of a file maps to the zstd frame(s) covering its bytes in the chunk's
uncompressed tar stream: one coalesced range GET fetches them, each frame's
sha256 (over the compressed bytes) is verified — a mismatch is a hard `EIO`,
never a silent bad read — and the decoded bytes cache as ordinary 1 MiB chunks.
Decompression runs off the read path. A **decoded-frame cache** ([#137](https://github.com/scttfrdmn/lith/issues/137))
means each frame is fetched and decoded only once no matter how many files or
1 MiB chunks it covers: the first fill decodes the frame; every later fill it
covers is served with no GET and no decode. So a tree walk moves ~1× the
archive's compressed bytes regardless of the frame size.

## Framed and frameless chunks

A 2.1 archive mixes two chunk kinds, and lith reads both:

- **Framed `.tar.zst`** (compressible content): a read maps to the covering zstd
  frame(s) — one coalesced range GET, checksum-verified, decoded.
- **Frameless plain `.tar`** (already-compressed or small content — a `.vcf.gz`,
  an index sibling, a Zarr chunk): a read is a **direct range GET** at the file's
  `archive_offset`, no decode. This needs `archive_offset` on every file, which
  CargoShip records from **v0.24.3** (older archives with a null offset on a
  frameless file are rejected with an upgrade message).

## Measured

`c8gd.4xlarge`, us-east-1, cold; lith-cargoship vs the in-place reader.

| workload | lith-cargoship | comparison |
|---|---|---|
| **A1** tree walk, 1985 small files | **0.64 s**, 12 GETs | native per-object 14.05 s, 1998 GETs |
| **A1** tree walk, 600 small files | **0.34 s**, 8 GETs | native 2.56 s, 604 GETs |
| **A2** one-month Zarr query (packed NWM chrtout) | **9.9 s**, 184 GETs | lith-native raw 10.7 s; xarray+s3fs 14.3 s |
| **A3** tabix region (frameless VCF) | **0.5 s**, 4 GETs | native 0.5 s, 28 GETs |

A1 is **7.5×–22× faster** with ~150× fewer GETs; `stat` is index-served (0 S3).
A2 (packed Zarr) beats both raw-native and s3fs. A3's frameless VCF is within
noise of native. <!-- numbers: sessions 33–34, #94 -->

## Limits (this release)

- **A large file framed as one giant zstd frame is not readable.** CargoShip
  frames only at file boundaries, so a large *compressible-looking* file (it
  framed a 3.5 GB CRAM) becomes one multi-GB frame — a zstd frame isn't
  seekable, so lith caps decodable frame size and errors clearly. Store large,
  already-compressed files in frameless `.tar` chunks (lith reads those as direct
  ranges), or cut sub-frames (cargoship#502).
- **Encrypted (KMS-envelope) manifests are rejected.** 2.0 archives are
  unsupported (no `archive_offset`); re-pack with v0.24.3.
A tree-order walk continues its readahead across chunk boundaries, so multi-chunk
archives read as cleanly as single-chunk ones — there is no cross-chunk penalty.

## Choosing a frame size

lith keeps a **decoded-frame cache** ([#137](https://github.com/scttfrdmn/lith/issues/137)):
a fetched zstd frame is decoded once and served to every fill it covers, so a
sequential walk fetches and decodes each frame exactly once — walk bytes are ~1×
the archive's compressed size regardless of frame size (`lith_backing_frame_reuse_total`
counts the fills served from cache). What the frame size still governs is
**random single-file** over-fetch: a point read of one small file pulls its whole
covering frame, which only a smaller frame can shrink. Smaller frames move fewer
wasted bytes on random reads; the cost is a larger frame table in the manifest.
Measured on the A1 archive (1,985 small files, ~13 MB compressed;
[`bench/results/framesize-curve.csv`](https://github.com/scttfrdmn/lith/blob/main/bench/results/framesize-curve.csv),
before the frame cache — the tree-walk column is now ~1× at every size):

| `--frame-size` | archive | frames | tree-walk bytes | one-file read |
|---|---|---|---|---|
| 16 MiB (current default) | 13.1 MB | 13 | 40.0 MB (3.0×) | 3.30 MB |
| 8 MiB | 13.1 MB | 22 | 25.6 MB (2.0×) | 1.70 MB |
| **4 MiB** | 13.1 MB | 42 | **20.2 MB (1.5×)** | **0.60 MB** |
| 1 MiB | 13.2 MB | 152 | 15.0 MB (1.1×) | 0.90 MB |

The **archive size is flat** across frame sizes (zstd's per-frame context reset
costs almost nothing on this content), so smaller frames are close to free on
storage. **The frame size no longer matters for a sequential walk** — the frame
cache makes it ~1× at every size (the tree-walk column above is pre-cache). It
**still matters for random single-file access**: a point read pulls its whole
covering frame either way, so a smaller frame wastes fewer bytes (0.6 MB at 4 MiB
vs 3.3 MB at 16 MiB). If your access is random rather than a full walk, **pack
with `--frame-size 4MiB`** (the curve's knee; recommended as the cargoship default
in [cargoship#512](https://github.com/scttfrdmn/cargoship/issues/512)); use
frameless chunks for already-compressed large files, which pay no over-fetch at
all.
<!-- numbers: bench/results/framesize-curve.csv, CloudTrail ledger, sessions 36–37 -->
