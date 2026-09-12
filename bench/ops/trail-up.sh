#!/usr/bin/env bash
# Stand up a CloudTrail trail that records S3 object-level DATA events for every
# bucket (arn:aws:s3:::*) plus read-only MANAGEMENT events (to capture LIST), so
# a neutral ledger of the S3 operations each benchmark tool performs can be read
# from the delivered log files. Single region (us-east-1). Tear down with
# trail-down.sh; the log bucket keeps a 7-day lifecycle. AWS_PROFILE=aws.
#
# Data events (Get/Head object) are NOT in LookupEvents — read the delivered
# log files under s3://$LOGBUCKET/AWSLogs/$ACCT/CloudTrail/$REGION/ instead.
set -euo pipefail
REGION=us-east-1
TRAIL=lith-bench-trail
LOGBUCKET=scttfrdmn-lith-bench-trail
ACCT=$(aws sts get-caller-identity --query Account --output text)
echo "account=$ACCT trail=$TRAIL logbucket=$LOGBUCKET region=$REGION"

# 1. Log bucket (private) + 7-day lifecycle + lith-bench tag.
if ! aws s3api head-bucket --bucket "$LOGBUCKET" 2>/dev/null; then
  aws s3api create-bucket --bucket "$LOGBUCKET" --region "$REGION" >/dev/null
fi
aws s3api put-bucket-tagging --bucket "$LOGBUCKET" \
  --tagging 'TagSet=[{Key=project,Value=lith-bench}]'
aws s3api put-bucket-lifecycle-configuration --bucket "$LOGBUCKET" \
  --lifecycle-configuration '{"Rules":[{"ID":"expire-7d","Status":"Enabled","Filter":{"Prefix":""},"Expiration":{"Days":7}}]}'

# 2. Bucket policy allowing CloudTrail to write log files (bucket-owner-full-control),
#    scoped to this trail's SourceArn.
cat > /tmp/trail-bucket-policy.json <<POLICY
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Sid": "CloudTrailAclCheck",
      "Effect": "Allow",
      "Principal": {"Service": "cloudtrail.amazonaws.com"},
      "Action": "s3:GetBucketAcl",
      "Resource": "arn:aws:s3:::$LOGBUCKET",
      "Condition": {"StringEquals": {"aws:SourceArn": "arn:aws:cloudtrail:$REGION:$ACCT:trail/$TRAIL"}}
    },
    {
      "Sid": "CloudTrailWrite",
      "Effect": "Allow",
      "Principal": {"Service": "cloudtrail.amazonaws.com"},
      "Action": "s3:PutObject",
      "Resource": "arn:aws:s3:::$LOGBUCKET/AWSLogs/$ACCT/*",
      "Condition": {
        "StringEquals": {
          "s3:x-amz-acl": "bucket-owner-full-control",
          "aws:SourceArn": "arn:aws:cloudtrail:$REGION:$ACCT:trail/$TRAIL"
        }
      }
    }
  ]
}
POLICY
aws s3api put-bucket-policy --bucket "$LOGBUCKET" --policy file:///tmp/trail-bucket-policy.json

# 3. Trail (single region), tagged.
if ! aws cloudtrail describe-trails --trail-name-list "$TRAIL" --query 'trailList[0].Name' --output text 2>/dev/null | grep -q "$TRAIL"; then
  aws cloudtrail create-trail --name "$TRAIL" --s3-bucket-name "$LOGBUCKET" \
    --no-is-multi-region-trail --no-include-global-service-events \
    --tags-list Key=project,Value=lith-bench >/dev/null
fi

# 4. Advanced event selectors: S3 object read DATA events for ALL buckets, plus
#    read-only MANAGEMENT events (ListObjectsV2 etc. are management events).
aws cloudtrail put-event-selectors --trail-name "$TRAIL" --advanced-event-selectors '[
  {"Name":"s3-object-read-all-buckets","FieldSelectors":[
    {"Field":"eventCategory","Equals":["Data"]},
    {"Field":"resources.type","Equals":["AWS::S3::Object"]},
    {"Field":"readOnly","Equals":["true"]}
  ]},
  {"Name":"read-management","FieldSelectors":[
    {"Field":"eventCategory","Equals":["Management"]},
    {"Field":"readOnly","Equals":["true"]}
  ]}
]' >/dev/null

# 5. Start logging.
aws cloudtrail start-logging --name "$TRAIL"
echo "trail up; logging=$(aws cloudtrail get-trail-status --name "$TRAIL" --query IsLogging --output text)"
