package ssm

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cesarmarin/aws-emulator/internal/accountctx"
	"github.com/cesarmarin/aws-emulator/internal/storage"
)

func newTestService(t *testing.T) *Service {
	t.Helper()
	db, err := storage.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return New(db)
}

func jsonRequest(action string, body map[string]any) *http.Request {
	raw, _ := json.Marshal(body)
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(string(raw)))
	r.Header.Set("X-Amz-Target", "AmazonSSM."+action)
	r.Header.Set("Content-Type", "application/x-amz-json-1.1")
	return r
}

func doSSM(svc *Service, action string, body map[string]any) (*httptest.ResponseRecorder, map[string]any) {
	w := httptest.NewRecorder()
	svc.ServeHTTP(w, jsonRequest(action, body))
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w, out
}

func TestPutParameter_AndGetParameter(t *testing.T) {
	svc := newTestService(t)
	w, out := doSSM(svc, "PutParameter", map[string]any{"Name": "/app/p1", "Value": "v1", "Type": "String"})
	if w.Code != http.StatusOK {
		t.Fatalf("PutParameter: status = %d, body = %s", w.Code, w.Body.String())
	}
	if out["Version"].(float64) != 1 {
		t.Fatalf("PutParameter Version = %v, esperaba 1", out["Version"])
	}

	w, out = doSSM(svc, "GetParameter", map[string]any{"Name": "/app/p1"})
	if w.Code != http.StatusOK {
		t.Fatalf("GetParameter: status = %d, body = %s", w.Code, w.Body.String())
	}
	p, _ := out["Parameter"].(map[string]any)
	if p["Value"] != "v1" {
		t.Fatalf("GetParameter Value = %v, esperaba v1", p["Value"])
	}
	arn, _ := p["ARN"].(string)
	if !strings.Contains(arn, accountctx.DefaultAccountID) {
		t.Fatalf("GetParameter ARN = %q, esperaba que contenga el account ID por default", arn)
	}
}

func TestPutParameter_RequiresFields(t *testing.T) {
	svc := newTestService(t)
	w, _ := doSSM(svc, "PutParameter", map[string]any{"Name": "/app/p1"})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("PutParameter sin Value/Type: status = %d, esperaba 400", w.Code)
	}
}

// TestPutParameter_RejectsDuplicateWithoutOverwrite cubre que, sin
// Overwrite=true, un segundo PutParameter sobre el mismo nombre falla.
func TestPutParameter_RejectsDuplicateWithoutOverwrite(t *testing.T) {
	svc := newTestService(t)
	doSSM(svc, "PutParameter", map[string]any{"Name": "/app/p1", "Value": "v1", "Type": "String"})

	w, _ := doSSM(svc, "PutParameter", map[string]any{"Name": "/app/p1", "Value": "v2", "Type": "String"})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("PutParameter duplicado sin Overwrite: status = %d, esperaba 400", w.Code)
	}
}

// TestPutParameter_OverwriteBumpsVersion cubre que Overwrite=true permite
// actualizar el valor y avanza la versión.
func TestPutParameter_OverwriteBumpsVersion(t *testing.T) {
	svc := newTestService(t)
	doSSM(svc, "PutParameter", map[string]any{"Name": "/app/p1", "Value": "v1", "Type": "String"})

	w, out := doSSM(svc, "PutParameter", map[string]any{"Name": "/app/p1", "Value": "v2", "Type": "String", "Overwrite": true})
	if w.Code != http.StatusOK {
		t.Fatalf("PutParameter con Overwrite: status = %d, body = %s", w.Code, w.Body.String())
	}
	if out["Version"].(float64) != 2 {
		t.Fatalf("PutParameter con Overwrite: Version = %v, esperaba 2", out["Version"])
	}

	_, out = doSSM(svc, "GetParameter", map[string]any{"Name": "/app/p1"})
	p, _ := out["Parameter"].(map[string]any)
	if p["Value"] != "v2" {
		t.Fatalf("GetParameter tras overwrite: Value = %v, esperaba v2", p["Value"])
	}
}

func TestGetParameter_NotFound(t *testing.T) {
	svc := newTestService(t)
	w, _ := doSSM(svc, "GetParameter", map[string]any{"Name": "/nope"})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("GetParameter inexistente: status = %d, esperaba 400", w.Code)
	}
}

func TestGetParameters_ReturnsFoundAndInvalid(t *testing.T) {
	svc := newTestService(t)
	doSSM(svc, "PutParameter", map[string]any{"Name": "/app/p1", "Value": "v1", "Type": "String"})

	w, out := doSSM(svc, "GetParameters", map[string]any{"Names": []any{"/app/p1", "/app/nope"}})
	if w.Code != http.StatusOK {
		t.Fatalf("GetParameters: status = %d, body = %s", w.Code, w.Body.String())
	}
	found, _ := out["Parameters"].([]any)
	invalid, _ := out["InvalidParameters"].([]any)
	if len(found) != 1 || len(invalid) != 1 {
		t.Fatalf("GetParameters: found=%d invalid=%d, esperaba 1 y 1, body = %s", len(found), len(invalid), w.Body.String())
	}
}

// TestGetParametersByPath_RecursiveVsNonRecursive cubre el filtrado por
// prefijo de path y el flag Recursive (excluye hijos anidados si es false).
func TestGetParametersByPath_RecursiveVsNonRecursive(t *testing.T) {
	svc := newTestService(t)
	doSSM(svc, "PutParameter", map[string]any{"Name": "/app/a", "Value": "1", "Type": "String"})
	doSSM(svc, "PutParameter", map[string]any{"Name": "/app/nested/b", "Value": "2", "Type": "String"})

	w, out := doSSM(svc, "GetParametersByPath", map[string]any{"Path": "/app", "Recursive": false})
	if w.Code != http.StatusOK {
		t.Fatalf("GetParametersByPath no-recursivo: status = %d, body = %s", w.Code, w.Body.String())
	}
	params, _ := out["Parameters"].([]any)
	if len(params) != 1 {
		t.Fatalf("GetParametersByPath no-recursivo: esperaba 1 parámetro, body = %s", w.Body.String())
	}

	w, out = doSSM(svc, "GetParametersByPath", map[string]any{"Path": "/app", "Recursive": true})
	if w.Code != http.StatusOK {
		t.Fatalf("GetParametersByPath recursivo: status = %d, body = %s", w.Code, w.Body.String())
	}
	params, _ = out["Parameters"].([]any)
	if len(params) != 2 {
		t.Fatalf("GetParametersByPath recursivo: esperaba 2 parámetros, body = %s", w.Body.String())
	}
}

func TestDeleteParameter(t *testing.T) {
	svc := newTestService(t)
	doSSM(svc, "PutParameter", map[string]any{"Name": "/app/p1", "Value": "v1", "Type": "String"})

	w, _ := doSSM(svc, "DeleteParameter", map[string]any{"Name": "/app/p1"})
	if w.Code != http.StatusOK {
		t.Fatalf("DeleteParameter: status = %d, body = %s", w.Code, w.Body.String())
	}

	w, _ = doSSM(svc, "GetParameter", map[string]any{"Name": "/app/p1"})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("GetParameter tras DeleteParameter: status = %d, esperaba 400", w.Code)
	}
}

func TestDeleteParameter_NotFound(t *testing.T) {
	svc := newTestService(t)
	w, _ := doSSM(svc, "DeleteParameter", map[string]any{"Name": "/nope"})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("DeleteParameter inexistente: status = %d, esperaba 400", w.Code)
	}
}

func TestDeleteParameters_ReturnsDeletedAndInvalid(t *testing.T) {
	svc := newTestService(t)
	doSSM(svc, "PutParameter", map[string]any{"Name": "/app/p1", "Value": "v1", "Type": "String"})

	w, out := doSSM(svc, "DeleteParameters", map[string]any{"Names": []any{"/app/p1", "/app/nope"}})
	if w.Code != http.StatusOK {
		t.Fatalf("DeleteParameters: status = %d, body = %s", w.Code, w.Body.String())
	}
	deleted, _ := out["DeletedParameters"].([]any)
	invalid, _ := out["InvalidParameters"].([]any)
	if len(deleted) != 1 || len(invalid) != 1 {
		t.Fatalf("DeleteParameters: deleted=%d invalid=%d, esperaba 1 y 1", len(deleted), len(invalid))
	}
}

func TestDescribeParameters_SortedByName(t *testing.T) {
	svc := newTestService(t)
	doSSM(svc, "PutParameter", map[string]any{"Name": "/zzz", "Value": "1", "Type": "String"})
	doSSM(svc, "PutParameter", map[string]any{"Name": "/aaa", "Value": "1", "Type": "String"})

	w, out := doSSM(svc, "DescribeParameters", map[string]any{})
	if w.Code != http.StatusOK {
		t.Fatalf("DescribeParameters: status = %d, body = %s", w.Code, w.Body.String())
	}
	params, _ := out["Parameters"].([]any)
	if len(params) != 2 {
		t.Fatalf("DescribeParameters: esperaba 2 parámetros, body = %s", w.Body.String())
	}
	first, _ := params[0].(map[string]any)
	if first["Name"] != "/aaa" {
		t.Fatalf("DescribeParameters: esperaba orden alfabético, primero = %v", first["Name"])
	}
}

// TestSecureString_RoundTripsValueRegardlessOfWithDecryption documenta el
// comportamiento explícito del emulador: no hay cifrado real, así que
// WithDecryption no cambia el valor devuelto.
func TestSecureString_RoundTripsValueRegardlessOfWithDecryption(t *testing.T) {
	svc := newTestService(t)
	doSSM(svc, "PutParameter", map[string]any{"Name": "/app/secret", "Value": "shh", "Type": "SecureString"})

	_, out := doSSM(svc, "GetParameter", map[string]any{"Name": "/app/secret", "WithDecryption": false})
	p, _ := out["Parameter"].(map[string]any)
	if p["Value"] != "shh" {
		t.Fatalf("GetParameter SecureString sin decrypt: Value = %v, esperaba shh", p["Value"])
	}

	_, out = doSSM(svc, "GetParameter", map[string]any{"Name": "/app/secret", "WithDecryption": true})
	p, _ = out["Parameter"].(map[string]any)
	if p["Value"] != "shh" {
		t.Fatalf("GetParameter SecureString con decrypt: Value = %v, esperaba shh", p["Value"])
	}
}

func TestListTagsForResource_AlwaysEmpty(t *testing.T) {
	svc := newTestService(t)
	w, out := doSSM(svc, "ListTagsForResource", map[string]any{})
	if w.Code != http.StatusOK {
		t.Fatalf("ListTagsForResource: status = %d", w.Code)
	}
	tags, _ := out["TagList"].([]any)
	if len(tags) != 0 {
		t.Fatalf("TagList = %v, esperaba vacío", tags)
	}
}

func TestServeHTTP_UnknownActionFails(t *testing.T) {
	svc := newTestService(t)
	w, _ := doSSM(svc, "PutParametersByPath", map[string]any{})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("acción no soportada: status = %d, esperaba 400", w.Code)
	}
}

func TestReset_ClearsParameters(t *testing.T) {
	svc := newTestService(t)
	doSSM(svc, "PutParameter", map[string]any{"Name": "/app/p1", "Value": "v1", "Type": "String"})

	if err := svc.Reset(); err != nil {
		t.Fatalf("Reset: %v", err)
	}

	_, out := doSSM(svc, "DescribeParameters", map[string]any{})
	params, _ := out["Parameters"].([]any)
	if len(params) != 0 {
		t.Fatalf("DescribeParameters tras Reset: esperaba vacío")
	}
}
