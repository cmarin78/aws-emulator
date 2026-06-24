#!/usr/bin/env bash
# Smoke-test of the emulator using the real AWS CLI.
#
# Unlike azure-emulator (which needs `az rest` because az/Terraform expect
# real ARM metadata discovery and AAD auth that the emulator doesn't
# implement), the AWS CLI works against this emulator out of the box: point
# it at --endpoint-url and any credentials satisfy it, since SigV4 is not
# validated. This is the same usage shown in README.md, exercised here
# end-to-end across all 5 implemented services plus every Phase 2 addition
# (see ROADMAP.md): queue attributes/batch ops, GSI/LSI Query, versioning,
# tagging, and multipart upload.
#
# Usage:
#   go run ./cmd/aws-emulator -addr :4566 -db .aws-emulator-data/state.db &
#   ./scripts/test-aws-cli.sh [http://localhost:4566]
set -euo pipefail

ENDPOINT="${1:-http://localhost:4566}"
export AWS_ACCESS_KEY_ID="${AWS_ACCESS_KEY_ID:-test}"
export AWS_SECRET_ACCESS_KEY="${AWS_SECRET_ACCESS_KEY:-test}"
export AWS_DEFAULT_REGION="${AWS_DEFAULT_REGION:-us-east-1}"

aws() {
  command aws --endpoint-url "${ENDPOINT}" --output json "$@"
}

echo "== Testing against ${ENDPOINT} =="

echo "-- health --"
curl -sf "${ENDPOINT}/_aws-emulator/health"; echo

echo "-- reset (start from a clean slate) --"
curl -sf -X POST "${ENDPOINT}/_aws-emulator/reset"; echo

###############################################################################
# S3
###############################################################################
BUCKET="tf-smoke-bucket"

echo "-- s3 mb --"
aws s3 mb "s3://${BUCKET}"

echo "-- s3 ls (buckets) --"
aws s3 ls

echo "-- s3api put-object --"
echo "hello from aws cli" > /tmp/aws-smoke-hello.txt
aws s3api put-object --bucket "${BUCKET}" --key hello.txt --body /tmp/aws-smoke-hello.txt

echo "-- s3api get-object --"
aws s3api get-object --bucket "${BUCKET}" --key hello.txt /tmp/aws-smoke-hello.out
cat /tmp/aws-smoke-hello.out; echo

echo "-- s3api head-object --"
aws s3api head-object --bucket "${BUCKET}" --key hello.txt

echo "-- s3api list-objects-v2 --"
aws s3api list-objects-v2 --bucket "${BUCKET}"

echo "-- s3api put-bucket-versioning --"
aws s3api put-bucket-versioning --bucket "${BUCKET}" --versioning-configuration Status=Enabled

echo "-- s3api get-bucket-versioning --"
aws s3api get-bucket-versioning --bucket "${BUCKET}"

echo "-- s3api put-object (new version) --"
echo "hello v2 from aws cli" > /tmp/aws-smoke-hello-v2.txt
VERSION_ID=$(aws s3api put-object --bucket "${BUCKET}" --key hello.txt --body /tmp/aws-smoke-hello-v2.txt --query 'VersionId' --output text)
echo "VersionId: ${VERSION_ID}"

echo "-- s3api get-object (specific version) --"
aws s3api get-object --bucket "${BUCKET}" --key hello.txt --version-id "${VERSION_ID}" /tmp/aws-smoke-hello-v2.out
cat /tmp/aws-smoke-hello-v2.out; echo

echo "-- s3api put-object-tagging --"
aws s3api put-object-tagging --bucket "${BUCKET}" --key hello.txt \
  --tagging 'TagSet=[{Key=env,Value=smoke-test}]'

echo "-- s3api get-object-tagging --"
aws s3api get-object-tagging --bucket "${BUCKET}" --key hello.txt

echo "-- s3api delete-object-tagging --"
aws s3api delete-object-tagging --bucket "${BUCKET}" --key hello.txt

echo "-- s3api create-multipart-upload --"
UPLOAD_ID=$(aws s3api create-multipart-upload --bucket "${BUCKET}" --key multipart.txt --query 'UploadId' --output text)
echo "UploadId: ${UPLOAD_ID}"

echo "-- s3api upload-part (x2) --"
head -c 5242880 /dev/zero | tr '\0' 'A' > /tmp/aws-smoke-part1
head -c 1048576 /dev/zero | tr '\0' 'B' > /tmp/aws-smoke-part2
ETAG1=$(aws s3api upload-part --bucket "${BUCKET}" --key multipart.txt --upload-id "${UPLOAD_ID}" \
  --part-number 1 --body /tmp/aws-smoke-part1 --query 'ETag' --output text)
ETAG2=$(aws s3api upload-part --bucket "${BUCKET}" --key multipart.txt --upload-id "${UPLOAD_ID}" \
  --part-number 2 --body /tmp/aws-smoke-part2 --query 'ETag' --output text)
echo "ETag1: ${ETAG1} ETag2: ${ETAG2}"

echo "-- s3api complete-multipart-upload --"
aws s3api complete-multipart-upload --bucket "${BUCKET}" --key multipart.txt --upload-id "${UPLOAD_ID}" \
  --multipart-upload "{\"Parts\":[{\"ETag\":${ETAG1},\"PartNumber\":1},{\"ETag\":${ETAG2},\"PartNumber\":2}]}"

echo "-- s3api head-object (assembled multipart object, expect 6MB) --"
aws s3api head-object --bucket "${BUCKET}" --key multipart.txt

echo "-- s3api delete-object (cleanup) --"
aws s3api delete-object --bucket "${BUCKET}" --key hello.txt
aws s3api delete-object --bucket "${BUCKET}" --key multipart.txt

echo "-- s3 rb (cleanup) --"
aws s3 rb "s3://${BUCKET}"

###############################################################################
# SQS
###############################################################################
QUEUE_NAME="tf-smoke-queue"

echo "-- sqs create-queue --"
QUEUE_URL=$(aws sqs create-queue --queue-name "${QUEUE_NAME}" --query 'QueueUrl' --output text)
echo "QueueUrl: ${QUEUE_URL}"

echo "-- sqs get-queue-url --"
aws sqs get-queue-url --queue-name "${QUEUE_NAME}"

echo "-- sqs list-queues --"
aws sqs list-queues

echo "-- sqs set-queue-attributes --"
aws sqs set-queue-attributes --queue-url "${QUEUE_URL}" --attributes VisibilityTimeout=45

echo "-- sqs get-queue-attributes --"
aws sqs get-queue-attributes --queue-url "${QUEUE_URL}" --attribute-names All

echo "-- sqs send-message --"
aws sqs send-message --queue-url "${QUEUE_URL}" --message-body "hello from aws cli"

echo "-- sqs send-message-batch --"
aws sqs send-message-batch --queue-url "${QUEUE_URL}" --entries \
  'Id=msg1,MessageBody=batch-message-1' 'Id=msg2,MessageBody=batch-message-2'

echo "-- sqs receive-message --"
RECEIVE=$(aws sqs receive-message --queue-url "${QUEUE_URL}" --max-number-of-messages 10)
echo "${RECEIVE}"
RECEIPT_HANDLE=$(echo "${RECEIVE}" | python3 -c 'import json,sys; m=json.load(sys.stdin).get("Messages",[]); print(m[0]["ReceiptHandle"] if m else "")')

if [ -n "${RECEIPT_HANDLE}" ]; then
  echo "-- sqs delete-message --"
  aws sqs delete-message --queue-url "${QUEUE_URL}" --receipt-handle "${RECEIPT_HANDLE}"
fi

echo "-- sqs purge-queue --"
aws sqs purge-queue --queue-url "${QUEUE_URL}"

echo "-- sqs delete-queue (cleanup) --"
aws sqs delete-queue --queue-url "${QUEUE_URL}"

###############################################################################
# IAM
###############################################################################
ROLE_NAME="tf-smoke-role"
USER_NAME="tf-smoke-user"
TRUST_POLICY='{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"Service":"ec2.amazonaws.com"},"Action":"sts:AssumeRole"}]}'
INLINE_POLICY='{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"s3:GetObject","Resource":"*"}]}'

echo "-- iam create-role --"
aws iam create-role --role-name "${ROLE_NAME}" --assume-role-policy-document "${TRUST_POLICY}"

echo "-- iam get-role --"
aws iam get-role --role-name "${ROLE_NAME}"

echo "-- iam list-roles --"
aws iam list-roles

echo "-- iam put-role-policy --"
aws iam put-role-policy --role-name "${ROLE_NAME}" --policy-name "tf-smoke-policy" --policy-document "${INLINE_POLICY}"

echo "-- iam get-role-policy --"
aws iam get-role-policy --role-name "${ROLE_NAME}" --policy-name "tf-smoke-policy"

echo "-- iam delete-role-policy --"
aws iam delete-role-policy --role-name "${ROLE_NAME}" --policy-name "tf-smoke-policy"

echo "-- iam create-user --"
aws iam create-user --user-name "${USER_NAME}"

echo "-- iam get-user --"
aws iam get-user --user-name "${USER_NAME}"

echo "-- iam list-users --"
aws iam list-users

echo "-- iam create-access-key --"
ACCESS_KEY_ID=$(aws iam create-access-key --user-name "${USER_NAME}" --query 'AccessKey.AccessKeyId' --output text)
echo "AccessKeyId: ${ACCESS_KEY_ID}"

echo "-- iam list-access-keys --"
aws iam list-access-keys --user-name "${USER_NAME}"

echo "-- iam delete-access-key (cleanup) --"
aws iam delete-access-key --user-name "${USER_NAME}" --access-key-id "${ACCESS_KEY_ID}"

echo "-- iam delete-user (cleanup) --"
aws iam delete-user --user-name "${USER_NAME}"

echo "-- iam delete-role (cleanup) --"
aws iam delete-role --role-name "${ROLE_NAME}"

###############################################################################
# STS
###############################################################################
echo "-- sts get-caller-identity --"
aws sts get-caller-identity

###############################################################################
# DynamoDB
###############################################################################
TABLE_NAME="tf-smoke-table"

echo "-- dynamodb create-table (with GSI) --"
aws dynamodb create-table \
  --table-name "${TABLE_NAME}" \
  --attribute-definitions \
    AttributeName=pk,AttributeType=S \
    AttributeName=sk,AttributeType=S \
    AttributeName=gsiPk,AttributeType=S \
  --key-schema AttributeName=pk,KeyType=HASH AttributeName=sk,KeyType=RANGE \
  --global-secondary-indexes \
    'IndexName=gsi1,KeySchema=[{AttributeName=gsiPk,KeyType=HASH}],Projection={ProjectionType=ALL}' \
  --billing-mode PAY_PER_REQUEST

echo "-- dynamodb describe-table --"
aws dynamodb describe-table --table-name "${TABLE_NAME}"

echo "-- dynamodb put-item (x2, same pk, different sk) --"
aws dynamodb put-item --table-name "${TABLE_NAME}" --item \
  '{"pk":{"S":"user#1"},"sk":{"S":"order#1"},"gsiPk":{"S":"status#open"},"amount":{"N":"10"}}'
aws dynamodb put-item --table-name "${TABLE_NAME}" --item \
  '{"pk":{"S":"user#1"},"sk":{"S":"order#2"},"gsiPk":{"S":"status#closed"},"amount":{"N":"20"}}'

echo "-- dynamodb get-item --"
aws dynamodb get-item --table-name "${TABLE_NAME}" --key '{"pk":{"S":"user#1"},"sk":{"S":"order#1"}}'

echo "-- dynamodb query (partition key only) --"
aws dynamodb query --table-name "${TABLE_NAME}" \
  --key-condition-expression "pk = :pk" \
  --expression-attribute-values '{":pk":{"S":"user#1"}}'

echo "-- dynamodb query (partition key + sort key condition) --"
aws dynamodb query --table-name "${TABLE_NAME}" \
  --key-condition-expression "pk = :pk AND sk = :sk" \
  --expression-attribute-values '{":pk":{"S":"user#1"},":sk":{"S":"order#1"}}'

echo "-- dynamodb query (GSI) --"
aws dynamodb query --table-name "${TABLE_NAME}" --index-name gsi1 \
  --key-condition-expression "gsiPk = :g" \
  --expression-attribute-values '{":g":{"S":"status#open"}}'

echo "-- dynamodb scan --"
aws dynamodb scan --table-name "${TABLE_NAME}"

echo "-- dynamodb delete-item (cleanup) --"
aws dynamodb delete-item --table-name "${TABLE_NAME}" --key '{"pk":{"S":"user#1"},"sk":{"S":"order#1"}}'
aws dynamodb delete-item --table-name "${TABLE_NAME}" --key '{"pk":{"S":"user#1"},"sk":{"S":"order#2"}}'

echo "-- dynamodb delete-table (cleanup) --"
aws dynamodb delete-table --table-name "${TABLE_NAME}"

rm -f /tmp/aws-smoke-hello.txt /tmp/aws-smoke-hello.out /tmp/aws-smoke-hello-v2.txt /tmp/aws-smoke-hello-v2.out /tmp/aws-smoke-part1 /tmp/aws-smoke-part2

echo "== All smoke tests passed =="
