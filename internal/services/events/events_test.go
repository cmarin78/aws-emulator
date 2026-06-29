package events

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cesarmarin/aws-emulator/internal/services/sns"
	"github.com/cesarmarin/aws-emulator/internal/services/sqs"
	"github.com/cesarmarin/aws-emulator/internal/storage"
)

func newTestService(t *testing.T) (*Service, *sqs.Service, *sns.Service) {
	t.Helper()
	db, err := storage.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	sqsSvc := sqs.New(db)
	snsSvc := sns.New(db, sqsSvc)
	return New(db, sqsSvc, snsSvc), sqsSvc, snsSvc
}

func jsonRequest(action string, body map[string]any) *http.Request {
	raw, _ := json.Marshal(body)
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(string(raw)))
	r.Header.Set("X-Amz-Target", "AWSEvents."+action)
	r.Header.Set("Content-Type", "application/x-amz-json-1.1")
	return r
}

func doEvents(svc *Service, action string, body map[string]any) (*httptest.ResponseRecorder, map[string]any) {
	w := httptest.NewRecorder()
	svc.ServeHTTP(w, jsonRequest(action, body))
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w, out
}

func queryRequest(action string, form url.Values) *http.Request {
	form = cloneValues(form)
	form.Set("Action", action)
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return r
}

func cloneValues(v url.Values) url.Values {
	out := url.Values{}
	for k, vals := range v {
		out[k] = append([]string{}, vals...)
	}
	return out
}

func TestPutRuleAndDescribeRule(t *testing.T) {
	svc, _, _ := newTestService(t)
	w, out := doEvents(svc, "PutRule", map[string]any{"Name": "r1"})
	if w.Code != http.StatusOK {
		t.Fatalf("PutRule: status = %d, body = %s", w.Code, w.Body.String())
	}
	arn, _ := out["RuleArn"].(string)
	if !strings.Contains(arn, "rule/default/r1") {
		t.Fatalf("RuleArn = %q, esperaba que contenga rule/default/r1", arn)
	}

	w, out = doEvents(svc, "DescribeRule", map[string]any{"Name": "r1"})
	if w.Code != http.StatusOK {
		t.Fatalf("DescribeRule: status = %d, body = %s", w.Code, w.Body.String())
	}
	if out["State"] != "ENABLED" {
		t.Fatalf("DescribeRule State = %v, esperaba ENABLED por default", out["State"])
	}
}

func TestPutRule_RequiresName(t *testing.T) {
	svc, _, _ := newTestService(t)
	w, _ := doEvents(svc, "PutRule", map[string]any{})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("PutRule sin Name: status = %d, esperaba 400", w.Code)
	}
}

func TestDescribeRule_NotFound(t *testing.T) {
	svc, _, _ := newTestService(t)
	w, _ := doEvents(svc, "DescribeRule", map[string]any{"Name": "nope"})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("DescribeRule inexistente: status = %d, esperaba 400", w.Code)
	}
}

func TestDeleteRule_AlsoRemovesTargets(t *testing.T) {
	svc, _, _ := newTestService(t)
	doEvents(svc, "PutRule", map[string]any{"Name": "r1"})
	doEvents(svc, "PutTargets", map[string]any{
		"Rule": "r1",
		"Targets": []any{
			map[string]any{"Id": "t1", "Arn": "arn:aws:sqs:us-east-1:000000000000:q1"},
		},
	})

	w, _ := doEvents(svc, "DeleteRule", map[string]any{"Name": "r1"})
	if w.Code != http.StatusOK {
		t.Fatalf("DeleteRule: status = %d, body = %s", w.Code, w.Body.String())
	}

	w, out := doEvents(svc, "ListTargetsByRule", map[string]any{"Rule": "r1"})
	targets, _ := out["Targets"].([]any)
	if len(targets) != 0 {
		t.Fatalf("ListTargetsByRule tras DeleteRule: esperaba sin targets, body = %s", w.Body.String())
	}
}

func TestListRules_ReturnsAllRules(t *testing.T) {
	svc, _, _ := newTestService(t)
	doEvents(svc, "PutRule", map[string]any{"Name": "r1"})
	doEvents(svc, "PutRule", map[string]any{"Name": "r2"})

	w, out := doEvents(svc, "ListRules", map[string]any{})
	if w.Code != http.StatusOK {
		t.Fatalf("ListRules: status = %d, body = %s", w.Code, w.Body.String())
	}
	rules, _ := out["Rules"].([]any)
	if len(rules) != 2 {
		t.Fatalf("ListRules: esperaba 2 reglas, body = %s", w.Body.String())
	}
}

func TestPutTargetsAndListTargetsByRule(t *testing.T) {
	svc, _, _ := newTestService(t)
	doEvents(svc, "PutRule", map[string]any{"Name": "r1"})

	w, _ := doEvents(svc, "PutTargets", map[string]any{
		"Rule": "r1",
		"Targets": []any{
			map[string]any{"Id": "t1", "Arn": "arn:aws:sqs:us-east-1:000000000000:q1"},
		},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("PutTargets: status = %d, body = %s", w.Code, w.Body.String())
	}

	w, out := doEvents(svc, "ListTargetsByRule", map[string]any{"Rule": "r1"})
	targets, _ := out["Targets"].([]any)
	if len(targets) != 1 {
		t.Fatalf("ListTargetsByRule: esperaba 1 target, body = %s", w.Body.String())
	}
}

func TestRemoveTargets(t *testing.T) {
	svc, _, _ := newTestService(t)
	doEvents(svc, "PutRule", map[string]any{"Name": "r1"})
	doEvents(svc, "PutTargets", map[string]any{
		"Rule": "r1",
		"Targets": []any{
			map[string]any{"Id": "t1", "Arn": "arn:aws:sqs:us-east-1:000000000000:q1"},
		},
	})

	w, _ := doEvents(svc, "RemoveTargets", map[string]any{"Rule": "r1", "Ids": []any{"t1"}})
	if w.Code != http.StatusOK {
		t.Fatalf("RemoveTargets: status = %d, body = %s", w.Code, w.Body.String())
	}

	_, out := doEvents(svc, "ListTargetsByRule", map[string]any{"Rule": "r1"})
	targets, _ := out["Targets"].([]any)
	if len(targets) != 0 {
		t.Fatalf("ListTargetsByRule tras RemoveTargets: esperaba sin targets")
	}
}

// TestPutEvents_DeliversToSQSTargetWhenPatternMatches cubre el camino
// central: una regla con EventPattern sobre "source", un target SQS, y un
// evento que matchea el pattern debe terminar entregado en la cola.
func TestPutEvents_DeliversToSQSTargetWhenPatternMatches(t *testing.T) {
	svc, sqsSvc, _ := newTestService(t)
	sqsSvc.ServeHTTP(httptest.NewRecorder(), queryRequest("CreateQueue", url.Values{"QueueName": {"q1"}}))

	doEvents(svc, "PutRule", map[string]any{
		"Name":         "r1",
		"EventPattern": `{"source":["myapp"]}`,
	})
	doEvents(svc, "PutTargets", map[string]any{
		"Rule": "r1",
		"Targets": []any{
			map[string]any{"Id": "t1", "Arn": "arn:aws:sqs:us-east-1:000000000000:q1"},
		},
	})

	w, out := doEvents(svc, "PutEvents", map[string]any{
		"Entries": []any{
			map[string]any{"Source": "myapp", "DetailType": "test", "Detail": `{"key":"value"}`},
		},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("PutEvents: status = %d, body = %s", w.Code, w.Body.String())
	}
	if failed, _ := out["FailedEntryCount"].(float64); failed != 0 {
		t.Fatalf("PutEvents FailedEntryCount = %v, esperaba 0", out["FailedEntryCount"])
	}

	wr := httptest.NewRecorder()
	sqsSvc.ServeHTTP(wr, queryRequest("ReceiveMessage", url.Values{"QueueName": {"q1"}}))
	if !strings.Contains(wr.Body.String(), "source") || !strings.Contains(wr.Body.String(), "myapp") {
		t.Fatalf("ReceiveMessage en q1: esperaba ver el evento entregado, body = %s", wr.Body.String())
	}
}

// TestPutEvents_DoesNotDeliverWhenPatternDoesNotMatch verifica que un
// evento que no matchea el EventPattern de la regla no se entregue.
func TestPutEvents_DoesNotDeliverWhenPatternDoesNotMatch(t *testing.T) {
	svc, sqsSvc, _ := newTestService(t)
	sqsSvc.ServeHTTP(httptest.NewRecorder(), queryRequest("CreateQueue", url.Values{"QueueName": {"q1"}}))

	doEvents(svc, "PutRule", map[string]any{
		"Name":         "r1",
		"EventPattern": `{"source":["otherapp"]}`,
	})
	doEvents(svc, "PutTargets", map[string]any{
		"Rule": "r1",
		"Targets": []any{
			map[string]any{"Id": "t1", "Arn": "arn:aws:sqs:us-east-1:000000000000:q1"},
		},
	})

	doEvents(svc, "PutEvents", map[string]any{
		"Entries": []any{
			map[string]any{"Source": "myapp", "DetailType": "test", "Detail": `{}`},
		},
	})

	w := httptest.NewRecorder()
	sqsSvc.ServeHTTP(w, queryRequest("ReceiveMessage", url.Values{"QueueName": {"q1"}}))
	if strings.Contains(w.Body.String(), "<Message>") {
		t.Fatalf("ReceiveMessage en q1: no esperaba entrega porque el pattern no matchea, body = %s", w.Body.String())
	}
}

// TestPutEvents_DisabledRuleDoesNotDeliver verifica que las reglas con
// State=DISABLED se excluyan de loadEnabledRules.
func TestPutEvents_DisabledRuleDoesNotDeliver(t *testing.T) {
	svc, sqsSvc, _ := newTestService(t)
	sqsSvc.ServeHTTP(httptest.NewRecorder(), queryRequest("CreateQueue", url.Values{"QueueName": {"q1"}}))

	doEvents(svc, "PutRule", map[string]any{"Name": "r1", "State": "DISABLED"})
	doEvents(svc, "PutTargets", map[string]any{
		"Rule": "r1",
		"Targets": []any{
			map[string]any{"Id": "t1", "Arn": "arn:aws:sqs:us-east-1:000000000000:q1"},
		},
	})

	doEvents(svc, "PutEvents", map[string]any{
		"Entries": []any{
			map[string]any{"Source": "myapp", "DetailType": "test", "Detail": `{}`},
		},
	})

	w := httptest.NewRecorder()
	sqsSvc.ServeHTTP(w, queryRequest("ReceiveMessage", url.Values{"QueueName": {"q1"}}))
	if strings.Contains(w.Body.String(), "<Message>") {
		t.Fatalf("ReceiveMessage en q1: regla DISABLED no debería entregar, body = %s", w.Body.String())
	}
}

// TestPutEvents_DeliversToSNSTarget cubre el target SNS (en vez de SQS),
// usando sns.Service.PublishMessage.
func TestPutEvents_DeliversToSNSTarget(t *testing.T) {
	svc, sqsSvc, snsSvc := newTestService(t)
	sqsSvc.ServeHTTP(httptest.NewRecorder(), queryRequest("CreateQueue", url.Values{"QueueName": {"q1"}}))

	w := httptest.NewRecorder()
	snsSvc.ServeHTTP(w, queryRequest("CreateTopic", url.Values{"Name": {"t1"}}))
	topicArn := extractTag(w.Body.String(), "TopicArn")
	snsSvc.ServeHTTP(httptest.NewRecorder(), queryRequest("Subscribe", url.Values{
		"TopicArn": {topicArn}, "Protocol": {"sqs"}, "Endpoint": {"arn:aws:sqs:us-east-1:000000000000:q1"},
	}))

	doEvents(svc, "PutRule", map[string]any{"Name": "r1"})
	doEvents(svc, "PutTargets", map[string]any{
		"Rule": "r1",
		"Targets": []any{
			map[string]any{"Id": "t1", "Arn": topicArn},
		},
	})

	doEvents(svc, "PutEvents", map[string]any{
		"Entries": []any{
			map[string]any{"Source": "myapp", "DetailType": "test", "Detail": `{}`},
		},
	})

	wr := httptest.NewRecorder()
	sqsSvc.ServeHTTP(wr, queryRequest("ReceiveMessage", url.Values{"QueueName": {"q1"}}))
	if !strings.Contains(wr.Body.String(), "source") || !strings.Contains(wr.Body.String(), "myapp") {
		t.Fatalf("ReceiveMessage en q1: esperaba el evento entregado vía SNS->SQS, body = %s", wr.Body.String())
	}
}

func extractTag(body, tag string) string {
	open, close := "<"+tag+">", "</"+tag+">"
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

func TestListTagsForResource_AlwaysEmpty(t *testing.T) {
	svc, _, _ := newTestService(t)
	w, out := doEvents(svc, "ListTagsForResource", map[string]any{})
	if w.Code != http.StatusOK {
		t.Fatalf("ListTagsForResource: status = %d", w.Code)
	}
	tags, _ := out["Tags"].([]any)
	if len(tags) != 0 {
		t.Fatalf("Tags = %v, esperaba vacío", tags)
	}
}

func TestServeHTTP_UnknownActionFails(t *testing.T) {
	svc, _, _ := newTestService(t)
	w, _ := doEvents(svc, "CreateEventBus", map[string]any{})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("acción no soportada: status = %d, esperaba 400", w.Code)
	}
}

func TestReset_ClearsRulesAndTargets(t *testing.T) {
	svc, _, _ := newTestService(t)
	doEvents(svc, "PutRule", map[string]any{"Name": "r1"})

	if err := svc.Reset(); err != nil {
		t.Fatalf("Reset: %v", err)
	}

	_, out := doEvents(svc, "ListRules", map[string]any{})
	rules, _ := out["Rules"].([]any)
	if len(rules) != 0 {
		t.Fatalf("ListRules tras Reset: esperaba vacío")
	}
}
