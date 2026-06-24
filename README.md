# aws-emulator

![aws-emulator](./assets/banner.svg)

Local AWS emulator in Go, sibling to [azure-emulator](../azure-emulator) and
[gcp-emulator](../gcp-emulator), built for development and integration
testing without depending on a real AWS account or Docker.

## Origin and relationship to ministack

[`ministack`](../ministack) is a mature, actively maintained Python emulator
covering roughly 56 AWS services. It's the project that proved this whole
approach — a local, dependency-free stand-in for AWS during development and
tests — was worth having, and it remains the broader reference for what
full AWS coverage looks like across this family of projects.

`aws-emulator` is not a replacement for `ministack`; it's a companion
rewrite in Go, for three concrete reasons:

1. **Stack consistency.** [azure-emulator](../azure-emulator) and
   [gcp-emulator](../gcp-emulator) are already Go: same single-binary
   distribution, same router/middleware/storage patterns. Having the AWS
   emulator on a different stack (Python, virtualenv, separate CI tooling)
   made it the odd one out among three sibling projects meant to be used
   together.
2. **Operational simplicity.** A static Go binary with an embedded BoltDB
   file needs no interpreter and no dependency resolution step — easier to
   drop into a CI job, a Docker image, or hand to a teammate who just wants
   `go run` and an endpoint.
3. **A deliberate excuse to re-derive a known design in Go.** AWS's
   single-endpoint multiplexed routing (`X-Amz-Target` / SigV4 credential
   scope / `Action` / `Host` / path) is the most interesting part of
   `ministack`'s design. Porting it by hand, rather than calling it from Go,
   is a way to validate that design against Go's type system and
   concurrency model — and to learn it more deeply in the process.

`ministack` keeps growing on its own track and stays the broader reference;
`aws-emulator` starts narrower (5 services, see below) and grows phase by
phase — see [ROADMAP.md](./ROADMAP.md).

## Why this differs from azure-emulator/gcp-emulator

Azure and GCP route by hierarchical path (one logical host per service,
nested REST routes). AWS multiplexes dozens of services over a **single
endpoint** — typically port `4566`, same as LocalStack — and distinguishes
the target service through a combination of signals: the `X-Amz-Target`
header, the credential scope in the `Authorization` header, the `Action`
parameter, the `Host` header, or the path. That routing logic lives in
[`internal/router`](./internal/router) and is the most distinctive piece of
this project compared to its siblings.

## Implemented services (Phase 1)

| Service | Protocol | Operations |
|---|---|---|
| S3 | Query/XML | buckets and objects: create/list/delete bucket, put/get/head/delete object |
| SQS | Query/XML | queues and messages: create/list/get-url/delete/purge queue, send/receive/delete message |
| IAM | Query/XML | roles: create/get/list/delete role |
| STS | Query/XML | GetCallerIdentity |
| DynamoDB | JSON 1.0 | tables and items: create/describe/delete table, put/get/delete item, scan (Query is handled as scan) |

The rest of the ~50 AWS services (Lambda, API Gateway, EventBridge, etc.)
are left for future phases — see [ROADMAP.md](./ROADMAP.md).

## Usage

```bash
go run ./cmd/aws-emulator -addr :4566 -db .aws-emulator-data/state.db
```

Point the AWS SDK/CLI at the emulator (any credentials work, the signature
is not validated):

```bash
export AWS_ACCESS_KEY_ID=test
export AWS_SECRET_ACCESS_KEY=test
export AWS_DEFAULT_REGION=us-east-1
aws --endpoint-url http://localhost:4566 s3 mb s3://my-bucket
aws --endpoint-url http://localhost:4566 s3 ls
```

Admin endpoints:

- `GET /_aws-emulator/health` — health check.
- `POST /_aws-emulator/reset` — clears all persisted state (all services), useful for integration tests that need to start from a clean slate.

## Development

```bash
go build ./...
go vet ./...
go test ./... -v -race
```

## Persistence

State is embedded in BoltDB (`go.etcd.io/bbolt`), a single file
(`.aws-emulator-data/state.db` by default). No external dependencies: no
Postgres or Docker required to run the emulator.
