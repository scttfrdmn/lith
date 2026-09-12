#!/usr/bin/env bash
# Session-38 NFS gateway prototype spike (#143/#144), one host + loopback client.
# Compares (b) native Go NFSv3 over lith core vs (a) nfs-ganesha VFS over the lith
# FUSE mount. Datasets from one list index over lith-bench/: a 3.82 GB object
# (sequential read + CPU/GB) and a 1,985-entry dir (ls -l, 100k stat).
set -uo pipefail
export AWS_REGION=us-east-1
BENCH=scttfrdmn-lith-bench
W=/mnt/nvme/work; LITH=/mnt/nvme/lith/bin/lith; SPIKE=/mnt/nvme/lith/bin/lithnfsspike
FUSEMP=/mnt/nvme/lithmp; NFSMP=/mnt/nvme/nfsmp; mkdir -p "$FUSEMP" "$NFSMP"
BIG="cargoship/a3-245/uploads/20260912-22be64f3/shard-0/chunk-1.tar"   # 3.82 GB, index-relative
DIR="a1-raw/changelog_details"
BIGSZ=3818658816
now(){ date +%s.%N; }; el(){ awk -v a="$1" -v b="$2" 'BEGIN{printf "%.2f",b-a}'; }
drop(){ sudo sh -c 'echo 3 > /proc/sys/vm/drop_caches'; }
cpu(){ awk '{print $14+$15}' /proc/"$1"/stat 2>/dev/null; }  # utime+stime (clock ticks)
HZ=$(getconf CLK_TCK)

# Build a list index over lith-bench/ (object-backed; both datasets under it).
"$LITH" index build "s3://$BENCH/lith-bench" --index-file "$W/nfs.idx" --region us-east-1 >/dev/null 2>&1
echo "INDEX keys=$($LITH index inspect "$W/nfs.idx" 2>/dev/null | awk '/^keys:/{print $2}')"

measure(){ # $1=config label  $2=gateway-pids (space-sep, for CPU)
  local label=$1; shift; local pids="$*"
  # sequential read
  drop; local c0 t0 t1; c0=0; for p in $pids; do c0=$((c0+$(cpu "$p"))); done
  t0=$(now); dd if="$NFSMP/$BIG" of=/dev/null bs=1M 2>/dev/null; t1=$(now)
  local c1=0; for p in $pids; do c1=$((c1+$(cpu "$p"))); done
  local secs; secs=$(el "$t0" "$t1")
  local mbps cpugb
  mbps=$(awk -v b=$BIGSZ -v s="$secs" 'BEGIN{printf "%.0f",(b/1048576)/s}')
  cpugb=$(awk -v c=$((c1-c0)) -v hz="$HZ" -v b=$BIGSZ 'BEGIN{printf "%.2f",(c/hz)/(b/1073741824)}')
  echo "RESULT|$label|seq_read_MBps=$mbps|seq_wall=$secs|cpu_per_GB_s=$cpugb"
  # ls -l cold
  drop; t0=$(now); ls -l "$NFSMP/$DIR" >/dev/null 2>&1; t1=$(now)
  echo "RESULT|$label|ls_l_1985_wall=$(el "$t0" "$t1")"
  # ~100k stat (50 passes over 1985 entries)
  t0=$(now); for i in $(seq 1 50); do stat "$NFSMP/$DIR"/* >/dev/null 2>&1; done; t1=$(now)
  echo "RESULT|$label|stat_100k_wall=$(el "$t0" "$t1")"
}

# ---- (b) native Go NFSv3 over lith core ----
sudo umount "$NFSMP" 2>/dev/null || true
pkill -x lithnfsspike 2>/dev/null || true; sleep 1
"$SPIKE" --index-file "$W/nfs.idx" --bucket "$BENCH" --region us-east-1 --addr :2049 >/tmp/spike.log 2>&1 &
SPID=$!; sleep 3
sudo mount -t nfs -o vers=3,proto=tcp,port=2049,mountport=2049,nolock,hard,timeo=60 127.0.0.1:/ "$NFSMP" 2>/tmp/mountb.log
if mountpoint -q "$NFSMP"; then measure "native-go-nfs" "$SPID"; else echo "RESULT|native-go-nfs|MOUNT_FAILED $(cat /tmp/mountb.log)"; fi
sudo umount "$NFSMP" 2>/dev/null || true; kill "$SPID" 2>/dev/null || true; sleep 2

# ---- (a) nfs-ganesha VFS over the lith FUSE mount ----
if command -v ganesha.nfsd >/dev/null; then
  fusermount3 -u "$FUSEMP" 2>/dev/null || true; pkill -x lith 2>/dev/null || true; sleep 1
  "$LITH" mount "s3://$BENCH/lith-bench" "$FUSEMP" --index-file "$W/nfs.idx" --daemon >/tmp/lithmount.log 2>&1
  for i in $(seq 1 60); do mountpoint -q "$FUSEMP" && break; sleep 0.5; done
  cat > /tmp/ganesha.conf <<EOF
NFS_CORE_PARAM { Nb_Worker = 16; NFS_Protocols = 3; }
EXPORT {
  Export_Id = 1; Path = $FUSEMP; Pseudo = /lithexport; Access_Type = RO;
  Squash = No_Root_Squash; Protocols = 3; Transports = TCP;
  FSAL { Name = VFS; }
}
EOF
  sudo mkdir -p /var/run/ganesha /var/lib/nfs/ganesha
  sudo systemctl start rpcbind 2>/dev/null || sudo rpcbind 2>/dev/null || true
  sudo pkill -x ganesha.nfsd 2>/dev/null || true; sleep 1
  sudo ganesha.nfsd -f /tmp/ganesha.conf -L /tmp/ganesha.log -N NIV_EVENT 2>/tmp/ganesha.err || true
  sleep 4
  GPID=$(pgrep -x ganesha.nfsd | head -1); LPID=$(pgrep -x lith | head -1)
  sudo mount -t nfs -o vers=3,proto=tcp,nolock,hard,timeo=60 127.0.0.1:$FUSEMP "$NFSMP" 2>/tmp/mounta.log || \
  sudo mount -t nfs -o vers=3,proto=tcp,nolock,hard,timeo=60 127.0.0.1:/lithexport "$NFSMP" 2>>/tmp/mounta.log
  if mountpoint -q "$NFSMP"; then measure "ganesha-over-fuse" "$GPID $LPID"; else echo "RESULT|ganesha-over-fuse|MOUNT_FAILED $(cat /tmp/mounta.log; tail -3 /tmp/ganesha.log 2>/dev/null)"; fi
  sudo umount "$NFSMP" 2>/dev/null || true; sudo pkill -x ganesha.nfsd 2>/dev/null || true
  fusermount3 -u "$FUSEMP" 2>/dev/null || true; pkill -x lith 2>/dev/null || true
else
  echo "RESULT|ganesha-over-fuse|GANESHA_NOT_INSTALLED"
fi
echo NFSSPIKE_DONE
