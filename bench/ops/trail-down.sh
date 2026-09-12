#!/usr/bin/env bash
# Tear down the ops-ledger trail. The log bucket is left in place (7-day
# lifecycle empties it); pass --delete-bucket to remove it now. AWS_PROFILE=aws.
set -uo pipefail
REGION=us-east-1
TRAIL=lith-bench-trail
LOGBUCKET=scttfrdmn-lith-bench-trail
aws cloudtrail stop-logging --name "$TRAIL" 2>/dev/null || true
aws cloudtrail delete-trail --name "$TRAIL" 2>/dev/null || true
echo "trails named $TRAIL remaining: $(aws cloudtrail describe-trails --trail-name-list "$TRAIL" --query 'length(trailList)' --output text 2>/dev/null || echo 0)"
if [ "${1:-}" = "--delete-bucket" ]; then
  aws s3 rm "s3://$LOGBUCKET" --recursive >/dev/null 2>&1 || true
  aws s3api delete-bucket --bucket "$LOGBUCKET" --region "$REGION" 2>/dev/null || true
  echo "log bucket deleted"
else
  echo "log bucket $LOGBUCKET left (7-day lifecycle)"
fi
