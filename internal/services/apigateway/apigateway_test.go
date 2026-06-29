package apigateway

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/cesarmarin/aws-emulator/internal/services/lambda"
	"github.com/cesarmarin/aws-emulator/internal/storage"
)

func newTestService(t *testing.T) (*Service, *lambda.Service) {
	t.Helper()
	db, err := storage.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	lambdaSvc := lambda.New(db)
	return New(db, lambdaSvc), lambdaSvc
}

func doAPIGW(svc *Service, method, path string, body map[string]any) (*httptest.ResponseRecorder, map[string]any) {
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

// createTestFunction crea una función Lambda mínima directamente contra el
// servicio Lambda subyacente (sin pasar por HTTP), para usarla como target
// de integraciones AWS_PROXY.
func createTestFunction(t *testing.T, lambdaSvc *lambda.Service, name string) {
	t.Helper()
	body := map[string]any{
		"FunctionName": name,
		"Runtime":      "go1.x",
		"Role":         "arn:aws:iam::000000000000:role/lambda-role",
		"Handler":      "main",
		"Code":         map[string]any{"ZipFile": base64.StdEncoding.EncodeToString([]byte("fake-zip-contents"))},
	}
	raw, _ := json.Marshal(body)
	r := httptest.NewRequest(http.MethodPost, "/2015-03-31/functions", bytes.NewReader(raw))
	w := httptest.NewRecorder()
	lambdaSvc.ServeHTTP(w, r)
	if w.Code != http.StatusCreated {
		t.Fatalf("createTestFunction(%s): status = %d, body = %s", name, w.Code, w.Body.String())
	}
}

func lambdaURI(functionName string) string {
	return "arn:aws:apigateway:us-east-1:lambda:path/2015-03-31/functions/arn:aws:lambda:us-east-1:000000000000:function:" + functionName + "/invocations"
}

func TestCreateRestApi_AlsoCreatesRootResource(t *testing.T) {
	svc, _ := newTestService(t)
	w, out := doAPIGW(svc, http.MethodPost, "/restapis", map[string]any{"name": "api1"})
	if w.Code != http.StatusCreated {
		t.Fatalf("CreateRestApi: status = %d, body = %s", w.Code, w.Body.String())
	}
	apiID, _ := out["id"].(string)
	if apiID == "" {
		t.Fatalf("CreateRestApi: esperaba id no vacío")
	}

	w, out = doAPIGW(svc, http.MethodGet, "/restapis/"+apiID+"/resources", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("GetResources: status = %d, body = %s", w.Code, w.Body.String())
	}
	items, _ := out["item"].([]any)
	if len(items) != 1 {
		t.Fatalf("GetResources tras CreateRestApi: esperaba 1 recurso raíz, body = %s", w.Body.String())
	}
	root, _ := items[0].(map[string]any)
	if root["path"] != "/" {
		t.Fatalf("recurso raíz: path = %v, esperaba /", root["path"])
	}
}

func TestCreateRestApi_RequiresName(t *testing.T) {
	svc, _ := newTestService(t)
	w, _ := doAPIGW(svc, http.MethodPost, "/restapis", map[string]any{})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("CreateRestApi sin name: status = %d, esperaba 400", w.Code)
	}
}

func TestGetRestApis_UsesItemKey(t *testing.T) {
	svc, _ := newTestService(t)
	doAPIGW(svc, http.MethodPost, "/restapis", map[string]any{"name": "api1"})

	w, out := doAPIGW(svc, http.MethodGet, "/restapis", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("GetRestApis: status = %d, body = %s", w.Code, w.Body.String())
	}
	if _, ok := out["item"]; !ok {
		t.Fatalf("GetRestApis: esperaba key 'item' (no 'items'), body = %s", w.Body.String())
	}
	items, _ := out["item"].([]any)
	if len(items) != 1 {
		t.Fatalf("GetRestApis: esperaba 1 API, body = %s", w.Body.String())
	}
}

func TestGetRestApi_NotFound(t *testing.T) {
	svc, _ := newTestService(t)
	w, _ := doAPIGW(svc, http.MethodGet, "/restapis/nope", nil)
	if w.Code != http.StatusNotFound {
		t.Fatalf("GetRestApi inexistente: status = %d, esperaba 404", w.Code)
	}
}

// TestDeleteRestApi_CascadesEverything cubre que borrar la API limpia
// también resources/methods/integrations/deployments/stages asociados.
func TestDeleteRestApi_CascadesEverything(t *testing.T) {
	svc, lambdaSvc := newTestService(t)
	createTestFunction(t, lambdaSvc, "fn1")
	_, api := doAPIGW(svc, http.MethodPost, "/restapis", map[string]any{"name": "api1"})
	apiID := api["id"].(string)

	_, resources := doAPIGW(svc, http.MethodGet, "/restapis/"+apiID+"/resources", nil)
	rootID := resources["item"].([]any)[0].(map[string]any)["id"].(string)

	doAPIGW(svc, http.MethodPut, "/restapis/"+apiID+"/resources/"+rootID+"/methods/GET", map[string]any{})
	doAPIGW(svc, http.MethodPut, "/restapis/"+apiID+"/resources/"+rootID+"/methods/GET/integration", map[string]any{
		"type": "AWS_PROXY", "httpMethod": "POST", "uri": lambdaURI("fn1"),
	})
	doAPIGW(svc, http.MethodPost, "/restapis/"+apiID+"/deployments", map[string]any{"stageName": "prod"})

	w, _ := doAPIGW(svc, http.MethodDelete, "/restapis/"+apiID, nil)
	if w.Code != http.StatusNoContent {
		t.Fatalf("DeleteRestApi: status = %d", w.Code)
	}

	w, _ = doAPIGW(svc, http.MethodGet, "/restapis/"+apiID+"/resources/"+rootID+"/methods/GET", nil)
	if w.Code != http.StatusNotFound {
		t.Fatalf("GetMethod tras DeleteRestApi: status = %d, esperaba 404 (cascada)", w.Code)
	}
	w, _ = doAPIGW(svc, http.MethodGet, "/restapis/"+apiID+"/stages/prod", nil)
	if w.Code != http.StatusNotFound {
		t.Fatalf("GetStage tras DeleteRestApi: status = %d, esperaba 404 (cascada)", w.Code)
	}
}

func TestCreateResource_AndGetResource(t *testing.T) {
	svc, _ := newTestService(t)
	_, api := doAPIGW(svc, http.MethodPost, "/restapis", map[string]any{"name": "api1"})
	apiID := api["id"].(string)
	_, resources := doAPIGW(svc, http.MethodGet, "/restapis/"+apiID+"/resources", nil)
	rootID := resources["item"].([]any)[0].(map[string]any)["id"].(string)

	w, out := doAPIGW(svc, http.MethodPost, "/restapis/"+apiID+"/resources/"+rootID, map[string]any{"pathPart": "items"})
	if w.Code != http.StatusCreated {
		t.Fatalf("CreateResource: status = %d, body = %s", w.Code, w.Body.String())
	}
	if out["path"] != "/items" {
		t.Fatalf("CreateResource: path = %v, esperaba /items", out["path"])
	}
	resID := out["id"].(string)

	w, out = doAPIGW(svc, http.MethodGet, "/restapis/"+apiID+"/resources/"+resID, nil)
	if w.Code != http.StatusOK || out["path"] != "/items" {
		t.Fatalf("GetResource: status = %d, body = %v", w.Code, out)
	}
}

func TestCreateResource_RequiresExistingParent(t *testing.T) {
	svc, _ := newTestService(t)
	_, api := doAPIGW(svc, http.MethodPost, "/restapis", map[string]any{"name": "api1"})
	apiID := api["id"].(string)

	w, _ := doAPIGW(svc, http.MethodPost, "/restapis/"+apiID+"/resources/nope", map[string]any{"pathPart": "items"})
	if w.Code != http.StatusNotFound {
		t.Fatalf("CreateResource con parent inexistente: status = %d, esperaba 404", w.Code)
	}
}

func TestCreateResource_RequiresPathPart(t *testing.T) {
	svc, _ := newTestService(t)
	_, api := doAPIGW(svc, http.MethodPost, "/restapis", map[string]any{"name": "api1"})
	apiID := api["id"].(string)
	_, resources := doAPIGW(svc, http.MethodGet, "/restapis/"+apiID+"/resources", nil)
	rootID := resources["item"].([]any)[0].(map[string]any)["id"].(string)

	w, _ := doAPIGW(svc, http.MethodPost, "/restapis/"+apiID+"/resources/"+rootID, map[string]any{})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("CreateResource sin pathPart: status = %d, esperaba 400", w.Code)
	}
}

func TestPutMethod_DefaultsAuthorizationTypeToNone(t *testing.T) {
	svc, _ := newTestService(t)
	_, api := doAPIGW(svc, http.MethodPost, "/restapis", map[string]any{"name": "api1"})
	apiID := api["id"].(string)
	_, resources := doAPIGW(svc, http.MethodGet, "/restapis/"+apiID+"/resources", nil)
	rootID := resources["item"].([]any)[0].(map[string]any)["id"].(string)

	w, out := doAPIGW(svc, http.MethodPut, "/restapis/"+apiID+"/resources/"+rootID+"/methods/GET", map[string]any{})
	if w.Code != http.StatusCreated {
		t.Fatalf("PutMethod: status = %d, body = %s", w.Code, w.Body.String())
	}
	if out["authorizationType"] != "NONE" {
		t.Fatalf("PutMethod authorizationType = %v, esperaba NONE por default", out["authorizationType"])
	}

	w, out = doAPIGW(svc, http.MethodGet, "/restapis/"+apiID+"/resources/"+rootID+"/methods/GET", nil)
	if w.Code != http.StatusOK || out["httpMethod"] != "GET" {
		t.Fatalf("GetMethod: status = %d, body = %v", w.Code, out)
	}
}

func TestDeleteMethod_AlsoRemovesIntegration(t *testing.T) {
	svc, lambdaSvc := newTestService(t)
	createTestFunction(t, lambdaSvc, "fn1")
	_, api := doAPIGW(svc, http.MethodPost, "/restapis", map[string]any{"name": "api1"})
	apiID := api["id"].(string)
	_, resources := doAPIGW(svc, http.MethodGet, "/restapis/"+apiID+"/resources", nil)
	rootID := resources["item"].([]any)[0].(map[string]any)["id"].(string)

	doAPIGW(svc, http.MethodPut, "/restapis/"+apiID+"/resources/"+rootID+"/methods/GET", map[string]any{})
	doAPIGW(svc, http.MethodPut, "/restapis/"+apiID+"/resources/"+rootID+"/methods/GET/integration", map[string]any{
		"type": "AWS_PROXY", "httpMethod": "POST", "uri": lambdaURI("fn1"),
	})

	w, _ := doAPIGW(svc, http.MethodDelete, "/restapis/"+apiID+"/resources/"+rootID+"/methods/GET", nil)
	if w.Code != http.StatusNoContent {
		t.Fatalf("DeleteMethod: status = %d", w.Code)
	}

	w, _ = doAPIGW(svc, http.MethodGet, "/restapis/"+apiID+"/resources/"+rootID+"/methods/GET/integration", nil)
	if w.Code != http.StatusNotFound {
		t.Fatalf("GetIntegration tras DeleteMethod: status = %d, esperaba 404", w.Code)
	}
}

// TestPutIntegration_RequiresMethodFirst cubre la validación documentada:
// PutIntegration sin un PutMethod previo debe fallar.
func TestPutIntegration_RequiresMethodFirst(t *testing.T) {
	svc, _ := newTestService(t)
	_, api := doAPIGW(svc, http.MethodPost, "/restapis", map[string]any{"name": "api1"})
	apiID := api["id"].(string)
	_, resources := doAPIGW(svc, http.MethodGet, "/restapis/"+apiID+"/resources", nil)
	rootID := resources["item"].([]any)[0].(map[string]any)["id"].(string)

	w, _ := doAPIGW(svc, http.MethodPut, "/restapis/"+apiID+"/resources/"+rootID+"/methods/GET/integration", map[string]any{
		"type": "AWS_PROXY",
	})
	if w.Code != http.StatusNotFound {
		t.Fatalf("PutIntegration sin método previo: status = %d, esperaba 404", w.Code)
	}
}

func TestCreateDeployment_AndGetDeployment(t *testing.T) {
	svc, _ := newTestService(t)
	_, api := doAPIGW(svc, http.MethodPost, "/restapis", map[string]any{"name": "api1"})
	apiID := api["id"].(string)

	w, out := doAPIGW(svc, http.MethodPost, "/restapis/"+apiID+"/deployments", map[string]any{"stageName": "prod"})
	if w.Code != http.StatusCreated {
		t.Fatalf("CreateDeployment: status = %d, body = %s", w.Code, w.Body.String())
	}
	depID := out["id"].(string)

	w, out = doAPIGW(svc, http.MethodGet, "/restapis/"+apiID+"/deployments/"+depID, nil)
	if w.Code != http.StatusOK || out["id"] != depID {
		t.Fatalf("GetDeployment: status = %d, body = %v", w.Code, out)
	}

	w, out = doAPIGW(svc, http.MethodGet, "/restapis/"+apiID+"/stages/prod", nil)
	if w.Code != http.StatusOK || out["deploymentId"] != depID {
		t.Fatalf("GetStage tras CreateDeployment con stageName: status = %d, body = %v", w.Code, out)
	}
}

func TestCreateDeployment_RequiresExistingApi(t *testing.T) {
	svc, _ := newTestService(t)
	w, _ := doAPIGW(svc, http.MethodPost, "/restapis/nope/deployments", map[string]any{})
	if w.Code != http.StatusNotFound {
		t.Fatalf("CreateDeployment sobre API inexistente: status = %d, esperaba 404", w.Code)
	}
}

func TestGetStages_ListsAllStages(t *testing.T) {
	svc, _ := newTestService(t)
	_, api := doAPIGW(svc, http.MethodPost, "/restapis", map[string]any{"name": "api1"})
	apiID := api["id"].(string)
	doAPIGW(svc, http.MethodPost, "/restapis/"+apiID+"/deployments", map[string]any{"stageName": "prod"})
	doAPIGW(svc, http.MethodPost, "/restapis/"+apiID+"/deployments", map[string]any{"stageName": "dev"})

	w, out := doAPIGW(svc, http.MethodGet, "/restapis/"+apiID+"/stages", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("GetStages: status = %d, body = %s", w.Code, w.Body.String())
	}
	items, _ := out["item"].([]any)
	if len(items) != 2 {
		t.Fatalf("GetStages: esperaba 2 stages, body = %s", w.Body.String())
	}
}

func TestDeleteStage(t *testing.T) {
	svc, _ := newTestService(t)
	_, api := doAPIGW(svc, http.MethodPost, "/restapis", map[string]any{"name": "api1"})
	apiID := api["id"].(string)
	doAPIGW(svc, http.MethodPost, "/restapis/"+apiID+"/deployments", map[string]any{"stageName": "prod"})

	w, _ := doAPIGW(svc, http.MethodDelete, "/restapis/"+apiID+"/stages/prod", nil)
	if w.Code != http.StatusNoContent {
		t.Fatalf("DeleteStage: status = %d", w.Code)
	}

	w, _ = doAPIGW(svc, http.MethodGet, "/restapis/"+apiID+"/stages/prod", nil)
	if w.Code != http.StatusNotFound {
		t.Fatalf("GetStage tras DeleteStage: status = %d, esperaba 404", w.Code)
	}
}

// TestInvoke_ProxiesToLambdaUsingProxyPlusWildcard cubre el camino central
// del servicio: resource -> integration AWS_PROXY -> invocación Lambda,
// incluyendo el comodín {proxy+} para matchear cualquier subpath.
func TestInvoke_ProxiesToLambdaUsingProxyPlusWildcard(t *testing.T) {
	svc, lambdaSvc := newTestService(t)
	createTestFunction(t, lambdaSvc, "fn1")

	_, api := doAPIGW(svc, http.MethodPost, "/restapis", map[string]any{"name": "api1"})
	apiID := api["id"].(string)
	_, resources := doAPIGW(svc, http.MethodGet, "/restapis/"+apiID+"/resources", nil)
	rootID := resources["item"].([]any)[0].(map[string]any)["id"].(string)

	_, proxyRes := doAPIGW(svc, http.MethodPost, "/restapis/"+apiID+"/resources/"+rootID, map[string]any{"pathPart": "{proxy+}"})
	proxyID := proxyRes["id"].(string)

	doAPIGW(svc, http.MethodPut, "/restapis/"+apiID+"/resources/"+proxyID+"/methods/ANY", map[string]any{})
	doAPIGW(svc, http.MethodPut, "/restapis/"+apiID+"/resources/"+proxyID+"/methods/ANY/integration", map[string]any{
		"type": "AWS_PROXY", "httpMethod": "POST", "uri": lambdaURI("fn1"),
	})
	doAPIGW(svc, http.MethodPost, "/restapis/"+apiID+"/deployments", map[string]any{"stageName": "prod"})

	r := httptest.NewRequest(http.MethodGet, "/execute-api/"+apiID+"/prod/hello/world", bytes.NewReader([]byte(`{"hi":"there"}`)))
	w := httptest.NewRecorder()
	svc.ServeHTTP(w, r)

	// El stub de Lambda hace eco del evento AWS_PROXY que le manda
	// invokeLambdaProxy (no tiene shape {statusCode,...}), así que cae al
	// fallback de invokeLambdaProxy: 200 + el body crudo de la invocación.
	if w.Code != http.StatusOK {
		t.Fatalf("Invoke vía execute-api: status = %d, body = %s", w.Code, w.Body.String())
	}
	var event map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &event); err != nil {
		t.Fatalf("Invoke vía execute-api: no se pudo decodificar el evento de eco: %v, body = %s", err, w.Body.String())
	}
	if event["path"] != "/hello/world" {
		t.Fatalf("Invoke vía execute-api: path en el evento = %v, esperaba /hello/world", event["path"])
	}
}

func TestInvoke_RequiresDeployedStage(t *testing.T) {
	svc, _ := newTestService(t)
	_, api := doAPIGW(svc, http.MethodPost, "/restapis", map[string]any{"name": "api1"})
	apiID := api["id"].(string)

	r := httptest.NewRequest(http.MethodGet, "/execute-api/"+apiID+"/prod/", bytes.NewReader(nil))
	w := httptest.NewRecorder()
	svc.ServeHTTP(w, r)
	if w.Code != http.StatusNotFound {
		t.Fatalf("Invoke sin stage desplegado: status = %d, esperaba 404", w.Code)
	}
}

// TestInvoke_RejectsNonProxyIntegration cubre que solo se soporta
// AWS_PROXY, documentado explícitamente en el código.
func TestInvoke_RejectsNonProxyIntegration(t *testing.T) {
	svc, _ := newTestService(t)
	_, api := doAPIGW(svc, http.MethodPost, "/restapis", map[string]any{"name": "api1"})
	apiID := api["id"].(string)
	_, resources := doAPIGW(svc, http.MethodGet, "/restapis/"+apiID+"/resources", nil)
	rootID := resources["item"].([]any)[0].(map[string]any)["id"].(string)

	doAPIGW(svc, http.MethodPut, "/restapis/"+apiID+"/resources/"+rootID+"/methods/GET", map[string]any{})
	doAPIGW(svc, http.MethodPut, "/restapis/"+apiID+"/resources/"+rootID+"/methods/GET/integration", map[string]any{
		"type": "MOCK",
	})
	doAPIGW(svc, http.MethodPost, "/restapis/"+apiID+"/deployments", map[string]any{"stageName": "prod"})

	r := httptest.NewRequest(http.MethodGet, "/execute-api/"+apiID+"/prod/", bytes.NewReader(nil))
	w := httptest.NewRecorder()
	svc.ServeHTTP(w, r)
	if w.Code != http.StatusNotImplemented {
		t.Fatalf("Invoke con integración MOCK: status = %d, esperaba 501", w.Code)
	}
}

func TestServeHTTP_UnsupportedRouteFails(t *testing.T) {
	svc, _ := newTestService(t)
	w, _ := doAPIGW(svc, http.MethodPatch, "/restapis/x/authorizers", nil)
	if w.Code != http.StatusNotFound {
		t.Fatalf("ruta no soportada: status = %d, esperaba 404", w.Code)
	}
}

func TestReset_ClearsEverything(t *testing.T) {
	svc, _ := newTestService(t)
	doAPIGW(svc, http.MethodPost, "/restapis", map[string]any{"name": "api1"})

	if err := svc.Reset(); err != nil {
		t.Fatalf("Reset: %v", err)
	}

	_, out := doAPIGW(svc, http.MethodGet, "/restapis", nil)
	items, _ := out["item"].([]any)
	if len(items) != 0 {
		t.Fatalf("GetRestApis tras Reset: esperaba vacío")
	}
}
