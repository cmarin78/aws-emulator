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

## Phase 2 — Harden the core (✅ complete)

- [x] Real `POST /_aws-emulator/reset` (clear all BoltDB buckets) — needed for integration tests that start from a clean state
- [x] IAM: inline policies (`PutRolePolicy`/`GetRolePolicy`/`DeleteRolePolicy`), users, access keys
- [x] SQS: queue attributes (`GetQueueAttributes`/`SetQueueAttributes`), batch operations
- [x] DynamoDB: secondary indexes (GSI/LSI) and real `Query` with key conditions (currently handled as `Scan`)
- [x] S3: versioning, tags, multipart upload
- [x] Integration tests with the real AWS CLI against the emulator (smoke test), following the `terraform/*-smoke-test` pattern in azure-emulator/gcp-emulator (`scripts/test-aws-cli.sh` and `scripts/test-aws-cli.ps1`)

## Phase 3 — Messaging and eventing (✅ complete)

- [x] SNS: topics, subscriptions, Publish (with SQS subscriptions wired to the existing queue storage)
- [x] EventBridge: default event bus, PutEvents, rules with simple pattern matching, targets limited to SQS/SNS to start
- [x] Lambda: local function invocation only (`CreateFunction` storing a handler reference, `Invoke` running it in-process or via a subprocess) — no real packaging/runtime emulation in this phase
- [x] Smoke test coverage for all three in `scripts/test-aws-cli.sh`/`.ps1`, verified end-to-end against the real AWS CLI

## Phase 4 — Observability and configuration (✅ complete)

- [x] CloudWatch Logs: log groups/streams, PutLogEvents, FilterLogEvents — common dependency for anything that already emits logs in tests
- [x] Secrets Manager: CreateSecret/GetSecretValue/PutSecretValue/DeleteSecret, plain BoltDB storage (no real encryption, this is a dev emulator)
- [x] SSM Parameter Store: PutParameter/GetParameter/GetParameters/DeleteParameter, including `SecureString` type (stored as plain text, same caveat as Secrets Manager)
- [x] Smoke test coverage for all three in `scripts/test-aws-cli.sh`/`.ps1`, verified end-to-end against the real AWS CLI

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
- SQS is actually dual-protocol in practice: modern botocore sends SQS requests as JSON (`X-Amz-Target: AmazonSQS.<Action>`) rather than classic Query/XML, and — critically — it picks its *response* parser based on how it sent the request, not on the response's `Content-Type`. So `internal/services/sqs/sqs.go` detects `X-Amz-Target` per-request and returns a parallel flat JSON shape instead of XML when present (`writeResult`/`writeError` plus a `*JSON` struct per response type). Returning XML to a JSON request doesn't error — fields just silently come back empty/`None` on the client side, which is a nasty failure mode to debug. Any future service that mixes both client generations needs the same per-request branching.
- Local dev environment caveat (not an emulator bug): aws-cli 1.36.40 on Python 3.14 throws `ValueError: badly formed help string` for a few operations whose message-body parameter help text contains a literal `%`/`&` (`s3api list-objects-v2`, `sqs send-message`, `sqs receive-message`, `sns publish`) before the request is even sent — Python 3.13+ tightened `argparse`'s help-string validation. Confirmed via `aws --debug` and, independently, by exercising the same code paths with raw HTTP requests that bypass the CLI entirely and succeed (e.g. SNS `Publish` → SQS delivery, EventBridge `PutEvents` → SQS delivery). Fixable by upgrading aws-cli or using Python <3.13, not something to chase in this repo.
- BoltDB anti-pattern worth remembering: `storage.DB.List` wraps its callback in a read-only `bolt.View`. Calling a write-backed method (anything that ends up in `db.Put`/`db.Update`) from *inside* that callback, on the same goroutine, deadlocks Bolt — the write's mmap remap blocks on the still-open read transaction, which never releases because its goroutine is itself stuck in the write call. Hit this for real in both `sns.deliverToSubscribers` and `events.deliverToTargets` (Publish/PutEvents hung the server solid on any topic/rule with subscribers). Fix: drain `List` into a slice first, let the read transaction close, then do writes/deliveries in a separate loop. Any future cross-service delivery code needs the same shape.
- AWS CLI requests to bare resource-collection paths (e.g. Lambda's `/2015-03-31/functions`) can arrive with a trailing slash depending on the operation (`list-functions`/`create-function` send `/2015-03-31/functions/`, confirmed by a live 404 against this emulator) — route matching on those paths should trim a trailing slash before comparing, the way `lambda.ServeHTTP` does now.
- `--zip-file fileb://...` validates that the referenced file is an actual ZIP (magic header), not just any bytes — a plain text fixture file fails client-side before the request is sent. Test scripts that exercise Lambda `CreateFunction` need a real (even if minimal/empty) zip file.
- Secrets Manager is the one JSON-protocol service with a lowercase `X-Amz-Target` prefix: `secretsmanager.{Action}` instead of something like `Logs_20140328.{Action}` or `AmazonSSM.{Action}` — confirmed with `aws secretsmanager create-secret --debug`. Its SigV4 credential-scope service name is `secretsmanager` (no hyphen); botocore's internal hyphenated name `secrets-manager` only shows up in its event-bus hook names, never on the wire. Easy to get wrong if you pattern-match against the other JSON services in `router.go`.
- CloudWatch Logs `PutLogEvents`/`UploadSequenceToken`: real CloudWatch Logs deprecated server-side sequence-token validation in 2021 (clients still send the field for backward compatibility). This emulator doesn't implement the old strict chaining — it just generates and returns a fresh token on every call. Don't "fix" this into strict validation without checking that real AWS still doesn't enforce it.
- Secrets Manager `PutSecretValue` mirrors real version-staging semantics: the new version becomes `AWSCURRENT` and whatever previously held that stage gets demoted to `AWSPREVIOUS` (see `putSecretValue` in `internal/services/secretsmanager/secretsmanager.go`). Any future staging-related feature (e.g. `UpdateSecretVersionStage`) should reuse this same stage-list rewrite pattern rather than introducing a separate one.
- SSM `SecureString` and Secrets Manager values are stored as plain text in BoltDB — no KMS, no real encryption. This is intentional for a dev emulator, but it means this project's BoltDB file should never be treated as if it held actually-protected secrets.
