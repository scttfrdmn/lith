#!/usr/bin/env bash
# Reach the window FLOOR, which 22 distinct objects could not: the divisor counts per
# DESCRIPTOR, not per object, so 256 descriptors on one file drive budgetBlocks/256 -> 2.
# That is the regime the reporting workload measured at 6-10x.
set -uo pipefail
MP=/mnt/mp; W=/tmp/w; PORT=9812; mkdir -p "$W"
P=s3://1000genomes/release/20130502
REG="--region us-east-1 --no-sign-request"
OBJ=ALL.chr1.phase3_shapeit2_mvncall_integrated_v5a.20130502.genotypes.vcf.gz
sv(){ awk -v k="$2" 'index($0,k)==1 {s+=$NF} END{printf "%.0f", s+0}' <<<"$1"; }
um(){ fusermount3 -u "$MP" 2>/dev/null; pkill -x lith 2>/dev/null; pkill -f holdfds 2>/dev/null; sleep 1; }

# A holder that opens the SAME object N times and reads nothing.
cat > /tmp/holdfds.py <<'PY'
import sys, time
path, n = sys.argv[1], int(sys.argv[2])
fds = [open(path, 'rb') for _ in range(n)]
sys.stderr.write("held %d\n" % len(fds)); sys.stderr.flush()
time.sleep(900)
PY

cell(){ # bin label holders
  local bin=$1 lab=$2 holders=$3
  um; rm -f "$W/idx"; sudo sh -c 'echo 3 > /proc/sys/vm/drop_caches'
  "$bin" mount "$P" "$MP" --index-file "$W/idx" $REG --nic-gbps 7.5 \
    --mem-cache 8GiB --metrics ":$PORT" --daemon >/dev/null 2>&1
  for i in $(seq 1 60); do mountpoint -q "$MP" && break; sleep 0.5; done
  mountpoint -q "$MP" || { echo "MOUNTFAIL $lab"; return 1; }
  if [ "$holders" -gt 0 ]; then
    python3 /tmp/holdfds.py "$MP/$OBJ" "$holders" 2>/tmp/held.$holders &
    for i in $(seq 1 60); do grep -q held /tmp/held.$holders 2>/dev/null && break; sleep 0.5; done
  fi
  local m0; m0=$(curl -s "http://127.0.0.1:$PORT/metrics")
  local t0 t1; t0=$(date +%s.%N); cat "$MP/$OBJ" >/dev/null 2>&1; t1=$(date +%s.%N)
  local m; m=$(curl -s "http://127.0.0.1:$PORT/metrics")
  echo "RESULT|$lab|holders=$holders|openHandles=$(sv "$m0" lith_open_handles)|window=$(sv "$m0" lith_readahead_window_blocks)|wall=$(awk -v a="$t0" -v b="$t1" 'BEGIN{printf "%.2f",b-a}')|bytes=$(sv "$m" lith_s3_bytes_total)|gets=$(sv "$m" lith_s3_requests_total)|committed=$(sv "$m" lith_prefetch_committed_bytes)|refused=$(sv "$m" lith_prefetch_refused_total)|evicted=$(sv "$m" lith_prefetch_evicted_unread_total)"
  um
}
echo "===== #301 phase 2b: reach the floor, 256 descriptors on one object $(date -u) ====="
for h in 64 256; do
  for r in 1 2 3; do
    cell /tmp/lith-old-bin "old-divisor"   "$h"
    cell /tmp/lith-new     "new-admission" "$h"
  done
done
echo "===== done ====="
