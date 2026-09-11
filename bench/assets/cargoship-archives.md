# CargoShip test archives (#94, session 33)

Built with `cargoship v0.24.2 upload --frame-size 16MiB` into the bench bucket
`s3://scttfrdmn-lith-bench/lith-bench/cargoship/` (7-day lifecycle; **expires ~2026-09-18** — re-run `bench/cargoship/bootstrap.sh` + the upload commands to rebuild). All from in-region 1000genomes.

| archive | source | files | chunks | notes |
|---|---|---|---|---|
| **A1** `a1-single/` | `s3://1000genomes/changelog_details/` (1985 small files) | 1985 | 3 (framed .tar.zst) | many-small-objects; fully framed. Manifest sha256 `b1f12c5574016d14cab4e60c0f4b9261b5ce816dc4d977f1fee8ecc8d0f51fcd` |
| **A3** `a3-framed/` | 1× 3.56 GB CRAM + .crai + chr22 VCF (205 MB) + .tbi | 4 | 3 (mixed) | CRAM framed; VCF/crai/tbi in plain .tar — needs the header-walk path (deferred) |

## Build commands (recorded)

```
# stage + pack A1 (compressible small files → fully framed)
s5cmd --no-sign-request cp "s3://1000genomes/changelog_details/*" ./a1src/
cargoship upload ./a1src s3://scttfrdmn-lith-bench/lith-bench/cargoship/a1-single \
  -r us-east-1 --frame-size 16MiB --shard-count 1 --direct-upload-threshold-mb 1

# A3 (mixed): CRAM + crai + VCF + tbi
cargoship upload ./a3src s3://scttfrdmn-lith-bench/lith-bench/cargoship/a3-framed \
  -r us-east-1 --frame-size 16MiB --shard-count 1 --compression-level 6
```

Drivers: `bench/cargoship/bootstrap.sh`, `bench/cargoship/run_a1.sh`.

## Notes
- A committed read-path fixture (small, deterministic) lives at `internal/cargoship/testdata/fixture/` (manifest + 38 KB chunk).
- CargoShip records intermediate staging snapshots of a chunk under one `s3_key`; the resolver keeps the complete (max `compressed_size`) entry. Chunk `id` is only unique within a shard, so chunks are keyed by `s3_key`.
