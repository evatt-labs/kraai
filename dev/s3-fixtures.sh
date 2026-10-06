#!/usr/bin/env bash
set -euo pipefail
# Fixtures for AWS::S3::Bucket read parity: buckets carrying every
# configuration the direct read maps, so each property and each absence
# code the override lists is exercised against Cloud Control. All in
# us-east-1, all named kraai-s3fx-<account>-*, near-free while they stand
# empty; down deletes everything, object versions included.
#
#   dev/s3-fixtures.sh up   [--apply]
#   dev/s3-fixtures.sh down [--apply]
#
# Dry run unless --apply. The "bare" bucket has its public access block and
# ownership controls deleted, the only way to observe the codes S3 answers
# for their absence; it stays private, empty and without a policy.

usage() { echo "usage: $0 up|down [--apply]" >&2; exit 2; }
[ $# -ge 1 ] || usage
mode="$1"
apply=false
[ "${2:-}" = "--apply" ] && apply=true
region=us-east-1
acct=$(aws sts get-caller-identity --query Account --output text)
prefix="kraai-s3fx-${acct}"
main="${prefix}-main"
dest="${prefix}-dest"
locked="${prefix}-locked"
bare="${prefix}-bare"
topic_name="${prefix}-events"
role_name="kraai-s3fx-replication"

run() {
  if $apply; then "$@"; else echo "would run: $*" >&2; fi
}

up() {
  if aws s3api head-bucket --bucket "$main" 2>/dev/null; then
    echo "fixtures already exist; run down first" >&2
    exit 1
  fi
  run aws s3api create-bucket --bucket "$main" --region "$region"
  run aws s3api create-bucket --bucket "$dest" --region "$region"
  run aws s3api create-bucket --bucket "$locked" --region "$region" --object-lock-enabled-for-bucket
  run aws s3api create-bucket --bucket "$bare" --region "$region"
  run aws s3api delete-public-access-block --bucket "$bare"
  run aws s3api delete-bucket-ownership-controls --bucket "$bare"

  for b in "$main" "$dest"; do
    run aws s3api put-bucket-versioning --bucket "$b" --versioning-configuration Status=Enabled
  done
  run aws s3api put-object-lock-configuration --bucket "$locked" \
    --object-lock-configuration '{"ObjectLockEnabled":"Enabled","Rule":{"DefaultRetention":{"Mode":"GOVERNANCE","Days":1}}}'

  # The destination accepts access logs and inventory reports from main.
  dest_policy=$(jq -cn --arg d "$dest" --arg m "$main" --arg a "$acct" '{Version:"2012-10-17",Statement:[
    {Sid:"logs",Effect:"Allow",Principal:{Service:"logging.s3.amazonaws.com"},Action:"s3:PutObject",Resource:("arn:aws:s3:::"+$d+"/*"),Condition:{ArnLike:{"aws:SourceArn":("arn:aws:s3:::"+$m)},StringEquals:{"aws:SourceAccount":$a}}},
    {Sid:"inventory",Effect:"Allow",Principal:{Service:"s3.amazonaws.com"},Action:"s3:PutObject",Resource:("arn:aws:s3:::"+$d+"/*"),Condition:{ArnLike:{"aws:SourceArn":("arn:aws:s3:::"+$m)},StringEquals:{"aws:SourceAccount":$a,"s3:x-amz-acl":"bucket-owner-full-control"}}},
    {Sid:"tls",Effect:"Deny",Principal:"*",Action:"s3:*",Resource:[("arn:aws:s3:::"+$d),("arn:aws:s3:::"+$d+"/*")],Condition:{Bool:{"aws:SecureTransport":"false"}}}]}')
  run aws s3api put-bucket-policy --bucket "$dest" --policy "$dest_policy"

  # A topic S3 may publish main's events to.
  topic_arn="arn:aws:sns:${region}:${acct}:${topic_name}"
  run aws sns create-topic --region "$region" --name "$topic_name"
  topic_policy=$(jq -cn --arg t "$topic_arn" --arg m "$main" --arg a "$acct" '{Version:"2012-10-17",Statement:[{Effect:"Allow",Principal:{Service:"s3.amazonaws.com"},Action:"sns:Publish",Resource:$t,Condition:{ArnLike:{"aws:SourceArn":("arn:aws:s3:::"+$m)},StringEquals:{"aws:SourceAccount":$a}}}]}')
  run aws sns set-topic-attributes --region "$region" --topic-arn "$topic_arn" --attribute-name Policy --attribute-value "$topic_policy"

  # A replication role S3 may assume, with no permissions: replication is
  # configured, never performed.
  trust=$(jq -cn --arg a "$acct" '{Version:"2012-10-17",Statement:[{Effect:"Allow",Principal:{Service:"s3.amazonaws.com"},Action:"sts:AssumeRole",Condition:{StringEquals:{"aws:SourceAccount":$a}}}]}')
  run aws iam create-role --role-name "$role_name" --assume-role-policy-document "$trust"
  role_arn="arn:aws:iam::${acct}:role/${role_name}"

  run aws s3api put-bucket-tagging --bucket "$main" --tagging '{"TagSet":[{"Key":"team","Value":"kraai"},{"Key":"purpose","Value":"parity"}]}'
  run aws s3api put-bucket-cors --bucket "$main" --cors-configuration '{"CORSRules":[{"ID":"web","AllowedMethods":["GET"],"AllowedOrigins":["https://example.com"],"AllowedHeaders":["*"],"ExposeHeaders":["ETag"],"MaxAgeSeconds":3000}]}'
  run aws s3api put-bucket-website --bucket "$main" --website-configuration '{"IndexDocument":{"Suffix":"index.html"},"ErrorDocument":{"Key":"error.html"}}'
  run aws s3api put-bucket-accelerate-configuration --bucket "$main" --accelerate-configuration Status=Enabled
  run aws s3api put-bucket-logging --bucket "$main" --bucket-logging-status "$(jq -cn --arg d "$dest" '{LoggingEnabled:{TargetBucket:$d,TargetPrefix:"logs/",TargetObjectKeyFormat:{PartitionedPrefix:{PartitionDateSource:"EventTime"}}}}')"
  run aws s3api put-bucket-lifecycle-configuration --bucket "$main" --lifecycle-configuration '{"Rules":[
    {"ID":"r1","Status":"Enabled","Filter":{"And":{"Prefix":"tmp/","Tags":[{"Key":"d","Value":"4"}],"ObjectSizeGreaterThan":1024}},"Expiration":{"Days":30}},
    {"ID":"r2","Status":"Enabled","Filter":{"Prefix":"old/"},"Transitions":[{"Days":40,"StorageClass":"STANDARD_IA"}],"NoncurrentVersionExpiration":{"NoncurrentDays":7},"AbortIncompleteMultipartUpload":{"DaysAfterInitiation":3}},
    {"ID":"r3","Status":"Enabled","Filter":{"Tag":{"Key":"e","Value":"5"}},"NoncurrentVersionTransitions":[{"NoncurrentDays":30,"StorageClass":"GLACIER","NewerNoncurrentVersions":2}],"Expiration":{"Days":365}}]}'
  run aws s3api put-bucket-analytics-configuration --bucket "$main" --id an1 --analytics-configuration '{"Id":"an1","Filter":{"And":{"Prefix":"logs/","Tags":[{"Key":"a","Value":"1"},{"Key":"b","Value":"2"}]}},"StorageClassAnalysis":{}}'
  run aws s3api put-bucket-metrics-configuration --bucket "$main" --id m1 --metrics-configuration '{"Id":"m1","Filter":{"And":{"Prefix":"data/","Tags":[{"Key":"c","Value":"3"}]}}}'
  run aws s3api put-bucket-metrics-configuration --bucket "$main" --id m2 --metrics-configuration '{"Id":"m2","Filter":{"Tag":{"Key":"a","Value":"1"}}}'
  run aws s3api put-bucket-intelligent-tiering-configuration --bucket "$main" --id it1 --intelligent-tiering-configuration '{"Id":"it1","Status":"Enabled","Filter":{"Prefix":"cold/"},"Tierings":[{"Days":90,"AccessTier":"ARCHIVE_ACCESS"}]}'
  run aws s3api put-bucket-inventory-configuration --bucket "$main" --id inv1 --inventory-configuration "$(jq -cn --arg d "$dest" --arg a "$acct" '{Id:"inv1",IsEnabled:true,Destination:{S3BucketDestination:{AccountId:$a,Bucket:("arn:aws:s3:::"+$d),Format:"CSV",Prefix:"inventory"}},Filter:{Prefix:"data/"},IncludedObjectVersions:"Current",OptionalFields:["Size","StorageClass"],Schedule:{Frequency:"Weekly"}}')"
  # Single-event configurations: Cloud Control keeps only the first event
  # of one with several, which the direct read spreads.
  run aws s3api put-bucket-notification-configuration --bucket "$main" --notification-configuration "$(jq -cn --arg t "$topic_arn" '{TopicConfigurations:[
    {Id:"t1",TopicArn:$t,Events:["s3:ObjectCreated:*"],Filter:{Key:{FilterRules:[{Name:"prefix",Value:"in/"},{Name:"suffix",Value:".csv"}]}}},
    {Id:"t2",TopicArn:$t,Events:["s3:ObjectRemoved:*"]}],EventBridgeConfiguration:{}}')"
  run aws s3api put-bucket-replication --bucket "$main" --replication-configuration "$(jq -cn --arg r "$role_arn" --arg d "$dest" '{Role:$r,Rules:[{ID:"rep1",Priority:1,Status:"Enabled",Filter:{Prefix:"rep/"},DeleteMarkerReplication:{Status:"Disabled"},Destination:{Bucket:("arn:aws:s3:::"+$d),StorageClass:"STANDARD_IA"}}]}')"
}

empty() {
  local b="$1" batch
  aws s3api head-bucket --bucket "$b" 2>/dev/null || return 0
  while :; do
    batch=$(aws s3api list-object-versions --bucket "$b" --max-items 500 --output json |
      jq -c '{Objects: ([.Versions[]?, .DeleteMarkers[]?] | map({Key, VersionId})), Quiet: true}')
    [ "$(jq '.Objects | length' <<<"$batch")" -gt 0 ] || break
    run aws s3api delete-objects --bucket "$b" --bypass-governance-retention --delete "$batch" >/dev/null
    $apply || break
  done
}

down() {
  for b in "$main" "$dest" "$locked" "$bare"; do
    empty "$b"
    if aws s3api head-bucket --bucket "$b" 2>/dev/null; then
      run aws s3api delete-bucket --bucket "$b" --region "$region"
    fi
  done
  if aws sns get-topic-attributes --region "$region" --topic-arn "arn:aws:sns:${region}:${acct}:${topic_name}" >/dev/null 2>&1; then
    run aws sns delete-topic --region "$region" --topic-arn "arn:aws:sns:${region}:${acct}:${topic_name}"
  fi
  if aws iam get-role --role-name "$role_name" >/dev/null 2>&1; then
    run aws iam delete-role --role-name "$role_name"
  fi
}

case "$mode" in
up) up ;;
down) down ;;
*) usage ;;
esac
