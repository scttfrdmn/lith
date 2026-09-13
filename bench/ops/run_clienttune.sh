#!/usr/bin/env bash
# Session-40 Step 1: NFS client mount-option grid for single-stream sequential
# read of the 3.82 GB object through the lith gateway, loopback, one host.
# Baseline is session-39's 74 MB/s (defaults); FUSE mount is the ceiling.
set -uo pipefail
export AWS_REGION=us-east-1
BENCH=scttfrdmn-lith-bench; ROOT=lith-bench; L=/mnt/nvme/lith/bin/lith; W=/mnt/nvme/work
FUSEMP=/mnt/nvme/fusemp; NFSMP=/mnt/nvme/nfsmp; mkdir -p "$FUSEMP" "$NFSMP" "$W"
BIG=cargoship/a3-245/uploads/20260912-22be64f3/shard-0/chunk-1.tar; BIGSZ=3818658816
now(){ date +%s.%N; }; el(){ awk -v a="$1" -v b="$2" 'BEGIN{printf "%.2f",b-a}'; }
mbps(){ awk -v b=$BIGSZ -v s="$1" 'BEGIN{printf "%.0f",(b/1048576)/s}'; }
drop(){ sudo sh -c 'echo 3 > /proc/sys/vm/drop_caches'; }
umnt(){ sudo umount "$NFSMP" 2>/dev/null||true; fusermount3 -u "$FUSEMP" 2>/dev/null||true; pkill -x lith 2>/dev/null||true; sleep 1; }

"$L" index build "s3://$BENCH/$ROOT" --index-file "$W/nfs.idx" --region us-east-1 >/dev/null 2>&1

# FUSE ceiling
umnt; drop
"$L" mount "s3://$BENCH/$ROOT" "$FUSEMP" --index-file "$W/nfs.idx" --daemon >/dev/null 2>&1
for i in $(seq 1 60); do mountpoint -q "$FUSEMP" && break; sleep 0.5; done
t0=$(now); dd if="$FUSEMP/$BIG" of=/dev/null bs=1M 2>/dev/null; t1=$(now)
echo "TUNE|fuse-ceiling|$(mbps "$(el "$t0" "$t1")") MB/s"
umnt

# Gateway
"$L" serve nfs "s3://$BENCH/$ROOT" --index-file "$W/nfs.idx" --listen :2049 >/tmp/gw.log 2>&1 &
GPID=$!; sleep 3
base="vers=3,proto=tcp,port=2049,mountport=2049,nolock,hard,timeo=60"
run(){ # $1=label $2=extra-opts
  sudo umount "$NFSMP" 2>/dev/null||true; drop
  sudo mount -t nfs -o "$base${2:+,$2}" 127.0.0.1:/ "$NFSMP" 2>/tmp/m.log || { echo "TUNE|$1|MOUNT_FAILED $(cat /tmp/m.log)"; return; }
  local t0 t1; t0=$(now); dd if="$NFSMP/$BIG" of=/dev/null bs=1M 2>/dev/null; t1=$(now)
  echo "TUNE|$1|$(mbps "$(el "$t0" "$t1")") MB/s | opts=$base${2:+,$2}"
  sudo umount "$NFSMP" 2>/dev/null||true
}
run "defaults" ""
run "rw1M" "rsize=1048576,wsize=1048576"
run "rw1M+nconnect4" "rsize=1048576,wsize=1048576,nconnect=4"
run "rw1M+nconnect8" "rsize=1048576,wsize=1048576,nconnect=8"
run "rw1M+nconnect16" "rsize=1048576,wsize=1048576,nconnect=16"
run "rw1M+nconnect8+actimeo600" "rsize=1048576,wsize=1048576,nconnect=8,actimeo=600"
kill "$GPID" 2>/dev/null||true
echo CLIENTTUNE_DONE
