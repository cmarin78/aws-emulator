// Package secretsmanager emula el subconjunto básico de AWS Secrets
// Manager: CreateSecret, GetSecretValue, PutSecretValue, DeleteSecret,
// DescribeSecret y ListSecrets.
//
// Protocolo real confirmado con `aws secretsmanager create-secret --debug`:
// X-Amz-Target: secretsmanager.{Action} — en minúscula, a diferencia de la
// mayoría de servicios JSON (que usan algo como Logs_20140328 o
// DynamoDB_20120810) — Content-Type: application/x-amz-json-1.1, cuerpo
// JSON con keys PascalCase. El nombre de servicio en el credential scope
// SigV4 es "secretsmanager" (sin guion); botocore usa internamente
// "secrets-manager" (con guion) solo en sus nombres de hooks de eventos,
// no en el wire protocol real.
//
// Sin cifrado real: los secretos se guardan en texto plano en BoltDB, igual
// que SSM SecureString — este es un emulador de desarrollo, no un KMS.
package secretsmanager

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/cesarmarin/aws-emulator/internal/server"
	"github.com/cesarmarin/aws-emulator/internal/storage"
)

// newUUID genera un UUID v4 sin depender de un módulo externo (este
// proyecto solo usa go.etcd.io/bbolt como dependencia directa).
func newUUID() string {
	buf := make([]byte, 16)
	_, _ = rand.Read(buf)
	buf[6] = (buf[6] & 0x0f) | 0x40 // versión 4
	buf[8] = (buf[8] & 0x3f) | 0x80 // variante RFC 4122
	return hex.EncodeToString(buf[0:4]) + "-" +
		hex.EncodeToString(buf[4:6]) + "-" +
		hex.EncodeToString(buf[6:8]) + "-" +
		hex.EncodeToString(buf[8:10]) + "-" +
		hex.EncodeToString(buf[10:16])
}

const (
	secretsBucket = "secretsmanager.secrets"
	accountID     = "000000000000"
)

// Service agrupa el estado del servicio Secrets Manager.
type Service struct {
	db *storage.DB
}

// New crea el servicio Secrets Manager.
func New(db *storage.DB) *Service {
	return &Service{db: db}
}

// secretVersion es una versión guardada del valor de un secreto.
type secretVersion struct {
	VersionID     string   `json:"versionId"`
	SecretString  string   `json:"secretString,omitempty"`
	SecretBinary  string   `json:"secretBinary,omitempty"`
	VersionStages []string `json:"versionStages"`
	CreatedDate   int64    `json:"createdDate"`
}

// Secret es la forma persistida de un secreto, con su historial de versiones.
type Secret struct {
	Name             string          `json:"name"`
	Arn              string          `json:"arn"`
	Description      string          `json:"description,omitempty"`
	KmsKeyID         string          `json:"kmsKeyId,omitempty"`
	CreatedDate      int64           `json:"createdDate"`
	LastChangedDate  int64           `json:"lastChangedDate"`
	Versions         []secretVersion `json:"versions"`
	CurrentVersionID string          `json:"currentVersionId"`
	DeletedDate      int64           `json:"deletedDate,omitempty"`
}

func secretArn(name string) string {
	suffix := randomSuffix()
	return "arn:aws:secretsmanager:us-east-1:" + accountID + ":secret:" + name + "-" + suffix
}

func randomSuffix() string {
	const alphabet = "abcdefghijklmnopqrstuvwxyz"
	buf := make([]byte, 6)
	_, _ = rand.Read(buf)
	out := make([]byte, 6)
	for i, b := range buf {
		out[i] = alphabet[int(b)%len(alphabet)]
	}
	return string(out)
}

func nowMillis() int64 {
	return time.Now().UTC().UnixMilli()
}

func (s *Service) currentVersion(secret *Secret) *secretVersion {
	for i := range secret.Versions {
		if secret.Versions[i].VersionID == secret.CurrentVersionID {
			return &secret.Versions[i]
		}
	}
	return nil
}

func (s *Service) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	target := r.Header.Get("X-Amz-Target")
	_, action, _ := strings.Cut(target, ".")

	body, _ := decodeJSONBody(r)

	switch action {
	case "CreateSecret":
		s.createSecret(w, body)
	case "GetSecretValue":
		s.getSecretValue(w, body)
	case "PutSecretValue":
		s.putSecretValue(w, body)
	case "DeleteSecret":
		s.deleteSecret(w, body)
	case "DescribeSecret":
		s.describeSecret(w, body)
	case "ListSecrets":
		s.listSecrets(w, body)
	default:
		server.WriteJSONError(w, http.StatusBadRequest, "InvalidAction",
			"acción Secrets Manager no soportada en este emulador: "+action)
	}
}

// Reset limpia todo el estado persistido de Secrets Manager. Implementa
// server.Resettable.
func (s *Service) Reset() error {
	return s.db.Reset(secretsBucket)
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

func (s *Service) createSecret(w http.ResponseWriter, body map[string]any) {
	name, _ := body["Name"].(string)
	if name == "" {
		server.WriteJSONError(w, http.StatusBadRequest, "ValidationException", "Name es requerido")
		return
	}
	var existing Secret
	if found, _ := s.db.Get(secretsBucket, name, &existing); found && existing.DeletedDate == 0 {
		server.WriteJSONError(w, http.StatusBadRequest, "ResourceExistsException",
			"el secreto ya existe: "+name)
		return
	}

	secretString, _ := body["SecretString"].(string)
	secretBinary, _ := body["SecretBinary"].(string)
	clientRequestToken, _ := body["ClientRequestToken"].(string)
	if clientRequestToken == "" {
		clientRequestToken = newUUID()
	}
	description, _ := body["Description"].(string)
	kmsKeyID, _ := body["KmsKeyId"].(string)

	now := nowMillis()
	ver := secretVersion{
		VersionID:     clientRequestToken,
		SecretString:  secretString,
		SecretBinary:  secretBinary,
		VersionStages: []string{"AWSCURRENT"},
		CreatedDate:   now,
	}
	secret := Secret{
		Name:             name,
		Arn:              secretArn(name),
		Description:      description,
		KmsKeyID:         kmsKeyID,
		CreatedDate:      now,
		LastChangedDate:  now,
		Versions:         []secretVersion{ver},
		CurrentVersionID: ver.VersionID,
	}
	if err := s.db.Put(secretsBucket, name, secret); err != nil {
		server.WriteJSONError(w, http.StatusInternalServerError, "InternalServiceError", err.Error())
		return
	}
	server.WriteJSON(w, http.StatusOK, map[string]any{
		"ARN":       secret.Arn,
		"Name":      secret.Name,
		"VersionId": ver.VersionID,
	})
}

func (s *Service) getSecretValue(w http.ResponseWriter, body map[string]any) {
	id, _ := body["SecretId"].(string)
	requestedVersionID, _ := body["VersionId"].(string)
	requestedStage, _ := body["VersionStage"].(string)

	var secret Secret
	found, _ := s.db.Get(secretsBucket, id, &secret)
	if !found || secret.DeletedDate != 0 {
		server.WriteJSONError(w, http.StatusBadRequest, "ResourceNotFoundException",
			"no existe el secreto: "+id)
		return
	}

	var ver *secretVersion
	switch {
	case requestedVersionID != "":
		for i := range secret.Versions {
			if secret.Versions[i].VersionID == requestedVersionID {
				ver = &secret.Versions[i]
				break
			}
		}
	case requestedStage != "":
		for i := range secret.Versions {
			for _, st := range secret.Versions[i].VersionStages {
				if st == requestedStage {
					ver = &secret.Versions[i]
					break
				}
			}
		}
	default:
		ver = s.currentVersion(&secret)
	}
	if ver == nil {
		server.WriteJSONError(w, http.StatusBadRequest, "ResourceNotFoundException",
			"no se encontró la versión solicitada del secreto: "+id)
		return
	}

	resp := map[string]any{
		"ARN":           secret.Arn,
		"Name":          secret.Name,
		"VersionId":     ver.VersionID,
		"VersionStages": ver.VersionStages,
		"CreatedDate":   float64(ver.CreatedDate) / 1000,
	}
	if ver.SecretString != "" {
		resp["SecretString"] = ver.SecretString
	}
	if ver.SecretBinary != "" {
		resp["SecretBinary"] = ver.SecretBinary
	}
	server.WriteJSON(w, http.StatusOK, resp)
}

func (s *Service) putSecretValue(w http.ResponseWriter, body map[string]any) {
	id, _ := body["SecretId"].(string)

	var secret Secret
	found, _ := s.db.Get(secretsBucket, id, &secret)
	if !found || secret.DeletedDate != 0 {
		server.WriteJSONError(w, http.StatusBadRequest, "ResourceNotFoundException",
			"no existe el secreto: "+id)
		return
	}

	secretString, _ := body["SecretString"].(string)
	secretBinary, _ := body["SecretBinary"].(string)
	clientRequestToken, _ := body["ClientRequestToken"].(string)
	if clientRequestToken == "" {
		clientRequestToken = newUUID()
	}

	// la nueva versión pasa a ser AWSCURRENT; la anterior pierde esa marca
	// (queda con AWSPREVIOUS, como en el servicio real).
	for i := range secret.Versions {
		stages := secret.Versions[i].VersionStages[:0]
		for _, st := range secret.Versions[i].VersionStages {
			if st == "AWSCURRENT" {
				stages = append(stages, "AWSPREVIOUS")
			} else if st != "AWSPREVIOUS" {
				stages = append(stages, st)
			}
		}
		secret.Versions[i].VersionStages = stages
	}

	now := nowMillis()
	ver := secretVersion{
		VersionID:     clientRequestToken,
		SecretString:  secretString,
		SecretBinary:  secretBinary,
		VersionStages: []string{"AWSCURRENT"},
		CreatedDate:   now,
	}
	secret.Versions = append(secret.Versions, ver)
	secret.CurrentVersionID = ver.VersionID
	secret.LastChangedDate = now

	if err := s.db.Put(secretsBucket, id, secret); err != nil {
		server.WriteJSONError(w, http.StatusInternalServerError, "InternalServiceError", err.Error())
		return
	}
	server.WriteJSON(w, http.StatusOK, map[string]any{
		"ARN":           secret.Arn,
		"Name":          secret.Name,
		"VersionId":     ver.VersionID,
		"VersionStages": ver.VersionStages,
	})
}

func (s *Service) deleteSecret(w http.ResponseWriter, body map[string]any) {
	id, _ := body["SecretId"].(string)
	forceDelete, _ := body["ForceDeleteWithoutRecovery"].(bool)

	var secret Secret
	found, _ := s.db.Get(secretsBucket, id, &secret)
	if !found {
		server.WriteJSONError(w, http.StatusBadRequest, "ResourceNotFoundException",
			"no existe el secreto: "+id)
		return
	}

	now := nowMillis()
	if forceDelete {
		if err := s.db.Delete(secretsBucket, id); err != nil {
			server.WriteJSONError(w, http.StatusInternalServerError, "InternalServiceError", err.Error())
			return
		}
	} else {
		secret.DeletedDate = now
		if err := s.db.Put(secretsBucket, id, secret); err != nil {
			server.WriteJSONError(w, http.StatusInternalServerError, "InternalServiceError", err.Error())
			return
		}
	}
	server.WriteJSON(w, http.StatusOK, map[string]any{
		"ARN":          secret.Arn,
		"Name":         secret.Name,
		"DeletionDate": float64(now) / 1000,
	})
}

func (s *Service) describeSecret(w http.ResponseWriter, body map[string]any) {
	id, _ := body["SecretId"].(string)
	var secret Secret
	found, _ := s.db.Get(secretsBucket, id, &secret)
	if !found {
		server.WriteJSONError(w, http.StatusBadRequest, "ResourceNotFoundException",
			"no existe el secreto: "+id)
		return
	}

	versionIDsToStages := map[string][]string{}
	for _, v := range secret.Versions {
		versionIDsToStages[v.VersionID] = v.VersionStages
	}

	resp := map[string]any{
		"ARN":                secret.Arn,
		"Name":               secret.Name,
		"Description":        secret.Description,
		"CreatedDate":        float64(secret.CreatedDate) / 1000,
		"LastChangedDate":    float64(secret.LastChangedDate) / 1000,
		"VersionIdsToStages": versionIDsToStages,
	}
	if secret.DeletedDate != 0 {
		resp["DeletedDate"] = float64(secret.DeletedDate) / 1000
	}
	server.WriteJSON(w, http.StatusOK, resp)
}

func (s *Service) listSecrets(w http.ResponseWriter, _ map[string]any) {
	var secrets []Secret
	_ = s.db.List(secretsBucket, "", func(_ string, raw []byte) error {
		var sec Secret
		if err := json.Unmarshal(raw, &sec); err == nil {
			secrets = append(secrets, sec)
		}
		return nil
	})
	sort.Slice(secrets, func(i, j int) bool { return secrets[i].Name < secrets[j].Name })

	out := make([]map[string]any, 0, len(secrets))
	for _, sec := range secrets {
		entry := map[string]any{
			"ARN":             sec.Arn,
			"Name":            sec.Name,
			"Description":     sec.Description,
			"CreatedDate":     float64(sec.CreatedDate) / 1000,
			"LastChangedDate": float64(sec.LastChangedDate) / 1000,
		}
		if sec.DeletedDate != 0 {
			entry["DeletedDate"] = float64(sec.DeletedDate) / 1000
		}
		out = append(out, entry)
	}
	server.WriteJSON(w, http.StatusOK, map[string]any{"SecretList": out})
}
