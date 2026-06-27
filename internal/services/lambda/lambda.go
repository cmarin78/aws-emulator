// Package lambda emula el subconjunto más usado de AWS Lambda:
// CreateFunction, GetFunction, ListFunctions, DeleteFunction e Invoke,
// con un protocolo distinto al resto de los servicios de este emulador:
// REST puro con rutas bajo /2015-03-31/functions/... y verbos HTTP
// estándar (GET/POST/DELETE) — sin X-Amz-Target ni Action, confirmado
// con `aws lambda invoke --debug` (ver ROADMAP.md). Por eso este servicio
// no usa router.FromHTTPRequest ni los helpers Query/JSON existentes:
// despacha directamente por método+path.
//
// "Local invocation only" significa que este emulador NO empaqueta ni
// ejecuta runtimes reales (Node/Python/Go en una sandbox Lambda). El
// código subido en CreateFunction se guarda tal cual (para CodeSha256 y
// para que `get-function`/`list-functions` se vean realistas), pero
// Invoke no lo desempaqueta. En su lugar:
//   - Si la función fue creada con la variable de entorno
//     EMULATOR_INVOKE_COMMAND, Invoke ejecuta ese comando como subproceso
//     local, le manda el payload por stdin y devuelve stdout como
//     respuesta — el "via a subprocess" del ROADMAP, útil para wirear
//     pruebas de infraestructura contra un handler local real sin tener
//     que reimplementar un runtime.
//   - Si no, Invoke simplemente hace eco del payload de entrada (stub
//     "in-process"), suficiente para validar que el wiring
//     trigger->Lambda funciona sin necesitar lógica de negocio real.
package lambda

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/cesarmarin/aws-emulator/internal/accountctx"
	"github.com/cesarmarin/aws-emulator/internal/server"
	"github.com/cesarmarin/aws-emulator/internal/storage"
)

const (
	functionsBucket = "lambda.functions"
)

var (
	functionPathRe = regexp.MustCompile(`^/2015-03-31/functions/([^/]+)$`)
	invokePathRe   = regexp.MustCompile(`^/2015-03-31/functions/([^/]+)/invocations$`)
	versionsPathRe = regexp.MustCompile(`^/2015-03-31/functions/([^/]+)/versions$`)
)

// Service agrupa el estado del servicio Lambda.
type Service struct {
	db *storage.DB
}

// New crea el servicio Lambda.
func New(db *storage.DB) *Service {
	return &Service{db: db}
}

// Function es la forma persistida de una función.
type Function struct {
	FunctionName string            `json:"functionName"`
	FunctionArn  string            `json:"functionArn"`
	Runtime      string            `json:"runtime"`
	Role         string            `json:"role"`
	Handler      string            `json:"handler"`
	CodeSha256   string            `json:"codeSha256"`
	CodeSize     int               `json:"codeSize"`
	Environment  map[string]string `json:"environment,omitempty"`
	LastModified time.Time         `json:"lastModified"`
}

func functionArn(accountID, name string) string {
	return "arn:aws:lambda:us-east-1:" + accountID + ":function:" + name
}

func (s *Service) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// El AWS CLI real manda GET/POST a "/2015-03-31/functions/" con barra
	// final (confirmado viendo el 404 real contra este mismo emulador: list
	// y create fallaban porque acá solo se comparaba contra la ruta sin
	// barra). Se la recorta antes de comparar para aceptar ambas formas sin
	// duplicar casos en el switch.
	path := strings.TrimSuffix(r.URL.Path, "/")
	if path == "" {
		path = "/"
	}

	switch {
	case r.Method == http.MethodPost && path == "/2015-03-31/functions":
		s.createFunction(w, r)
	case r.Method == http.MethodGet && path == "/2015-03-31/functions":
		s.listFunctions(w)
	case r.Method == http.MethodPost && invokePathRe.MatchString(path):
		name := invokePathRe.FindStringSubmatch(path)[1]
		s.invoke(w, r, name)
	case r.Method == http.MethodGet && versionsPathRe.MatchString(path):
		name := versionsPathRe.FindStringSubmatch(path)[1]
		s.listVersionsByFunction(w, name)
	case r.Method == http.MethodGet && functionPathRe.MatchString(path):
		name := functionPathRe.FindStringSubmatch(path)[1]
		s.getFunction(w, name)
	case r.Method == http.MethodDelete && functionPathRe.MatchString(path):
		name := functionPathRe.FindStringSubmatch(path)[1]
		s.deleteFunction(w, name)
	default:
		writeError(w, http.StatusNotFound, "ResourceNotFoundException",
			"ruta/método Lambda no soportado en este emulador: "+r.Method+" "+path)
	}
}

// Reset limpia todo el estado persistido de Lambda (funciones).
// Implementa server.Resettable.
func (s *Service) Reset() error {
	return s.db.Reset(functionsBucket)
}

// writeError replica el shape de error JSON de la API REST de Lambda:
// {"Message": "...", "Type": "..."} (a diferencia del "__type" usado por
// los servicios de protocolo JSON 1.0 clásico).
func writeError(w http.ResponseWriter, status int, errType, message string) {
	server.WriteJSON(w, status, map[string]any{"Message": message, "Type": errType})
}

type createFunctionRequest struct {
	FunctionName string `json:"FunctionName"`
	Runtime      string `json:"Runtime"`
	Role         string `json:"Role"`
	Handler      string `json:"Handler"`
	Code         struct {
		ZipFile string `json:"ZipFile"`
	} `json:"Code"`
	Environment struct {
		Variables map[string]string `json:"Variables"`
	} `json:"Environment"`
}

func (s *Service) createFunction(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()
	var req createFunctionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "InvalidRequestContentException", err.Error())
		return
	}
	if req.FunctionName == "" {
		writeError(w, http.StatusBadRequest, "InvalidParameterValueException", "FunctionName es requerido")
		return
	}

	var codeBytes []byte
	if req.Code.ZipFile != "" {
		decoded, err := base64.StdEncoding.DecodeString(req.Code.ZipFile)
		if err != nil {
			writeError(w, http.StatusBadRequest, "InvalidParameterValueException", "Code.ZipFile inválido: "+err.Error())
			return
		}
		codeBytes = decoded
	}
	sum := sha256.Sum256(codeBytes)
	accountID, _ := accountctx.FromContext(r.Context())

	fn := Function{
		FunctionName: req.FunctionName,
		FunctionArn:  functionArn(accountID, req.FunctionName),
		Runtime:      req.Runtime,
		Role:         req.Role,
		Handler:      req.Handler,
		CodeSha256:   hex.EncodeToString(sum[:]),
		CodeSize:     len(codeBytes),
		Environment:  req.Environment.Variables,
		LastModified: time.Now().UTC(),
	}
	if err := s.db.Put(functionsBucket, fn.FunctionName, fn); err != nil {
		writeError(w, http.StatusInternalServerError, "ServiceException", err.Error())
		return
	}
	server.WriteJSON(w, http.StatusCreated, configurationJSON(fn))
}

func configurationJSON(fn Function) map[string]any {
	return map[string]any{
		"FunctionName": fn.FunctionName,
		"FunctionArn":  fn.FunctionArn,
		"Runtime":      fn.Runtime,
		"Role":         fn.Role,
		"Handler":      fn.Handler,
		"CodeSha256":   fn.CodeSha256,
		"CodeSize":     fn.CodeSize,
		"LastModified": fn.LastModified.Format(time.RFC3339),
		"State":        "Active",
	}
}

func (s *Service) getFunction(w http.ResponseWriter, name string) {
	var fn Function
	if found, _ := s.db.Get(functionsBucket, name, &fn); !found {
		writeError(w, http.StatusNotFound, "ResourceNotFoundException", "la función no existe: "+name)
		return
	}
	server.WriteJSON(w, http.StatusOK, map[string]any{
		"Configuration": configurationJSON(fn),
		"Code":          map[string]any{"Location": ""},
	})
}

// listVersionsByFunction: este emulador no implementa versiones publicadas
// de Lambda (no hay PublishVersion) -- toda función solo tiene $LATEST.
// El provider de Terraform llama a esto durante el Read de
// aws_lambda_function para resolver la versión "actual", así que devolver
// solo $LATEST alcanza para no romper el apply. Encontrado vía
// terraform/aws-smoke-test, ver ROADMAP.md.
func (s *Service) listVersionsByFunction(w http.ResponseWriter, name string) {
	var fn Function
	if found, _ := s.db.Get(functionsBucket, name, &fn); !found {
		writeError(w, http.StatusNotFound, "ResourceNotFoundException", "la función no existe: "+name)
		return
	}
	cfg := configurationJSON(fn)
	cfg["Version"] = "$LATEST"
	server.WriteJSON(w, http.StatusOK, map[string]any{"Versions": []map[string]any{cfg}})
}

func (s *Service) listFunctions(w http.ResponseWriter) {
	var fns []Function
	_ = s.db.List(functionsBucket, "", func(_ string, raw []byte) error {
		var fn Function
		if err := json.Unmarshal(raw, &fn); err == nil {
			fns = append(fns, fn)
		}
		return nil
	})
	out := make([]map[string]any, 0, len(fns))
	for _, fn := range fns {
		out = append(out, configurationJSON(fn))
	}
	server.WriteJSON(w, http.StatusOK, map[string]any{"Functions": out})
}

func (s *Service) deleteFunction(w http.ResponseWriter, name string) {
	if err := s.db.Delete(functionsBucket, name); err != nil {
		writeError(w, http.StatusInternalServerError, "ServiceException", err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// invoke ejecuta la función "localmente": ver el comentario del paquete
// para la justificación de por qué esto no es un runtime real.
func (s *Service) invoke(w http.ResponseWriter, r *http.Request, name string) {
	defer r.Body.Close()
	var fn Function
	if found, _ := s.db.Get(functionsBucket, name, &fn); !found {
		writeError(w, http.StatusNotFound, "ResourceNotFoundException", "la función no existe: "+name)
		return
	}

	payload := new(bytes.Buffer)
	_, _ = payload.ReadFrom(r.Body)

	cmdline := strings.TrimSpace(fn.Environment["EMULATOR_INVOKE_COMMAND"])
	if cmdline == "" {
		// Stub in-process: devuelve el mismo payload recibido.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(payload.Bytes())
		return
	}

	cmd := exec.Command("sh", "-c", cmdline)
	cmd.Stdin = bytes.NewReader(payload.Bytes())
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		w.Header().Set("X-Amz-Function-Error", "Unhandled")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"errorMessage": err.Error(),
			"errorType":    "SubprocessError",
			"stderr":       stderr.String(),
		})
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(stdout.Bytes())
}
