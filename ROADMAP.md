# Roadmap

Migración incremental de [ministack](./ministack) (Python, ~56 servicios) a
Go, siguiendo la filosofía de azure-emulator/gcp-emulator: núcleo sólido
primero, servicios adicionales en fases sucesivas, sin tratar de portar todo
de una sola vez.

## Fase 1 — Núcleo + 5 servicios core (✅ completada)

- [x] Esqueleto del proyecto (`go.mod`, estructura de carpetas)
- [x] `internal/storage`: persistencia embebida BoltDB
- [x] `internal/server`: dispatcher HTTP único + middleware (CORS, logging, recover) + endpoints admin
- [x] `internal/router`: detección de servicio (X-Amz-Target / credential scope / Action / Host / path / fallback S3), portado de `ministack/core/router.py`
- [x] S3 (buckets + objetos)
- [x] SQS (colas + mensajes)
- [x] IAM (roles)
- [x] STS (GetCallerIdentity)
- [x] DynamoDB (tablas + items, protocolo JSON)
- [x] `cmd/aws-emulator/main.go`, CI, README, LICENSE

## Fase 2 — Endurecer el núcleo (próxima)

- [ ] `POST /_aws-emulator/reset` real (vaciar todos los buckets de BoltDB) — necesario para tests de integración que arrancan desde cero
- [ ] IAM: políticas inline (`PutRolePolicy`/`GetRolePolicy`/`DeleteRolePolicy`), usuarios, access keys
- [ ] SQS: atributos de cola (`GetQueueAttributes`/`SetQueueAttributes`), batch operations
- [ ] DynamoDB: índices secundarios (GSI/LSI) y `Query` real con condiciones de key (hoy se trata como `Scan`)
- [ ] S3: versionado, tags, multipart upload
- [ ] Tests de integración con el AWS CLI real contra el emulador (smoke test), siguiendo el patrón de `terraform/*-smoke-test` en azure-emulator/gcp-emulator

## Fase 3 — Servicios de mensajería y eventos

- [ ] SNS
- [ ] EventBridge
- [ ] Lambda (invocación local de funciones; ver qué tan lejos llega ministack hoy en `ministack/services/lambda_/`)

## Fase 4 — Resto de servicios de ministack

- [ ] Revisar `ministack/services/` y priorizar por uso real en los proyectos de Cesar (CloudWatch Logs, Secrets Manager, SSM Parameter Store son candidatos tempranos)
- [ ] Multi-tenancy real (hoy todo vive bajo un único `accountID = "000000000000"` hardcodeado en cada servicio — ver `defaultAccountID`/`accountID` en `internal/services/*`)

## Notas de diseño que no deberían perderse

- AWS no enruta por path como Azure/GCP — toda request entra por un único endpoint y el servicio se infiere (ver `internal/router`). Cualquier servicio nuevo necesita: (1) su patrón de detección en `router.go`, (2) su `Service` en `internal/services/<nombre>/`, (3) registrarlo en `main.go`.
- Go's `regexp` (RE2) no soporta lookahead/lookbehind, a diferencia de Python `re`. Al portar más patrones de `ministack/core/router.py` hay que revisar esto caso por caso (ya pasó una vez con un patrón de S3, ver el comentario en `router.go`).
- Dos protocolos de respuesta convivven: Query/XML (`server.WriteXML`/`WriteXMLError`) para S3/SQS/IAM/STS, JSON (`server.WriteJSON`/`WriteJSONError`) para DynamoDB. Un servicio nuevo debe identificar cuál de los dos protocolos usa la API real antes de implementarlo.
