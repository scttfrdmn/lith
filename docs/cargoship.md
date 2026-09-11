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

## Measured (A1: many small objects)

`changelog_details` (1000genomes), packed with `cargoship upload --frame-size
16MiB`; full tree walk (`find … -exec cat`), cold, c8gd.4xlarge, us-east-1.

| files | lith-cargoship | lith-native (per-object) |
|---|---|---|
| 600 | **0.34 s**, 8 GETs | 2.56 s, 604 GETs |
| 1985 | **0.64 s**, 12 GETs | 14.05 s, 1998 GETs |

**7.5×–22× faster, ~150× fewer GETs.** Metadata (`stat`) is served from the index
— zero S3. <!-- number: session 33, #94 -->

## Limits (this release)

lith reads **fully-framed** 2.1 archives. CargoShip routes already-compressed or
small content (a `.vcf.gz`, an index sibling, a Zarr chunk) into plain, unframed
`.tar` chunks; lith rejects those with a clear message for now. Pack a
compressible tree for the streaming win; mixed archives (the direct-range /
header-walk path) are tracked for a later release. Encrypted (KMS-envelope)
manifests are rejected.
