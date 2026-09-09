# v0.2 benchmark objects (1000genomes, us-east-1, recovered by exact byte-match to session 13)

All keys under `s3://1000genomes/`. Reference: GRCh37 (phase3 low_coverage CRAMs are
mapped to hs37d5; contig names have no `chr` prefix). CRAM decode uses samtools'
default EBI ref registry via `REF_CACHE`/`REF_PATH`.

| workload | object | bytes |
|---|---|---|
| **R1** tabix 1000×10kb | `release/20130502/ALL.chr20.phase3_shapeit2_mvncall_integrated_v5a.20130502.genotypes.vcf.gz` (+`.tbi`) | 341,680,844 |
| **S1** flagstat (3.8 GB stream) | `phase3/data/HG00096/alignment/HG00096.mapped.ILLUMINA.bwa.GBR.low_coverage.20120522.bam.cram` | 3,818,656,947 |
| **R2** samtools view -c 1000×1Mb (14 GB selective) | `phase3/data/HG00100/alignment/HG00100.mapped.ILLUMINA.bwa.GBR.low_coverage.20130415.bam.cram` (+`.crai`) | 14,086,328,874 |

**W3 / 8-reader heterogeneous set** (8 phase3 GBR low_coverage CRAMs, sum = 60,032,714,457 = session-13 W3 exactly):

| sample | date tag | bytes |
|---|---|---|
| HG00096 | 20120522 | 3,818,656,947 |
| HG00097 | 20130415 | 9,418,885,895 |
| HG00099 | 20130415 | 7,389,443,435 |
| HG00100 | 20130415 | 14,086,328,874 |
| HG00101 | 20130415 | 7,213,795,188 |
| HG00102 | 20130415 | 6,938,439,259 |
| HG00103 | 20120522 | 4,042,259,956 |
| HG00105 | 20130415 | 7,124,904,903 |

Path pattern: `phase3/data/<sample>/alignment/<sample>.mapped.ILLUMINA.bwa.GBR.low_coverage.<date>.bam.cram`.

Region files (deterministic, seed 13): `r1-regions-chr20-10kb.txt` (1000×10kb on chr20), `r2-regions-1Mb.txt` (1000×1Mb concentrated on chr20 — heavy overlap so lith's chunk cache dedups re-fetches to a ~0.8 GB working set, matching session-13's selective R2; scattered genome-wide 1Mb windows would defeat the cache and read >2× the file).
