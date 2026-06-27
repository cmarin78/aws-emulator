// Package sts emula el subconjunto mínimo de AWS STS necesario para que
// los SDKs hagan su chequeo inicial de credenciales: GetCallerIdentity.
// AssumeRole queda fuera de Fase 1 (no hay modelo de roles/trust-policy
// todavía — eso vive en internal/services/iam, ver ROADMAP.md).
package sts

import (
	"encoding/xml"
	"net/http"

	"github.com/cesarmarin/aws-emulator/internal/accountctx"
	"github.com/cesarmarin/aws-emulator/internal/router"
	"github.com/cesarmarin/aws-emulator/internal/server"
)

// Service no necesita estado persistente: STS en este emulador es
// puramente informativo (no hay modelo de cuentas/usuarios real detrás).
type Service struct{}

// New crea el servicio STS.
func New() *Service { return &Service{} }

type getCallerIdentityResponse struct {
	XMLName xml.Name                `xml:"https://sts.amazonaws.com/doc/2011-06-15/ GetCallerIdentityResponse"`
	Result  getCallerIdentityResult `xml:"GetCallerIdentityResult"`
}

type getCallerIdentityResult struct {
	UserId  string `xml:"UserId"`
	Account string `xml:"Account"`
	Arn     string `xml:"Arn"`
}

func (s *Service) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	req := router.FromHTTPRequest(r)
	action := req.Action
	if action == "" {
		action = r.URL.Query().Get("Action")
	}

	switch action {
	case "GetCallerIdentity", "":
		accessKey := router.AccessKeyIDFromAuthorization(req.Authorization)
		if accessKey == "" {
			accessKey = "AKIAEMULATOR"
		}
		// El account ID ahora se deriva por credencial (mismo access key ->
		// mismo account ID, distinto access key -> distinto account ID), ver
		// internal/accountctx -- antes era un único "tenant" fijo.
		accountID, _ := accountctx.FromContext(r.Context())
		server.WriteXML(w, http.StatusOK, getCallerIdentityResponse{
			Result: getCallerIdentityResult{
				UserId:  accessKey,
				Account: accountID,
				Arn:     "arn:aws:iam::" + accountID + ":user/aws-emulator",
			},
		})
	default:
		server.WriteXMLError(w, http.StatusBadRequest, "InvalidAction",
			"acción STS no soportada en este emulador: "+action)
	}
}
