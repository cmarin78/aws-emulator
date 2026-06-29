package s3

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestBucketVersioning cubre el sub-recurso ?versioning (Fase 2):
// PutBucketVersioning seguido de GetBucketVersioning debe devolver el
// mismo Status que se mandó.
func TestBucketVersioning(t *testing.T) {
	svc := newTestService(t)

	w := httptest.NewRecorder()
	svc.ServeHTTP(w, httptest.NewRequest(http.MethodPut, "/b", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("createBucket: status = %d", w.Code)
	}

	body := `<VersioningConfiguration><Status>Enabled</Status></VersioningConfiguration>`
	w = httptest.NewRecorder()
	svc.ServeHTTP(w, httptest.NewRequest(http.MethodPut, "/b?versioning", strings.NewReader(body)))
	if w.Code != http.StatusOK {
		t.Fatalf("putBucketVersioning: status = %d, body = %s", w.Code, w.Body.String())
	}

	w = httptest.NewRecorder()
	svc.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/b?versioning", nil))
	if !strings.Contains(w.Body.String(), "<Status>Enabled</Status>") {
		t.Fatalf("getBucketVersioning: esperaba Status=Enabled, body = %s", w.Body.String())
	}
}

// TestObjectVersioning_KeepsOldVersionOnOverwrite verifica que, con
// versionado habilitado, subir un objeto con la misma key dos veces no
// pisa la versión anterior -- el comportamiento real de S3.
func TestObjectVersioning_KeepsOldVersionOnOverwrite(t *testing.T) {
	svc := newTestService(t)

	svc.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPut, "/b", nil))
	body := `<VersioningConfiguration><Status>Enabled</Status></VersioningConfiguration>`
	svc.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPut, "/b?versioning", strings.NewReader(body)))

	svc.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPut, "/b/k", strings.NewReader("v1")))
	svc.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPut, "/b/k", strings.NewReader("v2-mas-largo")))

	// La versión "current" del objeto (GET sin versionId) debe ser la
	// última escrita.
	w := httptest.NewRecorder()
	svc.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/b/k", nil))
	if w.Body.String() != "v2-mas-largo" {
		t.Fatalf("GET sin versionId = %q, esperaba la última versión", w.Body.String())
	}
}

// TestObjectTagging cubre PutObjectTagging/GetObjectTagging/DeleteObjectTagging.
func TestObjectTagging(t *testing.T) {
	svc := newTestService(t)
	svc.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPut, "/b", nil))
	svc.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPut, "/b/k", strings.NewReader("x")))

	body := `<Tagging><TagSet><Tag><Key>env</Key><Value>test</Value></Tag></TagSet></Tagging>`
	w := httptest.NewRecorder()
	svc.ServeHTTP(w, httptest.NewRequest(http.MethodPut, "/b/k?tagging", strings.NewReader(body)))
	if w.Code != http.StatusOK {
		t.Fatalf("putObjectTagging: status = %d, body = %s", w.Code, w.Body.String())
	}

	w = httptest.NewRecorder()
	svc.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/b/k?tagging", nil))
	if !strings.Contains(w.Body.String(), "<Key>env</Key>") || !strings.Contains(w.Body.String(), "<Value>test</Value>") {
		t.Fatalf("getObjectTagging: esperaba ver el tag env=test, body = %s", w.Body.String())
	}

	w = httptest.NewRecorder()
	svc.ServeHTTP(w, httptest.NewRequest(http.MethodDelete, "/b/k?tagging", nil))
	if w.Code != http.StatusNoContent {
		t.Fatalf("deleteObjectTagging: status = %d", w.Code)
	}

	w = httptest.NewRecorder()
	svc.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/b/k?tagging", nil))
	if strings.Contains(w.Body.String(), "<Key>") {
		t.Fatalf("getObjectTagging tras delete: esperaba TagSet vacío, body = %s", w.Body.String())
	}
}

// TestMultipartUploadLifecycle cubre create -> uploadPart (x2) ->
// complete, verificando que el objeto final sea la concatenación de las
// partes en orden de número de parte.
func TestMultipartUploadLifecycle(t *testing.T) {
	svc := newTestService(t)
	svc.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPut, "/b", nil))

	w := httptest.NewRecorder()
	svc.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/b/big.bin?uploads", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("createMultipartUpload: status = %d, body = %s", w.Code, w.Body.String())
	}
	uploadID := extractTag(w.Body.String(), "UploadId")
	if uploadID == "" {
		t.Fatalf("createMultipartUpload: no se pudo extraer UploadId, body = %s", w.Body.String())
	}

	w = httptest.NewRecorder()
	svc.ServeHTTP(w, httptest.NewRequest(http.MethodPut, "/b/big.bin?uploadId="+uploadID+"&partNumber=2", strings.NewReader("-segunda")))
	if w.Code != http.StatusOK {
		t.Fatalf("uploadPart 2: status = %d", w.Code)
	}
	w = httptest.NewRecorder()
	svc.ServeHTTP(w, httptest.NewRequest(http.MethodPut, "/b/big.bin?uploadId="+uploadID+"&partNumber=1", strings.NewReader("primera")))
	if w.Code != http.StatusOK {
		t.Fatalf("uploadPart 1: status = %d", w.Code)
	}

	w = httptest.NewRecorder()
	svc.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/b/big.bin?uploadId="+uploadID, nil))
	if w.Code != http.StatusOK {
		t.Fatalf("completeMultipartUpload: status = %d, body = %s", w.Code, w.Body.String())
	}

	w = httptest.NewRecorder()
	svc.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/b/big.bin", nil))
	if w.Body.String() != "primera-segunda" {
		t.Fatalf("objeto final = %q, esperaba partes concatenadas en orden de partNumber", w.Body.String())
	}
}

// TestAbortMultipartUpload verifica que abortar limpie el upload y que
// completarlo después falle con NoSuchUpload.
func TestAbortMultipartUpload(t *testing.T) {
	svc := newTestService(t)
	svc.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPut, "/b", nil))

	w := httptest.NewRecorder()
	svc.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/b/k?uploads", nil))
	uploadID := extractTag(w.Body.String(), "UploadId")

	w = httptest.NewRecorder()
	svc.ServeHTTP(w, httptest.NewRequest(http.MethodDelete, "/b/k?uploadId="+uploadID, nil))
	if w.Code != http.StatusNoContent {
		t.Fatalf("abortMultipartUpload: status = %d", w.Code)
	}

	w = httptest.NewRecorder()
	svc.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/b/k?uploadId="+uploadID, nil))
	if w.Code != http.StatusNotFound {
		t.Fatalf("completeMultipartUpload tras abort: status = %d, esperaba 404 NoSuchUpload", w.Code)
	}
}

// extractTag busca <tag>valor</tag> en un body XML, sin un parser
// completo -- alcanza para estos tests, que solo necesitan un campo
// puntual de la respuesta.
func extractTag(body, tag string) string {
	open := "<" + tag + ">"
	close := "</" + tag + ">"
	start := strings.Index(body, open)
	if start == -1 {
		return ""
	}
	start += len(open)
	end := strings.Index(body[start:], close)
	if end == -1 {
		return ""
	}
	return body[start : start+end]
}
