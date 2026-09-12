#!/usr/bin/env bash
# R4 Parquet projection: lith-native (streaming) vs pyarrow-native, staged
# Overture in the bench bucket. Plus a clean-archive A1-cargoship (a1-single).
# Exclusive windows, 65s guard. $1 = a1-single manifest URL.
set -uo pipefail
export AWS_REGION=us-east-1
W=/mnt/nvme/work; MP=/mnt/nvme/mp; PY=/mnt/nvme/venv/bin/python
LITH=/mnt/nvme/lith/bin/lith; MAIN=/mnt/nvme/lith/bin/lith-main
A1S="$1"
BENCH=scttfrdmn-lith-bench; OVP=lith-bench/overture-places
uz(){ date -u +%Y-%m-%dT%H:%M:%SZ; }
now(){ date +%s.%N; }; el(){ awk -v a="$1" -v b="$2" 'BEGIN{printf "%.2f",b-a}'; }
lgets(){ curl -s "http://127.0.0.1:$1/metrics" 2>/dev/null | awk '/^lith_s3_requests_total\{op="get"/{s+=$2} END{print s+0}'; }
um(){ fusermount3 -u "$MP" 2>/dev/null; pkill -x lith 2>/dev/null; pkill -x lith-main 2>/dev/null; sleep 1; }
drop(){ sudo sh -c 'echo 3 > /proc/sys/vm/drop_caches'; }
gap(){ sleep 65; }
OV=$(AWS_REGION=us-east-1 aws s3 ls "s3://$BENCH/$OVP/" 2>/dev/null | awk '{print $4}' | head -1)

cat > /tmp/pyq_r4.py <<'PYQ'
import sys,time,pyarrow.dataset as ds, pyarrow.compute as pc, pyarrow.fs as fs
mode,target=sys.argv[1],sys.argv[2]
cols=["id","confidence","basic_category"]; filt=pc.field("basic_category")=="restaurant"
if mode=="s3":
    dataset=ds.dataset(target,filesystem=fs.S3FileSystem(region="us-east-1"),format="parquet")
else:
    dataset=ds.dataset(target,format="parquet")
t0=time.time(); tab=dataset.to_table(columns=cols,filter=filt); print(f"{time.time()-t0:.2f} {tab.num_rows}")
PYQ

# lith-native (streaming, tier2 off) over the mounted parquet
"$LITH" index build "s3://$BENCH/$OVP" --index-file "$W/r4.idx" >/dev/null 2>&1
um; drop; "$LITH" mount "s3://$BENCH/$OVP" "$MP" --index-file "$W/r4.idx" --metrics :9921 --daemon >/dev/null 2>&1
for i in $(seq 1 120); do mountpoint -q "$MP" && break; sleep 0.5; done
s=$(uz); t0=$(now); "$PY" /tmp/pyq_r4.py local "$MP/$OV" >/dev/null 2>&1; t1=$(now); e=$(uz)
echo "RUN|R4|lith-native|wall=$(el "$t0" "$t1")|start=$s|end=$e|lith_gets=$(lgets 9921)"; um; gap

# pyarrow-native (byte-precise) against the bench parquet directly
drop; s=$(uz); t0=$(now); "$PY" /tmp/pyq_r4.py s3 "$BENCH/$OVP/$OV" >/dev/null 2>&1; t1=$(now); e=$(uz)
echo "RUN|R4|pyarrow-native|wall=$(el "$t0" "$t1")|start=$s|end=$e|lith_gets=NA"; gap

# A1-cargoship on the clean single-chunk archive (a1-single)
"$LITH" index build --cargoship "$A1S" --index-file "$W/a1s.idx" --region us-east-1 >/dev/null 2>&1
um; drop; "$LITH" mount "s3://$BENCH" "$MP" --index-file "$W/a1s.idx" --metrics :9922 --daemon >/dev/null 2>&1
for i in $(seq 1 120); do mountpoint -q "$MP" && break; sleep 0.5; done
s=$(uz); t0=$(now); find "$MP" -type f -exec cat {} + >/dev/null 2>&1; t1=$(now); e=$(uz)
echo "RUN|A1|lith-cargoship-1chunk|wall=$(el "$t0" "$t1")|start=$s|end=$e|lith_gets=$(lgets 9922)"; um
