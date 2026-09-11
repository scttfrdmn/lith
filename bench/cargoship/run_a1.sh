#!/usr/bin/env bash
# A1 small-files: full tree walk (cat every file) over a CargoShip-packed archive
# vs the native prefix. Criterion 4: lith-cargoship cold <= 1/2 lith-native wall
# with <= 1/10 the GETs.
set -uo pipefail
export AWS_REGION=us-east-1
W=/mnt/nvme/work; LOG=$W/a1.log; : > "$LOG"; exec > >(tee -a "$LOG") 2>&1
LITH=/mnt/nvme/lith/bin/lith; MAIN=/mnt/nvme/lith/bin/lith-main
MAN="$1" # cargoship manifest s3:// URL
BENCH=scttfrdmn-lith-bench
now(){ date +%s.%N; }; el(){ awk -v a="$1" -v b="$2" 'BEGIN{printf "%.2f",b-a}'; }
scrape(){ curl -s "http://127.0.0.1:$1/metrics" 2>/dev/null; }
mval(){ awk -v k="$2" '$1==k{print $2}' <<<"$1"|tail -1; }
mgets(){ awk '/^lith_s3_requests_total\{op="get"/{s+=$2} END{print s+0}' <<<"$1"; }
mb(){ awk -v b="${1:-0}" 'BEGIN{printf "%.0f",b/1048576}'; }
MP=/mnt/nvme/mp; mkdir -p "$MP"
um(){ fusermount3 -u "$MP" 2>/dev/null; pkill -x lith 2>/dev/null; pkill -x lith-main 2>/dev/null; sleep 1; }
drop(){ sudo sh -c 'echo 3 > /proc/sys/vm/drop_caches'; }

# Build the indexes once.
"$LITH" index build --cargoship "$MAN" --index-file "$W/a1c.idx" --region us-east-1 >/dev/null 2>&1 || { echo "cargo index build failed"; exit 1; }
"$MAIN" index build s3://1000genomes/changelog_details --index-file "$W/a1n.idx" --no-sign-request >/dev/null 2>&1 || { echo "native index build failed"; exit 1; }

walk(){ find "$MP" -type f -exec cat {} + >/dev/null 2>&1; }

runcargo(){ local label=$1 c=() warm m
  for r in 1 2 3; do um; drop
    "$LITH" mount "s3://$BENCH" "$MP" --index-file "$W/a1c.idx" --metrics :9901 --daemon >/dev/null 2>&1
    for i in $(seq 1 120); do mountpoint -q "$MP" && break; sleep 0.5; done
    local t0; t0=$(now); walk; local t1; t1=$(now); c+=("$(el "$t0" "$t1")")
    if [ "$r" = 1 ]; then m=$(scrape 9901); local w0; w0=$(now); walk; local w1; w1=$(now); warm=$(el "$w0" "$w1"); fi
    um; done
  echo "A1|lith-cargoship|cold=${c[0]},${c[1]},${c[2]}|warm=$warm|s3_MB=$(mb "$(mval "$m" lith_s3_bytes_total)")|gets=$(mgets "$m")|frames=$(mval "$m" lith_backing_frames_fetched_total)|uncovered=$(mval "$m" lith_prefetch_uncovered_total)"
}
runnative(){ local c=() warm m
  for r in 1 2 3; do um; drop
    "$MAIN" mount s3://1000genomes "$MP" --index-file "$W/a1n.idx" --no-sign-request --metrics :9902 --daemon >/dev/null 2>&1
    for i in $(seq 1 120); do mountpoint -q "$MP" && break; sleep 0.5; done
    local t0; t0=$(now); walk; local t1; t1=$(now); c+=("$(el "$t0" "$t1")")
    if [ "$r" = 1 ]; then m=$(scrape 9902); local w0; w0=$(now); walk; local w1; w1=$(now); warm=$(el "$w0" "$w1"); fi
    um; done
  echo "A1|lith-native|cold=${c[0]},${c[1]},${c[2]}|warm=$warm|s3_MB=$(mb "$(mval "$m" lith_s3_bytes_total)")|gets=$(mgets "$m")|frames=NA|uncovered=$(mval "$m" lith_prefetch_uncovered_total)"
}
# metadata: 100k stat on the cargoship mount = 0 S3
metastat(){ um; drop; "$LITH" mount "s3://$BENCH" "$MP" --index-file "$W/a1c.idx" --metrics :9903 --daemon >/dev/null 2>&1
  for i in $(seq 1 120); do mountpoint -q "$MP" && break; sleep 0.5; done
  mapfile -t FILES < <(find "$MP" -type f | head -100)
  local t0; t0=$(now); for i in $(seq 1 1000); do for f in "${FILES[@]}"; do stat "$f" >/dev/null 2>&1; done; done; local t1; t1=$(now)
  m=$(scrape 9903)
  echo "A1META|100k_stat|wall=$(el "$t0" "$t1")|gets=$(mgets "$m")"
  um; }

runcargo
runnative
metastat
echo "===== A1 DONE ====="
grep -E '^A1' "$LOG"
