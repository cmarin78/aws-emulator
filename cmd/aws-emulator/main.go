// Command aws-emulator levanta el servidor HTTP del emulador local de AWS.
// A diferencia de azure-emulator/gcp-emulator (un host por servicio), AWS
// multiplexa todo sobre un único puerto — ver internal/router para el
// porqué — así que este main.go registra un solo http.Server con todos
// los servicios habilitados detrás del dispatcher de internal/server.
package main

import (
	"flag"
	"log"
	"net/http"

	"github.com/cesarmarin/aws-emulator/internal/router"
	"github.com/cesarmarin/aws-emulator/internal/server"
	"github.com/cesarmarin/aws-emulator/internal/services/apigateway"
	"github.com/cesarmarin/aws-emulator/internal/services/dynamodb"
	"github.com/cesarmarin/aws-emulator/internal/services/events"
	"github.com/cesarmarin/aws-emulator/internal/services/iam"
	"github.com/cesarmarin/aws-emulator/internal/services/kms"
	"github.com/cesarmarin/aws-emulator/internal/services/lambda"
	"github.com/cesarmarin/aws-emulator/internal/services/logs"
	"github.com/cesarmarin/aws-emulator/internal/services/s3"
	"github.com/cesarmarin/aws-emulator/internal/services/secretsmanager"
	"github.com/cesarmarin/aws-emulator/internal/services/sns"
	"github.com/cesarmarin/aws-emulator/internal/services/sqs"
	"github.com/cesarmarin/aws-emulator/internal/services/ssm"
	"github.com/cesarmarin/aws-emulator/internal/services/sts"
	"github.com/cesarmarin/aws-emulator/internal/storage"
)

func main() {
	addr := flag.String("addr", ":4566", "dirección donde escucha el emulador (un solo puerto para todos los servicios, como LocalStack)")
	dbPath := flag.String("db", ".aws-emulator-data/state.db", "ruta del archivo de persistencia BoltDB")
	flag.Parse()

	db, err := storage.Open(*dbPath)
	if err != nil {
		log.Fatalf("no se pudo abrir la base de datos: %v", err)
	}
	defer db.Close()

	srv := server.New(detectWithAdmin)
	srv.Register("s3", s3.New(db))
	sqsSvc := sqs.New(db)
	srv.Register("sqs", sqsSvc)
	srv.Register("iam", iam.New(db))
	srv.Register("sts", sts.New())
	srv.Register("dynamodb", dynamodb.New(db))

	// SNS y EventBridge necesitan una referencia al *sqs.Service ya
	// construido para poder entregar mensajes a colas suscriptas/target
	// (Publish y PutEvents respectivamente) reusando sqs.DeliverMessage en
	// vez de escribir directamente en los buckets internos de SQS. Ver
	// ROADMAP.md ("Fase 3 — Messaging and eventing").
	snsSvc := sns.New(db, sqsSvc)
	srv.Register("sns", snsSvc)
	srv.Register("events", events.New(db, sqsSvc, snsSvc))
	lambdaSvc := lambda.New(db)
	srv.Register("lambda", lambdaSvc)

	srv.Register("logs", logs.New(db))
	srv.Register("secretsmanager", secretsmanager.New(db))
	srv.Register("ssm", ssm.New(db))

	srv.Register("kms", kms.New())
	// API Gateway necesita una referencia al *lambda.Service ya construido
	// para proxyar invocaciones AWS_PROXY en proceso — mismo patrón que
	// sns/events con *sqs.Service. Ver internal/services/apigateway.
	srv.Register("apigateway", apigateway.New(db, lambdaSvc))

	admin := server.NewAdmin(srv.Reset) // reset de estado real (Fase 2), ver ROADMAP.md
	srv.Register("_admin", admin)

	log.Printf("aws-emulator escuchando en %s (db: %s)", *addr, *dbPath)
	log.Printf("servicios habilitados: s3, sqs, iam, sts, dynamodb, sns, events, lambda, logs, secretsmanager, ssm, kms, apigateway")
	if err := http.ListenAndServe(*addr, srv.Handler()); err != nil {
		log.Fatalf("error del servidor: %v", err)
	}
}

// detectWithAdmin envuelve router.DetectService para que las rutas
// administrativas (/_aws-emulator/health, /_aws-emulator/reset) se sirvan
// sin pasar por la heurística de detección de servicios AWS.
func detectWithAdmin(r *http.Request) string {
	if server.IsAdminPath(r.URL.Path) {
		return "_admin"
	}
	return router.DetectService(router.FromHTTPRequest(r))
}
