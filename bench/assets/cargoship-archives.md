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

## Session 34 (cargoship v0.24.3, frameless backing)

Re-uploaded with v0.24.3 (`archive_offset` on every file); 7-day lifecycle, **expires ~2026-09-19**.

| archive | prefix | manifest sha256 | notes |
|---|---|---|---|
| **A3 mixed** | `a3-243/` | `d5663cd90eb66020c4bdd53aa539e75e078316918a5edf96f735cb1bf5946267` | CRAM framed (giant frame, unreadable — cargoship#502); VCF/crai/tbi frameless (readable) |
| **A2 packed-Zarr** | `a2-zarr/` | `b836bdaf5ce13b832e4e60fc1c23c3d423a329e2e6887a7d291ec52647caf090` | 104 files (one-month streamflow selection) → 4 framed chunks; A2 criterion MET |
| A2 native-raw | `zarr-native/chrtout.zarr/` | — | raw staged chunks for the lith-native comparison |

Drivers: `bench/cargoship/{bootstrap34.sh,run_a3.sh,run_a1.sh}`, plus A2 `/tmp/run_a2.sh`+`pyq_a2.py` (one-month streamflow query).
