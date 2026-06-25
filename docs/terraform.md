# Using Terraform against aws-emulator

This tutorial walks through pointing the real `hashicorp/aws` Terraform
provider at `aws-emulator` — no real AWS account, no LocalStack, no mocked
provider. Because it's the *real* provider talking the *real* wire protocol,
this is also the strongest compatibility test this project has (see
[ROADMAP.md](../ROADMAP.md), Phase 6) — bugs the AWS CLI/botocore tolerate
will surface here first.

A full working example with every implemented service is kept at
[`terraform/aws-smoke-test/main.tf`](../terraform/aws-smoke-test/main.tf) and
is exercised end-to-end (`apply` → `plan` → `destroy`) as part of this
project's test process. Treat it as the canonical reference; this tutorial
extracts and explains the pieces in isolation.

## 1. Prerequisites

- Terraform >= 1.5 (any recent 1.x release works)
- The `aws-emulator` binary running locally:

  ```bash
  go run ./cmd/aws-emulator -addr :4566 -db .aws-emulator-data/state.db
  ```

- No AWS account, no real credentials. The emulator doesn't validate
  signatures, so any non-empty access key/secret works.

## 2. Point the provider at the emulator

The real `aws` provider supports redirecting each service to a custom
endpoint via the `endpoints {}` block — the same mechanism people use against
LocalStack. Create a `main.tf`:

```hcl
terraform {
  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 5.0"
    }
  }
}

variable "endpoint" {
  type    = string
  default = "http://localhost:4566"
}

provider "aws" {
  region = "us-east-1"

  access_key = "test"
  secret_key = "test"

  # The emulator doesn't validate credentials, doesn't expose EC2 instance
  # metadata, and doesn't need a real account ID resolved.
  skip_credentials_validation = true
  skip_metadata_api_check     = true
  skip_requesting_account_id  = true

  # Real S3 expects virtual-hosted-style requests (bucket.s3.amazonaws.com).
  # This emulator, like LocalStack, only understands path-style requests
  # (localhost:4566/bucket/key).
  s3_use_path_style = true

  endpoints {
    s3       = var.endpoint
    sqs      = var.endpoint
    dynamodb = var.endpoint
    iam      = var.endpoint
    sts      = var.endpoint
    sns      = var.endpoint
    # ...add one entry per service you use; see the full list in
    # terraform/aws-smoke-test/main.tf for every service this emulator
    # implements today.
  }
}
```

Every implemented service needs its own line in `endpoints {}` — there's no
wildcard. If you add a resource for a service you forgot to list there,
Terraform will try to reach real AWS for it and fail to authenticate.

## 3. Write a resource and apply it

A minimal S3 example:

```hcl
resource "aws_s3_bucket" "example" {
  bucket = "my-local-bucket"
}

resource "aws_s3_object" "hello" {
  bucket  = aws_s3_bucket.example.id
  key     = "hello.txt"
  content = "hello from terraform"
}
```

```bash
terraform init
terraform apply -auto-approve
```

Verify it landed in the emulator with the AWS CLI:

```bash
aws --endpoint-url http://localhost:4566 s3 ls s3://my-local-bucket
```

## 4. Tear it down

```bash
terraform destroy -auto-approve
```

`destroy` exercises a different code path than `apply` (delete handlers,
existence-check waiters), so it's worth running even in throwaway local
testing — several real bugs in this project were only found by destroy, not
apply (see ROADMAP.md's Phase 6 notes on the SQS delete-waiter and the IAM
role's pre-delete instance-profile check).

## 5. Check for drift with `terraform plan`

```bash
terraform plan -detailed-exitcode
```

Exit code `0` means no changes (fully idempotent), `2` means Terraform wants
to update something, `1` means an error. Running this immediately after a
clean `apply` is a good way to catch "we accepted the value but didn't echo
it back on read" bugs. As of Phase 6, a few resources still show drift here
(DynamoDB billing mode/attributes, IAM role's `max_session_duration`, API
Gateway integration's `uri`/`timeout_milliseconds`, and most of
`aws_lambda_function`'s computed fields) — see ROADMAP.md for the current
list of known gaps. `apply` and `destroy` themselves are unaffected.

## 6. Multi-service example

Resources can reference each other across services exactly like real AWS —
Terraform's dependency graph doesn't care that the backend is an emulator.
For example, an SNS topic delivering to an SQS queue:

```hcl
resource "aws_sns_topic" "events" {
  name = "my-topic"
}

resource "aws_sqs_queue" "events_queue" {
  name = "my-queue"
}

resource "aws_sns_topic_subscription" "fanout" {
  topic_arn = aws_sns_topic.events.arn
  protocol  = "sqs"
  endpoint  = aws_sqs_queue.events_queue.arn
}
```

After `apply`, `aws sns publish --topic-arn <arn> --message hi` against the
emulator delivers a real message into `my-queue`, exactly as it would in
production — see [`terraform/aws-smoke-test/main.tf`](../terraform/aws-smoke-test/main.tf)
for the equivalent wired into the full smoke test, plus the EventBridge →
SQS, Lambda invocation via API Gateway, and other cross-service examples.

## Services covered

Anything listed in the README's service table can be driven by Terraform,
with two exceptions: KMS (this emulator only stubs `Encrypt`/`Decrypt`/
`GenerateDataKey`, there's no `CreateKey`, so there's no manageable
`aws_kms_key` resource — only usable from the SDK/CLI directly) and CloudWatch
Logs/Secrets Manager/SSM, which work but have no interesting cross-service
wiring beyond what's already in the smoke test.

## Troubleshooting

- **"no valid credential sources" / signing errors**: double-check
  `skip_credentials_validation`/`skip_metadata_api_check` are set — without
  them the provider tries to resolve a real identity before sending requests.
- **A resource's `Read` silently produces empty/wrong values after apply**:
  likely one of the known drift gaps above, or a new one — check
  ROADMAP.md's design notes, then file/fix it the same way prior gaps were
  found (compare against the real `terraform-provider-aws` source for that
  resource's Read function).
- **Provider tries to hit real AWS for a service you're using**: you forgot
  to add that service to the `endpoints {}` block.
