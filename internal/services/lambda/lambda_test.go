package lambda

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
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

func doLambda(svc *Service, method, path string, body map[string]any) (*httptest.ResponseRecorder, map[string]any) {
	var reader *bytes.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		reader = bytes.NewReader(raw)
	} else {
		reader = bytes.NewReader(nil)
	}
	r := httptest.NewRequest(method, path, reader)
	w := httptest.NewRecorder()
	svc.ServeHTTP(w, r)
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w, out
}

func createTestFunction(t *testing.T, svc *Service, name string, env map[string]string) {
	t.Helper()
	body := map[string]any{
		"FunctionName": name,
		"Runtime":      "go1.x",
		"Role":         "arn:aws:iam::000000000000:role/lambda-role",
		"Handler":      "main",
		"Code":         map[string]any{"ZipFile": base64.StdEncoding.EncodeToString([]byte("fake-zip-contents"))},
		"Environment":  map[string]any{"Variables": env},
	}
	w, _ := doLambda(svc, http.MethodPost, "/2015-03-31/functions", body)
	if w.Code != http.StatusCreated {
		t.Fatalf("CreateFunction(%s): status = %d, body = %s", name, w.Code, w.Body.String())
	}
}

func TestCreateFunction_AndGetFunction(t *testing.T) {
	svc := newTestService(t)
	createTestFunction(t, svc, "fn1", nil)

	w, out := doLambda(svc, http.MethodGet, "/2015-03-31/functions/fn1", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("GetFunction: status = %d, body = %s", w.Code, w.Body.String())
	}
	cfg, _ := out["Configuration"].(map[string]any)
	if cfg["FunctionName"] != "fn1" {
		t.Fatalf("GetFunction Configuration.FunctionName = %v, esperaba fn1", cfg["FunctionName"])
	}
	arn, _ := cfg["FunctionArn"].(string)
	wantArn := functionArn(accountctx.DefaultAccountID, "fn1")
	if arn != wantArn {
		t.Fatalf("GetFunction FunctionArn = %q, esperaba %q", arn, wantArn)
	}
	if cfg["CodeSize"].(float64) == 0 {
		t.Fatalf("GetFunction CodeSize = 0, esperaba > 0 dado que se mandó código")
	}
}

func TestCreateFunction_RequiresFunctionName(t *testing.T) {
	svc := newTestService(t)
	w, _ := doLambda(svc, http.MethodPost, "/2015-03-31/functions", map[string]any{"Runtime": "go1.x"})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("CreateFunction sin FunctionName: status = %d, esperaba 400", w.Code)
	}
}

func TestCreateFunction_InvalidZipFileFails(t *testing.T) {
	svc := newTestService(t)
	body := map[string]any{
		"FunctionName": "fn1",
		"Code":         map[string]any{"ZipFile": "no-es-base64-valido-!!!"},
	}
	w, _ := doLambda(svc, http.MethodPost, "/2015-03-31/functions", body)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("CreateFunction con ZipFile inválido: status = %d, esperaba 400", w.Code)
	}
}

func TestGetFunction_NotFound(t *testing.T) {
	svc := newTestService(t)
	w, _ := doLambda(svc, http.MethodGet, "/2015-03-31/functions/nope", nil)
	if w.Code != http.StatusNotFound {
		t.Fatalf("GetFunction inexistente: status = %d, esperaba 404", w.Code)
	}
}

func TestListFunctions_ReturnsAllFunctions(t *testing.T) {
	svc := newTestService(t)
	createTestFunction(t, svc, "fn1", nil)
	createTestFunction(t, svc, "fn2", nil)

	w, out := doLambda(svc, http.MethodGet, "/2015-03-31/functions", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("ListFunctions: status = %d, body = %s", w.Code, w.Body.String())
	}
	fns, _ := out["Functions"].([]any)
	if len(fns) != 2 {
		t.Fatalf("ListFunctions: esperaba 2 funciones, body = %s", w.Body.String())
	}
}

func TestListFunctions_AcceptsTrailingSlash(t *testing.T) {
	svc := newTestService(t)
	createTestFunction(t, svc, "fn1", nil)

	w, out := doLambda(svc, http.MethodGet, "/2015-03-31/functions/", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("ListFunctions con barra final: status = %d, body = %s", w.Code, w.Body.String())
	}
	fns, _ := out["Functions"].([]any)
	if len(fns) != 1 {
		t.Fatalf("ListFunctions con barra final: esperaba 1 función, body = %s", w.Body.String())
	}
}

func TestDeleteFunction(t *testing.T) {
	svc := newTestService(t)
	createTestFunction(t, svc, "fn1", nil)

	w, _ := doLambda(svc, http.MethodDelete, "/2015-03-31/functions/fn1", nil)
	if w.Code != http.StatusNoContent {
		t.Fatalf("DeleteFunction: status = %d, esperaba 204", w.Code)
	}

	w, _ = doLambda(svc, http.MethodGet, "/2015-03-31/functions/fn1", nil)
	if w.Code != http.StatusNotFound {
		t.Fatalf("GetFunction tras DeleteFunction: status = %d, esperaba 404", w.Code)
	}
}

func TestListVersionsByFunction_AlwaysLatest(t *testing.T) {
	svc := newTestService(t)
	createTestFunction(t, svc, "fn1", nil)

	w, out := doLambda(svc, http.MethodGet, "/2015-03-31/functions/fn1/versions", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("ListVersionsByFunction: status = %d, body = %s", w.Code, w.Body.String())
	}
	versions, _ := out["Versions"].([]any)
	if len(versions) != 1 {
		t.Fatalf("ListVersionsByFunction: esperaba 1 versión, body = %s", w.Body.String())
	}
	v, _ := versions[0].(map[string]any)
	if v["Version"] != "$LATEST" {
		t.Fatalf("ListVersionsByFunction Version = %v, esperaba $LATEST", v["Version"])
	}
}

func TestListVersionsByFunction_NotFound(t *testing.T) {
	svc := newTestService(t)
	w, _ := doLambda(svc, http.MethodGet, "/2015-03-31/functions/nope/versions", nil)
	if w.Code != http.StatusNotFound {
		t.Fatalf("ListVersionsByFunction inexistente: status = %d, esperaba 404", w.Code)
	}
}

// TestInvoke_StubEchoesPayload cubre el modo "in-process" sin
// EMULATOR_INVOKE_COMMAND: Invoke debe simplemente hacer eco del payload.
func TestInvoke_StubEchoesPayload(t *testing.T) {
	svc := newTestService(t)
	createTestFunction(t, svc, "fn1", nil)

	r := httptest.NewRequest(http.MethodPost, "/2015-03-31/functions/fn1/invocations", bytes.NewReader([]byte(`{"hello":"world"}`)))
	w := httptest.NewRecorder()
	svc.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("Invoke: status = %d, body = %s", w.Code, w.Body.String())
	}
	if w.Body.String() != `{"hello":"world"}` {
		t.Fatalf("Invoke stub: body = %q, esperaba eco del payload", w.Body.String())
	}
}

func TestInvoke_NotFound(t *testing.T) {
	svc := newTestService(t)
	r := httptest.NewRequest(http.MethodPost, "/2015-03-31/functions/nope/invocations", bytes.NewReader([]byte(`{}`)))
	w := httptest.NewRecorder()
	svc.ServeHTTP(w, r)
	if w.Code != http.StatusNotFound {
		t.Fatalf("Invoke función inexistente: status = %d, esperaba 404", w.Code)
	}
}

// TestInvoke_RunsConfiguredSubprocessCommand cubre el modo "via a
// subprocess": si la función fue creada con la variable de entorno
// EMULATOR_INVOKE_COMMAND, Invoke ejecuta ese comando, le pasa el payload
// por stdin, y devuelve su stdout como respuesta.
func TestInvoke_RunsConfiguredSubprocessCommand(t *testing.T) {
	svc := newTestService(t)
	createTestFunction(t, svc, "fn1", map[string]string{"EMULATOR_INVOKE_COMMAND": "cat"})

	r := httptest.NewRequest(http.MethodPost, "/2015-03-31/functions/fn1/invocations", bytes.NewReader([]byte(`{"echo":"via-subprocess"}`)))
	w := httptest.NewRecorder()
	svc.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("Invoke vía subproceso: status = %d, body = %s", w.Code, w.Body.String())
	}
	if w.Body.String() != `{"echo":"via-subprocess"}` {
		t.Fatalf("Invoke vía subproceso: body = %q, esperaba el stdout de `cat`", w.Body.String())
	}
}

// TestInvoke_SubprocessFailureSetsFunctionErrorHeader cubre el camino de
// error: un comando que falla debe devolver 200 con
// X-Amz-Function-Error: Unhandled y un cuerpo describiendo el error,
// replicando el comportamiento real de Lambda Invoke ante un error del
// handler.
func TestInvoke_SubprocessFailureSetsFunctionErrorHeader(t *testing.T) {
	svc := newTestService(t)
	createTestFunction(t, svc, "fn1", map[string]string{"EMULATOR_INVOKE_COMMAND": "sh -c 'exit 1'"})

	r := httptest.NewRequest(http.MethodPost, "/2015-03-31/functions/fn1/invocations", bytes.NewReader([]byte(`{}`)))
	w := httptest.NewRecorder()
	svc.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("Invoke con subproceso que falla: status = %d, esperaba 200 con X-Amz-Function-Error", w.Code)
	}
	if w.Header().Get("X-Amz-Function-Error") != "Unhandled" {
		t.Fatalf("Invoke con subproceso que falla: esperaba header X-Amz-Function-Error=Unhandled")
	}
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	if out["errorType"] != "SubprocessError" {
		t.Fatalf("Invoke con subproceso que falla: errorType = %v, esperaba SubprocessError", out["errorType"])
	}
}

func TestReset_ClearsFunctions(t *testing.T) {
	svc := newTestService(t)
	createTestFunction(t, svc, "fn1", nil)

	if err := svc.Reset(); err != nil {
		t.Fatalf("Reset: %v", err)
	}

	_, out := doLambda(svc, http.MethodGet, "/2015-03-31/functions", nil)
	fns, _ := out["Functions"].([]any)
	if len(fns) != 0 {
		t.Fatalf("ListFunctions tras Reset: esperaba vacío")
	}
}

func TestServeHTTP_UnsupportedRouteFails(t *testing.T) {
	svc := newTestService(t)
	w, _ := doLambda(svc, http.MethodPut, "/2015-03-31/functions/fn1/concurrency", nil)
	if w.Code != http.StatusNotFound {
		t.Fatalf("ruta no soportada: status = %d, esperaba 404", w.Code)
	}
}
