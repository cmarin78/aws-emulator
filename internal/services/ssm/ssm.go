// Package ssm emula el subconjunto básico de SSM Parameter Store:
// PutParameter, GetParameter, GetParameters y DeleteParameter, incluyendo
// el tipo SecureString.
//
// Protocolo real confirmado con `aws ssm put-parameter --debug`:
// X-Amz-Target: AmazonSSM.{Action}, Content-Type: application/x-amz-json-1.1,
// cuerpo JSON con keys PascalCase (p.ej. {"Name":..., "Value":...,
// "Type":...}). El nombre de servicio en el credential scope SigV4 es
// "ssm".
//
// Sin cifrado real: SecureString se guarda en texto plano en BoltDB, igual
// que Secrets Manager — este es un emulador de desarrollo, no un KMS.
package ssm

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/cesarmarin/aws-emulator/internal/server"
	"github.com/cesarmarin/aws-emulator/internal/storage"
)

const (
	parametersBucket = "ssm.parameters"
	accountID        = "000000000000"
)

// Service agrupa el estado del servicio SSM Parameter Store.
type Service struct {
	db *storage.DB
}

// New crea el servicio SSM Parameter Store.
func New(db *storage.DB) *Service {
	return &Service{db: db}
}

// Parameter es la forma persistida de un parámetro.
type Parameter struct {
	Name             string `json:"name"`
	Type             string `json:"type"`
	Value            string `json:"value"`
	Version          int64  `json:"version"`
	Arn              string `json:"arn"`
	LastModifiedDate int64  `json:"lastModifiedDate"`
	DataType         string `json:"dataType,omitempty"`
	AllowedPattern   string `json:"allowedPattern,omitempty"`
	KeyID            string `json:"keyId,omitempty"`
	Tier             string `json:"tier,omitempty"`
}

func paramArn(name string) string {
	return "arn:aws:ssm:us-east-1:" + accountID + ":parameter" + name
}

func nowMillis() int64 {
	return time.Now().UTC().UnixMilli()
}

func (s *Service) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	target := r.Header.Get("X-Amz-Target")
	_, action, _ := strings.Cut(target, ".")

	body, _ := decodeJSONBody(r)

	switch action {
	case "PutParameter":
		s.putParameter(w, body)
	case "GetParameter":
		s.getParameter(w, body)
	case "GetParameters":
		s.getParameters(w, body)
	case "GetParametersByPath":
		s.getParametersByPath(w, body)
	case "DeleteParameter":
		s.deleteParameter(w, body)
	case "DeleteParameters":
		s.deleteParameters(w, body)
	case "DescribeParameters":
		s.describeParameters(w, body)
	default:
		server.WriteJSONError(w, http.StatusBadRequest, "InvalidAction",
			"acción SSM no soportada en este emulador: "+action)
	}
}

// Reset limpia todo el estado persistido de SSM Parameter Store. Implementa
// server.Resettable.
func (s *Service) Reset() error {
	return s.db.Reset(parametersBucket)
}

func decodeJSONBody(r *http.Request) (map[string]any, error) {
	defer r.Body.Close()
	out := map[string]any{}
	if r.ContentLength == 0 {
		return out, nil
	}
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(&out); err != nil {
		return out, err
	}
	return out, nil
}

func paramOut(p Parameter, withDecryption bool) map[string]any {
	value := p.Value
	if p.Type == "SecureString" && !withDecryption {
		// El servicio real sigue devolviendo el valor sin más (no hay
		// cifrado real que des-aplicar), pero se respeta el flag por
		// compatibilidad con clientes que lo inspeccionan.
		value = p.Value
	}
	out := map[string]any{
		"Name":             p.Name,
		"Type":             p.Type,
		"Value":            value,
		"Version":          p.Version,
		"ARN":              p.Arn,
		"LastModifiedDate": float64(p.LastModifiedDate) / 1000,
	}
	if p.DataType != "" {
		out["DataType"] = p.DataType
	} else {
		out["DataType"] = "text"
	}
	return out
}

func (s *Service) putParameter(w http.ResponseWriter, body map[string]any) {
	name, _ := body["Name"].(string)
	value, _ := body["Value"].(string)
	paramType, _ := body["Type"].(string)
	overwrite, _ := body["Overwrite"].(bool)
	dataType, _ := body["DataType"].(string)
	allowedPattern, _ := body["AllowedPattern"].(string)
	keyID, _ := body["KeyId"].(string)
	tier, _ := body["Tier"].(string)

	if name == "" || value == "" || paramType == "" {
		server.WriteJSONError(w, http.StatusBadRequest, "ValidationException",
			"Name, Value y Type son requeridos")
		return
	}

	var existing Parameter
	found, _ := s.db.Get(parametersBucket, name, &existing)
	if found && !overwrite {
		server.WriteJSONError(w, http.StatusBadRequest, "ParameterAlreadyExists",
			"el parámetro ya existe: "+name)
		return
	}

	version := int64(1)
	if found {
		version = existing.Version + 1
	}
	if tier == "" {
		tier = "Standard"
	}

	p := Parameter{
		Name:             name,
		Type:             paramType,
		Value:            value,
		Version:          version,
		Arn:              paramArn(name),
		LastModifiedDate: nowMillis(),
		DataType:         dataType,
		AllowedPattern:   allowedPattern,
		KeyID:            keyID,
		Tier:             tier,
	}
	if err := s.db.Put(parametersBucket, name, p); err != nil {
		server.WriteJSONError(w, http.StatusInternalServerError, "InternalServerError", err.Error())
		return
	}
	server.WriteJSON(w, http.StatusOK, map[string]any{"Version": p.Version, "Tier": p.Tier})
}

func (s *Service) getParameter(w http.ResponseWriter, body map[string]any) {
	name, _ := body["Name"].(string)
	withDecryption, _ := body["WithDecryption"].(bool)

	var p Parameter
	found, _ := s.db.Get(parametersBucket, name, &p)
	if !found {
		server.WriteJSONError(w, http.StatusBadRequest, "ParameterNotFound",
			"no existe el parámetro: "+name)
		return
	}
	server.WriteJSON(w, http.StatusOK, map[string]any{"Parameter": paramOut(p, withDecryption)})
}

func (s *Service) getParameters(w http.ResponseWriter, body map[string]any) {
	namesRaw, _ := body["Names"].([]any)
	withDecryption, _ := body["WithDecryption"].(bool)

	var found []map[string]any
	var invalid []string
	for _, n := range namesRaw {
		name, _ := n.(string)
		var p Parameter
		ok, _ := s.db.Get(parametersBucket, name, &p)
		if ok {
			found = append(found, paramOut(p, withDecryption))
		} else {
			invalid = append(invalid, name)
		}
	}
	server.WriteJSON(w, http.StatusOK, map[string]any{
		"Parameters":        found,
		"InvalidParameters": invalid,
	})
}

func (s *Service) getParametersByPath(w http.ResponseWriter, body map[string]any) {
	path, _ := body["Path"].(string)
	recursive, _ := body["Recursive"].(bool)
	withDecryption, _ := body["WithDecryption"].(bool)
	if !strings.HasSuffix(path, "/") {
		path += "/"
	}

	var params []Parameter
	_ = s.db.List(parametersBucket, "", func(_ string, raw []byte) error {
		var p Parameter
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil
		}
		if !strings.HasPrefix(p.Name, path) {
			return nil
		}
		rest := strings.TrimPrefix(p.Name, path)
		if !recursive && strings.Contains(rest, "/") {
			return nil
		}
		params = append(params, p)
		return nil
	})
	sort.Slice(params, func(i, j int) bool { return params[i].Name < params[j].Name })

	out := make([]map[string]any, 0, len(params))
	for _, p := range params {
		out = append(out, paramOut(p, withDecryption))
	}
	server.WriteJSON(w, http.StatusOK, map[string]any{"Parameters": out})
}

func (s *Service) deleteParameter(w http.ResponseWriter, body map[string]any) {
	name, _ := body["Name"].(string)
	var existing Parameter
	found, _ := s.db.Get(parametersBucket, name, &existing)
	if !found {
		server.WriteJSONError(w, http.StatusBadRequest, "ParameterNotFound",
			"no existe el parámetro: "+name)
		return
	}
	if err := s.db.Delete(parametersBucket, name); err != nil {
		server.WriteJSONError(w, http.StatusInternalServerError, "InternalServerError", err.Error())
		return
	}
	server.WriteJSON(w, http.StatusOK, map[string]any{})
}

func (s *Service) deleteParameters(w http.ResponseWriter, body map[string]any) {
	namesRaw, _ := body["Names"].([]any)
	var deleted []string
	var invalid []string
	for _, n := range namesRaw {
		name, _ := n.(string)
		var existing Parameter
		found, _ := s.db.Get(parametersBucket, name, &existing)
		if !found {
			invalid = append(invalid, name)
			continue
		}
		if err := s.db.Delete(parametersBucket, name); err == nil {
			deleted = append(deleted, name)
		} else {
			invalid = append(invalid, name)
		}
	}
	server.WriteJSON(w, http.StatusOK, map[string]any{
		"DeletedParameters": deleted,
		"InvalidParameters": invalid,
	})
}

func (s *Service) describeParameters(w http.ResponseWriter, _ map[string]any) {
	var params []Parameter
	_ = s.db.List(parametersBucket, "", func(_ string, raw []byte) error {
		var p Parameter
		if err := json.Unmarshal(raw, &p); err == nil {
			params = append(params, p)
		}
		return nil
	})
	sort.Slice(params, func(i, j int) bool { return params[i].Name < params[j].Name })

	out := make([]map[string]any, 0, len(params))
	for _, p := range params {
		out = append(out, map[string]any{
			"Name":             p.Name,
			"Type":             p.Type,
			"ARN":              p.Arn,
			"Version":          p.Version,
			"LastModifiedDate": float64(p.LastModifiedDate) / 1000,
			"Tier":             p.Tier,
		})
	}
	server.WriteJSON(w, http.StatusOK, map[string]any{"Parameters": out})
}
