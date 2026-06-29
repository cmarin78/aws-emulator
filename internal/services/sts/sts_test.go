package sts

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cesarmarin/aws-emulator/internal/accountctx"
)

func TestGetCallerIdentity_DefaultsWithoutAuthorization(t *testing.T) {
	svc := New()
	r := httptest.NewRequest(http.MethodPost, "/?Action=GetCallerIdentity", nil)
	w := httptest.NewRecorder()
	svc.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("GetCallerIdentity: status = %d, body = %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "<Account>"+accountctx.DefaultAccountID+"</Account>") {
		t.Fatalf("GetCallerIdentity sin Authorization: esperaba el account ID default, body = %s", body)
	}
	if !strings.Contains(body, "<UserId>AKIAEMULATOR</UserId>") {
		t.Fatalf("GetCallerIdentity sin Authorization: esperaba UserId AKIAEMULATOR, body = %s", body)
	}
}

// TestGetCallerIdentity_DerivesAccountFromAuthorization pasa la request
// por accountctx.Middleware (el mismo middleware que server.go encadena
// delante de todos los servicios) para que el account ID quede resuelto
// en el contexto antes de llegar a sts.Service, igual que en producción.
func TestGetCallerIdentity_DerivesAccountFromAuthorization(t *testing.T) {
	svc := New()
	handler := accountctx.Middleware(svc)

	r := httptest.NewRequest(http.MethodPost, "/?Action=GetCallerIdentity", nil)
	r.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential=AKIAEXAMPLE1/20260627/us-east-1/sts/aws4_request, SignedHeaders=host, Signature=deadbeef")

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)

	want := accountctx.DeriveAccountID("AKIAEXAMPLE1")
	body := w.Body.String()
	if !strings.Contains(body, "<Account>"+want+"</Account>") {
		t.Fatalf("GetCallerIdentity: esperaba Account=%s derivado del access key, body = %s", want, body)
	}
	if !strings.Contains(body, "<UserId>AKIAEXAMPLE1</UserId>") {
		t.Fatalf("GetCallerIdentity: esperaba UserId=AKIAEXAMPLE1 (extraído de Authorization), body = %s", body)
	}
	if !strings.Contains(body, "arn:aws:iam::"+want+":user/aws-emulator") {
		t.Fatalf("GetCallerIdentity: esperaba Arn con el account derivado, body = %s", body)
	}
}

func TestServeHTTP_UnknownActionFails(t *testing.T) {
	svc := New()
	r := httptest.NewRequest(http.MethodPost, "/?Action=AssumeRole", nil)
	w := httptest.NewRecorder()
	svc.ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("acción desconocida: status = %d, esperaba 400", w.Code)
	}
}
