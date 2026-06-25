# Smoke-test del provider REAL `aws` (no el AWS CLI) contra el emulador.
#
# Mismo espíritu que terraform/azurerm-smoke-test (azure-emulator) y
# examples/terraform-real-poc (gcp-emulator): ejercitar un segundo cliente
# real (acá, el SDK Go de AWS embebido en el provider, en vez de botocore
# vía la CLI) contra cada servicio implementado, para encontrar gaps de
# protocolo que `scripts/test-aws-cli.sh`/`.ps1` no necesariamente cubren —
# este es el "broader compatibility pass" de la Fase 6 (ver ROADMAP.md).
#
# El provider `aws` real soporta apuntar cada servicio a un endpoint custom
# vía el bloque `endpoints {}` (a diferencia de azurerm, que necesita un
# `metadata_host` + HTTPS real, o de google, que necesita *_custom_endpoint
# por servicio) — es el mismo patrón que LocalStack documenta, y este
# emulador es compatible con él de la misma forma.
#
# No hay recurso para KMS: este emulador solo implementa Encrypt/Decrypt/
# GenerateDataKey (ver internal/services/kms), no CreateKey/DescribeKey, así
# que no hay ningún recurso administrable de `aws_kms_key` que aplicar acá
# — KMS solo se ejercita hoy desde el AWS CLI (ver scripts/test-aws-cli.sh).

terraform {
  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 5.0"
    }
    archive = {
      source  = "hashicorp/archive"
      version = "~> 2.0"
    }
  }
}

variable "endpoint" {
  type    = string
  default = "http://localhost:4566"
}

variable "region" {
  type    = string
  default = "us-east-1"
}

provider "aws" {
  region = var.region

  access_key = "test"
  secret_key = "test"

  # El emulador no valida credenciales reales, no tiene metadata de
  # instancia, ni necesita resolver el account ID real — mismo motivo que
  # azurerm-smoke-test usa client_id/secret falsos contra el emisor de
  # tokens stub de aadtoken.
  skip_credentials_validation = true
  skip_metadata_api_check     = true
  skip_requesting_account_id  = true

  # S3 real espera un host virtual-hosted-style (bucket.s3.amazonaws.com);
  # este emulador, como LocalStack, solo entiende path-style
  # (localhost:4566/bucket/key) — ver internal/router.go, patrón "s3" como
  # fallback final sin distinguir host virtual.
  s3_use_path_style = true

  endpoints {
    s3             = var.endpoint
    sqs            = var.endpoint
    dynamodb       = var.endpoint
    iam            = var.endpoint
    sts            = var.endpoint
    sns            = var.endpoint
    events         = var.endpoint
    lambda         = var.endpoint
    cloudwatchlogs = var.endpoint
    secretsmanager = var.endpoint
    ssm            = var.endpoint
    apigateway     = var.endpoint
  }
}

###############################################################################
# S3
###############################################################################
resource "aws_s3_bucket" "test" {
  bucket = "tf-real-smoke-bucket"
}

resource "aws_s3_object" "test" {
  bucket  = aws_s3_bucket.test.id
  key     = "hello.txt"
  content = "hello from the real terraform aws provider"
}

###############################################################################
# SQS
###############################################################################
resource "aws_sqs_queue" "test" {
  name = "tf-real-smoke-queue"
}

###############################################################################
# DynamoDB
###############################################################################
resource "aws_dynamodb_table" "test" {
  name         = "tf-real-smoke-table"
  billing_mode = "PAY_PER_REQUEST"
  hash_key     = "pk"

  attribute {
    name = "pk"
    type = "S"
  }
}

resource "aws_dynamodb_table_item" "test" {
  table_name = aws_dynamodb_table.test.name
  hash_key   = aws_dynamodb_table.test.hash_key

  item = jsonencode({
    pk   = { S = "item#1" }
    note = { S = "created by the real aws provider" }
  })
}

###############################################################################
# IAM
###############################################################################
resource "aws_iam_role" "test" {
  name = "tf-real-smoke-role"

  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect    = "Allow"
      Principal = { Service = "lambda.amazonaws.com" }
      Action    = "sts:AssumeRole"
    }]
  })
}

###############################################################################
# Lambda (in-process stub invocation -- see ROADMAP.md)
###############################################################################
data "archive_file" "lambda" {
  type        = "zip"
  output_path = "${path.module}/.tmp-lambda.zip"

  source {
    content  = "def main(event, context):\n    return event\n"
    filename = "handler.py"
  }
}

resource "aws_lambda_function" "test" {
  function_name = "tf-real-smoke-fn"
  role          = aws_iam_role.test.arn
  handler       = "handler.main"
  runtime       = "python3.12"

  filename         = data.archive_file.lambda.output_path
  source_code_hash = data.archive_file.lambda.output_base64sha256
}

###############################################################################
# SNS + SQS (delivery wiring, Fase 3)
###############################################################################
resource "aws_sns_topic" "test" {
  name = "tf-real-smoke-topic"
}

resource "aws_sqs_queue" "sns_target" {
  name = "tf-real-smoke-sns-queue"
}

resource "aws_sns_topic_subscription" "test" {
  topic_arn = aws_sns_topic.test.arn
  protocol  = "sqs"
  endpoint  = aws_sqs_queue.sns_target.arn
}

###############################################################################
# EventBridge + SQS (Fase 3)
###############################################################################
resource "aws_sqs_queue" "events_target" {
  name = "tf-real-smoke-events-queue"
}

resource "aws_cloudwatch_event_rule" "test" {
  name = "tf-real-smoke-rule"

  event_pattern = jsonencode({
    source = ["tf.real.smoke"]
  })
}

resource "aws_cloudwatch_event_target" "test" {
  rule = aws_cloudwatch_event_rule.test.name
  arn  = aws_sqs_queue.events_target.arn
}

###############################################################################
# CloudWatch Logs (Fase 4)
###############################################################################
resource "aws_cloudwatch_log_group" "test" {
  name = "/tf-real-smoke/group"
}

resource "aws_cloudwatch_log_stream" "test" {
  name           = "tf-real-smoke-stream"
  log_group_name = aws_cloudwatch_log_group.test.name
}

###############################################################################
# Secrets Manager (Fase 4)
###############################################################################
resource "aws_secretsmanager_secret" "test" {
  name = "tf-real-smoke-secret"
}

resource "aws_secretsmanager_secret_version" "test" {
  secret_id     = aws_secretsmanager_secret.test.id
  secret_string = "hello-secret"
}

###############################################################################
# SSM Parameter Store (Fase 4)
###############################################################################
resource "aws_ssm_parameter" "test" {
  name  = "/tf-real-smoke/param"
  type  = "String"
  value = "hello-param"
}

###############################################################################
# API Gateway (Fase 5)
###############################################################################
resource "aws_api_gateway_rest_api" "test" {
  name = "tf-real-smoke-api"
}

# La raíz "/" se crea automáticamente al crear la REST API (ver
# internal/services/apigateway/apigateway.go, createRestApi) -- igual que
# en AWS real, se la busca con este data source en vez de crearla.
data "aws_api_gateway_resource" "root" {
  rest_api_id = aws_api_gateway_rest_api.test.id
  path        = "/"
}

resource "aws_api_gateway_method" "test" {
  rest_api_id   = aws_api_gateway_rest_api.test.id
  resource_id   = data.aws_api_gateway_resource.root.id
  http_method   = "ANY"
  authorization = "NONE"
}

resource "aws_api_gateway_integration" "test" {
  rest_api_id             = aws_api_gateway_rest_api.test.id
  resource_id             = data.aws_api_gateway_resource.root.id
  http_method             = aws_api_gateway_method.test.http_method
  type                    = "AWS_PROXY"
  integration_http_method = "POST"
  uri                     = "arn:aws:apigateway:${var.region}:lambda:path/2015-03-31/functions/${aws_lambda_function.test.arn}/invocations"
}

# stage_name en aws_api_gateway_deployment está deprecado en el provider v5
# (recomienda un aws_api_gateway_stage separado, que llama a CreateStage),
# pero este emulador todavía no implementa CreateStage como operación
# propia -- createDeployment ya crea el stage como efecto secundario si se
# le pasa stageName en el body (ver createDeployment en apigateway.go), que
# es exactamente lo que produce este atributo "deprecado". Revisar si vale
# la pena implementar CreateStage por separado si esto llega a romper con
# una versión futura del provider que deje de mandar el campo.
resource "aws_api_gateway_deployment" "test" {
  rest_api_id = aws_api_gateway_rest_api.test.id
  stage_name  = "dev"

  depends_on = [aws_api_gateway_integration.test]

  triggers = {
    redeployment = sha1(jsonencode([
      aws_api_gateway_method.test.id,
      aws_api_gateway_integration.test.id,
    ]))
  }
}

output "s3_bucket_id" {
  value = aws_s3_bucket.test.id
}

output "dynamodb_table_arn" {
  value = aws_dynamodb_table.test.arn
}

output "lambda_function_arn" {
  value = aws_lambda_function.test.arn
}

output "sns_topic_arn" {
  value = aws_sns_topic.test.arn
}

output "rest_api_id" {
  value = aws_api_gateway_rest_api.test.id
}

output "execute_api_invoke_url" {
  value = "${var.endpoint}/execute-api/${aws_api_gateway_rest_api.test.id}/${aws_api_gateway_deployment.test.stage_name}/"
}
