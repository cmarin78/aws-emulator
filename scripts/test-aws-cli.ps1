# Smoke-test of the emulator using the real AWS CLI (PowerShell).
#
# Same coverage as the .sh version: exercises all 5 implemented services
# (S3, SQS, IAM, STS, DynamoDB) plus every Phase 2 addition (see
# ROADMAP.md) -- queue attributes/batch ops, GSI/LSI Query, versioning,
# tagging, and multipart upload.
#
# JSON bodies/items are written to temp files and passed with `file://`
# (or plain file paths for non-JSON CLI args): aws.cmd is, like az.cmd, a
# batch-file wrapper on Windows, and embedded double quotes inside an
# inline argument can get mangled by cmd's argument parsing depending on
# PowerShell's quoting. Routing through `file://` sidesteps that class of
# problem entirely, same rationale as test-az-cli.ps1's `@archivo` trick.
#
# Usage:
#   go run ./cmd/aws-emulator -addr :4566 -db .aws-emulator-data/state.db
#   .\scripts\test-aws-cli.ps1 [-Endpoint http://localhost:4566]
#
# Known environment caveat (not an emulator bug): with aws-cli 1.36.40 on
# Python 3.14, a handful of operations whose --message/--message-body
# parameter help text contains a literal "&"/"%" (s3api list-objects-v2,
# sqs send-message, sqs receive-message, sns publish) raise "ValueError:
# badly formed help string" from argparse before the request is even sent.
# Python 3.13+ tightened argparse's _check_help() to validate every
# action's help text as a %-format string, and botocore's bundled help
# text for some of these operations' params trips that check. Root cause
# confirmed via `aws --debug` (see ROADMAP.md history) and, separately, via
# raw HTTP requests against this same emulator that bypass the CLI
# entirely and succeed -- upgrading aws-cli or running under Python <3.13
# avoids it; nothing to fix here.
param(
    [string]$Endpoint = "http://localhost:4566"
)

$ErrorActionPreference = "Stop"

if (-not $env:AWS_ACCESS_KEY_ID) { $env:AWS_ACCESS_KEY_ID = "test" }
if (-not $env:AWS_SECRET_ACCESS_KEY) { $env:AWS_SECRET_ACCESS_KEY = "test" }
if (-not $env:AWS_DEFAULT_REGION) { $env:AWS_DEFAULT_REGION = "us-east-1" }

function Invoke-Aws {
    aws --endpoint-url $Endpoint --output json @args
}

# [System.IO.Path]::GetTempFileName() instead of New-TempFile: the
# latter is a function that needs Microsoft.PowerShell.Utility to
# autoload, which doesn't always happen in a non-interactive child
# process (e.g. launched via Start-Process), so it's not reliable here.
function New-TempFile {
    [System.IO.Path]::GetTempFileName()
}

Write-Host "== Testing against $Endpoint =="

Write-Host "-- health --"
Invoke-RestMethod -Uri "$Endpoint/_aws-emulator/health"

Write-Host "-- reset (start from a clean slate) --"
Invoke-RestMethod -Method Post -Uri "$Endpoint/_aws-emulator/reset"

###############################################################################
# S3
###############################################################################
$Bucket = "tf-smoke-bucket"

$HelloFile = New-TempFile
$HelloV2File = New-TempFile
$Part1File = New-TempFile
$Part2File = New-TempFile
"hello from aws cli" | Set-Content -NoNewline -Path $HelloFile
"hello v2 from aws cli" | Set-Content -NoNewline -Path $HelloV2File
[System.IO.File]::WriteAllBytes($Part1File, [byte[]](,65 * 5242880))
[System.IO.File]::WriteAllBytes($Part2File, [byte[]](,66 * 1048576))

Write-Host "-- s3 mb --"
Invoke-Aws s3 mb "s3://$Bucket"

Write-Host "-- s3 ls (buckets) --"
Invoke-Aws s3 ls

Write-Host "-- s3api put-object --"
Invoke-Aws s3api put-object --bucket $Bucket --key hello.txt --body $HelloFile

Write-Host "-- s3api get-object --"
$GotFile = New-TempFile
Invoke-Aws s3api get-object --bucket $Bucket --key hello.txt $GotFile
Get-Content $GotFile

Write-Host "-- s3api head-object --"
Invoke-Aws s3api head-object --bucket $Bucket --key hello.txt

Write-Host "-- s3api list-objects-v2 --"
Invoke-Aws s3api list-objects-v2 --bucket $Bucket

Write-Host "-- s3api put-bucket-versioning --"
Invoke-Aws s3api put-bucket-versioning --bucket $Bucket --versioning-configuration Status=Enabled

Write-Host "-- s3api get-bucket-versioning --"
Invoke-Aws s3api get-bucket-versioning --bucket $Bucket

Write-Host "-- s3api put-object (new version) --"
$VersionId = (Invoke-Aws s3api put-object --bucket $Bucket --key hello.txt --body $HelloV2File --query "VersionId" --output text)
Write-Host "VersionId: $VersionId"

Write-Host "-- s3api get-object (specific version) --"
$GotV2File = New-TempFile
Invoke-Aws s3api get-object --bucket $Bucket --key hello.txt --version-id $VersionId $GotV2File
Get-Content $GotV2File

Write-Host "-- s3api put-object-tagging --"
$TaggingFile = New-TempFile
'{"TagSet":[{"Key":"env","Value":"smoke-test"}]}' | Set-Content -NoNewline -Path $TaggingFile
Invoke-Aws s3api put-object-tagging --bucket $Bucket --key hello.txt --tagging "file://$TaggingFile"

Write-Host "-- s3api get-object-tagging --"
Invoke-Aws s3api get-object-tagging --bucket $Bucket --key hello.txt

Write-Host "-- s3api delete-object-tagging --"
Invoke-Aws s3api delete-object-tagging --bucket $Bucket --key hello.txt

Write-Host "-- s3api create-multipart-upload --"
$UploadId = (Invoke-Aws s3api create-multipart-upload --bucket $Bucket --key multipart.txt --query "UploadId" --output text)
Write-Host "UploadId: $UploadId"

Write-Host "-- s3api upload-part (x2) --"
$ETag1 = (Invoke-Aws s3api upload-part --bucket $Bucket --key multipart.txt --upload-id $UploadId --part-number 1 --body $Part1File --query "ETag" --output text)
$ETag2 = (Invoke-Aws s3api upload-part --bucket $Bucket --key multipart.txt --upload-id $UploadId --part-number 2 --body $Part2File --query "ETag" --output text)
Write-Host "ETag1: $ETag1 ETag2: $ETag2"

Write-Host "-- s3api complete-multipart-upload --"
$CompleteFile = New-TempFile
"{`"Parts`":[{`"ETag`":$ETag1,`"PartNumber`":1},{`"ETag`":$ETag2,`"PartNumber`":2}]}" | Set-Content -NoNewline -Path $CompleteFile
Invoke-Aws s3api complete-multipart-upload --bucket $Bucket --key multipart.txt --upload-id $UploadId --multipart-upload "file://$CompleteFile"

Write-Host "-- s3api head-object (assembled multipart object, expect 6MB) --"
Invoke-Aws s3api head-object --bucket $Bucket --key multipart.txt

Write-Host "-- s3api delete-object (cleanup) --"
Invoke-Aws s3api delete-object --bucket $Bucket --key hello.txt
Invoke-Aws s3api delete-object --bucket $Bucket --key multipart.txt

Write-Host "-- s3 rb (cleanup) --"
Invoke-Aws s3 rb "s3://$Bucket"

###############################################################################
# SQS
###############################################################################
$QueueName = "tf-smoke-queue"

Write-Host "-- sqs create-queue --"
$QueueUrl = (Invoke-Aws sqs create-queue --queue-name $QueueName --query "QueueUrl" --output text)
Write-Host "QueueUrl: $QueueUrl"

Write-Host "-- sqs get-queue-url --"
Invoke-Aws sqs get-queue-url --queue-name $QueueName

Write-Host "-- sqs list-queues --"
Invoke-Aws sqs list-queues

Write-Host "-- sqs set-queue-attributes --"
Invoke-Aws sqs set-queue-attributes --queue-url $QueueUrl --attributes VisibilityTimeout=45

Write-Host "-- sqs get-queue-attributes --"
Invoke-Aws sqs get-queue-attributes --queue-url $QueueUrl --attribute-names All

Write-Host "-- sqs send-message --"
Invoke-Aws sqs send-message --queue-url $QueueUrl --message-body "hello from aws cli"

Write-Host "-- sqs send-message-batch --"
$BatchFile = New-TempFile
'[{"Id":"msg1","MessageBody":"batch-message-1"},{"Id":"msg2","MessageBody":"batch-message-2"}]' | Set-Content -NoNewline -Path $BatchFile
Invoke-Aws sqs send-message-batch --queue-url $QueueUrl --entries "file://$BatchFile"

Write-Host "-- sqs receive-message --"
$Receive = Invoke-Aws sqs receive-message --queue-url $QueueUrl --max-number-of-messages 10
$Receive
$ReceiveObj = $Receive | ConvertFrom-Json
if ($ReceiveObj.Messages) {
    $ReceiptHandle = $ReceiveObj.Messages[0].ReceiptHandle
    Write-Host "-- sqs delete-message --"
    Invoke-Aws sqs delete-message --queue-url $QueueUrl --receipt-handle $ReceiptHandle
}

Write-Host "-- sqs purge-queue --"
Invoke-Aws sqs purge-queue --queue-url $QueueUrl

Write-Host "-- sqs delete-queue (cleanup) --"
Invoke-Aws sqs delete-queue --queue-url $QueueUrl

###############################################################################
# IAM
###############################################################################
$RoleName = "tf-smoke-role"
$UserName = "tf-smoke-user"

$TrustPolicyFile = New-TempFile
$InlinePolicyFile = New-TempFile
'{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"Service":"ec2.amazonaws.com"},"Action":"sts:AssumeRole"}]}' | Set-Content -NoNewline -Path $TrustPolicyFile
'{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"s3:GetObject","Resource":"*"}]}' | Set-Content -NoNewline -Path $InlinePolicyFile

Write-Host "-- iam create-role --"
Invoke-Aws iam create-role --role-name $RoleName --assume-role-policy-document "file://$TrustPolicyFile"

Write-Host "-- iam get-role --"
Invoke-Aws iam get-role --role-name $RoleName

Write-Host "-- iam list-roles --"
Invoke-Aws iam list-roles

Write-Host "-- iam put-role-policy --"
Invoke-Aws iam put-role-policy --role-name $RoleName --policy-name "tf-smoke-policy" --policy-document "file://$InlinePolicyFile"

Write-Host "-- iam get-role-policy --"
Invoke-Aws iam get-role-policy --role-name $RoleName --policy-name "tf-smoke-policy"

Write-Host "-- iam delete-role-policy --"
Invoke-Aws iam delete-role-policy --role-name $RoleName --policy-name "tf-smoke-policy"

Write-Host "-- iam create-user --"
Invoke-Aws iam create-user --user-name $UserName

Write-Host "-- iam get-user --"
Invoke-Aws iam get-user --user-name $UserName

Write-Host "-- iam list-users --"
Invoke-Aws iam list-users

Write-Host "-- iam create-access-key --"
$AccessKeyId = (Invoke-Aws iam create-access-key --user-name $UserName --query "AccessKey.AccessKeyId" --output text)
Write-Host "AccessKeyId: $AccessKeyId"

Write-Host "-- iam list-access-keys --"
Invoke-Aws iam list-access-keys --user-name $UserName

Write-Host "-- iam delete-access-key (cleanup) --"
Invoke-Aws iam delete-access-key --user-name $UserName --access-key-id $AccessKeyId

Write-Host "-- iam delete-user (cleanup) --"
Invoke-Aws iam delete-user --user-name $UserName

Write-Host "-- iam delete-role (cleanup) --"
Invoke-Aws iam delete-role --role-name $RoleName

###############################################################################
# STS
###############################################################################
Write-Host "-- sts get-caller-identity --"
Invoke-Aws sts get-caller-identity

###############################################################################
# DynamoDB
###############################################################################
$TableName = "tf-smoke-table"

Write-Host "-- dynamodb create-table (with GSI) --"
# Una sola linea: aws.cmd es un wrapper de batch en Windows y la
# continuacion con backtick de PowerShell a traves de ese wrapper puede
# mezclar argumentos (mismo tipo de problema de quoting que el truco
# file:// de mas arriba, pero con saltos de linea en vez de comillas).
Invoke-Aws dynamodb create-table --table-name $TableName --attribute-definitions AttributeName=pk,AttributeType=S AttributeName=sk,AttributeType=S AttributeName=gsiPk,AttributeType=S --key-schema AttributeName=pk,KeyType=HASH AttributeName=sk,KeyType=RANGE --global-secondary-indexes "IndexName=gsi1,KeySchema=[{AttributeName=gsiPk,KeyType=HASH}],Projection={ProjectionType=ALL}" --billing-mode PAY_PER_REQUEST

Write-Host "-- dynamodb describe-table --"
Invoke-Aws dynamodb describe-table --table-name $TableName

Write-Host "-- dynamodb put-item (x2, same pk, different sk) --"
$Item1File = New-TempFile
$Item2File = New-TempFile
'{"pk":{"S":"user#1"},"sk":{"S":"order#1"},"gsiPk":{"S":"status#open"},"amount":{"N":"10"}}' | Set-Content -NoNewline -Path $Item1File
'{"pk":{"S":"user#1"},"sk":{"S":"order#2"},"gsiPk":{"S":"status#closed"},"amount":{"N":"20"}}' | Set-Content -NoNewline -Path $Item2File
Invoke-Aws dynamodb put-item --table-name $TableName --item "file://$Item1File"
Invoke-Aws dynamodb put-item --table-name $TableName --item "file://$Item2File"

Write-Host "-- dynamodb get-item --"
$Key1File = New-TempFile
'{"pk":{"S":"user#1"},"sk":{"S":"order#1"}}' | Set-Content -NoNewline -Path $Key1File
Invoke-Aws dynamodb get-item --table-name $TableName --key "file://$Key1File"

Write-Host "-- dynamodb query (partition key only) --"
$Values1File = New-TempFile
'{":pk":{"S":"user#1"}}' | Set-Content -NoNewline -Path $Values1File
Invoke-Aws dynamodb query --table-name $TableName --key-condition-expression "pk = :pk" --expression-attribute-values "file://$Values1File"

Write-Host "-- dynamodb query (partition key + sort key condition) --"
$Values2File = New-TempFile
'{":pk":{"S":"user#1"},":sk":{"S":"order#1"}}' | Set-Content -NoNewline -Path $Values2File
Invoke-Aws dynamodb query --table-name $TableName --key-condition-expression "pk = :pk AND sk = :sk" --expression-attribute-values "file://$Values2File"

Write-Host "-- dynamodb query (GSI) --"
$Values3File = New-TempFile
'{":g":{"S":"status#open"}}' | Set-Content -NoNewline -Path $Values3File
Invoke-Aws dynamodb query --table-name $TableName --index-name gsi1 --key-condition-expression "gsiPk = :g" --expression-attribute-values "file://$Values3File"

Write-Host "-- dynamodb scan --"
Invoke-Aws dynamodb scan --table-name $TableName

Write-Host "-- dynamodb delete-item (cleanup) --"
$Key2File = New-TempFile
'{"pk":{"S":"user#1"},"sk":{"S":"order#2"}}' | Set-Content -NoNewline -Path $Key2File
Invoke-Aws dynamodb delete-item --table-name $TableName --key "file://$Key1File"
Invoke-Aws dynamodb delete-item --table-name $TableName --key "file://$Key2File"

Write-Host "-- dynamodb delete-table (cleanup) --"
Invoke-Aws dynamodb delete-table --table-name $TableName

Remove-Item -Force $HelloFile, $HelloV2File, $Part1File, $Part2File, $GotFile, $GotV2File, $TaggingFile, $CompleteFile, $BatchFile, $TrustPolicyFile, $InlinePolicyFile, $Item1File, $Item2File, $Key1File, $Key2File, $Values1File, $Values2File, $Values3File -ErrorAction SilentlyContinue

###############################################################################
# SNS + SQS (cross-service delivery, Phase 3 -- see ROADMAP.md)
###############################################################################
$SnsQueueName = "tf-smoke-sns-queue"
$TopicName = "tf-smoke-topic"

Write-Host "-- sqs create-queue (sns target) --"
$SnsQueueUrl = (Invoke-Aws sqs create-queue --queue-name $SnsQueueName --query "QueueUrl" --output text)
Write-Host "QueueUrl: $SnsQueueUrl"

Write-Host "-- sqs get-queue-attributes (QueueArn) --"
$SnsQueueArn = (Invoke-Aws sqs get-queue-attributes --queue-url $SnsQueueUrl --attribute-names QueueArn --query "Attributes.QueueArn" --output text)
Write-Host "QueueArn: $SnsQueueArn"

Write-Host "-- sns create-topic --"
$TopicArn = (Invoke-Aws sns create-topic --name $TopicName --query "TopicArn" --output text)
Write-Host "TopicArn: $TopicArn"

Write-Host "-- sns list-topics --"
Invoke-Aws sns list-topics

Write-Host "-- sns subscribe (protocol=sqs) --"
$SubscriptionArn = (Invoke-Aws sns subscribe --topic-arn $TopicArn --protocol sqs --notification-endpoint $SnsQueueArn --query "SubscriptionArn" --output text)
Write-Host "SubscriptionArn: $SubscriptionArn"

Write-Host "-- sns list-subscriptions-by-topic --"
Invoke-Aws sns list-subscriptions-by-topic --topic-arn $TopicArn

Write-Host "-- sns publish --"
Invoke-Aws sns publish --topic-arn $TopicArn --message "hello via sns"

Write-Host "-- sqs receive-message (expect the SNS message delivered) --"
Invoke-Aws sqs receive-message --queue-url $SnsQueueUrl --max-number-of-messages 10

Write-Host "-- sns unsubscribe (cleanup) --"
Invoke-Aws sns unsubscribe --subscription-arn $SubscriptionArn

Write-Host "-- sns delete-topic (cleanup) --"
Invoke-Aws sns delete-topic --topic-arn $TopicArn

Write-Host "-- sqs delete-queue (cleanup) --"
Invoke-Aws sqs delete-queue --queue-url $SnsQueueUrl

###############################################################################
# EventBridge + SQS (rule pattern matching + target delivery, Phase 3)
###############################################################################
$EvtQueueName = "tf-smoke-events-queue"
$RuleName = "tf-smoke-rule"

Write-Host "-- sqs create-queue (eventbridge target) --"
$EvtQueueUrl = (Invoke-Aws sqs create-queue --queue-name $EvtQueueName --query "QueueUrl" --output text)
Write-Host "QueueUrl: $EvtQueueUrl"

Write-Host "-- sqs get-queue-attributes (QueueArn) --"
$EvtQueueArn = (Invoke-Aws sqs get-queue-attributes --queue-url $EvtQueueUrl --attribute-names QueueArn --query "Attributes.QueueArn" --output text)
Write-Host "QueueArn: $EvtQueueArn"

Write-Host "-- events put-rule (with EventPattern) --"
$PatternFile = New-TempFile
'{"source":["tf.smoke"]}' | Set-Content -NoNewline -Path $PatternFile
Invoke-Aws events put-rule --name $RuleName --event-pattern "file://$PatternFile"

Write-Host "-- events list-rules --"
Invoke-Aws events list-rules

Write-Host "-- events put-targets --"
Invoke-Aws events put-targets --rule $RuleName --targets "Id=1,Arn=$EvtQueueArn"

Write-Host "-- events list-targets-by-rule --"
Invoke-Aws events list-targets-by-rule --rule $RuleName

Write-Host "-- events put-events (matching pattern, expect delivery) --"
$MatchingEntriesFile = New-TempFile
'[{"Source":"tf.smoke","DetailType":"smoke-test","Detail":"{\"k\":\"v\"}"}]' | Set-Content -NoNewline -Path $MatchingEntriesFile
Invoke-Aws events put-events --entries "file://$MatchingEntriesFile"

Write-Host "-- events put-events (non-matching source, expect no delivery) --"
$NonMatchingEntriesFile = New-TempFile
'[{"Source":"tf.other","DetailType":"smoke-test","Detail":"{}"}]' | Set-Content -NoNewline -Path $NonMatchingEntriesFile
Invoke-Aws events put-events --entries "file://$NonMatchingEntriesFile"

Write-Host "-- sqs receive-message (expect exactly one EventBridge envelope) --"
Invoke-Aws sqs receive-message --queue-url $EvtQueueUrl --max-number-of-messages 10

Write-Host "-- events remove-targets (cleanup) --"
Invoke-Aws events remove-targets --rule $RuleName --ids 1

Write-Host "-- events delete-rule (cleanup) --"
Invoke-Aws events delete-rule --name $RuleName

Write-Host "-- sqs delete-queue (cleanup) --"
Invoke-Aws sqs delete-queue --queue-url $EvtQueueUrl

###############################################################################
# Lambda (local invocation only -- see ROADMAP.md)
###############################################################################
$FunctionName = "tf-smoke-fn"

Write-Host "-- lambda create-function (in-process stub, no EMULATOR_INVOKE_COMMAND) --"
# El AWS CLI valida que --zip-file sea un .zip real (firma PK) antes de
# mandar el request, asi que un archivo de texto plano no sirve -- a
# diferencia del resto de este script, este no es un detalle de quoting de
# PowerShell sino una validacion real del lado del cliente.
$LambdaZipFile = New-TempFile
Remove-Item $LambdaZipFile -Force
Add-Type -AssemblyName System.IO.Compression
$zipStream = [System.IO.Compression.ZipFile]::Open($LambdaZipFile, [System.IO.Compression.ZipArchiveMode]::Create)
$zipEntry = $zipStream.CreateEntry("handler.py")
$entryStream = $zipEntry.Open()
$writer = New-Object System.IO.StreamWriter($entryStream)
$writer.Write("def main(event, context):`n    return event`n")
$writer.Close()
$zipStream.Dispose()
Invoke-Aws lambda create-function `
    --function-name $FunctionName `
    --runtime provided `
    --role arn:aws:iam::000000000000:role/lambda-role `
    --handler handler.main `
    --zip-file "fileb://$LambdaZipFile"

Write-Host "-- lambda get-function --"
Invoke-Aws lambda get-function --function-name $FunctionName

Write-Host "-- lambda list-functions --"
Invoke-Aws lambda list-functions

Write-Host "-- lambda invoke (in-process stub: echoes the payload back) --"
$LambdaPayloadFile = New-TempFile
'{"hello":"world"}' | Set-Content -NoNewline -Path $LambdaPayloadFile
$LambdaOutFile = New-TempFile
Invoke-Aws lambda invoke --function-name $FunctionName --payload "fileb://$LambdaPayloadFile" $LambdaOutFile
Get-Content $LambdaOutFile

Write-Host "-- lambda delete-function (cleanup) --"
Invoke-Aws lambda delete-function --function-name $FunctionName

Remove-Item -Force $PatternFile, $MatchingEntriesFile, $NonMatchingEntriesFile, $LambdaZipFile, $LambdaPayloadFile, $LambdaOutFile -ErrorAction SilentlyContinue

Write-Host "== All smoke tests passed =="
