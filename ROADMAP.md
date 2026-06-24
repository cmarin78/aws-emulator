# Roadmap

Incremental, from-scratch build of `aws-emulator` in Go, following the
azure-emulator/gcp-emulator philosophy: a solid core first, additional
services in successive phases, without trying to cover everything at once.

## Phase 1 — Core + 5 core services (✅ complete)

- [x] Project skeleton (`go.mod`, folder structure)
- [x] `internal/storage`: embedded BoltDB persistence
- [x] `internal/server`: single HTTP dispatcher + middleware (CORS, logging, recover) + admin endpoints
- [x] `internal/router`: service detection (X-Amz-Target / credential scope / Action / Host / path / S3 fallback)
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

## Phase 3 — Messaging and eventing

- [ ] SNS: topics, subscriptions, Publish (with SQS subscriptions wired to the existing queue storage)
- [ ] EventBridge: default event bus, PutEvents, rules with simple pattern matching, targets limited to SQS/SNS to start
- [ ] Lambda: local function invocation only (`CreateFunction` storing a handler reference, `Invoke` running it in-process or via a subprocess) — no real packaging/runtime emulation in this phase

## Phase 4 — Observability and configuration

- [ ] CloudWatch Logs: log groups/streams, PutLogEvents, FilterLogEvents — common dependency for anything that already emits logs in tests
- [ ] Secrets Manager: CreateSecret/GetSecretValue/PutSecretValue/DeleteSecret, plain BoltDB storage (no real encryption, this is a dev emulator)
- [ ] SSM Parameter Store: PutParameter/GetParameter/GetParameters/DeleteParameter, including `SecureString` type (stored as plain text, same caveat as Secrets Manager)

## Phase 5 — Broader API surface

- [ ] API Gateway: minimal REST/HTTP API proxy semantics (resource → integration → Lambda invocation), enough to test API Gateway + Lambda wiring locally
- [ ] KMS: stub Encrypt/Decrypt/GenerateDataKey (base64 passthrough, not real cryptography) so SDKs that call KMS as a side effect don't fail
- [ ] Revisit this phase's scope based on which services actually show up in Cesar's projects — prioritize by real usage over AWS's full catalog

## Phase 6 — Platform hardening

- [ ] Real multi-tenancy: today everything lives under a single hardcoded `accountID = "000000000000"` in each service (see `defaultAccountID`/`accountID` in `internal/services/*`) — move to per-credential account/region scoping
- [ ] Persistence migrations/versioning for the BoltDB schema, so upgrading the emulator doesn't require wiping local state
- [ ] Broader compatibility pass: run the real AWS CLI and at least one SDK (Go, Python) against every implemented service and close any protocol gaps found

## Design notes that shouldn't get lost

- AWS doesn't route by path like Azure/GCP — every request comes in through a single endpoint and the service is inferred (see `internal/router`). Any new service needs: (1) its detection pattern in `router.go`, (2) its `Service` in `internal/services/<name>/`, (3) registration in `main.go`.
- Go's `regexp` (RE2) doesn't support lookahead/lookbehind, unlike Python's `re`. When adding new detection patterns to `router.go`, this needs to be checked case by case (already happened once with an S3 pattern, see the comment in `router.go`).
- Two response protocols coexist: Query/XML (`server.WriteXML`/`WriteXMLError`) for S3/SQS/IAM/STS, JSON (`server.WriteJSON`/`WriteJSONError`) for DynamoDB. A new service must identify which of the two protocols its real API uses before implementing it.
