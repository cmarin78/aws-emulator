package accountctx

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestDeriveAccountID_Deterministic(t *testing.T) {
	a := DeriveAccountID("AKIAEXAMPLE1")
	b := DeriveAccountID("AKIAEXAMPLE1")
	if a != b {
		t.Fatalf("el mismo access key debería derivar siempre el mismo account ID: %q != %q", a, b)
	}
	if len(a) != 12 {
		t.Fatalf("DeriveAccountID = %q, esperaba 12 dígitos", a)
	}
}

func TestDeriveAccountID_DifferentKeysDifferentAccounts(t *testing.T) {
	a := DeriveAccountID("AKIAEXAMPLE1")
	b := DeriveAccountID("AKIAEXAMPLE2")
	if a == b {
		t.Fatalf("access keys distintos derivaron el mismo account ID (%q) -- colisión inesperada en este test", a)
	}
}

func TestDeriveAccountID_EmptyKeyIsDefault(t *testing.T) {
	if got := DeriveAccountID(""); got != DefaultAccountID {
		t.Fatalf("DeriveAccountID(\"\") = %q, esperaba DefaultAccountID %q", got, DefaultAccountID)
	}
}

func TestFromAuthorization_ParsesCredentialScope(t *testing.T) {
	auth := "AWS4-HMAC-SHA256 Credential=AKIAEXAMPLE1/20260627/us-west-2/s3/aws4_request, SignedHeaders=host, Signature=deadbeef"
	accountID, region := FromAuthorization(auth)

	if region != "us-west-2" {
		t.Fatalf("region = %q, esperaba us-west-2", region)
	}
	if want := DeriveAccountID("AKIAEXAMPLE1"); accountID != want {
		t.Fatalf("accountID = %q, esperaba %q (derivado del access key del header)", accountID, want)
	}
}

func TestFromAuthorization_EmptyHeaderReturnsDefaults(t *testing.T) {
	accountID, region := FromAuthorization("")
	if accountID != DefaultAccountID || region != DefaultRegion {
		t.Fatalf("header vacío = (%q, %q), esperaba defaults (%q, %q)", accountID, region, DefaultAccountID, DefaultRegion)
	}
}

func TestFromAuthorization_MalformedHeaderReturnsDefaults(t *testing.T) {
	accountID, region := FromAuthorization("Bearer not-a-sigv4-header")
	if accountID != DefaultAccountID || region != DefaultRegion {
		t.Fatalf("header malformado = (%q, %q), esperaba defaults (%q, %q)", accountID, region, DefaultAccountID, DefaultRegion)
	}
}

func TestFromContext_NoMiddlewareReturnsDefaults(t *testing.T) {
	accountID, region := FromContext(context.Background())
	if accountID != DefaultAccountID || region != DefaultRegion {
		t.Fatalf("contexto sin Middleware = (%q, %q), esperaba defaults", accountID, region)
	}
}

func TestMiddleware_PropagatesIdentityToContext(t *testing.T) {
	var gotAccountID, gotRegion string
	handler := Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAccountID, gotRegion = FromContext(r.Context())
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential=AKIAEXAMPLE1/20260627/eu-central-1/sts/aws4_request, SignedHeaders=host, Signature=deadbeef")
	handler.ServeHTTP(httptest.NewRecorder(), req)

	if want := DeriveAccountID("AKIAEXAMPLE1"); gotAccountID != want {
		t.Fatalf("accountID en contexto = %q, esperaba %q", gotAccountID, want)
	}
	if gotRegion != "eu-central-1" {
		t.Fatalf("region en contexto = %q, esperaba eu-central-1", gotRegion)
	}
}

func TestFromRequest_MatchesFromAuthorization(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential=AKIAEXAMPLE9/20260627/ap-south-1/iam/aws4_request, SignedHeaders=host, Signature=deadbeef")

	accountID, region := FromRequest(req)
	wantAccountID, wantRegion := FromAuthorization(req.Header.Get("Authorization"))
	if accountID != wantAccountID || region != wantRegion {
		t.Fatalf("FromRequest = (%q, %q), esperaba lo mismo que FromAuthorization (%q, %q)", accountID, region, wantAccountID, wantRegion)
	}
}
