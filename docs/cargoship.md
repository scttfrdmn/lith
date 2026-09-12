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
# 2.1 archive manifest (any upload under the archive prefix)
lith index build --cargoship s3://bucket/prefix/uploads/<id>/manifest.json.gz \
  --index-file archive.idx
lith mount s3://bucket archive.idx /mnt/archive
```

`lith index inspect archive.idx` prints the archive id, format/features, and the
chunk and frame counts. The index records the manifest's sha256 as provenance.

## What the frame path costs

A read of a file maps to the zstd frame(s) covering its bytes in the chunk's
uncompressed tar stream: one coalesced range GET fetches them, each frame's
sha256 (over the compressed bytes) is verified — a mismatch is a hard `EIO`,
never a silent bad read — and the decoded bytes cache as ordinary 1 MiB chunks.
Decompression runs off the read path.

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
- **Framed-chunk reads re-fetch a frame per fill** (no frame cache yet), so a
  tree walk *over-fetches bytes* — each 1 MiB fill decodes its whole covering
  frame and discards the rest. This shows up as **bytes, not GETs**: readahead
  still coalesces the walk into a handful of GETs (a 1,985-file, 3-chunk archive
  walks in ~24 GETs regardless of frame size), but the bytes moved are a multiple
  of the archive size — 3× at the 16 MiB default (see "Choosing a frame size").
  It is not chunk-count–dependent: a tree-order walk continues its readahead
  across chunk boundaries, so multi-chunk archives read as cleanly as single-chunk
  ones. Frameless plain-`.tar` chunks avoid it entirely. Tracked in
  [#137](https://github.com/scttfrdmn/lith/issues/137).

## Choosing a frame size

CargoShip's `--frame-size` sets the zstd frame granularity. Because lith fetches
whole frames, the frame size is the **over-fetch unit**: a single-file random read
pulls its entire covering frame, and a sequential walk (until the frame cache in
[#137](https://github.com/scttfrdmn/lith/issues/137) lands) re-fetches each frame
once per 1 MiB fill it covers. Smaller frames move fewer wasted bytes; the cost is
a larger frame table in the manifest. Measured on the A1 archive (1,985 small
files, ~13 MB compressed; [`bench/results/framesize-curve.csv`](https://github.com/scttfrdmn/lith/blob/main/bench/results/framesize-curve.csv)):

| `--frame-size` | archive | frames | tree-walk bytes | one-file read |
|---|---|---|---|---|
| 16 MiB (current default) | 13.1 MB | 13 | 40.0 MB (3.0×) | 3.30 MB |
| 8 MiB | 13.1 MB | 22 | 25.6 MB (2.0×) | 1.70 MB |
| **4 MiB** | 13.1 MB | 42 | **20.2 MB (1.5×)** | **0.60 MB** |
| 1 MiB | 13.2 MB | 152 | 15.0 MB (1.1×) | 0.90 MB |

The **archive size is flat** across frame sizes (zstd's per-frame context reset
costs almost nothing on this content), so smaller frames are close to free on
storage while cutting over-fetch sharply. The knee is **4 MiB**: over-fetch drops
from 3.0× to 1.5× and a single-file read from 3.3 MB to 0.6 MB, then flattens
while the frame table keeps growing. **Pack with `--frame-size 4MiB`** for
mount-heavy archives (recommended as the cargoship default in
[cargoship#512](https://github.com/scttfrdmn/cargoship/issues/512)); use frameless
chunks for already-compressed large files, which pay no over-fetch at all.
<!-- numbers: bench/results/framesize-curve.csv, CloudTrail ledger, session 36 -->
