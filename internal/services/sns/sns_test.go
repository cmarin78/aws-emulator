package sns

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cesarmarin/aws-emulator/internal/accountctx"
	"github.com/cesarmarin/aws-emulator/internal/services/sqs"
	"github.com/cesarmarin/aws-emulator/internal/storage"
)

func newTestService(t *testing.T) (*Service, *sqs.Service) {
	t.Helper()
	db, err := storage.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	sqsSvc := sqs.New(db)
	return New(db, sqsSvc), sqsSvc
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

func TestCreateTopic_IsIdempotent(t *testing.T) {
	svc, _ := newTestService(t)
	w := httptest.NewRecorder()
	svc.ServeHTTP(w, queryRequest("CreateTopic", url.Values{"Name": {"t1"}}))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "<TopicArn>") {
		t.Fatalf("CreateTopic: status = %d, body = %s", w.Code, w.Body.String())
	}
	arn1 := extractTag(w.Body.String(), "TopicArn")

	w = httptest.NewRecorder()
	svc.ServeHTTP(w, queryRequest("CreateTopic", url.Values{"Name": {"t1"}}))
	arn2 := extractTag(w.Body.String(), "TopicArn")
	if arn1 != arn2 {
		t.Fatalf("CreateTopic duplicado: esperaba el mismo ARN, %q != %q", arn1, arn2)
	}

	w = httptest.NewRecorder()
	svc.ServeHTTP(w, queryRequest("ListTopics", url.Values{}))
	if strings.Count(w.Body.String(), "<TopicArn>") != 1 {
		t.Fatalf("ListTopics: esperaba un solo tópico tras CreateTopic duplicado, body = %s", w.Body.String())
	}
}

func TestDeleteTopic_AlsoRemovesSubscriptions(t *testing.T) {
	svc, _ := newTestService(t)
	w := httptest.NewRecorder()
	svc.ServeHTTP(w, queryRequest("CreateTopic", url.Values{"Name": {"t1"}}))
	arn := extractTag(w.Body.String(), "TopicArn")

	w = httptest.NewRecorder()
	svc.ServeHTTP(w, queryRequest("Subscribe", url.Values{
		"TopicArn": {arn}, "Protocol": {"sqs"}, "Endpoint": {"arn:aws:sqs:us-east-1:000000000000:q1"},
	}))
	if w.Code != http.StatusOK {
		t.Fatalf("Subscribe: status = %d, body = %s", w.Code, w.Body.String())
	}

	w = httptest.NewRecorder()
	svc.ServeHTTP(w, queryRequest("DeleteTopic", url.Values{"TopicArn": {arn}}))
	if w.Code != http.StatusOK {
		t.Fatalf("DeleteTopic: status = %d", w.Code)
	}

	w = httptest.NewRecorder()
	svc.ServeHTTP(w, queryRequest("ListSubscriptionsByTopic", url.Values{"TopicArn": {arn}}))
	if strings.Contains(w.Body.String(), "<SubscriptionArn>") {
		t.Fatalf("ListSubscriptionsByTopic tras DeleteTopic: esperaba sin suscripciones, body = %s", w.Body.String())
	}
}

func TestSubscribeAndListSubscriptionsByTopic(t *testing.T) {
	svc, _ := newTestService(t)
	w := httptest.NewRecorder()
	svc.ServeHTTP(w, queryRequest("CreateTopic", url.Values{"Name": {"t1"}}))
	arn := extractTag(w.Body.String(), "TopicArn")

	w = httptest.NewRecorder()
	svc.ServeHTTP(w, queryRequest("Subscribe", url.Values{
		"TopicArn": {arn}, "Protocol": {"sqs"}, "Endpoint": {"arn:aws:sqs:us-east-1:000000000000:q1"},
	}))
	subArn := extractTag(w.Body.String(), "SubscriptionArn")
	if subArn == "" {
		t.Fatalf("Subscribe: no se pudo extraer SubscriptionArn, body = %s", w.Body.String())
	}

	w = httptest.NewRecorder()
	svc.ServeHTTP(w, queryRequest("ListSubscriptionsByTopic", url.Values{"TopicArn": {arn}}))
	if !strings.Contains(w.Body.String(), subArn) {
		t.Fatalf("ListSubscriptionsByTopic: esperaba ver %q, body = %s", subArn, w.Body.String())
	}
}

func TestSubscribe_RequiresFields(t *testing.T) {
	svc, _ := newTestService(t)
	w := httptest.NewRecorder()
	svc.ServeHTTP(w, queryRequest("Subscribe", url.Values{"TopicArn": {"arn:aws:sns:us-east-1:000000000000:t1"}}))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("Subscribe sin Protocol/Endpoint: status = %d, esperaba 400", w.Code)
	}
}

func TestUnsubscribe_RemovesSubscription(t *testing.T) {
	svc, _ := newTestService(t)
	w := httptest.NewRecorder()
	svc.ServeHTTP(w, queryRequest("CreateTopic", url.Values{"Name": {"t1"}}))
	arn := extractTag(w.Body.String(), "TopicArn")

	w = httptest.NewRecorder()
	svc.ServeHTTP(w, queryRequest("Subscribe", url.Values{
		"TopicArn": {arn}, "Protocol": {"sqs"}, "Endpoint": {"arn:aws:sqs:us-east-1:000000000000:q1"},
	}))
	subArn := extractTag(w.Body.String(), "SubscriptionArn")

	w = httptest.NewRecorder()
	svc.ServeHTTP(w, queryRequest("Unsubscribe", url.Values{"SubscriptionArn": {subArn}}))
	if w.Code != http.StatusOK {
		t.Fatalf("Unsubscribe: status = %d", w.Code)
	}

	w = httptest.NewRecorder()
	svc.ServeHTTP(w, queryRequest("ListSubscriptionsByTopic", url.Values{"TopicArn": {arn}}))
	if strings.Contains(w.Body.String(), subArn) {
		t.Fatalf("ListSubscriptionsByTopic tras Unsubscribe: no debería ver %q, body = %s", subArn, w.Body.String())
	}
}

// TestPublish_DeliversToSQSSubscription cubre el camino central de SNS en
// este emulador: Publish a un tópico con una suscripción "sqs" entrega el
// mensaje a esa cola vía sqs.Service.DeliverMessage.
func TestPublish_DeliversToSQSSubscription(t *testing.T) {
	svc, sqsSvc := newTestService(t)

	wq := httptest.NewRecorder()
	sqsSvc.ServeHTTP(wq, queryRequest("CreateQueue", url.Values{"QueueName": {"q1"}}))
	if wq.Code != http.StatusOK {
		t.Fatalf("CreateQueue: status = %d, body = %s", wq.Code, wq.Body.String())
	}

	w := httptest.NewRecorder()
	svc.ServeHTTP(w, queryRequest("CreateTopic", url.Values{"Name": {"t1"}}))
	arn := extractTag(w.Body.String(), "TopicArn")

	svc.ServeHTTP(httptest.NewRecorder(), queryRequest("Subscribe", url.Values{
		"TopicArn": {arn}, "Protocol": {"sqs"}, "Endpoint": {"arn:aws:sqs:us-east-1:000000000000:q1"},
	}))

	w = httptest.NewRecorder()
	svc.ServeHTTP(w, queryRequest("Publish", url.Values{"TopicArn": {arn}, "Message": {"hola-sns"}}))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "<MessageId>") {
		t.Fatalf("Publish: status = %d, body = %s", w.Code, w.Body.String())
	}

	wr := httptest.NewRecorder()
	sqsSvc.ServeHTTP(wr, queryRequest("ReceiveMessage", url.Values{"QueueName": {"q1"}}))
	if !strings.Contains(wr.Body.String(), "<Body>hola-sns</Body>") {
		t.Fatalf("ReceiveMessage en q1: esperaba ver el mensaje publicado, body = %s", wr.Body.String())
	}
}

func TestPublish_RequiresTopicArn(t *testing.T) {
	svc, _ := newTestService(t)
	w := httptest.NewRecorder()
	svc.ServeHTTP(w, queryRequest("Publish", url.Values{"Message": {"x"}}))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("Publish sin TopicArn: status = %d, esperaba 400", w.Code)
	}
}

func TestPublishMessage_InternalAPIUsedByOtherServices(t *testing.T) {
	svc, sqsSvc := newTestService(t)
	sqsSvc.ServeHTTP(httptest.NewRecorder(), queryRequest("CreateQueue", url.Values{"QueueName": {"q1"}}))

	w := httptest.NewRecorder()
	svc.ServeHTTP(w, queryRequest("CreateTopic", url.Values{"Name": {"t1"}}))
	svc.ServeHTTP(httptest.NewRecorder(), queryRequest("Subscribe", url.Values{
		"TopicArn": {extractTag(w.Body.String(), "TopicArn")}, "Protocol": {"sqs"},
		"Endpoint": {"arn:aws:sqs:us-east-1:000000000000:q1"},
	}))

	id, err := svc.PublishMessage("t1", "entregado-directo")
	if err != nil {
		t.Fatalf("PublishMessage: %v", err)
	}
	if id == "" {
		t.Fatalf("PublishMessage: esperaba un messageID no vacío")
	}

	wr := httptest.NewRecorder()
	sqsSvc.ServeHTTP(wr, queryRequest("ReceiveMessage", url.Values{"QueueName": {"q1"}}))
	if !strings.Contains(wr.Body.String(), "<Body>entregado-directo</Body>") {
		t.Fatalf("ReceiveMessage: esperaba ver el mensaje de PublishMessage, body = %s", wr.Body.String())
	}
}

func TestPublishMessage_UnknownTopicErrors(t *testing.T) {
	svc, _ := newTestService(t)
	if _, err := svc.PublishMessage("nope", "x"); err == nil {
		t.Fatalf("PublishMessage sobre tópico inexistente: esperaba error")
	}
}

func TestGetTopicAttributes_IncludesOwnerAndPolicy(t *testing.T) {
	svc, _ := newTestService(t)
	w := httptest.NewRecorder()
	svc.ServeHTTP(w, queryRequest("CreateTopic", url.Values{"Name": {"t1"}}))
	arn := extractTag(w.Body.String(), "TopicArn")

	w = httptest.NewRecorder()
	svc.ServeHTTP(w, queryRequest("GetTopicAttributes", url.Values{"TopicArn": {arn}}))
	body := w.Body.String()
	if !strings.Contains(body, "<value>"+accountctx.DefaultAccountID+"</value>") {
		t.Fatalf("GetTopicAttributes: esperaba Owner=%s, body = %s", accountctx.DefaultAccountID, body)
	}
	if !strings.Contains(body, `Statement`) {
		t.Fatalf("GetTopicAttributes: esperaba un Policy con Statement, body = %s", body)
	}
}

func TestSetTopicAttributes_TopicMustExist(t *testing.T) {
	svc, _ := newTestService(t)
	w := httptest.NewRecorder()
	svc.ServeHTTP(w, queryRequest("SetTopicAttributes", url.Values{"TopicArn": {"arn:aws:sns:us-east-1:000000000000:nope"}}))
	if w.Code != http.StatusNotFound {
		t.Fatalf("SetTopicAttributes sobre tópico inexistente: status = %d, esperaba 404", w.Code)
	}
}

func TestListTagsForResource_AlwaysEmpty(t *testing.T) {
	svc, _ := newTestService(t)
	w := httptest.NewRecorder()
	svc.ServeHTTP(w, queryRequest("CreateTopic", url.Values{"Name": {"t1"}}))
	arn := extractTag(w.Body.String(), "TopicArn")

	w = httptest.NewRecorder()
	svc.ServeHTTP(w, queryRequest("ListTagsForResource", url.Values{"ResourceArn": {arn}}))
	if w.Code != http.StatusOK || strings.Contains(w.Body.String(), "<member>") {
		t.Fatalf("ListTagsForResource: status = %d, body = %s", w.Code, w.Body.String())
	}
}

func TestGetSubscriptionAttributes_AlwaysConfirmed(t *testing.T) {
	svc, _ := newTestService(t)
	w := httptest.NewRecorder()
	svc.ServeHTTP(w, queryRequest("CreateTopic", url.Values{"Name": {"t1"}}))
	arn := extractTag(w.Body.String(), "TopicArn")

	w = httptest.NewRecorder()
	svc.ServeHTTP(w, queryRequest("Subscribe", url.Values{
		"TopicArn": {arn}, "Protocol": {"sqs"}, "Endpoint": {"arn:aws:sqs:us-east-1:000000000000:q1"},
	}))
	subArn := extractTag(w.Body.String(), "SubscriptionArn")

	w = httptest.NewRecorder()
	svc.ServeHTTP(w, queryRequest("GetSubscriptionAttributes", url.Values{"SubscriptionArn": {subArn}}))
	body := w.Body.String()
	if !strings.Contains(body, "<value>false</value>") {
		t.Fatalf("GetSubscriptionAttributes: esperaba PendingConfirmation=false, body = %s", body)
	}
}

func TestReset_ClearsTopicsAndSubscriptions(t *testing.T) {
	svc, _ := newTestService(t)
	svc.ServeHTTP(httptest.NewRecorder(), queryRequest("CreateTopic", url.Values{"Name": {"t1"}}))

	if err := svc.Reset(); err != nil {
		t.Fatalf("Reset: %v", err)
	}

	w := httptest.NewRecorder()
	svc.ServeHTTP(w, queryRequest("ListTopics", url.Values{}))
	if strings.Contains(w.Body.String(), "<TopicArn>") {
		t.Fatalf("ListTopics tras Reset: esperaba vacío, body = %s", w.Body.String())
	}
}
