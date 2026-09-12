#!/usr/bin/env bash
# A3 mixed archive: flagstat on the framed CRAM + tabix on the frameless VCF,
# lith-cargoship vs native. Cold x2. Criterion 4: within 10% on wall.
# $1 = cargoship manifest s3:// URL
set -uo pipefail
export AWS_REGION=us-east-1
W=/mnt/nvme/work; MP=/mnt/nvme/mp; mkdir -p "$MP"
LITH=/mnt/nvme/lith/bin/lith; MAIN=/mnt/nvme/lith/bin/lith-main
MAN="$1"
CRAM=HG00096.mapped.ILLUMINA.bwa.GBR.low_coverage.20120522.bam.cram
VCF=ALL.chr22.phase3_shapeit2_mvncall_integrated_v5a.20130502.genotypes.vcf.gz
now(){ date +%s.%N; }; el(){ awk -v a="$1" -v b="$2" 'BEGIN{printf "%.2f",b-a}'; }
scr(){ curl -s "http://127.0.0.1:$1/metrics" 2>/dev/null; }
gets(){ awk '/^lith_s3_requests_total\{op="get"/{s+=$2} END{print s+0}' <<<"$1"; }
mvk(){ awk -v k="$2" '$1==k{print $2+0}' <<<"$1"; }
um(){ fusermount3 -u "$MP" 2>/dev/null; pkill -x lith 2>/dev/null; pkill -x lith-main 2>/dev/null; sleep 1; }
drop(){ sudo sh -c 'echo 3 > /proc/sys/vm/drop_caches'; }

printf 'phase3/data/HG00096/alignment/%s\nphase3/data/HG00096/alignment/%s.crai\nrelease/20130502/%s\nrelease/20130502/%s.tbi\n' "$CRAM" "$CRAM" "$VCF" "$VCF" > "$W/a3keys.txt"
"$LITH" index build --cargoship "$MAN" --index-file "$W/a3c.idx" --region us-east-1 >/dev/null 2>&1 || { echo "cargo build failed"; exit 1; }
"$MAIN" index build s3://1000genomes --keys "$W/a3keys.txt" --no-sign-request --index-file "$W/a3n.idx" >/dev/null 2>&1 || { echo "native build failed"; exit 1; }

cmount(){ um; drop; "$LITH" mount s3://scttfrdmn-lith-bench "$MP" --index-file "$W/a3c.idx" --metrics ":$1" --daemon >/dev/null 2>&1
  for i in $(seq 1 120); do mountpoint -q "$MP" && break; sleep 0.5; done; }
nmount(){ um; drop; "$MAIN" mount s3://1000genomes "$MP" --index-file "$W/a3n.idx" --no-sign-request --metrics ":$1" --daemon >/dev/null 2>&1
  for i in $(seq 1 120); do mountpoint -q "$MP" && break; sleep 0.5; done; }

for r in 1 2; do
  cmount 9911; t0=$(now); samtools flagstat "$MP/$CRAM" >/dev/null 2>&1; t1=$(now); m=$(scr 9911)
  echo "A3|cargoship-flagstat|rep$r|wall=$(el "$t0" "$t1")|gets=$(gets "$m")|frames=$(mvk "$m" lith_backing_frames_fetched_total)|s3_MB=$(awk -v b=$(mvk "$m" lith_s3_bytes_total) 'BEGIN{printf "%.0f",b/1048576}')"; um
  cmount 9911; t0=$(now); tabix "$MP/$VCF" 22:16000000-17000000 2>/dev/null | wc -l >/dev/null; t1=$(now); m=$(scr 9911)
  echo "A3|cargoship-tabix|rep$r|wall=$(el "$t0" "$t1")|gets=$(gets "$m")|s3_MB=$(awk -v b=$(mvk "$m" lith_s3_bytes_total) 'BEGIN{printf "%.1f",b/1048576}')"; um
done
for r in 1 2; do
  nmount 9912; t0=$(now); samtools flagstat "$MP/phase3/data/HG00096/alignment/$CRAM" >/dev/null 2>&1; t1=$(now); m=$(scr 9912)
  echo "A3|native-flagstat|rep$r|wall=$(el "$t0" "$t1")|gets=$(gets "$m")"; um
  nmount 9912; t0=$(now); tabix "$MP/release/20130502/$VCF" 22:16000000-17000000 2>/dev/null | wc -l >/dev/null; t1=$(now); m=$(scr 9912)
  echo "A3|native-tabix|rep$r|wall=$(el "$t0" "$t1")|gets=$(gets "$m")"; um
done
