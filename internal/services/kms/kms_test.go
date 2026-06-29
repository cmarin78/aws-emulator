package kms

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func jsonRequest(action string, body map[string]any) *http.Request {
	raw, _ := json.Marshal(body)
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(string(raw)))
	r.Header.Set("X-Amz-Target", "TrentService."+action)
	r.Header.Set("Content-Type", "application/x-amz-json-1.1")
	return r
}

func doKMS(svc *Service, action string, body map[string]any) (*httptest.ResponseRecorder, map[string]any) {
	w := httptest.NewRecorder()
	svc.ServeHTTP(w, jsonRequest(action, body))
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w, out
}

// TestEncryptThenDecrypt_RoundTrips cubre el camino central: el
// CiphertextBlob "stub" producido por Encrypt debe poder pasarse de
// vuelta a Decrypt y recuperar tanto el plaintext original como el KeyId,
// sin que el caller tenga que volver a pasar el KeyId (igual que la API
// real).
func TestEncryptThenDecrypt_RoundTrips(t *testing.T) {
	svc := New()
	plaintext := base64.StdEncoding.EncodeToString([]byte("dato-secreto"))

	w, out := doKMS(svc, "Encrypt", map[string]any{"KeyId": "alias/mi-llave", "Plaintext": plaintext})
	if w.Code != http.StatusOK {
		t.Fatalf("Encrypt: status = %d, body = %s", w.Code, w.Body.String())
	}
	blob, _ := out["CiphertextBlob"].(string)
	if blob == "" {
		t.Fatalf("Encrypt: esperaba CiphertextBlob no vacío")
	}
	if out["KeyId"] != "alias/mi-llave" {
		t.Fatalf("Encrypt: KeyId = %v, esperaba alias/mi-llave", out["KeyId"])
	}

	w, out = doKMS(svc, "Decrypt", map[string]any{"CiphertextBlob": blob})
	if w.Code != http.StatusOK {
		t.Fatalf("Decrypt: status = %d, body = %s", w.Code, w.Body.String())
	}
	if out["KeyId"] != "alias/mi-llave" {
		t.Fatalf("Decrypt: KeyId = %v, esperaba alias/mi-llave (recuperado del blob, sin pasarlo)", out["KeyId"])
	}
	gotPlaintext, _ := base64.StdEncoding.DecodeString(out["Plaintext"].(string))
	if string(gotPlaintext) != "dato-secreto" {
		t.Fatalf("Decrypt: Plaintext = %q, esperaba dato-secreto", gotPlaintext)
	}
}

// TestEncrypt_DefaultsKeyIDWhenMissing cubre que, sin KeyId explícito, se
// usa la alias por default del emulador.
func TestEncrypt_DefaultsKeyIDWhenMissing(t *testing.T) {
	svc := New()
	plaintext := base64.StdEncoding.EncodeToString([]byte("x"))

	_, out := doKMS(svc, "Encrypt", map[string]any{"Plaintext": plaintext})
	if out["KeyId"] != defaultKeyID {
		t.Fatalf("Encrypt sin KeyId: KeyId = %v, esperaba %q", out["KeyId"], defaultKeyID)
	}
}

func TestEncrypt_InvalidPlaintextFails(t *testing.T) {
	svc := New()
	w, _ := doKMS(svc, "Encrypt", map[string]any{"Plaintext": "no-es-base64-!!!"})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("Encrypt con Plaintext inválido: status = %d, esperaba 400", w.Code)
	}
}

func TestDecrypt_InvalidCiphertextFails(t *testing.T) {
	svc := New()
	w, _ := doKMS(svc, "Decrypt", map[string]any{"CiphertextBlob": "no-es-base64-!!!"})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("Decrypt con CiphertextBlob inválido: status = %d, esperaba 400", w.Code)
	}
}

func TestGenerateDataKey_DefaultsTo32Bytes(t *testing.T) {
	svc := New()
	w, out := doKMS(svc, "GenerateDataKey", map[string]any{"KeyId": "alias/mi-llave"})
	if w.Code != http.StatusOK {
		t.Fatalf("GenerateDataKey: status = %d, body = %s", w.Code, w.Body.String())
	}
	plaintext, _ := base64.StdEncoding.DecodeString(out["Plaintext"].(string))
	if len(plaintext) != 32 {
		t.Fatalf("GenerateDataKey: len(Plaintext) = %d, esperaba 32 (AES_256 default)", len(plaintext))
	}

	// El CiphertextBlob devuelto debe poder decodificarse vía Decrypt,
	// igual que con Encrypt.
	_, decOut := doKMS(svc, "Decrypt", map[string]any{"CiphertextBlob": out["CiphertextBlob"]})
	gotPlaintext, _ := base64.StdEncoding.DecodeString(decOut["Plaintext"].(string))
	if string(gotPlaintext) != string(plaintext) {
		t.Fatalf("Decrypt sobre CiphertextBlob de GenerateDataKey: no coincide el plaintext")
	}
}

func TestGenerateDataKey_RespectsKeySpec(t *testing.T) {
	svc := New()
	w, out := doKMS(svc, "GenerateDataKey", map[string]any{"KeySpec": "AES_128"})
	if w.Code != http.StatusOK {
		t.Fatalf("GenerateDataKey con KeySpec=AES_128: status = %d, body = %s", w.Code, w.Body.String())
	}
	plaintext, _ := base64.StdEncoding.DecodeString(out["Plaintext"].(string))
	if len(plaintext) != 16 {
		t.Fatalf("GenerateDataKey con KeySpec=AES_128: len(Plaintext) = %d, esperaba 16", len(plaintext))
	}
}

func TestGenerateDataKey_RespectsNumberOfBytes(t *testing.T) {
	svc := New()
	w, out := doKMS(svc, "GenerateDataKey", map[string]any{"NumberOfBytes": float64(8)})
	if w.Code != http.StatusOK {
		t.Fatalf("GenerateDataKey con NumberOfBytes=8: status = %d, body = %s", w.Code, w.Body.String())
	}
	plaintext, _ := base64.StdEncoding.DecodeString(out["Plaintext"].(string))
	if len(plaintext) != 8 {
		t.Fatalf("GenerateDataKey con NumberOfBytes=8: len(Plaintext) = %d, esperaba 8", len(plaintext))
	}
}

func TestServeHTTP_UnknownActionFails(t *testing.T) {
	svc := New()
	w, _ := doKMS(svc, "CreateKey", map[string]any{})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("acción no soportada: status = %d, esperaba 400", w.Code)
	}
}

func TestReset_IsNoOp(t *testing.T) {
	svc := New()
	if err := svc.Reset(); err != nil {
		t.Fatalf("Reset: %v", err)
	}
}
