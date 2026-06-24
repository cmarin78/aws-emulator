# aws-emulator

Emulador local de AWS en Go, hermano de [azure-emulator](../azure-emulator) y
[gcp-emulator](../gcp-emulator), pensado para desarrollo y tests de
integración sin depender de una cuenta real de AWS ni de Docker.

Reemplaza a [`ministack`](./ministack) (la versión en Python de este mismo
proyecto, que se conserva como referencia histórica y para comparar
cobertura de servicios) — ver la decisión de reescritura en
[ministack/CLEANUP.md](./ministack/CLEANUP.md).

## Por qué es distinto a azure-emulator/gcp-emulator

Azure y GCP enrutan por path jerárquico (un host lógico por servicio, rutas
REST anidadas). AWS multiplexa decenas de servicios sobre un **único
endpoint** — normalmente el puerto `4566`, igual que LocalStack — y
distingue el servicio destino por una combinación de señales: el header
`X-Amz-Target`, el credential scope del header `Authorization`, el parámetro
`Action`, el header `Host`, o el path. Ese enrutamiento vive en
[`internal/router`](./internal/router) y es la pieza más particular de este
proyecto frente a sus hermanos.

## Servicios implementados (Fase 1)

| Servicio | Protocolo | Operaciones |
|---|---|---|
| S3 | Query/XML | buckets y objetos: crear/listar/borrar bucket, put/get/head/delete objeto |
| SQS | Query/XML | colas y mensajes: create/list/get-url/delete/purge queue, send/receive/delete message |
| IAM | Query/XML | roles: create/get/list/delete role |
| STS | Query/XML | GetCallerIdentity |
| DynamoDB | JSON 1.0 | tablas e items: create/describe/delete table, put/get/delete item, scan (Query se trata como scan) |

El resto de los ~50 servicios de AWS (Lambda, API Gateway, EventBridge, etc.)
quedan para fases siguientes — ver [ROADMAP.md](./ROADMAP.md).

## Uso

```bash
go run ./cmd/aws-emulator -addr :4566 -db .aws-emulator-data/state.db
```

Apuntar el SDK/CLI de AWS al emulador (cualquier credencial sirve, no se
valida la firma):

```bash
export AWS_ACCESS_KEY_ID=test
export AWS_SECRET_ACCESS_KEY=test
export AWS_DEFAULT_REGION=us-east-1
aws --endpoint-url http://localhost:4566 s3 mb s3://mi-bucket
aws --endpoint-url http://localhost:4566 s3 ls
```

Endpoints administrativos:

- `GET /_aws-emulator/health` — chequeo de salud.
- `POST /_aws-emulator/reset` — no habilitado todavía (ver ROADMAP.md).

## Desarrollo

```bash
go build ./...
go vet ./...
go test ./... -v -race
```

## Persistencia

Estado embebido en BoltDB (`go.etcd.io/bbolt`), un único archivo
(`.aws-emulator-data/state.db` por defecto). Sin dependencias externas: no
hace falta Postgres ni Docker para correr el emulador.
