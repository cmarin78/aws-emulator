// Package kms emula un subconjunto mínimo de KMS: Encrypt, Decrypt y
// GenerateDataKey, sin criptografía real (ver ROADMAP.md, Fase 5 — el
// objetivo es que los SDKs que llaman a KMS como efecto colateral (p. ej.
// para "encriptar" un secreto antes de guardarlo en otro servicio) no
// fallen, no proveer confidencialidad real).
//
// Protocolo real confirmado con `aws kms encrypt --debug`: X-Amz-Target:
// TrentService.{Action} (sí, "TrentService" — nombre interno histórico de
// KMS en AWS, no un typo), Content-Type: application/x-amz-json-1.1,
// cuerpo JSON con keys PascalCase (KeyId, Plaintext, CiphertextBlob, todos
// los blobs en base64). El nombre de servicio en el credential scope SigV4
// es "kms".
//
// "Cifrado" stub: en vez de devolver bytes realmente cifrados, CiphertextBlob
// es simplemente el KeyId + Plaintext empaquetados y reexpresados en base64
// (ver makeCiphertext/parseCiphertext). Esto alcanza para que Encrypt/Decrypt
// hagan roundtrip correctamente (incluyendo recuperar el KeyId en Decrypt sin
// que el caller lo pase, como hace la API real) sin necesitar gestión de
// llaves ni una librería de criptografía real.
package kms

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/cesarmarin/aws-emulator/internal/server"
)

const defaultKeyID = "alias/aws-emulator-default"

// Service agrupa el estado del servicio KMS. No tiene persistencia propia:
// no hay gestión de llaves real, así que no hay nada que guardar en BoltDB
// (a diferencia del resto de los servicios de este proyecto).
type Service struct{}

// New crea el servicio KMS.
func New() *Service {
	return &Service{}
}

func (s *Service) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	target := r.Header.Get("X-Amz-Target")
	_, action, _ := strings.Cut(target, ".")

	body, _ := decodeJSONBody(r)

	switch action {
	case "Encrypt":
		s.encrypt(w, body)
	case "Decrypt":
		s.decrypt(w, body)
	case "GenerateDataKey":
		s.generateDataKey(w, body)
	default:
		server.WriteJSONError(w, http.StatusBadRequest, "InvalidAction",
			"acción KMS no soportada en este emulador: "+action)
	}
}

// Reset no hace nada: KMS no tiene estado persistido en este emulador.
// Implementa server.Resettable de todos modos por consistencia con el
// resto de los servicios (y por si en el futuro se agrega CreateKey/
// gestión real de llaves, que sí necesitaría limpiarse).
func (s *Service) Reset() error {
	return nil
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

// makeCiphertext empaqueta keyID+plaintext en el formato que este emulador
// usa como "CiphertextBlob": no es cifrado real, solo permite que Decrypt
// recupere ambos valores después (igual que la API real, donde Decrypt no
// requiere que el caller vuelva a pasar el KeyId).
func makeCiphertext(keyID string, plaintext []byte) string {
	idBytes := []byte(keyID)
	buf := make([]byte, 0, 2+len(idBytes)+len(plaintext))
	buf = append(buf, byte(len(idBytes)))
	buf = append(buf, idBytes...)
	buf = append(buf, plaintext...)
	return base64.StdEncoding.EncodeToString(buf)
}

func parseCiphertext(blobB64 string) (keyID string, plaintext []byte, err error) {
	buf, err := base64.StdEncoding.DecodeString(blobB64)
	if err != nil {
		return "", nil, err
	}
	if len(buf) < 1 {
		return "", nil, errors.New("ciphertext blob inválido")
	}
	n := int(buf[0])
	if len(buf) < 1+n {
		return "", nil, errors.New("ciphertext blob inválido")
	}
	return string(buf[1 : 1+n]), buf[1+n:], nil
}

func (s *Service) encrypt(w http.ResponseWriter, body map[string]any) {
	keyID, _ := body["KeyId"].(string)
	plaintextB64, _ := body["Plaintext"].(string)
	if keyID == "" {
		keyID = defaultKeyID
	}
	plaintext, err := base64.StdEncoding.DecodeString(plaintextB64)
	if err != nil {
		server.WriteJSONError(w, http.StatusBadRequest, "InvalidCiphertextException",
			"Plaintext inválido: "+err.Error())
		return
	}
	server.WriteJSON(w, http.StatusOK, map[string]any{
		"CiphertextBlob":      makeCiphertext(keyID, plaintext),
		"KeyId":               keyID,
		"EncryptionAlgorithm": "SYMMETRIC_DEFAULT",
	})
}

func (s *Service) decrypt(w http.ResponseWriter, body map[string]any) {
	blob, _ := body["CiphertextBlob"].(string)
	keyID, plaintext, err := parseCiphertext(blob)
	if err != nil {
		server.WriteJSONError(w, http.StatusBadRequest, "InvalidCiphertextException",
			"CiphertextBlob inválido: "+err.Error())
		return
	}
	server.WriteJSON(w, http.StatusOK, map[string]any{
		"KeyId":               keyID,
		"Plaintext":           base64.StdEncoding.EncodeToString(plaintext),
		"EncryptionAlgorithm": "SYMMETRIC_DEFAULT",
	})
}

func (s *Service) generateDataKey(w http.ResponseWriter, body map[string]any) {
	keyID, _ := body["KeyId"].(string)
	if keyID == "" {
		keyID = defaultKeyID
	}
	numBytes := 32 // AES_256 por default, igual que el comportamiento real cuando no se especifica KeySpec/NumberOfBytes
	if spec, ok := body["KeySpec"].(string); ok {
		switch spec {
		case "AES_128":
			numBytes = 16
		case "AES_256":
			numBytes = 32
		}
	}
	if n, ok := body["NumberOfBytes"].(float64); ok && n > 0 {
		numBytes = int(n)
	}

	plaintext := make([]byte, numBytes)
	if _, err := rand.Read(plaintext); err != nil {
		server.WriteJSONError(w, http.StatusInternalServerError, "InternalServerError", err.Error())
		return
	}

	server.WriteJSON(w, http.StatusOK, map[string]any{
		"KeyId":          keyID,
		"Plaintext":      base64.StdEncoding.EncodeToString(plaintext),
		"CiphertextBlob": makeCiphertext(keyID, plaintext),
	})
}
