// Package server arma el http.Server principal del emulador. A diferencia
// de azure-emulator/gcp-emulator (un host lógico por servicio, rutas REST
// jerárquicas), la API de AWS multiplexa decenas de servicios sobre un
// único endpoint plano (un solo puerto, normalmente 4566) y distingue el
// servicio destino por encabezados/Action/host — ver internal/router. Por
// eso este paquete no registra rutas por patrón de path: registra un único
// handler raíz que delega en el router para decidir a qué Service llamar.
package server

import (
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/cesarmarin/aws-emulator/internal/accountctx"
)

// Service es la interfaz que implementa cada servicio emulado (s3, sqs,
// dynamodb, iam, sts, ...). ServeHTTP recibe la request ya identificada
// como propia de ese servicio por el router.
type Service interface {
	ServeHTTP(w http.ResponseWriter, r *http.Request)
}

// Resettable lo implementan los servicios con estado persistente que
// necesitan limpiarse en POST /_aws-emulator/reset (todos salvo sts, que
// no tiene estado). No es parte de Service porque no todos los servicios
// tienen algo que resetear.
type Resettable interface {
	Reset() error
}

// Server agrupa el dispatcher principal: un mapa servicio→Service más el
// fallback de salud/operaciones administrativas.
type Server struct {
	services map[string]Service
	detect   func(r *http.Request) string
}

// New crea un Server vacío. detect es la función de detección de servicio
// (normalmente router.DetectService), inyectada para que server no dependa
// del paquete router y evitar un ciclo de imports.
func New(detect func(r *http.Request) string) *Server {
	return &Server{
		services: make(map[string]Service),
		detect:   detect,
	}
}

// Register asocia un nombre de servicio (p. ej. "s3", "sqs") con su
// implementación. Llamar dos veces con el mismo nombre sobreescribe.
func (s *Server) Register(name string, svc Service) {
	s.services[name] = svc
}

// ServeHTTP implementa http.Handler: detecta el servicio destino y le
// delega la request. Si no hay match, devuelve 404 con forma de error AWS.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	name := s.detect(r)
	svc, ok := s.services[name]
	if !ok {
		WriteXMLError(w, http.StatusNotFound, "UnknownOperationException",
			"el emulador no reconoce el servicio destino de esta solicitud (revisar X-Amz-Target / Authorization / Action / host)")
		return
	}
	svc.ServeHTTP(w, r)
}

// Reset limpia el estado de todos los servicios registrados que
// implementan Resettable. Pensado para usarse directamente como el
// callback de server.NewAdmin desde main.go (server.NewAdmin(srv.Reset)).
func (s *Server) Reset() error {
	for name, svc := range s.services {
		r, ok := svc.(Resettable)
		if !ok {
			continue
		}
		if err := r.Reset(); err != nil {
			return fmt.Errorf("server: error reseteando servicio %q: %w", name, err)
		}
	}
	return nil
}

// Handler envuelve el Server con logging y recuperación de panics. CORS
// está siempre habilitado porque los SDKs de AWS desde un navegador
// (Amplify, etc.) necesitan poder apuntar aquí sin parches.
//
// accountctx.Middleware va después de withRecover (para que un panic en la
// derivación de identidad -- no debería pasar, pero por las dudas -- caiga
// en el recover) y antes del dispatcher, así todo Service puede leer el
// account ID/región resueltos vía accountctx.FromContext(r.Context()).
func (s *Server) Handler() http.Handler {
	return withCORS(withLogging(withRecover(accountctx.Middleware(s))))
}

func withRecover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				log.Printf("panic atendiendo %s %s: %v", r.Method, r.URL.Path, rec)
				WriteXMLError(w, http.StatusInternalServerError, "InternalError",
					"el emulador encontró un error interno procesando la solicitud")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func withLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		log.Printf("%s %s %s", r.Method, r.URL.Path, time.Since(start))
	})
}

func withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET,POST,PUT,PATCH,DELETE,HEAD,OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "*")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}
