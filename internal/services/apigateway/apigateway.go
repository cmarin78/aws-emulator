// Package apigateway emula el subconjunto mínimo de API Gateway (REST API)
// necesario para probar el cableado API Gateway -> Lambda en local: crear
// una API, su árbol de recursos, métodos e integraciones, desplegarla, e
// invocarla.
//
// Protocolo real confirmado con `aws apigateway create-rest-api --debug` y
// una batería de llamadas equivalentes para el resto de las operaciones
// (ver ROADMAP.md): a diferencia de todos los demás servicios de este
// proyecto, la API de administración de API Gateway es REST puro sobre
// rutas bajo /restapis/... con verbos HTTP estándar — sin X-Amz-Target, sin
// Action=. El cuerpo es JSON plano con keys camelCase (name, pathPart,
// authorizationType, type/httpMethod/uri, stageName). El nombre de servicio
// en el credential scope SigV4 es "apigateway".
//
// Invocación ("execute-api"): en AWS real, una vez desplegada, una API se
// invoca contra un host completamente distinto —
// https://{api-id}.execute-api.{region}.amazonaws.com/{stage}/{path}—, ya
// que ahí no hay X-Amz-Target/Action/credential-scope que la identifique:
// la única señal es el subdominio. Este emulador, al exponer un único
// endpoint plano (un solo puerto para todo, sin DNS wildcard), no puede
// replicar el ruteo por subdominio sin pedirle al usuario que edite su
// /etc/hosts o pase --resolve en cada llamada. En cambio, expone la
// invocación bajo un path explícito de este emulador:
// /execute-api/{restApiId}/{stageName}/{proxy+} — no es el shape real de
// AWS, es una convención de conveniencia local, documentada acá para que no
// se confunda con una réplica fiel del protocolo (al contrario que el resto
// de las rutas /restapis/..., que sí son fieles).
//
// Solo se soporta integración de tipo AWS_PROXY (el caso de uso explícito
// del ROADMAP: "resource -> integration -> Lambda invocation"); MOCK/HTTP/
// otros tipos de integración no están implementados.
package apigateway

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"time"

	"github.com/cesarmarin/aws-emulator/internal/server"
	"github.com/cesarmarin/aws-emulator/internal/storage"
)

const (
	restApisBucket     = "apigateway.restapis"
	resourcesBucket    = "apigateway.resources"
	methodsBucket      = "apigateway.methods"
	integrationsBucket = "apigateway.integrations"
	deploymentsBucket  = "apigateway.deployments"
	stagesBucket       = "apigateway.stages"
)

var (
	restApisRe    = regexp.MustCompile(`^/restapis$`)
	restApiRe     = regexp.MustCompile(`^/restapis/([^/]+)$`)
	resourcesRe   = regexp.MustCompile(`^/restapis/([^/]+)/resources$`)
	resourceRe    = regexp.MustCompile(`^/restapis/([^/]+)/resources/([^/]+)$`)
	methodRe      = regexp.MustCompile(`^/restapis/([^/]+)/resources/([^/]+)/methods/([^/]+)$`)
	integrationRe = regexp.MustCompile(`^/restapis/([^/]+)/resources/([^/]+)/methods/([^/]+)/integration$`)
	deploymentsRe = regexp.MustCompile(`^/restapis/([^/]+)/deployments$`)
	deploymentRe  = regexp.MustCompile(`^/restapis/([^/]+)/deployments/([^/]+)$`)
	stagesRe      = regexp.MustCompile(`^/restapis/([^/]+)/stages$`)
	stageRe       = regexp.MustCompile(`^/restapis/([^/]+)/stages/([^/]+)$`)
	executeApiRe  = regexp.MustCompile(`^/execute-api/([^/]+)/([^/]+)(/.*)?$`)
)

// lambdaInvoker es el subconjunto de lambda.Service que apigateway necesita
// para proxyar invocaciones AWS_PROXY: solo ServeHTTP, llamado en proceso
// (vía httptest.Recorder, sin un round-trip de red real) en vez de
// importar lambda directamente, para no acoplar el paquete a su
// implementación interna más de lo necesario.
type lambdaInvoker interface {
	ServeHTTP(w http.ResponseWriter, r *http.Request)
}

// Service agrupa el estado del servicio API Gateway.
type Service struct {
	db     *storage.DB
	lambda lambdaInvoker
}

// New crea el servicio API Gateway. lambda es el servicio Lambda ya
// construido (igual que sns/events reciben *sqs.Service): las integraciones
// AWS_PROXY lo invocan directamente para evitar reimplementar el ciclo de
// invocación de funciones.
func New(db *storage.DB, lambda lambdaInvoker) *Service {
	return &Service{db: db, lambda: lambda}
}

// Reset limpia todo el estado persistido de API Gateway. Implementa
// server.Resettable.
func (s *Service) Reset() error {
	return s.db.Reset(restApisBucket, resourcesBucket, methodsBucket, integrationsBucket, deploymentsBucket, stagesBucket)
}

// RestApi es la forma persistida de una REST API.
type RestApi struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	CreatedDate int64  `json:"createdDate"`
}

// Resource es la forma persistida de un nodo del árbol de recursos.
type Resource struct {
	ID        string `json:"id"`
	RestApiID string `json:"restApiId"`
	ParentID  string `json:"parentId,omitempty"`
	PathPart  string `json:"pathPart,omitempty"`
	Path      string `json:"path"`
}

// Method es la forma persistida de un método HTTP en un recurso.
type Method struct {
	RestApiID         string `json:"restApiId"`
	ResourceID        string `json:"resourceId"`
	HTTPMethod        string `json:"httpMethod"`
	AuthorizationType string `json:"authorizationType"`
}

// Integration es la forma persistida de la integración de un método.
type Integration struct {
	RestApiID             string `json:"restApiId"`
	ResourceID            string `json:"resourceId"`
	HTTPMethod            string `json:"httpMethod"`
	Type                  string `json:"type"`
	IntegrationHTTPMethod string `json:"integrationHttpMethod,omitempty"`
	URI                   string `json:"uri,omitempty"`
}

// Deployment es la forma persistida de un deployment.
type Deployment struct {
	ID          string `json:"id"`
	RestApiID   string `json:"restApiId"`
	CreatedDate int64  `json:"createdDate"`
}

// Stage es la forma persistida de un stage.
type Stage struct {
	RestApiID    string `json:"restApiId"`
	StageName    string `json:"stageName"`
	DeploymentID string `json:"deploymentId"`
}

func randomID() string {
	b := make([]byte, 5)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func nowMillis() int64 { return time.Now().UTC().UnixMilli() }

func writeError(w http.ResponseWriter, status int, errType, message string) {
	server.WriteJSONError(w, status, errType, message)
}

func decodeJSONBody(r *http.Request) map[string]any {
	defer r.Body.Close()
	out := map[string]any{}
	if r.ContentLength == 0 {
		return out
	}
	_ = json.NewDecoder(r.Body).Decode(&out)
	return out
}

func (s *Service) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimSuffix(r.URL.Path, "/")
	if path == "" {
		path = "/"
	}
	method := r.Method

	switch {
	case method == http.MethodPost && restApisRe.MatchString(path):
		s.createRestApi(w, r)
	case method == http.MethodGet && restApisRe.MatchString(path):
		s.getRestApis(w)
	case method == http.MethodGet && restApiRe.MatchString(path):
		s.getRestApi(w, restApiRe.FindStringSubmatch(path)[1])
	case method == http.MethodDelete && restApiRe.MatchString(path):
		s.deleteRestApi(w, restApiRe.FindStringSubmatch(path)[1])
	case method == http.MethodGet && resourcesRe.MatchString(path):
		s.getResources(w, resourcesRe.FindStringSubmatch(path)[1])
	case method == http.MethodPost && resourceRe.MatchString(path):
		m := resourceRe.FindStringSubmatch(path)
		s.createResource(w, r, m[1], m[2])
	case method == http.MethodGet && resourceRe.MatchString(path):
		m := resourceRe.FindStringSubmatch(path)
		s.getResource(w, m[1], m[2])
	case method == http.MethodDelete && resourceRe.MatchString(path):
		m := resourceRe.FindStringSubmatch(path)
		s.deleteResource(w, m[1], m[2])
	case method == http.MethodPut && integrationRe.MatchString(path):
		m := integrationRe.FindStringSubmatch(path)
		s.putIntegration(w, r, m[1], m[2], m[3])
	case method == http.MethodGet && integrationRe.MatchString(path):
		m := integrationRe.FindStringSubmatch(path)
		s.getIntegration(w, m[1], m[2], m[3])
	case method == http.MethodPut && methodRe.MatchString(path):
		m := methodRe.FindStringSubmatch(path)
		s.putMethod(w, r, m[1], m[2], m[3])
	case method == http.MethodGet && methodRe.MatchString(path):
		m := methodRe.FindStringSubmatch(path)
		s.getMethod(w, m[1], m[2], m[3])
	case method == http.MethodDelete && methodRe.MatchString(path):
		m := methodRe.FindStringSubmatch(path)
		s.deleteMethod(w, m[1], m[2], m[3])
	case method == http.MethodPost && deploymentsRe.MatchString(path):
		s.createDeployment(w, r, deploymentsRe.FindStringSubmatch(path)[1])
	case method == http.MethodGet && deploymentRe.MatchString(path):
		m := deploymentRe.FindStringSubmatch(path)
		s.getDeployment(w, m[1], m[2])
	case method == http.MethodGet && stagesRe.MatchString(path):
		s.getStages(w, stagesRe.FindStringSubmatch(path)[1])
	case method == http.MethodGet && stageRe.MatchString(path):
		m := stageRe.FindStringSubmatch(path)
		s.getStage(w, m[1], m[2])
	case method == http.MethodDelete && stageRe.MatchString(path):
		m := stageRe.FindStringSubmatch(path)
		s.deleteStage(w, m[1], m[2])
	case executeApiRe.MatchString(path):
		m := executeApiRe.FindStringSubmatch(path)
		s.invoke(w, r, m[1], m[2], m[3])
	default:
		writeError(w, http.StatusNotFound, "NotFoundException",
			"ruta/método API Gateway no soportado en este emulador: "+method+" "+path)
	}
}

// formatCreatedDate convierte millis-desde-epoch (forma de almacenamiento
// interna) a segundos-desde-epoch como número JSON: es el formato real que
// usa el protocolo rest-json de API Gateway para sus shapes "timestamp" sin
// un trait timestampFormat explícito (default "epoch-seconds" en
// Smithy/rest-json1, igual que el resto de los protocolos JSON de AWS).
//
// Una versión anterior de este comentario decía que el formato real era un
// string ISO8601 y que mandar un número rompía a botocore en silencio --
// eso fue un diagnóstico equivocado: el bug real en ese momento era la key
// "item" vs "items" (ver getRestApis), y mandar ISO8601 "funcionaba" solo
// porque ningún cliente probado en ese momento validaba estrictamente el
// tipo. El provider real de Terraform (basado en el SDK de Go, no en
// botocore) sí valida el tipo y falla explícitamente con "expected
// Timestamp to be a JSON Number, got string instead" si se manda un
// string -- encontrado vía terraform/aws-smoke-test, ver ROADMAP.md.
func formatCreatedDate(millis int64) float64 {
	return float64(millis) / 1000
}

func restApiOut(a RestApi) map[string]any {
	return map[string]any{
		"id":          a.ID,
		"name":        a.Name,
		"description": a.Description,
		"createdDate": formatCreatedDate(a.CreatedDate),
	}
}

func (s *Service) createRestApi(w http.ResponseWriter, r *http.Request) {
	body := decodeJSONBody(r)
	name, _ := body["name"].(string)
	if name == "" {
		writeError(w, http.StatusBadRequest, "BadRequestException", "name es requerido")
		return
	}
	desc, _ := body["description"].(string)

	api := RestApi{ID: randomID(), Name: name, Description: desc, CreatedDate: nowMillis()}
	if err := s.db.Put(restApisBucket, api.ID, api); err != nil {
		writeError(w, http.StatusInternalServerError, "InternalServerError", err.Error())
		return
	}

	// AWS real crea automáticamente el recurso raíz "/" al crear la API —
	// confirmado con `aws apigateway get-resources --debug` justo después de
	// create-rest-api, que ya trae un recurso con path "/" sin haber llamado
	// a create-resource.
	root := Resource{ID: randomID(), RestApiID: api.ID, PathPart: "", Path: "/"}
	if err := s.db.Put(resourcesBucket, api.ID+"/"+root.ID, root); err != nil {
		writeError(w, http.StatusInternalServerError, "InternalServerError", err.Error())
		return
	}

	server.WriteJSON(w, http.StatusCreated, restApiOut(api))
}

func (s *Service) getRestApis(w http.ResponseWriter) {
	var apis []RestApi
	_ = s.db.List(restApisBucket, "", func(_ string, raw []byte) error {
		var a RestApi
		if err := json.Unmarshal(raw, &a); err == nil {
			apis = append(apis, a)
		}
		return nil
	})
	out := make([]map[string]any, 0, len(apis))
	for _, a := range apis {
		out = append(out, restApiOut(a))
	}
	// Ojo: la key del body es "item", no "items" — el modelo rest-json de
	// API Gateway define locationName "item" para este miembro de lista
	// (legado de su modelo XML), y botocore busca literalmente esa key al
	// parsear la respuesta. Mandar "items" hace que el parser de botocore
	// descarte el campo en silencio (devuelve solo ResponseMetadata, sin
	// excepción visible ni en --debug) — confirmado inspeccionando
	// botocore.parsers.BaseJSONParser._handle_structure y el shape real vía
	// `session.get_service_model('apigateway').operation_model('GetRestApis')`.
	// Esto fue la causa real de que `aws apigateway get-rest-apis` no
	// imprimiera nada con exit 0; el bug de timestamp ISO8601 (ver
	// formatCreatedDate) era un bug real y separado, no la causa de esto.
	// Ver ROADMAP.md.
	server.WriteJSON(w, http.StatusOK, map[string]any{"item": out})
}

func (s *Service) getRestApi(w http.ResponseWriter, id string) {
	var a RestApi
	found, _ := s.db.Get(restApisBucket, id, &a)
	if !found {
		writeError(w, http.StatusNotFound, "NotFoundException", "no existe la REST API: "+id)
		return
	}
	server.WriteJSON(w, http.StatusOK, restApiOut(a))
}

func (s *Service) deleteRestApi(w http.ResponseWriter, id string) {
	_ = s.db.Delete(restApisBucket, id)
	_ = s.db.DeletePrefix(resourcesBucket, id+"/")
	_ = s.db.DeletePrefix(methodsBucket, id+"/")
	_ = s.db.DeletePrefix(integrationsBucket, id+"/")
	_ = s.db.DeletePrefix(deploymentsBucket, id+"/")
	_ = s.db.DeletePrefix(stagesBucket, id+"/")
	w.WriteHeader(http.StatusNoContent)
}

func resourceOut(res Resource) map[string]any {
	out := map[string]any{
		"id":   res.ID,
		"path": res.Path,
	}
	if res.ParentID != "" {
		out["parentId"] = res.ParentID
	}
	if res.PathPart != "" {
		out["pathPart"] = res.PathPart
	}
	return out
}

func (s *Service) getResources(w http.ResponseWriter, restApiID string) {
	var resources []Resource
	_ = s.db.List(resourcesBucket, restApiID+"/", func(_ string, raw []byte) error {
		var res Resource
		if err := json.Unmarshal(raw, &res); err == nil {
			resources = append(resources, res)
		}
		return nil
	})
	out := make([]map[string]any, 0, len(resources))
	for _, res := range resources {
		out = append(out, resourceOut(res))
	}
	server.WriteJSON(w, http.StatusOK, map[string]any{"item": out}) // ver nota en getRestApis sobre "item" vs "items"
}

func (s *Service) getResource(w http.ResponseWriter, restApiID, resourceID string) {
	var res Resource
	found, _ := s.db.Get(resourcesBucket, restApiID+"/"+resourceID, &res)
	if !found {
		writeError(w, http.StatusNotFound, "NotFoundException", "no existe el recurso: "+resourceID)
		return
	}
	server.WriteJSON(w, http.StatusOK, resourceOut(res))
}

func (s *Service) createResource(w http.ResponseWriter, r *http.Request, restApiID, parentID string) {
	var parent Resource
	found, _ := s.db.Get(resourcesBucket, restApiID+"/"+parentID, &parent)
	if !found {
		writeError(w, http.StatusNotFound, "NotFoundException", "no existe el recurso padre: "+parentID)
		return
	}

	body := decodeJSONBody(r)
	pathPart, _ := body["pathPart"].(string)
	if pathPart == "" {
		writeError(w, http.StatusBadRequest, "BadRequestException", "pathPart es requerido")
		return
	}

	fullPath := strings.TrimSuffix(parent.Path, "/") + "/" + pathPart
	res := Resource{ID: randomID(), RestApiID: restApiID, ParentID: parentID, PathPart: pathPart, Path: fullPath}
	if err := s.db.Put(resourcesBucket, restApiID+"/"+res.ID, res); err != nil {
		writeError(w, http.StatusInternalServerError, "InternalServerError", err.Error())
		return
	}
	server.WriteJSON(w, http.StatusCreated, resourceOut(res))
}

func (s *Service) deleteResource(w http.ResponseWriter, restApiID, resourceID string) {
	_ = s.db.Delete(resourcesBucket, restApiID+"/"+resourceID)
	_ = s.db.DeletePrefix(methodsBucket, restApiID+"/"+resourceID+"/")
	_ = s.db.DeletePrefix(integrationsBucket, restApiID+"/"+resourceID+"/")
	w.WriteHeader(http.StatusNoContent)
}

func methodOut(m Method) map[string]any {
	return map[string]any{
		"httpMethod":        m.HTTPMethod,
		"authorizationType": m.AuthorizationType,
	}
}

func (s *Service) putMethod(w http.ResponseWriter, r *http.Request, restApiID, resourceID, httpMethod string) {
	body := decodeJSONBody(r)
	authType, _ := body["authorizationType"].(string)
	if authType == "" {
		authType = "NONE"
	}
	m := Method{RestApiID: restApiID, ResourceID: resourceID, HTTPMethod: httpMethod, AuthorizationType: authType}
	key := restApiID + "/" + resourceID + "/" + httpMethod
	if err := s.db.Put(methodsBucket, key, m); err != nil {
		writeError(w, http.StatusInternalServerError, "InternalServerError", err.Error())
		return
	}
	server.WriteJSON(w, http.StatusCreated, methodOut(m))
}

func (s *Service) getMethod(w http.ResponseWriter, restApiID, resourceID, httpMethod string) {
	var m Method
	found, _ := s.db.Get(methodsBucket, restApiID+"/"+resourceID+"/"+httpMethod, &m)
	if !found {
		writeError(w, http.StatusNotFound, "NotFoundException", "no existe el método: "+httpMethod)
		return
	}
	server.WriteJSON(w, http.StatusOK, methodOut(m))
}

func (s *Service) deleteMethod(w http.ResponseWriter, restApiID, resourceID, httpMethod string) {
	key := restApiID + "/" + resourceID + "/" + httpMethod
	_ = s.db.Delete(methodsBucket, key)
	_ = s.db.Delete(integrationsBucket, key)
	w.WriteHeader(http.StatusNoContent)
}

func integrationOut(i Integration) map[string]any {
	out := map[string]any{
		"type": i.Type,
	}
	if i.IntegrationHTTPMethod != "" {
		out["httpMethod"] = i.IntegrationHTTPMethod
	}
	if i.URI != "" {
		out["uri"] = i.URI
	}
	return out
}

func (s *Service) putIntegration(w http.ResponseWriter, r *http.Request, restApiID, resourceID, httpMethod string) {
	key := restApiID + "/" + resourceID + "/" + httpMethod
	var m Method
	if found, _ := s.db.Get(methodsBucket, key, &m); !found {
		writeError(w, http.StatusNotFound, "NotFoundException",
			"no existe el método "+httpMethod+" en el recurso "+resourceID+" — hay que llamar a PutMethod antes de PutIntegration")
		return
	}

	body := decodeJSONBody(r)
	typ, _ := body["type"].(string)
	if typ == "" {
		writeError(w, http.StatusBadRequest, "BadRequestException", "type es requerido")
		return
	}
	integMethod, _ := body["httpMethod"].(string)
	uri, _ := body["uri"].(string)

	i := Integration{
		RestApiID:             restApiID,
		ResourceID:            resourceID,
		HTTPMethod:            httpMethod,
		Type:                  typ,
		IntegrationHTTPMethod: integMethod,
		URI:                   uri,
	}
	if err := s.db.Put(integrationsBucket, key, i); err != nil {
		writeError(w, http.StatusInternalServerError, "InternalServerError", err.Error())
		return
	}
	server.WriteJSON(w, http.StatusCreated, integrationOut(i))
}

func (s *Service) getIntegration(w http.ResponseWriter, restApiID, resourceID, httpMethod string) {
	var i Integration
	found, _ := s.db.Get(integrationsBucket, restApiID+"/"+resourceID+"/"+httpMethod, &i)
	if !found {
		writeError(w, http.StatusNotFound, "NotFoundException", "no existe la integración del método: "+httpMethod)
		return
	}
	server.WriteJSON(w, http.StatusOK, integrationOut(i))
}

func (s *Service) createDeployment(w http.ResponseWriter, r *http.Request, restApiID string) {
	var api RestApi
	if found, _ := s.db.Get(restApisBucket, restApiID, &api); !found {
		writeError(w, http.StatusNotFound, "NotFoundException", "no existe la REST API: "+restApiID)
		return
	}

	body := decodeJSONBody(r)
	stageName, _ := body["stageName"].(string)

	dep := Deployment{ID: randomID(), RestApiID: restApiID, CreatedDate: nowMillis()}
	if err := s.db.Put(deploymentsBucket, restApiID+"/"+dep.ID, dep); err != nil {
		writeError(w, http.StatusInternalServerError, "InternalServerError", err.Error())
		return
	}

	if stageName != "" {
		stage := Stage{RestApiID: restApiID, StageName: stageName, DeploymentID: dep.ID}
		if err := s.db.Put(stagesBucket, restApiID+"/"+stageName, stage); err != nil {
			writeError(w, http.StatusInternalServerError, "InternalServerError", err.Error())
			return
		}
	}

	server.WriteJSON(w, http.StatusCreated, map[string]any{
		"id":          dep.ID,
		"createdDate": formatCreatedDate(dep.CreatedDate),
	})
}

// getDeployment: faltaba un GET singular para un deployment puntual --
// solo existía createDeployment (POST .../deployments). El provider real
// de Terraform llama a GetDeployment durante el Read de
// aws_api_gateway_deployment para refrescar su estado, y como no había
// ruta registrada, el dispatch caía al default ("couldn't find resource"
// del lado del provider, generado a partir de un 404 genérico). Encontrado
// vía terraform/aws-smoke-test, ver ROADMAP.md.
func (s *Service) getDeployment(w http.ResponseWriter, restApiID, deploymentID string) {
	var dep Deployment
	if found, _ := s.db.Get(deploymentsBucket, restApiID+"/"+deploymentID, &dep); !found {
		writeError(w, http.StatusNotFound, "NotFoundException", "no existe el deployment: "+deploymentID)
		return
	}
	server.WriteJSON(w, http.StatusOK, map[string]any{
		"id":          dep.ID,
		"createdDate": formatCreatedDate(dep.CreatedDate),
	})
}

func (s *Service) getStages(w http.ResponseWriter, restApiID string) {
	var stages []Stage
	_ = s.db.List(stagesBucket, restApiID+"/", func(_ string, raw []byte) error {
		var st Stage
		if err := json.Unmarshal(raw, &st); err == nil {
			stages = append(stages, st)
		}
		return nil
	})
	out := make([]map[string]any, 0, len(stages))
	for _, st := range stages {
		out = append(out, map[string]any{"stageName": st.StageName, "deploymentId": st.DeploymentID})
	}
	server.WriteJSON(w, http.StatusOK, map[string]any{"item": out})
}

func (s *Service) getStage(w http.ResponseWriter, restApiID, stageName string) {
	var st Stage
	found, _ := s.db.Get(stagesBucket, restApiID+"/"+stageName, &st)
	if !found {
		writeError(w, http.StatusNotFound, "NotFoundException", "no existe el stage: "+stageName)
		return
	}
	server.WriteJSON(w, http.StatusOK, map[string]any{"stageName": st.StageName, "deploymentId": st.DeploymentID})
}

// deleteStage: el recurso aws_api_gateway_stage de Terraform necesita esta
// ruta en su Delete -- sin ella, terraform destroy fallaba con
// NotFoundException aunque la stage existiera. Encontrado vía
// terraform/aws-smoke-test, ver ROADMAP.md.
func (s *Service) deleteStage(w http.ResponseWriter, restApiID, stageName string) {
	if err := s.db.Delete(stagesBucket, restApiID+"/"+stageName); err != nil {
		writeError(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// invoke resuelve restApiID+stageName+proxyPath contra el árbol de
// recursos desplegado y, si encuentra un método con integración AWS_PROXY,
// la invoca contra el servicio Lambda en proceso (ver lambdaInvoker). Es la
// implementación de "resource -> integration -> Lambda invocation" del
// ROADMAP — solo soporta AWS_PROXY, no MOCK/HTTP.
func (s *Service) invoke(w http.ResponseWriter, r *http.Request, restApiID, stageName, proxyPath string) {
	var st Stage
	if found, _ := s.db.Get(stagesBucket, restApiID+"/"+stageName, &st); !found {
		writeError(w, http.StatusNotFound, "NotFoundException",
			"no existe el stage "+stageName+" de la API "+restApiID+" (¿falta CreateDeployment?)")
		return
	}

	if proxyPath == "" {
		proxyPath = "/"
	}

	res, ok := s.findResource(restApiID, proxyPath)
	if !ok {
		writeError(w, http.StatusNotFound, "NotFoundException", "ningún recurso desplegado matchea el path: "+proxyPath)
		return
	}

	var integ Integration
	key := restApiID + "/" + res.ID + "/" + r.Method
	if found, _ := s.db.Get(integrationsBucket, key, &integ); !found {
		// ANY también cubre cualquier método cuando el recurso fue
		// configurado con PutMethod --http-method ANY, igual que AWS real.
		if found, _ = s.db.Get(integrationsBucket, restApiID+"/"+res.ID+"/ANY", &integ); !found {
			writeError(w, http.StatusNotFound, "NotFoundException",
				"no hay integración configurada para "+r.Method+" "+proxyPath)
			return
		}
	}

	if integ.Type != "AWS_PROXY" {
		writeError(w, http.StatusNotImplemented, "NotImplementedException",
			"este emulador solo soporta integraciones AWS_PROXY, no "+integ.Type)
		return
	}

	functionName := functionNameFromURI(integ.URI)
	if functionName == "" || s.lambda == nil {
		writeError(w, http.StatusInternalServerError, "InternalServerError",
			"no se pudo resolver la función Lambda de la integración (uri: "+integ.URI+")")
		return
	}

	s.invokeLambdaProxy(w, r, functionName, proxyPath)
}

// findResource hace un matching simple del path contra los recursos
// guardados: primero exacto, después por segmentos permitiendo
// {param}/{proxy+} como wildcard de un segmento — alcanza para el caso de
// uso de "proxyear todo a una función" sin reimplementar el motor de
// mapping de API Gateway real.
func (s *Service) findResource(restApiID, path string) (Resource, bool) {
	var resources []Resource
	_ = s.db.List(resourcesBucket, restApiID+"/", func(_ string, raw []byte) error {
		var res Resource
		if err := json.Unmarshal(raw, &res); err == nil {
			resources = append(resources, res)
		}
		return nil
	})

	want := strings.Split(strings.Trim(path, "/"), "/")
	if len(want) == 1 && want[0] == "" {
		want = nil
	}

	var best Resource
	bestScore := -1
	for _, res := range resources {
		have := strings.Split(strings.Trim(res.Path, "/"), "/")
		if len(have) == 1 && have[0] == "" {
			have = nil
		}
		if score, ok := matchSegments(have, want); ok && score > bestScore {
			best, bestScore = res, score
		}
	}
	return best, bestScore >= 0
}

// matchSegments compara los segmentos de un recurso registrado contra los
// del path solicitado. Coincidencia exacta puntúa más alto que un
// comodín ({id}, {proxy+}), para que un recurso literal gane por sobre uno
// genérico si ambos matchean.
func matchSegments(have, want []string) (score int, ok bool) {
	for i, h := range have {
		isProxyTail := strings.HasSuffix(h, "+}")
		if isProxyTail {
			return score, true // {proxy+} consume el resto del path
		}
		if i >= len(want) {
			return 0, false
		}
		if strings.HasPrefix(h, "{") && strings.HasSuffix(h, "}") {
			continue // {param}: matchea cualquier segmento, sin sumar score
		}
		if h != want[i] {
			return 0, false
		}
		score++
	}
	if len(want) != len(have) {
		return 0, false
	}
	return score, true
}

func functionNameFromURI(uri string) string {
	// uri típica:
	// arn:aws:apigateway:{region}:lambda:path/2015-03-31/functions/arn:aws:lambda:{region}:{account}:function:{name}/invocations
	i := strings.LastIndex(uri, ":function:")
	if i == -1 {
		return ""
	}
	rest := uri[i+len(":function:"):]
	rest = strings.TrimSuffix(rest, "/invocations")
	return rest
}

// invokeLambdaProxy construye un evento de Lambda Proxy Integration mínimo
// y lo manda al servicio Lambda en proceso (vía httptest.Recorder, sin
// round-trip de red), después traduce la respuesta de vuelta a HTTP.
func (s *Service) invokeLambdaProxy(w http.ResponseWriter, r *http.Request, functionName, path string) {
	bodyBytes := new(bytes.Buffer)
	_, _ = bodyBytes.ReadFrom(r.Body)

	headers := map[string]string{}
	for k := range r.Header {
		headers[k] = r.Header.Get(k)
	}
	qs := map[string]string{}
	for k, v := range r.URL.Query() {
		if len(v) > 0 {
			qs[k] = v[0]
		}
	}

	event := map[string]any{
		"resource":              path,
		"path":                  path,
		"httpMethod":            r.Method,
		"headers":               headers,
		"queryStringParameters": qs,
		"body":                  bodyBytes.String(),
		"isBase64Encoded":       false,
	}
	payload, _ := json.Marshal(event)

	invokeReq := httptest.NewRequest(http.MethodPost, "/2015-03-31/functions/"+functionName+"/invocations", bytes.NewReader(payload))
	rec := httptest.NewRecorder()
	s.lambda.ServeHTTP(rec, invokeReq)

	if rec.Code >= 300 {
		writeError(w, rec.Code, "InternalServerError", "la invocación Lambda subyacente falló: "+rec.Body.String())
		return
	}

	// Respuesta esperada de un handler AWS_PROXY: {statusCode, headers,
	// body, isBase64Encoded}. Si la función no devuelve ese shape (p. ej.
	// el stub de eco de lambda.Service cuando no hay
	// EMULATOR_INVOKE_COMMAND configurado), se hace fallback a 200 +
	// el body crudo, para que igual se pueda probar el wiring sin tener
	// que escribir un handler real.
	var proxyResp struct {
		StatusCode      int               `json:"statusCode"`
		Headers         map[string]string `json:"headers"`
		Body            string            `json:"body"`
		IsBase64Encoded bool              `json:"isBase64Encoded"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &proxyResp); err != nil || proxyResp.StatusCode == 0 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(rec.Body.Bytes())
		return
	}

	for k, v := range proxyResp.Headers {
		w.Header().Set(k, v)
	}
	w.WriteHeader(proxyResp.StatusCode)
	if proxyResp.IsBase64Encoded {
		decoded, err := base64.StdEncoding.DecodeString(proxyResp.Body)
		if err == nil {
			_, _ = w.Write(decoded)
			return
		}
	}
	_, _ = w.Write([]byte(proxyResp.Body))
}
