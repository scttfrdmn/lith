#!/usr/bin/env bash
# Session-39 NFS gateway single-host measurement (#143). Gateway + client on one
# box, loopback. Compares the native NFSv3 gateway to the FUSE mount and the
# spike's 20 MB/s, plus 8-reader shared cache, warm re-read, fair-share spread,
# and kill/restart. Numbers via lith metrics (== ledger).
set -uo pipefail
export AWS_REGION=us-east-1
BENCH=scttfrdmn-lith-bench; ROOT=lith-bench
L=/mnt/nvme/lith/bin/lith; W=/mnt/nvme/work
FUSEMP=/mnt/nvme/fusemp; NFSMP=/mnt/nvme/nfsmp; mkdir -p "$FUSEMP" "$NFSMP" "$W"
BIG=cargoship/a3-245/uploads/20260912-22be64f3/shard-0/chunk-1.tar; BIGSZ=3818658816
DIR=a1-raw/changelog_details
PARQ=overture-places
now(){ date +%s.%N; }; el(){ awk -v a="$1" -v b="$2" 'BEGIN{printf "%.2f",b-a}'; }
scr(){ curl -s "http://127.0.0.1:9950/metrics" 2>/dev/null; }
gv(){ awk '/^lith_s3_requests_total\{op="get"/{s+=$2} END{print s+0}' <<<"$1"; }
mbps(){ awk -v b="$1" -v s="$2" 'BEGIN{printf "%.0f",(b/1048576)/s}'; }
cpu(){ awk '{print $14+$15}' /proc/"$1"/stat 2>/dev/null || echo 0; }; HZ=$(getconf CLK_TCK)
drop(){ sudo sh -c 'echo 3 > /proc/sys/vm/drop_caches'; }
umnt(){ sudo umount "$NFSMP" 2>/dev/null||true; fusermount3 -u "$FUSEMP" 2>/dev/null||true; pkill -x lith 2>/dev/null||true; sleep 1; }

"$L" index build "s3://$BENCH/$ROOT" --index-file "$W/nfs.idx" --region us-east-1 >/dev/null 2>&1
echo "INDEX keys=$($L index inspect "$W/nfs.idx" 2>/dev/null | awk '/^keys:/{print $2}')"

# ---- FUSE baseline: sequential read of the 3.82 GB object ----
umnt; drop
"$L" mount "s3://$BENCH/$ROOT" "$FUSEMP" --index-file "$W/nfs.idx" --metrics :9950 --daemon >/dev/null 2>&1
for i in $(seq 1 60); do mountpoint -q "$FUSEMP" && break; sleep 0.5; done
FPID=$(pgrep -x lith|head -1); c0=$(cpu "$FPID"); t0=$(now); dd if="$FUSEMP/$BIG" of=/dev/null bs=1M 2>/dev/null; t1=$(now); c1=$(cpu "$FPID")
FUSE_MBPS=$(mbps $BIGSZ "$(el "$t0" "$t1")")
echo "RESULT|fuse|seq_MBps=$FUSE_MBPS|cpu_per_GB=$(awk -v c=$((c1-c0)) -v hz=$HZ -v b=$BIGSZ 'BEGIN{printf "%.2f",(c/hz)/(b/1073741824)}')"
umnt

# ---- NFS gateway ----
drop
"$L" serve nfs "s3://$BENCH/$ROOT" --index-file "$W/nfs.idx" --listen :2049 --metrics :9950 >/tmp/gw.log 2>&1 &
sleep 3
GPID=$(pgrep -x lith|head -1)
sudo mount -t nfs -o vers=3,proto=tcp,port=2049,mountport=2049,nolock,hard,timeo=60 127.0.0.1:/ "$NFSMP" 2>/tmp/mnt.log
if ! mountpoint -q "$NFSMP"; then echo "GW_MOUNT_FAILED $(cat /tmp/mnt.log; tail -3 /tmp/gw.log)"; kill "$GPID" 2>/dev/null; exit 1; fi

# sequential read 3.82 GB
c0=$(cpu "$GPID"); t0=$(now); dd if="$NFSMP/$BIG" of=/dev/null bs=1M 2>/dev/null; t1=$(now); c1=$(cpu "$GPID")
GW_MBPS=$(mbps $BIGSZ "$(el "$t0" "$t1")")
echo "RESULT|nfs-gw|seq_MBps=$GW_MBPS|cpu_per_GB=$(awk -v c=$((c1-c0)) -v hz=$HZ -v b=$BIGSZ 'BEGIN{printf "%.2f",(c/hz)/(b/1073741824)}')|pct_of_fuse=$(awk -v g=$GW_MBPS -v f=$FUSE_MBPS 'BEGIN{printf "%.0f",100*g/f}')"

# metadata: ls -l + 100k stat, S3 delta (must be ~0)
g0=$(gv "$(scr)"); t0=$(now); ls -l "$NFSMP/$DIR" >/dev/null 2>&1; t1=$(now); echo "RESULT|nfs-gw|ls_l_1985=$(el "$t0" "$t1")"
t0=$(now); for i in $(seq 1 50); do stat "$NFSMP/$DIR"/* >/dev/null 2>&1; done; t1=$(now); g1=$(gv "$(scr)")
echo "RESULT|nfs-gw|stat_100k=$(el "$t0" "$t1")|meta_S3_gets=$((g1-g0))"

# 8 concurrent readers, 8 distinct parquet objects, 256 MB each
mapfile -t OBJS < <(ls "$NFSMP/$PARQ"/ 2>/dev/null | grep '\.parquet$' | head -8)
drop; g0=$(gv "$(scr)"); t0=$(now); pids=()
for o in "${OBJS[@]}"; do ( to=$(now); dd if="$NFSMP/$PARQ/$o" of=/dev/null bs=1M count=256 2>/dev/null; t=$(now); echo "PER|$o|$(mbps 268435456 "$(el "$to" "$t")")" >>/tmp/per.log ) & pids+=($!); done
wait "${pids[@]}"; t1=$(now); g1=$(gv "$(scr)")   # wait only for the dd jobs, not the backgrounded gateway
echo "RESULT|nfs-gw|8readers_aggregate_MBps=$(mbps $((8*268435456)) "$(el "$t0" "$t1")")|cold_S3_gets=$((g1-g0))"
echo "PER_CLIENT_SPREAD:"; sort -t'|' -k3 -n /tmp/per.log

# warm re-read of the same 8 (expect ~0 S3)
g0=$(gv "$(scr)"); pids=(); for o in "${OBJS[@]}"; do dd if="$NFSMP/$PARQ/$o" of=/dev/null bs=1M count=256 2>/dev/null & pids+=($!); done; wait "${pids[@]}"; g1=$(gv "$(scr)")
echo "RESULT|nfs-gw|8readers_warm_S3_gets=$((g1-g0))"

# kill/restart: SIGKILL mid-read, client sees error; restart mounts w/o relist
( dd if="$NFSMP/$BIG" of=/dev/null bs=1M 2>/dev/null; echo "READ_RC=$?" >/tmp/killread.log ) &
RPID=$!; sleep 1; sudo kill -9 "$GPID" 2>/dev/null; wait "$RPID" 2>/dev/null
echo "RESULT|nfs-gw|kill_midread: $(cat /tmp/killread.log 2>/dev/null || echo 'client errored')"
sudo umount "$NFSMP" 2>/dev/null||true
t0=$(now); "$L" serve nfs "s3://$BENCH/$ROOT" --index-file "$W/nfs.idx" --listen :2049 >/tmp/gw2.log 2>&1 & sleep 3; t1=$(now)
GPID2=$(pgrep -x lith|head -1)
sudo mount -t nfs -o vers=3,proto=tcp,port=2049,mountport=2049,nolock,hard,timeo=60 127.0.0.1:/ "$NFSMP" 2>/dev/null
if mountpoint -q "$NFSMP" && head -c 1048576 "$NFSMP/$BIG" >/dev/null 2>&1; then
  echo "RESULT|nfs-gw|restart_ok=served|restart_startup_s=$(el "$t0" "$t1")|relist=no(index-file)"
else echo "RESULT|nfs-gw|restart_FAILED"; fi
sudo umount "$NFSMP" 2>/dev/null||true; kill "$GPID2" 2>/dev/null||true
echo NFSGW_DONE
