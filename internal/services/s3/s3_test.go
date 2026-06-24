package s3

import (
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

func TestBucketAndObjectLifecycle(t *testing.T) {
	svc := newTestService(t)

	// Crear bucket.
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPut, "/mybucket", nil)
	svc.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("createBucket: status = %d, body = %s", w.Code, w.Body.String())
	}

	// Listar buckets: debe aparecer.
	w = httptest.NewRecorder()
	r = httptest.NewRequest(http.MethodGet, "/", nil)
	svc.ServeHTTP(w, r)
	if !strings.Contains(w.Body.String(), "<Name>mybucket</Name>") {
		t.Fatalf("listBuckets: esperaba ver mybucket, body = %s", w.Body.String())
	}

	// Subir objeto.
	w = httptest.NewRecorder()
	r = httptest.NewRequest(http.MethodPut, "/mybucket/hello.txt", strings.NewReader("hola mundo"))
	svc.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("putObject: status = %d, body = %s", w.Code, w.Body.String())
	}
	if w.Header().Get("ETag") == "" {
		t.Fatalf("putObject: esperaba header ETag")
	}

	// Descargar objeto.
	w = httptest.NewRecorder()
	r = httptest.NewRequest(http.MethodGet, "/mybucket/hello.txt", nil)
	svc.ServeHTTP(w, r)
	if w.Code != http.StatusOK || w.Body.String() != "hola mundo" {
		t.Fatalf("getObject: status = %d, body = %q", w.Code, w.Body.String())
	}

	// Listar objetos del bucket.
	w = httptest.NewRecorder()
	r = httptest.NewRequest(http.MethodGet, "/mybucket", nil)
	svc.ServeHTTP(w, r)
	if !strings.Contains(w.Body.String(), "<Key>hello.txt</Key>") {
		t.Fatalf("listObjects: esperaba ver hello.txt, body = %s", w.Body.String())
	}

	// Borrar objeto.
	w = httptest.NewRecorder()
	r = httptest.NewRequest(http.MethodDelete, "/mybucket/hello.txt", nil)
	svc.ServeHTTP(w, r)
	if w.Code != http.StatusNoContent {
		t.Fatalf("deleteObject: status = %d", w.Code)
	}

	// Borrar bucket (ahora vacío) debe funcionar.
	w = httptest.NewRecorder()
	r = httptest.NewRequest(http.MethodDelete, "/mybucket", nil)
	svc.ServeHTTP(w, r)
	if w.Code != http.StatusNoContent {
		t.Fatalf("deleteBucket: status = %d, body = %s", w.Code, w.Body.String())
	}
}

func TestDeleteNonEmptyBucketFails(t *testing.T) {
	svc := newTestService(t)

	w := httptest.NewRecorder()
	svc.ServeHTTP(w, httptest.NewRequest(http.MethodPut, "/b", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("createBucket: status = %d", w.Code)
	}

	w = httptest.NewRecorder()
	svc.ServeHTTP(w, httptest.NewRequest(http.MethodPut, "/b/k", strings.NewReader("x")))
	if w.Code != http.StatusOK {
		t.Fatalf("putObject: status = %d", w.Code)
	}

	w = httptest.NewRecorder()
	svc.ServeHTTP(w, httptest.NewRequest(http.MethodDelete, "/b", nil))
	if w.Code != http.StatusConflict {
		t.Fatalf("deleteBucket no vacío: status = %d, esperaba 409", w.Code)
	}
}

func TestGetMissingObjectReturns404(t *testing.T) {
	svc := newTestService(t)
	w := httptest.NewRecorder()
	svc.ServeHTTP(w, httptest.NewRequest(http.MethodPut, "/b", nil))

	w = httptest.NewRecorder()
	svc.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/b/nope.txt", nil))
	if w.Code != http.StatusNotFound {
		t.Fatalf("getObject inexistente: status = %d, esperaba 404", w.Code)
	}
}
