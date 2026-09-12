#!/usr/bin/env bash
# Session-37 frame-cache re-measure (#137/#141, v0.3.2). Measures the A1 tree walk
# and a single-file random read BEFORE (lith-main = main) and AFTER (lith =
# framecache-failclosed branch), at 16 MiB and 4 MiB frame sizes, plus A3/A2
# regression. Exit: after-walk bytes within 20% of the archive's compressed size
# at BOTH frame sizes; a same-frame neighbour read is 0 GETs. Every cargoship
# index build ASSERTS source==cargoship before mounting (closes the auto-list
# footgun). Exclusive 65s ledger windows; numbers via lith metrics (== ledger).
# Args: $1=fs16 manifest key  $2=fs4 manifest key  $3=a3-245 manifest key  $4=a2 manifest key
set -uo pipefail
export AWS_REGION=us-east-1
W=/mnt/nvme/work; MP=/mnt/nvme/mp; mkdir -p "$MP"
AFTER=/mnt/nvme/lith/bin/lith; BEFORE=/mnt/nvme/lith/bin/lith-main
BENCH=scttfrdmn-lith-bench
CRAM=HG00096.mapped.ILLUMINA.bwa.GBR.low_coverage.20120522.bam.cram
FS16="$1"; FS4="$2"; A3M="$3"; A2M="$4"
uz(){ date -u +%Y-%m-%dT%H:%M:%SZ; }
now(){ date +%s.%N; }; el(){ awk -v a="$1" -v b="$2" 'BEGIN{printf "%.2f",b-a}'; }
scr(){ curl -s "http://127.0.0.1:$1/metrics" 2>/dev/null; }
gv(){ awk '/^lith_s3_requests_total\{op="get"/{s+=$2} END{print s+0}' <<<"$1"; }
bv(){ awk '$1=="lith_s3_bytes_total"{printf "%.1f",$2/1048576}' <<<"$1"; }
dv(){ awk '$1=="lith_backing_decompress_bytes_total"{printf "%.1f",$2/1048576}' <<<"$1"; }
rv(){ awk '$1=="lith_backing_frame_reuse_total"{print $2+0}' <<<"$1"; }
fv(){ awk '$1=="lith_backing_frames_fetched_total"{print $2+0}' <<<"$1"; }
um(){ fusermount3 -u "$MP" 2>/dev/null; pkill -x lith 2>/dev/null; pkill -x lith-main 2>/dev/null; sleep 1; }
drop(){ sudo sh -c 'echo 3 > /proc/sys/vm/drop_caches'; }
gap(){ sleep 65; }

buildcargo(){ # $1=binary $2=manifest $3=idxout
  "$1" index build --cargoship "$2" --index-file "$3" --region us-east-1 >/dev/null 2>&1
  local src; src=$("$1" index inspect "$3" 2>/dev/null | awk '$1=="source:"{print $2}')
  [ "$src" = cargoship ] || { echo "FATAL: $3 source=$src (not cargoship)"; return 1; }
}
archsize(){ s5cmd cat "s3://$BENCH/$1" 2>/dev/null | gzip -dc 2>/dev/null | python3 -c "import json,sys;d=json.load(sys.stdin);print(sum(c.get('compressed_size',0) for c in d['chunks']), sum(len(c.get('frames',[])) for c in d['chunks']))"; }

walk(){ # $1=binary $2=idx $3=label $4=port
  um; drop; "$1" mount "s3://$BENCH" "$MP" --index-file "$2" --metrics ":$4" --daemon >/dev/null 2>&1
  for i in $(seq 1 120); do mountpoint -q "$MP" && break; sleep 0.5; done
  local nf s t0 t1 m; nf=$(find "$MP" -type f|wc -l); s=$(uz); t0=$(now)
  find "$MP" -type f -exec cat {} + >/dev/null 2>&1; t1=$(now); m=$(scr "$4")
  echo "WALK|$3|files=$nf|wall=$(el "$t0" "$t1")|gets=$(gv "$m")|MB=$(bv "$m")|decomp_MB=$(dv "$m")|reuse=$(rv "$m")|frames=$(fv "$m")|start=$s|end=$(uz)"
  um
}

echo "ARCH|fs16|$(archsize "$FS16")"   # compressed_bytes frame_count
echo "ARCH|fs4|$(archsize "$FS4")"

# A1 tree walk — before/after at each frame size (exclusive windows).
buildcargo "$BEFORE" "s3://$BENCH/$FS16" "$W/b16.idx" || exit 1
buildcargo "$AFTER"  "s3://$BENCH/$FS16" "$W/a16.idx" || exit 1
buildcargo "$BEFORE" "s3://$BENCH/$FS4"  "$W/b4.idx"  || exit 1
buildcargo "$AFTER"  "s3://$BENCH/$FS4"  "$W/a4.idx"  || exit 1
walk "$BEFORE" "$W/b16.idx" "before-16MiB" 9960; gap
walk "$AFTER"  "$W/a16.idx" "after-16MiB"  9961; gap
walk "$BEFORE" "$W/b4.idx"  "before-4MiB"  9962; gap
walk "$AFTER"  "$W/a4.idx"  "after-4MiB"   9963; gap

# Single-file random read + same-frame neighbour (after, 16 MiB frames).
um; drop; "$AFTER" mount "s3://$BENCH" "$MP" --index-file "$W/a16.idx" --metrics :9970 --daemon >/dev/null 2>&1
for i in $(seq 1 120); do mountpoint -q "$MP" && break; sleep 0.5; done
mapfile -t F < <(find "$MP" -type f | sort)
m0=$(scr 9970); cat "${F[900]}" >/dev/null 2>&1; m1=$(scr 9970)
echo "RANDREAD1|after-16MiB|file=${F[900]##*/}|gets=$(( $(gv "$m1")-$(gv "$m0") ))|MB=$(awk -v a=$(bv "$m0") -v b=$(bv "$m1") 'BEGIN{printf "%.2f",b-a}')|decomp_MB=$(awk -v a=$(dv "$m0") -v b=$(dv "$m1") 'BEGIN{printf "%.2f",b-a}')"
m0=$(scr 9970); cat "${F[901]}" >/dev/null 2>&1; m1=$(scr 9970)
echo "RANDREAD2-neighbour|after-16MiB|file=${F[901]##*/}|gets=$(( $(gv "$m1")-$(gv "$m0") ))|reuse_delta=$(( $(rv "$m1")-$(rv "$m0") ))"
um; gap

# Single-file random read (after, 4 MiB frames) — smaller covering frame.
um; drop; "$AFTER" mount "s3://$BENCH" "$MP" --index-file "$W/a4.idx" --metrics :9971 --daemon >/dev/null 2>&1
for i in $(seq 1 120); do mountpoint -q "$MP" && break; sleep 0.5; done
mapfile -t F < <(find "$MP" -type f | sort)
m0=$(scr 9971); cat "${F[900]}" >/dev/null 2>&1; m1=$(scr 9971)
echo "RANDREAD1|after-4MiB|file=${F[900]##*/}|gets=$(( $(gv "$m1")-$(gv "$m0") ))|MB=$(awk -v a=$(bv "$m0") -v b=$(bv "$m1") 'BEGIN{printf "%.2f",b-a}')|decomp_MB=$(awk -v a=$(dv "$m0") -v b=$(dv "$m1") 'BEGIN{printf "%.2f",b-a}')"
um; gap

# A3 flagstat: frameless CRAM (after) vs native.
buildcargo "$AFTER" "s3://$BENCH/$A3M" "$W/a3.idx" || exit 1
um; drop; "$AFTER" mount "s3://$BENCH" "$MP" --index-file "$W/a3.idx" --metrics :9980 --daemon >/dev/null 2>&1
for i in $(seq 1 120); do mountpoint -q "$MP" && break; sleep 0.5; done
s=$(uz); t0=$(now); samtools flagstat "$MP/$CRAM" >/dev/null 2>&1; t1=$(now); m=$(scr 9980)
echo "A3|after-flagstat|wall=$(el "$t0" "$t1")|gets=$(gv "$m")|MB=$(bv "$m")|start=$s|end=$(uz)"; um; gap
printf 'phase3/data/HG00096/alignment/%s\nphase3/data/HG00096/alignment/%s.crai\n' "$CRAM" "$CRAM" > "$W/a3keys.txt"
"$AFTER" index build s3://1000genomes --keys "$W/a3keys.txt" --no-sign-request --index-file "$W/a3n.idx" >/dev/null 2>&1
um; drop; "$AFTER" mount s3://1000genomes "$MP" --index-file "$W/a3n.idx" --no-sign-request --metrics :9981 --daemon >/dev/null 2>&1
for i in $(seq 1 120); do mountpoint -q "$MP" && break; sleep 0.5; done
s=$(uz); t0=$(now); samtools flagstat "$MP/phase3/data/HG00096/alignment/$CRAM" >/dev/null 2>&1; t1=$(now); m=$(scr 9981)
echo "A3|native-flagstat|wall=$(el "$t0" "$t1")|gets=$(gv "$m")|MB=$(bv "$m")|start=$s|end=$(uz)"; um; gap

# A2 packed-Zarr walk (after) — regression check.
buildcargo "$AFTER" "s3://$BENCH/$A2M" "$W/a2.idx" || exit 1
um; drop; "$AFTER" mount "s3://$BENCH" "$MP" --index-file "$W/a2.idx" --metrics :9990 --daemon >/dev/null 2>&1
for i in $(seq 1 120); do mountpoint -q "$MP" && break; sleep 0.5; done
s=$(uz); t0=$(now); find "$MP" -type f -exec cat {} + >/dev/null 2>&1; t1=$(now); m=$(scr 9990)
echo "A2|after-walk|wall=$(el "$t0" "$t1")|gets=$(gv "$m")|MB=$(bv "$m")|reuse=$(rv "$m")|start=$s|end=$(uz)"; um
echo MEASURE37_DONE
