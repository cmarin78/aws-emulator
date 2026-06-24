# Roadmap

Incremental, from-scratch build of `aws-emulator` in Go, inspired by
[ministack](../ministack) (Python, ~56 services) and following the
azure-emulator/gcp-emulator philosophy: a solid core first, additional
services in successive phases, without trying to cover everything at once.

## Phase 1 — Core + 5 core services (✅ complete)

- [x] Project skeleton (`go.mod`, folder structure)
- [x] `internal/storage`: embedded BoltDB persistence
- [x] `internal/server`: single HTTP dispatcher + middleware (CORS, logging, recover) + admin endpoints
- [x] `internal/router`: service detection (X-Amz-Target / credential scope / Action / Host / path / S3 fallback), ported from `ministack/core/router.py`
- [x] S3 (buckets + objects)
- [x] SQS (queues + messages)
- [x] IAM (roles)
- [x] STS (GetCallerIdentity)
- [x] DynamoDB (tables + items, JSON protocol)
- [x] `cmd/aws-emulator/main.go`, CI, README, LICENSE

## Phase 2 — Harden the core (next)

- [ ] Real `POST /_aws-emulator/reset` (clear all BoltDB buckets) — needed for integration tests that start from a clean state
- [ ] IAM: inline policies (`PutRolePolicy`/`GetRolePolicy`/`DeleteRolePolicy`), users, access keys
- [ ] SQS: queue attributes (`GetQueueAttributes`/`SetQueueAttributes`), batch operations
- [ ] DynamoDB: secondary indexes (GSI/LSI) and real `Query` with key conditions (currently handled as `Scan`)
- [ ] S3: versioning, tags, multipart upload
- [ ] Integration tests with the real AWS CLI against the emulator (smoke test), following the `terraform/*-smoke-test` pattern in azure-emulator/gcp-emulator

## Phase 3 — Messaging and event services

- [ ] SNS
- [ ] EventBridge
- [ ] Lambda (local function invocation; see how far ministack currently gets in `ministack/services/lambda_/`)

## Phase 4 — Remaining ministack services

- [ ] Review `ministack/services/` and prioritize by real usage in Cesar's projects (CloudWatch Logs, Secrets Manager, SSM Parameter Store are early candidates)
- [ ] Real multi-tenancy (today everything lives under a single hardcoded `accountID = "000000000000"` in each service — see `defaultAccountID`/`accountID` in `internal/services/*`)

## Design notes that shouldn't get lost

- AWS doesn't route by path like Azure/GCP — every request comes in through a single endpoint and the service is inferred (see `internal/router`). Any new service needs: (1) its detection pattern in `router.go`, (2) its `Service` in `internal/services/<name>/`, (3) registration in `main.go`.
- Go's `regexp` (RE2) doesn't support lookahead/lookbehind, unlike Python's `re`. When porting more patterns from `ministack/core/router.py`, this needs to be checked case by case (already happened once with an S3 pattern, see the comment in `router.go`).
- Two response protocols coexist: Query/XML (`server.WriteXML`/`WriteXMLError`) for S3/SQS/IAM/STS, JSON (`server.WriteJSON`/`WriteJSONError`) for DynamoDB. A new service must identify which of the two protocols its real API uses before implementing it.
