package secretsmanager

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

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
	r.Header.Set("X-Amz-Target", "secretsmanager."+action)
	r.Header.Set("Content-Type", "application/x-amz-json-1.1")
	return r
}

func doSecrets(svc *Service, action string, body map[string]any) (*httptest.ResponseRecorder, map[string]any) {
	w := httptest.NewRecorder()
	svc.ServeHTTP(w, jsonRequest(action, body))
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w, out
}

func TestCreateSecret_AndGetSecretValue(t *testing.T) {
	svc := newTestService(t)
	w, out := doSecrets(svc, "CreateSecret", map[string]any{"Name": "s1", "SecretString": "topsecret"})
	if w.Code != http.StatusOK {
		t.Fatalf("CreateSecret: status = %d, body = %s", w.Code, w.Body.String())
	}
	arn, _ := out["ARN"].(string)
	if arn == "" {
		t.Fatalf("CreateSecret: esperaba ARN no vacío")
	}

	w, out = doSecrets(svc, "GetSecretValue", map[string]any{"SecretId": "s1"})
	if w.Code != http.StatusOK {
		t.Fatalf("GetSecretValue: status = %d, body = %s", w.Code, w.Body.String())
	}
	if out["SecretString"] != "topsecret" {
		t.Fatalf("GetSecretValue SecretString = %v, esperaba topsecret", out["SecretString"])
	}
}

func TestCreateSecret_RequiresName(t *testing.T) {
	svc := newTestService(t)
	w, _ := doSecrets(svc, "CreateSecret", map[string]any{"SecretString": "x"})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("CreateSecret sin Name: status = %d, esperaba 400", w.Code)
	}
}

func TestCreateSecret_AlreadyExists(t *testing.T) {
	svc := newTestService(t)
	doSecrets(svc, "CreateSecret", map[string]any{"Name": "s1", "SecretString": "x"})
	w, _ := doSecrets(svc, "CreateSecret", map[string]any{"Name": "s1", "SecretString": "y"})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("CreateSecret duplicado: status = %d, esperaba 400", w.Code)
	}
}

// TestGetSecretValue_ByARN cubre findSecret resolviendo por ARN completo
// (con el sufijo random), no solo por nombre simple — el bug real
// documentado en el código que rompía el waiter de Terraform.
func TestGetSecretValue_ByARN(t *testing.T) {
	svc := newTestService(t)
	_, out := doSecrets(svc, "CreateSecret", map[string]any{"Name": "s1", "SecretString": "x"})
	arn, _ := out["ARN"].(string)

	w, out := doSecrets(svc, "GetSecretValue", map[string]any{"SecretId": arn})
	if w.Code != http.StatusOK {
		t.Fatalf("GetSecretValue por ARN: status = %d, body = %s", w.Code, w.Body.String())
	}
	if out["SecretString"] != "x" {
		t.Fatalf("GetSecretValue por ARN: SecretString = %v, esperaba x", out["SecretString"])
	}
}

func TestGetSecretValue_NotFound(t *testing.T) {
	svc := newTestService(t)
	w, _ := doSecrets(svc, "GetSecretValue", map[string]any{"SecretId": "nope"})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("GetSecretValue inexistente: status = %d, esperaba 400", w.Code)
	}
}

// TestPutSecretValue_CreatesNewVersionAndDemotesPrevious cubre la
// semántica de versionado: la versión nueva pasa a AWSCURRENT y la
// anterior pasa a AWSPREVIOUS.
func TestPutSecretValue_CreatesNewVersionAndDemotesPrevious(t *testing.T) {
	svc := newTestService(t)
	_, out := doSecrets(svc, "CreateSecret", map[string]any{"Name": "s1", "SecretString": "v1"})
	v1ID, _ := out["VersionId"].(string)

	w, out := doSecrets(svc, "PutSecretValue", map[string]any{"SecretId": "s1", "SecretString": "v2"})
	if w.Code != http.StatusOK {
		t.Fatalf("PutSecretValue: status = %d, body = %s", w.Code, w.Body.String())
	}
	v2ID, _ := out["VersionId"].(string)
	if v2ID == v1ID {
		t.Fatalf("PutSecretValue: esperaba un VersionId distinto al original")
	}

	w, out = doSecrets(svc, "GetSecretValue", map[string]any{"SecretId": "s1"})
	if out["SecretString"] != "v2" {
		t.Fatalf("GetSecretValue tras PutSecretValue: esperaba el valor más nuevo, got %v", out["SecretString"])
	}

	w, out = doSecrets(svc, "GetSecretValue", map[string]any{"SecretId": "s1", "VersionStage": "AWSPREVIOUS"})
	if w.Code != http.StatusOK || out["SecretString"] != "v1" {
		t.Fatalf("GetSecretValue con VersionStage=AWSPREVIOUS: esperaba v1, got status=%d, body=%v", w.Code, out)
	}
}

func TestPutSecretValue_NotFound(t *testing.T) {
	svc := newTestService(t)
	w, _ := doSecrets(svc, "PutSecretValue", map[string]any{"SecretId": "nope", "SecretString": "x"})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("PutSecretValue sobre secreto inexistente: status = %d, esperaba 400", w.Code)
	}
}

// TestDeleteSecret_SoftDeleteThenForceDelete cubre ambos modos: soft
// delete (default, marca DeletedDate pero sigue en BoltDB) y
// ForceDeleteWithoutRecovery (lo borra definitivamente).
func TestDeleteSecret_SoftDeleteThenForceDelete(t *testing.T) {
	svc := newTestService(t)
	doSecrets(svc, "CreateSecret", map[string]any{"Name": "s1", "SecretString": "x"})

	w, _ := doSecrets(svc, "DeleteSecret", map[string]any{"SecretId": "s1"})
	if w.Code != http.StatusOK {
		t.Fatalf("DeleteSecret (soft): status = %d, body = %s", w.Code, w.Body.String())
	}

	w, _ = doSecrets(svc, "GetSecretValue", map[string]any{"SecretId": "s1"})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("GetSecretValue tras soft delete: status = %d, esperaba 400 (secreto pendiente de borrado)", w.Code)
	}

	w, out := doSecrets(svc, "DescribeSecret", map[string]any{"SecretId": "s1"})
	if w.Code != http.StatusOK || out["DeletedDate"] == nil {
		t.Fatalf("DescribeSecret tras soft delete: esperaba DeletedDate seteado, body = %v", out)
	}

	w, _ = doSecrets(svc, "DeleteSecret", map[string]any{"SecretId": "s1", "ForceDeleteWithoutRecovery": true})
	if w.Code != http.StatusOK {
		t.Fatalf("DeleteSecret (force): status = %d, body = %s", w.Code, w.Body.String())
	}
	w, _ = doSecrets(svc, "DescribeSecret", map[string]any{"SecretId": "s1"})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("DescribeSecret tras force delete: status = %d, esperaba 400 (ya no existe)", w.Code)
	}
}

func TestDescribeSecret_NotFound(t *testing.T) {
	svc := newTestService(t)
	w, _ := doSecrets(svc, "DescribeSecret", map[string]any{"SecretId": "nope"})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("DescribeSecret inexistente: status = %d, esperaba 400", w.Code)
	}
}

func TestListSecrets_SortedByName(t *testing.T) {
	svc := newTestService(t)
	doSecrets(svc, "CreateSecret", map[string]any{"Name": "zzz", "SecretString": "x"})
	doSecrets(svc, "CreateSecret", map[string]any{"Name": "aaa", "SecretString": "x"})

	w, out := doSecrets(svc, "ListSecrets", map[string]any{})
	if w.Code != http.StatusOK {
		t.Fatalf("ListSecrets: status = %d, body = %s", w.Code, w.Body.String())
	}
	list, _ := out["SecretList"].([]any)
	if len(list) != 2 {
		t.Fatalf("ListSecrets: esperaba 2 secretos, body = %s", w.Body.String())
	}
	first, _ := list[0].(map[string]any)
	if first["Name"] != "aaa" {
		t.Fatalf("ListSecrets: esperaba orden alfabético, primero = %v", first["Name"])
	}
}

func TestGetResourcePolicy_RequiresExistingSecret(t *testing.T) {
	svc := newTestService(t)
	doSecrets(svc, "CreateSecret", map[string]any{"Name": "s1", "SecretString": "x"})

	w, out := doSecrets(svc, "GetResourcePolicy", map[string]any{"SecretId": "s1"})
	if w.Code != http.StatusOK || out["Name"] != "s1" {
		t.Fatalf("GetResourcePolicy: status = %d, body = %v", w.Code, out)
	}

	w, _ = doSecrets(svc, "GetResourcePolicy", map[string]any{"SecretId": "nope"})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("GetResourcePolicy inexistente: status = %d, esperaba 400", w.Code)
	}
}

func TestServeHTTP_UnknownActionFails(t *testing.T) {
	svc := newTestService(t)
	w, _ := doSecrets(svc, "RotateSecret", map[string]any{})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("acción no soportada: status = %d, esperaba 400", w.Code)
	}
}

func TestReset_ClearsSecrets(t *testing.T) {
	svc := newTestService(t)
	doSecrets(svc, "CreateSecret", map[string]any{"Name": "s1", "SecretString": "x"})

	if err := svc.Reset(); err != nil {
		t.Fatalf("Reset: %v", err)
	}

	_, out := doSecrets(svc, "ListSecrets", map[string]any{})
	list, _ := out["SecretList"].([]any)
	if len(list) != 0 {
		t.Fatalf("ListSecrets tras Reset: esperaba vacío")
	}
}
