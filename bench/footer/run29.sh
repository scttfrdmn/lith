#!/usr/bin/env bash
# R4 #124 in-region: base (main) vs post (#122+#124) vs pyarrow-native, staged bucket.
set -uo pipefail
W=/mnt/nvme/work; LOG=$W/r29.log
exec > >(tee -a "$LOG") 2>&1
echo "===== r29 $(date -u) ====="
PY=/mnt/nvme/venv/bin/python; Q=/tmp/ft/pyquery29.py
BKT=scttfrdmn-lith-bench; REL=lith-bench/overture-places
PREFIX=s3://$BKT/$REL
mapfile -t FILES < /mnt/nvme/work/ov8.txt
MP=/mnt/nvme/mp; mkdir -p "$MP"; IDX=$W/r29.idx
scrape(){ curl -s "http://127.0.0.1:$1/metrics" 2>/dev/null; }
mval(){ awk -v k="$2" '$1==k{print $2}' <<<"$1"|tail -1; }
mgets(){ awk '/^lith_s3_requests_total\{op="get"/{s+=$2} END{print s+0}' <<<"$1"; }
mfill(){ awk -v kind="$2" '$0 ~ "lith_fill_bytes_total\\{kind=\""kind"\"" {s+=$2} END{printf "%.0f",s+0}' <<<"$1"; }
IFACE=$(ip route get 1.1.1.1 2>/dev/null | grep -oP "dev \K\S+" | head -1); rx(){ cat /sys/class/net/$IFACE/statistics/rx_bytes; }
now(){ date +%s.%N; }; el(){ awk -v a="$1" -v b="$2" 'BEGIN{printf "%.2f",b-a}'; }
mb(){ awk -v b="${1:-0}" 'BEGIN{printf "%.1f",b/1048576}'; }
um(){ fusermount3 -u "$MP" 2>/dev/null; pkill -x lith-base 2>/dev/null; pkill -x lith-post 2>/dev/null; sleep 1; }
mount_fresh(){ um; rm -f "$IDX"; sudo sh -c 'echo 3 > /proc/sys/vm/drop_caches'
  "$1" mount "$PREFIX" "$MP" --index-file "$IDX" --metrics ":$2" --daemon
  for i in $(seq 1 100); do mountpoint -q "$MP" && break; sleep 0.5; done; mountpoint -q "$MP"||{ echo MFAIL;return 1;}; }

single(){ # label binpath port
  local label=$1 bin=$2 port=$3 c=() warm m
  mount_fresh "$bin" "$port" || return 1; "$PY" "$Q" local "$MP/${FILES[0]}" >/dev/null 2>&1; um
  for r in 1 2 3; do
    mount_fresh "$bin" "$port" || return 1
    local t0; t0=$(now); "$PY" "$Q" local "$MP/${FILES[0]}" >/dev/null 2>&1; local t1; t1=$(now); c+=("$(el "$t0" "$t1")")
    if [ "$r" = 1 ]; then m=$(scrape "$port")
      local w0; w0=$(now); "$PY" "$Q" local "$MP/${FILES[0]}" >/dev/null 2>&1; local w1; w1=$(now); warm=$(el "$w0" "$w1"); fi
    um
  done
  echo "R29S|$label|cold=${c[0]},${c[1]},${c[2]}|warm=$warm|s3_MB=$(mb "$(mval "$m" lith_s3_bytes_total)")|gets=$(mgets "$m")|distinct_MB=$(mb "$(mval "$m" lith_distinct_bytes_read)")|runs=$(mval "$m" lith_fill_runs_total)|plan_MB=$(mb "$(mfill "$m" plan)")|gap_MB=$(mb "$(mfill "$m" gap)")|demand_MB=$(mb "$(mfill "$m" demand)")|whole_MB=$(mb "$(mfill "$m" whole)")"
}
single_py(){ local c=() rxmb
  for r in 1 2 3; do sudo sh -c 'echo 3 > /proc/sys/vm/drop_caches'
    local r0; r0=$(rx); local t0; t0=$(now); "$PY" "$Q" s3 "$BKT/$REL/${FILES[0]}" >/dev/null 2>&1; local t1; t1=$(now); local r1; r1=$(rx)
    c+=("$(el "$t0" "$t1")"); rxmb=$(mb "$((r1-r0))"); done
  echo "R29S|pyarrow-native|cold=${c[0]},${c[1]},${c[2]}|warm=NA|s3_MB=$rxmb(rx)|gets=NA|distinct_MB=NA"
}
conc(){ local label=$1 bin=$2 port=$3
  for rep in 1 2; do mount_fresh "$bin" "$port" || return 1
    local t0; t0=$(now); pids=()
    for i in 0 1 2 3 4 5 6 7; do "$PY" "$Q" local "$MP/${FILES[$i]}" >/dev/null 2>&1 & pids+=($!); done
    for p in "${pids[@]}"; do wait "$p"; done
    local t1; t1=$(now); m=$(scrape "$port")
    echo "R29C|$label|rep$rep|wall=$(el "$t0" "$t1")|s3_MB=$(mb "$(mval "$m" lith_s3_bytes_total)")|gets=$(mgets "$m")"
    um; done
}
conc_py(){ for rep in 1 2; do sudo sh -c 'echo 3 > /proc/sys/vm/drop_caches'
    local r0; r0=$(rx); local t0; t0=$(now); pids=()
    for i in 0 1 2 3 4 5 6 7; do "$PY" "$Q" s3 "$BKT/$REL/${FILES[$i]}" >/dev/null 2>&1 & pids+=($!); done
    for p in "${pids[@]}"; do wait "$p"; done
    local t1; t1=$(now); local r1; r1=$(rx)
    echo "R29C|pyarrow-native|rep$rep|wall=$(el "$t0" "$t1")|s3_MB=$(mb "$((r1-r0))")(rx)|gets=NA"; done
}
BB=/mnt/nvme/lith/bin/lith-base; BP=/mnt/nvme/lith/bin/lith-post
single "base"        "$BB" 9801
single "post(#124)"  "$BP" 9802
single_py
conc "base"        "$BB" 9803
conc "post(#124)"  "$BP" 9804
conc_py
echo "===== r29 DONE $(date -u) ====="
grep -E '^R29' "$LOG"
