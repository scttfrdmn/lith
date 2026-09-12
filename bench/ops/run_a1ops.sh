#!/usr/bin/env bash
# A1 tree walk (find -exec cat) across four tools, one per EXCLUSIVE time window
# (65s gap = 60s ledger guard + slack), so CloudTrail S3 data events attribute to
# one run. Prints RUN lines: tool, wall, UTC start/end, lith self-reported GETs.
# $1 = cargoship manifest URL.
set -uo pipefail
export AWS_REGION=us-east-1
W=/mnt/nvme/work; MP=/mnt/nvme/mp; CP=/mnt/nvme/copydst; mkdir -p "$MP" "$CP"
LITH=/mnt/nvme/lith/bin/lith; MAIN=/mnt/nvme/lith/bin/lith-main
MAN="$1"
BENCH=scttfrdmn-lith-bench; RAW=lith-bench/a1-raw/changelog_details
uz(){ date -u +%Y-%m-%dT%H:%M:%SZ; }
now(){ date +%s.%N; }; el(){ awk -v a="$1" -v b="$2" 'BEGIN{printf "%.2f",b-a}'; }
lgets(){ curl -s "http://127.0.0.1:$1/metrics" 2>/dev/null | awk '/^lith_s3_requests_total\{op="get"/{s+=$2} END{print s+0}'; }
um(){ fusermount3 -u "$MP" 2>/dev/null; pkill -x lith 2>/dev/null; pkill -x lith-main 2>/dev/null; pkill -x mount-s3 2>/dev/null; sleep 1; }
drop(){ sudo sh -c 'echo 3 > /proc/sys/vm/drop_caches'; }
walk(){ find "$1" -type f -exec cat {} + >/dev/null 2>&1; }
gap(){ sleep 65; }

TOK=$(curl -sX PUT "http://169.254.169.254/latest/api/token" -H "X-aws-ec2-metadata-token-ttl-seconds:300" 2>/dev/null)
ROLE=$(curl -s -H "X-aws-ec2-metadata-token:$TOK" http://169.254.169.254/latest/meta-data/iam/security-credentials/ 2>/dev/null)
echo "BOX_ROLE=$ROLE"

"$LITH" index build --cargoship "$MAN" --index-file "$W/a1c.idx" --region us-east-1 >/dev/null 2>&1
"$MAIN" index build "s3://$BENCH/$RAW" --index-file "$W/a1n.idx" >/dev/null 2>&1

# 1. lith-cargoship
um; drop; "$LITH" mount "s3://$BENCH" "$MP" --index-file "$W/a1c.idx" --metrics :9901 --daemon >/dev/null 2>&1
for i in $(seq 1 120); do mountpoint -q "$MP" && break; sleep 0.5; done
s=$(uz); t0=$(now); walk "$MP"; t1=$(now); e=$(uz)
echo "RUN|A1|lith-cargoship|wall=$(el "$t0" "$t1")|start=$s|end=$e|lith_gets=$(lgets 9901)"; um; gap

# 2. lith-native
um; drop; "$MAIN" mount "s3://$BENCH/$RAW" "$MP" --index-file "$W/a1n.idx" --metrics :9902 --daemon >/dev/null 2>&1
for i in $(seq 1 120); do mountpoint -q "$MP" && break; sleep 0.5; done
s=$(uz); t0=$(now); walk "$MP"; t1=$(now); e=$(uz)
echo "RUN|A1|lith-native|wall=$(el "$t0" "$t1")|start=$s|end=$e|lith_gets=$(lgets 9902)"; um; gap

# 3. mount-s3
um; drop; mount-s3 "$BENCH" "$MP" --prefix "$RAW/" --read-only >/dev/null 2>&1
for i in $(seq 1 120); do mountpoint -q "$MP" && break; sleep 0.5; done
s=$(uz); t0=$(now); walk "$MP"; t1=$(now); e=$(uz)
echo "RUN|A1|mount-s3|wall=$(el "$t0" "$t1")|start=$s|end=$e|lith_gets=NA"; fusermount3 -u "$MP" 2>/dev/null; pkill -x mount-s3 2>/dev/null; sleep 1; gap

# 4. copy-best (s5cmd download, then cat = compute)
rm -rf "$CP"/*; drop
s=$(uz); t0=$(now); s5cmd cp "s3://$BENCH/$RAW/*" "$CP/" >/dev/null 2>&1; walk "$CP"; t1=$(now); e=$(uz)
echo "RUN|A1|copy-best|wall=$(el "$t0" "$t1")|start=$s|end=$e|lith_gets=NA"
