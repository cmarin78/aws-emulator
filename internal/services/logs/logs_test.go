package logs

import (
	"encoding/json"
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

func jsonRequest(action string, body map[string]any) *http.Request {
	raw, _ := json.Marshal(body)
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(string(raw)))
	r.Header.Set("X-Amz-Target", "Logs_20140328."+action)
	r.Header.Set("Content-Type", "application/x-amz-json-1.1")
	return r
}

func doLogs(svc *Service, action string, body map[string]any) (*httptest.ResponseRecorder, map[string]any) {
	w := httptest.NewRecorder()
	svc.ServeHTTP(w, jsonRequest(action, body))
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w, out
}

func TestCreateLogGroup_AndDescribeLogGroups(t *testing.T) {
	svc := newTestService(t)
	w, _ := doLogs(svc, "CreateLogGroup", map[string]any{"logGroupName": "/app/g1"})
	if w.Code != http.StatusOK {
		t.Fatalf("CreateLogGroup: status = %d, body = %s", w.Code, w.Body.String())
	}

	w, out := doLogs(svc, "DescribeLogGroups", map[string]any{})
	if w.Code != http.StatusOK {
		t.Fatalf("DescribeLogGroups: status = %d, body = %s", w.Code, w.Body.String())
	}
	groups, _ := out["logGroups"].([]any)
	if len(groups) != 1 {
		t.Fatalf("DescribeLogGroups: esperaba 1 grupo, body = %s", w.Body.String())
	}
}

func TestCreateLogGroup_RequiresName(t *testing.T) {
	svc := newTestService(t)
	w, _ := doLogs(svc, "CreateLogGroup", map[string]any{})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("CreateLogGroup sin logGroupName: status = %d, esperaba 400", w.Code)
	}
}

func TestCreateLogGroup_AlreadyExists(t *testing.T) {
	svc := newTestService(t)
	doLogs(svc, "CreateLogGroup", map[string]any{"logGroupName": "g1"})
	w, _ := doLogs(svc, "CreateLogGroup", map[string]any{"logGroupName": "g1"})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("CreateLogGroup duplicado: status = %d, esperaba 400", w.Code)
	}
}

func TestDeleteLogGroup_AlsoRemovesStreamsAndEvents(t *testing.T) {
	svc := newTestService(t)
	doLogs(svc, "CreateLogGroup", map[string]any{"logGroupName": "g1"})
	doLogs(svc, "CreateLogStream", map[string]any{"logGroupName": "g1", "logStreamName": "s1"})
	doLogs(svc, "PutLogEvents", map[string]any{
		"logGroupName": "g1", "logStreamName": "s1",
		"logEvents": []any{map[string]any{"timestamp": float64(1000), "message": "hola"}},
	})

	w, _ := doLogs(svc, "DeleteLogGroup", map[string]any{"logGroupName": "g1"})
	if w.Code != http.StatusOK {
		t.Fatalf("DeleteLogGroup: status = %d, body = %s", w.Code, w.Body.String())
	}

	_, out := doLogs(svc, "DescribeLogStreams", map[string]any{"logGroupName": "g1"})
	streams, _ := out["logStreams"].([]any)
	if len(streams) != 0 {
		t.Fatalf("DescribeLogStreams tras DeleteLogGroup: esperaba sin streams")
	}
}

func TestCreateLogStream_AndDescribeLogStreams(t *testing.T) {
	svc := newTestService(t)
	doLogs(svc, "CreateLogGroup", map[string]any{"logGroupName": "g1"})

	w, _ := doLogs(svc, "CreateLogStream", map[string]any{"logGroupName": "g1", "logStreamName": "s1"})
	if w.Code != http.StatusOK {
		t.Fatalf("CreateLogStream: status = %d, body = %s", w.Code, w.Body.String())
	}

	w, out := doLogs(svc, "DescribeLogStreams", map[string]any{"logGroupName": "g1"})
	if w.Code != http.StatusOK {
		t.Fatalf("DescribeLogStreams: status = %d, body = %s", w.Code, w.Body.String())
	}
	streams, _ := out["logStreams"].([]any)
	if len(streams) != 1 {
		t.Fatalf("DescribeLogStreams: esperaba 1 stream, body = %s", w.Body.String())
	}
}

func TestCreateLogStream_RequiresGroupAndName(t *testing.T) {
	svc := newTestService(t)
	w, _ := doLogs(svc, "CreateLogStream", map[string]any{"logGroupName": "g1"})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("CreateLogStream sin logStreamName: status = %d, esperaba 400", w.Code)
	}
}

func TestDeleteLogStream_RemovesStreamAndEvents(t *testing.T) {
	svc := newTestService(t)
	doLogs(svc, "CreateLogGroup", map[string]any{"logGroupName": "g1"})
	doLogs(svc, "CreateLogStream", map[string]any{"logGroupName": "g1", "logStreamName": "s1"})
	doLogs(svc, "PutLogEvents", map[string]any{
		"logGroupName": "g1", "logStreamName": "s1",
		"logEvents": []any{map[string]any{"timestamp": float64(1000), "message": "hola"}},
	})

	w, _ := doLogs(svc, "DeleteLogStream", map[string]any{"logGroupName": "g1", "logStreamName": "s1"})
	if w.Code != http.StatusOK {
		t.Fatalf("DeleteLogStream: status = %d, body = %s", w.Code, w.Body.String())
	}

	_, out := doLogs(svc, "DescribeLogStreams", map[string]any{"logGroupName": "g1"})
	streams, _ := out["logStreams"].([]any)
	if len(streams) != 0 {
		t.Fatalf("DescribeLogStreams tras DeleteLogStream: esperaba sin streams")
	}
}

// TestPutLogEvents_RequiresExistingStream cubre la validación: PutLogEvents
// sobre un stream que no existe debe fallar.
func TestPutLogEvents_RequiresExistingStream(t *testing.T) {
	svc := newTestService(t)
	w, _ := doLogs(svc, "PutLogEvents", map[string]any{
		"logGroupName": "g1", "logStreamName": "nope",
		"logEvents": []any{map[string]any{"timestamp": float64(1000), "message": "x"}},
	})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("PutLogEvents sobre stream inexistente: status = %d, esperaba 400", w.Code)
	}
}

func TestPutLogEvents_AndGetLogEvents(t *testing.T) {
	svc := newTestService(t)
	doLogs(svc, "CreateLogGroup", map[string]any{"logGroupName": "g1"})
	doLogs(svc, "CreateLogStream", map[string]any{"logGroupName": "g1", "logStreamName": "s1"})

	w, out := doLogs(svc, "PutLogEvents", map[string]any{
		"logGroupName": "g1", "logStreamName": "s1",
		"logEvents": []any{
			map[string]any{"timestamp": float64(2000), "message": "segundo"},
			map[string]any{"timestamp": float64(1000), "message": "primero"},
		},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("PutLogEvents: status = %d, body = %s", w.Code, w.Body.String())
	}
	if out["nextSequenceToken"] == "" || out["nextSequenceToken"] == nil {
		t.Fatalf("PutLogEvents: esperaba nextSequenceToken no vacío")
	}

	w, out = doLogs(svc, "GetLogEvents", map[string]any{"logGroupName": "g1", "logStreamName": "s1"})
	if w.Code != http.StatusOK {
		t.Fatalf("GetLogEvents: status = %d, body = %s", w.Code, w.Body.String())
	}
	events, _ := out["events"].([]any)
	if len(events) != 2 {
		t.Fatalf("GetLogEvents: esperaba 2 eventos, body = %s", w.Body.String())
	}
	// GetLogEvents ordena por timestamp ascendente.
	first, _ := events[0].(map[string]any)
	if first["message"] != "primero" {
		t.Fatalf("GetLogEvents: esperaba orden ascendente por timestamp, primer evento = %v", first["message"])
	}
}

// TestFilterLogEvents_FiltersByPatternAndTimeRange cubre el filtrado por
// substring literal y por rango de tiempo (startTime/endTime), el
// subconjunto "simplificado a propósito" de FilterLogEvents.
func TestFilterLogEvents_FiltersByPatternAndTimeRange(t *testing.T) {
	svc := newTestService(t)
	doLogs(svc, "CreateLogGroup", map[string]any{"logGroupName": "g1"})
	doLogs(svc, "CreateLogStream", map[string]any{"logGroupName": "g1", "logStreamName": "s1"})
	doLogs(svc, "PutLogEvents", map[string]any{
		"logGroupName": "g1", "logStreamName": "s1",
		"logEvents": []any{
			map[string]any{"timestamp": float64(1000), "message": "error: algo falló"},
			map[string]any{"timestamp": float64(2000), "message": "info: todo bien"},
			map[string]any{"timestamp": float64(3000), "message": "error: otro problema"},
		},
	})

	w, out := doLogs(svc, "FilterLogEvents", map[string]any{
		"logGroupName":  "g1",
		"filterPattern": "error",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("FilterLogEvents: status = %d, body = %s", w.Code, w.Body.String())
	}
	events, _ := out["events"].([]any)
	if len(events) != 2 {
		t.Fatalf("FilterLogEvents con filterPattern=error: esperaba 2 eventos, body = %s", w.Body.String())
	}

	w, out = doLogs(svc, "FilterLogEvents", map[string]any{
		"logGroupName": "g1",
		"startTime":    float64(1500),
		"endTime":      float64(2500),
	})
	if w.Code != http.StatusOK {
		t.Fatalf("FilterLogEvents con rango: status = %d, body = %s", w.Code, w.Body.String())
	}
	events, _ = out["events"].([]any)
	if len(events) != 1 {
		t.Fatalf("FilterLogEvents con rango [1500,2500): esperaba 1 evento, body = %s", w.Body.String())
	}
}

func TestListTagsForResource_AlwaysEmpty(t *testing.T) {
	svc := newTestService(t)
	w, out := doLogs(svc, "ListTagsForResource", map[string]any{})
	if w.Code != http.StatusOK {
		t.Fatalf("ListTagsForResource: status = %d", w.Code)
	}
	tags, _ := out["tags"].(map[string]any)
	if len(tags) != 0 {
		t.Fatalf("tags = %v, esperaba vacío", tags)
	}
}

func TestServeHTTP_UnknownActionFails(t *testing.T) {
	svc := newTestService(t)
	w, _ := doLogs(svc, "PutRetentionPolicy", map[string]any{})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("acción no soportada: status = %d, esperaba 400", w.Code)
	}
}

func TestReset_ClearsGroupsStreamsAndEvents(t *testing.T) {
	svc := newTestService(t)
	doLogs(svc, "CreateLogGroup", map[string]any{"logGroupName": "g1"})

	if err := svc.Reset(); err != nil {
		t.Fatalf("Reset: %v", err)
	}

	_, out := doLogs(svc, "DescribeLogGroups", map[string]any{})
	groups, _ := out["logGroups"].([]any)
	if len(groups) != 0 {
		t.Fatalf("DescribeLogGroups tras Reset: esperaba vacío")
	}
}
