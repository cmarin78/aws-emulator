package server

import (
	"net/http"
	"strings"
)

// AdminService atiende los endpoints administrativos del emulador, bajo
// el prefijo /_aws-emulator/ — equivalente a /_ministack/ en ministack
// (health check y reset de estado para que los tests de integración
// puedan arrancar cada caso desde cero sin reiniciar el proceso).
type AdminService struct {
	reset func() error
}

// NewAdmin crea el servicio admin. reset puede ser nil si el caller no
// quiere exponer /_aws-emulator/reset.
func NewAdmin(reset func() error) *AdminService {
	return &AdminService{reset: reset}
}

func (a *AdminService) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/_aws-emulator/health":
		WriteJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	case r.URL.Path == "/_aws-emulator/reset" && r.Method == http.MethodPost:
		if a.reset == nil {
			WriteJSONError(w, http.StatusNotImplemented, "NotImplemented", "reset no está habilitado en este servidor")
			return
		}
		if err := a.reset(); err != nil {
			WriteJSONError(w, http.StatusInternalServerError, "InternalError", err.Error())
			return
		}
		WriteJSON(w, http.StatusOK, map[string]string{"status": "reset"})
	default:
		WriteJSONError(w, http.StatusNotFound, "NotFound", "endpoint administrativo desconocido")
	}
}

// IsAdminPath indica si un path corresponde al namespace administrativo,
// para que el router lo identifique antes de intentar detectar un
// servicio AWS real.
func IsAdminPath(path string) bool {
	return strings.HasPrefix(path, "/_aws-emulator/")
}
