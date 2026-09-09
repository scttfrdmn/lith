#!/usr/bin/env bash
# Throwaway probe: reveal how spore exposes the array index, and test S3 write.
BKT=scttfrdmn-lith-bench
TOK=$(curl -sX PUT http://169.254.169.254/latest/api/token -H 'X-aws-ec2-metadata-token-ttl-seconds: 120')
md() { curl -s -H "X-aws-ec2-metadata-token: $TOK" "http://169.254.169.254/latest/meta-data/$1" 2>/dev/null; }
echo "===ENV(index/array/spore/spawn)==="
env | sort | grep -iE 'index|array|spore|spawn|job' || echo "(none matched)"
echo "===INSTANCE==="
echo "instance-id=$(md instance-id) type=$(md instance-type) hostname=$(hostname)"
echo "===TAGS==="
md tags/instance/ || echo "(instance tags not in metadata)"
echo ""
md tags/instance/Name && echo " <- Name tag" || echo "(no Name tag in metadata)"
echo "===AWSCLI==="
command -v aws || echo "NO AWSCLI"
echo "===S3WRITE==="
if command -v aws >/dev/null 2>&1; then
  echo "probe $(md instance-id)" | aws s3 cp - "s3://$BKT/lith-bench/probe/$(md instance-id).txt" 2>&1 && echo S3WRITE_OK || echo S3WRITE_FAIL
else echo "skip (no awscli)"; fi
echo "===PROBE_DONE==="
