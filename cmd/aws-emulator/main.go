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
	"github.com/cesarmarin/aws-emulator/internal/services/dynamodb"
	"github.com/cesarmarin/aws-emulator/internal/services/iam"
	"github.com/cesarmarin/aws-emulator/internal/services/s3"
	"github.com/cesarmarin/aws-emulator/internal/services/sqs"
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
	srv.Register("sqs", sqs.New(db))
	srv.Register("iam", iam.New(db))
	srv.Register("sts", sts.New())
	srv.Register("dynamodb", dynamodb.New(db))

	admin := server.NewAdmin(nil) // reset de estado: pendiente para una fase posterior (ver ROADMAP.md)
	srv.Register("_admin", admin)

	log.Printf("aws-emulator escuchando en %s (db: %s)", *addr, *dbPath)
	log.Printf("servicios habilitados: s3, sqs, iam, sts, dynamodb")
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
