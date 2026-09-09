#!/usr/bin/env bash
# launch.sh MODE N TTLmin [VOLGIB] — the fan-out launch spec (session 17).
# One invocation per cell; MODE=lith|copy. Launches a spawn spot array of
# c8g.2xlarge, each node runs node.sh (fetched from this branch) for its shard,
# polls the results prefix to completion, and reports wall + billed cost.
# lifecycle: --cost-limit 1/node, --on-complete terminate, --ttl per cell.
set -u
MODE=$1; N=$2; TTL=$3; VOLGIB=${4:-20}
RUN="${MODE}-n${N}"; ARR="fanout-$RUN-$(date +%s | tail -c 5)"
RAW=https://raw.githubusercontent.com/scttfrdmn/lith/bench/fanout-v0.2/bench/fanout/node.sh
RATE=0.31904
export AWS_PROFILE=aws
aws s3 rm "s3://scttfrdmn-lith-bench/lith-bench/fanout/$RUN/" --recursive --quiet 2>/dev/null
SUBMIT=$(date +%s)
spawn launch "$ARR" --instance-type c8g.2xlarge --region us-east-1 --ami ami-0246d714afcc1d494 \
  --count "$N" --job-array-name "$ARR" --spot --instance-names "$ARR-{index}" \
  --command "curl -fsSL $RAW | bash -s $MODE $N $RUN" \
  --ttl "${TTL}m" --cost-limit 1 --volume-size ${VOLGIB} --on-complete terminate >/tmp/launch_$RUN.log 2>&1
echo "[$RUN] launched $(grep -oE 'Job Array ID: \S+' /tmp/launch_$RUN.log | head -1)"
# poll results
DONE=0; MAX=$((TTL*60/20))
for i in $(seq 1 $MAX); do
  n=$(aws s3 ls "s3://scttfrdmn-lith-bench/lith-bench/fanout/$RUN/" --recursive 2>/dev/null | grep -c '\.flagstat$')
  if [ "$n" -ge 64 ]; then DONE=$(date +%s); echo "[$RUN] 64 flagstats at wall=$((DONE-SUBMIT))s"; break; fi
  sleep 20
done
[ "$DONE" = 0 ] && { echo "[$RUN] TIMEOUT: only $n flagstats"; DONE=$(date +%s); }
WALL=$((DONE-SUBMIT))
# wait for all nodes to terminate
for i in $(seq 1 30); do
  r=$(aws ec2 describe-instances --region us-east-1 --filters "Name=tag:Name,Values=$ARR*" "Name=instance-state-name,Values=running,pending" --query 'length(Reservations[].Instances[])' --output text 2>/dev/null)
  [ "$r" = 0 ] && break; sleep 15
done
# billed node-seconds from EC2
BILLED=$(aws ec2 describe-instances --region us-east-1 --filters "Name=tag:Name,Values=$ARR*" \
  --query 'Reservations[].Instances[].[LaunchTime,StateTransitionReason]' --output text 2>/dev/null | python3 -c "
import sys,re,datetime
tot=0;n=0
for line in sys.stdin:
    p=line.split('\t')
    if len(p)<2: continue
    lt=datetime.datetime.fromisoformat(p[0].strip())
    m=re.search(r'\((\d{4}-\d\d-\d\d \d\d:\d\d:\d\d) GMT\)',p[1])
    if not m: continue
    tt=datetime.datetime.strptime(m.group(1),'%Y-%m-%d %H:%M:%S').replace(tzinfo=datetime.timezone.utc)
    tot+=(tt-lt).total_seconds(); n+=1
print(f'{tot:.0f} {n}')")
# timing aggregate
mkdir -p /tmp/timing/$RUN; aws s3 cp "s3://scttfrdmn-lith-bench/lith-bench/fanout/$RUN/_timing/" /tmp/timing/$RUN/ --recursive --quiet 2>/dev/null
python3 -c "
import json,glob
r=[json.load(open(f)) for f in glob.glob('/tmp/timing/$RUN/*.json')]
bs='$BILLED'.split(); billed=float(bs[0]) if bs else 0; nn=int(bs[1]) if len(bs)>1 else 0
cost=billed*$RATE/3600
cs=sum(x['copy_s'] for x in r); cp=sum(x['compute_s'] for x in r)
print(f'RESULT $RUN N=$N wall=${WALL}s nodes={nn} billed_node_s={billed:.0f} cost=\${cost:.4f} sum_copy_s={cs:.0f} sum_compute_s={cp:.0f} results={len(r)}')
"
