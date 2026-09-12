#!/usr/bin/env bash
# Session-36 verified re-measure on cargoship v0.24.5. Builds each cargoship
# index and ASSERTS it is a cargoship index (source==cargoship) before mounting,
# so a failed build can never silently auto-list the bucket (the session-35
# artifact). Exclusive 65s windows; prints RUN lines with UTC + lith metrics.
# $1 = A1-245 manifest URL, $2 = A3-245 manifest URL.
set -uo pipefail
export AWS_REGION=us-east-1
W=/mnt/nvme/work; MP=/mnt/nvme/mp; mkdir -p "$MP"
LITH=/mnt/nvme/lith/bin/lith; MAIN=/mnt/nvme/lith/bin/lith-main
A1M="$1"; A3M="$2"
BENCH=scttfrdmn-lith-bench; RAW=lith-bench/a1-raw/changelog_details
CRAM=HG00096.mapped.ILLUMINA.bwa.GBR.low_coverage.20120522.bam.cram
uz(){ date -u +%Y-%m-%dT%H:%M:%SZ; }
now(){ date +%s.%N; }; el(){ awk -v a="$1" -v b="$2" 'BEGIN{printf "%.2f",b-a}'; }
scr(){ curl -s "http://127.0.0.1:$1/metrics" 2>/dev/null; }
gv(){ awk '/^lith_s3_requests_total\{op="get"/{s+=$2} END{print s+0}' <<<"$1"; }
bv(){ awk '$1=="lith_s3_bytes_total"{printf "%.0f",$2/1048576}' <<<"$1"; }
fv(){ awk '$1=="lith_backing_frames_fetched_total"{print $2+0}' <<<"$1"; }
um(){ fusermount3 -u "$MP" 2>/dev/null; pkill -x lith 2>/dev/null; pkill -x lith-main 2>/dev/null; sleep 1; }
drop(){ sudo sh -c 'echo 3 > /proc/sys/vm/drop_caches'; }
gap(){ sleep 65; }
# build + ASSERT cargoship, else abort
buildcargo(){ "$LITH" index build --cargoship "$1" --index-file "$2" --region us-east-1 >/dev/null 2>&1
  local src=$("$LITH" index inspect "$2" 2>/dev/null | awk '$1=="source:"{print $2}')
  [ "$src" = "cargoship" ] || { echo "FATAL: $2 source=$src (not cargoship) — build failed"; return 1; }; }

buildcargo "$A1M" "$W/a1.idx" || exit 1
buildcargo "$A3M" "$W/a3.idx" || exit 1
"$MAIN" index build "s3://$BENCH/$RAW" --index-file "$W/a1n.idx" >/dev/null 2>&1
printf 'phase3/data/HG00096/alignment/%s\nphase3/data/HG00096/alignment/%s.crai\n' "$CRAM" "$CRAM" > "$W/a3keys.txt"
"$MAIN" index build s3://1000genomes --keys "$W/a3keys.txt" --no-sign-request --index-file "$W/a3n.idx" >/dev/null 2>&1

# A1 lith-cargoship (v0.24.5)
um; drop; "$LITH" mount "s3://$BENCH" "$MP" --index-file "$W/a1.idx" --metrics :9940 --daemon >/dev/null 2>&1
for i in $(seq 1 120); do mountpoint -q "$MP" && break; sleep 0.5; done
nf=$(find "$MP" -type f|wc -l); s=$(uz); t0=$(now); find "$MP" -type f -exec cat {} + >/dev/null 2>&1; t1=$(now); m=$(scr 9940)
echo "RUN|A1|lith-cargoship-245|files=$nf|wall=$(el "$t0" "$t1")|gets=$(gv "$m")|MB=$(bv "$m")|frames=$(fv "$m")|start=$s|end=$(uz)"; um; gap
# A1 lith-native
um; drop; "$MAIN" mount "s3://$BENCH/$RAW" "$MP" --index-file "$W/a1n.idx" --metrics :9941 --daemon >/dev/null 2>&1
for i in $(seq 1 120); do mountpoint -q "$MP" && break; sleep 0.5; done
s=$(uz); t0=$(now); find "$MP" -type f -exec cat {} + >/dev/null 2>&1; t1=$(now); m=$(scr 9941)
echo "RUN|A1|lith-native|wall=$(el "$t0" "$t1")|gets=$(gv "$m")|MB=$(bv "$m")|start=$s|end=$(uz)"; um; gap
# A3 flagstat lith-cargoship (v0.24.5 frameless CRAM)
um; drop; "$LITH" mount "s3://$BENCH" "$MP" --index-file "$W/a3.idx" --metrics :9942 --daemon >/dev/null 2>&1
for i in $(seq 1 120); do mountpoint -q "$MP" && break; sleep 0.5; done
s=$(uz); t0=$(now); samtools flagstat "$MP/$CRAM" >/dev/null 2>&1; t1=$(now); m=$(scr 9942)
echo "RUN|A3-flagstat|lith-cargoship-245|wall=$(el "$t0" "$t1")|gets=$(gv "$m")|MB=$(bv "$m")|frames=$(fv "$m")|start=$s|end=$(uz)"; um; gap
# A3 flagstat lith-native
um; drop; "$MAIN" mount s3://1000genomes "$MP" --index-file "$W/a3n.idx" --no-sign-request --metrics :9943 --daemon >/dev/null 2>&1
for i in $(seq 1 120); do mountpoint -q "$MP" && break; sleep 0.5; done
s=$(uz); t0=$(now); samtools flagstat "$MP/phase3/data/HG00096/alignment/$CRAM" >/dev/null 2>&1; t1=$(now); m=$(scr 9943)
echo "RUN|A3-flagstat|lith-native|wall=$(el "$t0" "$t1")|gets=$(gv "$m")|MB=$(bv "$m")|start=$s|end=$(uz)"; um
