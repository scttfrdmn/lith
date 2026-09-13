#!/usr/bin/env bash
# bench/cluster/node.sh — one compute node in the W3 cluster campaign (#143).
# Mounts per $CONFIG, coordinates via S3 barrier markers, runs samtools flagstat
# on its assigned CRAM per phase, and reports timing. Config comes from an env
# file the driver writes to barrier/<run>/config.env. Env in: RUN_ID, N, BUCKET,
# CONFIG (gateway|indep|efs|fsx), GW_IP, IDXKEY, EFS_DNS, FSX_MNT, PHASES.
set -uo pipefail
export AWS_REGION=us-east-1
export PATH=/usr/local/bin:$PATH
B="s3://$BUCKET"; MP=/mnt/data; sudo mkdir -p "$MP"
m(){ aws s3 ls "$1" >/dev/null 2>&1; }         # marker exists?
put(){ printf '%s' "$2" | aws s3 cp - "$1" >/dev/null 2>&1; }

# node index from the Name tag spawn set (w3-<index>)
TOK=$(curl -sX PUT "http://169.254.169.254/latest/api/token" -H "X-aws-ec2-metadata-token-ttl-seconds:300")
IID=$(curl -s -H "X-aws-ec2-metadata-token:$TOK" http://169.254.169.254/latest/meta-data/instance-id)
NAME=$(aws ec2 describe-tags --region us-east-1 --filters "Name=resource-id,Values=$IID" "Name=key,Values=Name" --query 'Tags[0].Value' --output text)
IDX=${NAME##*-}
CRAMS=(
HG00096.mapped.ILLUMINA.bwa.GBR.low_coverage.20120522.bam.cram
HG00097.mapped.ILLUMINA.bwa.GBR.low_coverage.20130415.bam.cram
HG00099.mapped.ILLUMINA.bwa.GBR.low_coverage.20130415.bam.cram
HG00100.mapped.ILLUMINA.bwa.GBR.low_coverage.20130415.bam.cram
HG00101.mapped.ILLUMINA.bwa.GBR.low_coverage.20130415.bam.cram
HG00102.mapped.ILLUMINA.bwa.GBR.low_coverage.20130415.bam.cram
HG00103.mapped.ILLUMINA.bwa.GBR.low_coverage.20120522.bam.cram
HG00105.mapped.ILLUMINA.bwa.GBR.low_coverage.20130415.bam.cram
)
CRAM=${CRAMS[$IDX]}

REC="vers=3,proto=tcp,port=2049,mountport=2049,nolock,hard,rsize=1048576,wsize=1048576,nconnect=4,actimeo=600"
case "$CONFIG" in
  gateway) sudo mount -t nfs -o "$REC" "$GW_IP:/" "$MP"; CRAMPATH="$MP/$CRAM" ;;
  indep)   aws s3 cp "$B/$IDXKEY" /tmp/w3.idx >/dev/null 2>&1
           /usr/local/bin/lith mount "s3://$BUCKET/lith-bench/w3" "$MP" --index-file /tmp/w3.idx --daemon >/tmp/lith.log 2>&1
           for i in $(seq 1 60); do mountpoint -q "$MP" && break; sleep 0.5; done; CRAMPATH="$MP/$CRAM" ;;
  efs)     sudo mount -t nfs4 -o nfsvers=4.1,rsize=1048576,wsize=1048576,hard,timeo=600 "$EFS_DNS:/" "$MP"; CRAMPATH="$MP/$CRAM" ;;
  fsx)     sudo mount -t lustre -o relatime,flock "$FSX_MNT" "$MP"; CRAMPATH="$MP/$CRAM" ;;
esac
put "$B/barrier/$RUN_ID/ready/$IDX" "$IID $CRAM"
# wait until N nodes are ready
while [ "$(aws s3 ls "$B/barrier/$RUN_ID/ready/" 2>/dev/null | wc -l)" -lt "$N" ]; do sleep 2; done

for ph in $PHASES; do
  while ! m "$B/barrier/$RUN_ID/go/$ph"; do sleep 2; done
  sudo sh -c "echo 3 > /proc/sys/vm/drop_caches" 2>/dev/null
  s=$(date -u +%s.%N)
  samtools flagstat "$CRAMPATH" >"/tmp/fs.$ph.out" 2>"/tmp/fs.$ph.err"; rc=$?
  e=$(date -u +%s.%N)
  put "$B/barrier/$RUN_ID/done/$ph/$IDX" "idx=$IDX cram=$CRAM start=$s end=$e rc=$rc"
done
