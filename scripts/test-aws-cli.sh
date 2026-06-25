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

###############################################################################
# SNS + SQS (cross-service delivery, Phase 3 — see ROADMAP.md)
###############################################################################
SNS_QUEUE_NAME="tf-smoke-sns-queue"
TOPIC_NAME="tf-smoke-topic"

echo "-- sqs create-queue (sns target) --"
SNS_QUEUE_URL=$(aws sqs create-queue --queue-name "${SNS_QUEUE_NAME}" --query 'QueueUrl' --output text)
echo "QueueUrl: ${SNS_QUEUE_URL}"

echo "-- sqs get-queue-attributes (QueueArn) --"
SNS_QUEUE_ARN=$(aws sqs get-queue-attributes --queue-url "${SNS_QUEUE_URL}" --attribute-names QueueArn --query 'Attributes.QueueArn' --output text)
echo "QueueArn: ${SNS_QUEUE_ARN}"

echo "-- sns create-topic --"
TOPIC_ARN=$(aws sns create-topic --name "${TOPIC_NAME}" --query 'TopicArn' --output text)
echo "TopicArn: ${TOPIC_ARN}"

echo "-- sns list-topics --"
aws sns list-topics

echo "-- sns subscribe (protocol=sqs) --"
SUBSCRIPTION_ARN=$(aws sns subscribe --topic-arn "${TOPIC_ARN}" --protocol sqs --notification-endpoint "${SNS_QUEUE_ARN}" --query 'SubscriptionArn' --output text)
echo "SubscriptionArn: ${SUBSCRIPTION_ARN}"

echo "-- sns list-subscriptions-by-topic --"
aws sns list-subscriptions-by-topic --topic-arn "${TOPIC_ARN}"

echo "-- sns publish --"
aws sns publish --topic-arn "${TOPIC_ARN}" --message "hello via sns"

echo "-- sqs receive-message (expect the SNS message delivered) --"
SNS_RECEIVE=$(aws sqs receive-message --queue-url "${SNS_QUEUE_URL}" --max-number-of-messages 10)
echo "${SNS_RECEIVE}"

echo "-- sns unsubscribe (cleanup) --"
aws sns unsubscribe --subscription-arn "${SUBSCRIPTION_ARN}"

echo "-- sns delete-topic (cleanup) --"
aws sns delete-topic --topic-arn "${TOPIC_ARN}"

echo "-- sqs delete-queue (cleanup) --"
aws sqs delete-queue --queue-url "${SNS_QUEUE_URL}"

###############################################################################
# EventBridge + SQS (rule pattern matching + target delivery, Phase 3)
###############################################################################
EVT_QUEUE_NAME="tf-smoke-events-queue"
RULE_NAME="tf-smoke-rule"

echo "-- sqs create-queue (eventbridge target) --"
EVT_QUEUE_URL=$(aws sqs create-queue --queue-name "${EVT_QUEUE_NAME}" --query 'QueueUrl' --output text)
echo "QueueUrl: ${EVT_QUEUE_URL}"

echo "-- sqs get-queue-attributes (QueueArn) --"
EVT_QUEUE_ARN=$(aws sqs get-queue-attributes --queue-url "${EVT_QUEUE_URL}" --attribute-names QueueArn --query 'Attributes.QueueArn' --output text)
echo "QueueArn: ${EVT_QUEUE_ARN}"

echo "-- events put-rule (with EventPattern) --"
aws events put-rule --name "${RULE_NAME}" --event-pattern '{"source":["tf.smoke"]}'

echo "-- events list-rules --"
aws events list-rules

echo "-- events put-targets --"
aws events put-targets --rule "${RULE_NAME}" --targets "Id=1,Arn=${EVT_QUEUE_ARN}"

echo "-- events list-targets-by-rule --"
aws events list-targets-by-rule --rule "${RULE_NAME}"

echo "-- events put-events (matching pattern, expect delivery) --"
aws events put-events --entries "[{\"Source\":\"tf.smoke\",\"DetailType\":\"smoke-test\",\"Detail\":\"{\\\"k\\\":\\\"v\\\"}\"}]"

echo "-- events put-events (non-matching source, expect no delivery) --"
aws events put-events --entries '[{"Source":"tf.other","DetailType":"smoke-test","Detail":"{}"}]'

echo "-- sqs receive-message (expect exactly one EventBridge envelope) --"
EVT_RECEIVE=$(aws sqs receive-message --queue-url "${EVT_QUEUE_URL}" --max-number-of-messages 10)
echo "${EVT_RECEIVE}"

echo "-- events remove-targets (cleanup) --"
aws events remove-targets --rule "${RULE_NAME}" --ids 1

echo "-- events delete-rule (cleanup) --"
aws events delete-rule --name "${RULE_NAME}"

echo "-- sqs delete-queue (cleanup) --"
aws sqs delete-queue --queue-url "${EVT_QUEUE_URL}"

###############################################################################
# Lambda (local invocation only — see ROADMAP.md)
###############################################################################
FUNCTION_NAME="tf-smoke-fn"

echo "-- lambda create-function (in-process stub, no EMULATOR_INVOKE_COMMAND) --"
# El AWS CLI valida que --zip-file sea un .zip real (firma PK) antes de
# mandar el request, así que un archivo de texto plano no sirve -- a
# diferencia del resto de este script, este no es un detalle de quoting de
# PowerShell sino una validación real del lado del cliente.
python3 -c "
import zipfile
with zipfile.ZipFile('/tmp/aws-smoke-lambda.zip', 'w') as zf:
    zf.writestr('handler.py', 'def main(event, context):\n    return event\n')
"
aws lambda create-function \
  --function-name "${FUNCTION_NAME}" \
  --runtime provided \
  --role arn:aws:iam::000000000000:role/lambda-role \
  --handler handler.main \
  --zip-file fileb:///tmp/aws-smoke-lambda.zip

echo "-- lambda get-function --"
aws lambda get-function --function-name "${FUNCTION_NAME}"

echo "-- lambda list-functions --"
aws lambda list-functions

echo "-- lambda invoke (in-process stub: echoes the payload back) --"
echo -n '{"hello":"world"}' > /tmp/aws-smoke-lambda-payload.json
aws lambda invoke --function-name "${FUNCTION_NAME}" --payload "fileb:///tmp/aws-smoke-lambda-payload.json" /tmp/aws-smoke-lambda-out.json
cat /tmp/aws-smoke-lambda-out.json; echo

echo "-- lambda delete-function (cleanup) --"
aws lambda delete-function --function-name "${FUNCTION_NAME}"

rm -f /tmp/aws-smoke-lambda.zip /tmp/aws-smoke-lambda-payload.json /tmp/aws-smoke-lambda-out.json

###############################################################################
# CloudWatch Logs (Phase 4 — see ROADMAP.md)
###############################################################################
LOG_GROUP="/tf-smoke/group"
LOG_STREAM="tf-smoke-stream"

echo "-- logs create-log-group --"
aws logs create-log-group --log-group-name "${LOG_GROUP}"

echo "-- logs describe-log-groups --"
aws logs describe-log-groups --log-group-name-prefix "${LOG_GROUP}"

echo "-- logs create-log-stream --"
aws logs create-log-stream --log-group-name "${LOG_GROUP}" --log-stream-name "${LOG_STREAM}"

echo "-- logs describe-log-streams --"
aws logs describe-log-streams --log-group-name "${LOG_GROUP}"

echo "-- logs put-log-events --"
NOW_MS=$(($(date +%s) * 1000))
aws logs put-log-events --log-group-name "${LOG_GROUP}" --log-stream-name "${LOG_STREAM}" \
  --log-events "[{\"timestamp\":${NOW_MS},\"message\":\"hello from aws cli\"},{\"timestamp\":${NOW_MS},\"message\":\"second smoke event\"}]"

echo "-- logs filter-log-events --"
aws logs filter-log-events --log-group-name "${LOG_GROUP}" --filter-pattern "smoke"

echo "-- logs delete-log-stream (cleanup) --"
aws logs delete-log-stream --log-group-name "${LOG_GROUP}" --log-stream-name "${LOG_STREAM}"

echo "-- logs delete-log-group (cleanup) --"
aws logs delete-log-group --log-group-name "${LOG_GROUP}"

###############################################################################
# Secrets Manager (Phase 4)
###############################################################################
SECRET_NAME="tf-smoke-secret"

echo "-- secretsmanager create-secret --"
aws secretsmanager create-secret --name "${SECRET_NAME}" --secret-string "hello-secret"

echo "-- secretsmanager get-secret-value --"
aws secretsmanager get-secret-value --secret-id "${SECRET_NAME}"

echo "-- secretsmanager put-secret-value (new version) --"
aws secretsmanager put-secret-value --secret-id "${SECRET_NAME}" --secret-string "rotated-secret"

echo "-- secretsmanager get-secret-value (expect rotated value) --"
aws secretsmanager get-secret-value --secret-id "${SECRET_NAME}"

echo "-- secretsmanager list-secrets --"
aws secretsmanager list-secrets

echo "-- secretsmanager delete-secret (cleanup) --"
aws secretsmanager delete-secret --secret-id "${SECRET_NAME}" --force-delete-without-recovery

###############################################################################
# SSM Parameter Store (Phase 4)
###############################################################################
PARAM_NAME="/tf-smoke/param"
SECURE_PARAM_NAME="/tf-smoke/secure-param"

echo "-- ssm put-parameter (String) --"
aws ssm put-parameter --name "${PARAM_NAME}" --value "hello-param" --type String

echo "-- ssm get-parameter --"
aws ssm get-parameter --name "${PARAM_NAME}"

echo "-- ssm put-parameter (SecureString) --"
aws ssm put-parameter --name "${SECURE_PARAM_NAME}" --value "hello-secure" --type SecureString

echo "-- ssm get-parameter (with-decryption) --"
aws ssm get-parameter --name "${SECURE_PARAM_NAME}" --with-decryption

echo "-- ssm get-parameters (batch) --"
aws ssm get-parameters --names "${PARAM_NAME}" "${SECURE_PARAM_NAME}"

echo "-- ssm put-parameter (overwrite) --"
aws ssm put-parameter --name "${PARAM_NAME}" --value "hello-param-v2" --type String --overwrite

echo "-- ssm delete-parameter (cleanup) --"
aws ssm delete-parameter --name "${PARAM_NAME}"
aws ssm delete-parameter --name "${SECURE_PARAM_NAME}"

echo "== All smoke tests passed =="
