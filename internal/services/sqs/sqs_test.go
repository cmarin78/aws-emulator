package sqs

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cesarmarin/aws-emulator/internal/accountctx"
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

// queryRequest construye una request del protocolo Query/XML clásico
// (Action + parámetros como form values) con la identidad por defecto en
// el contexto, igual que la dejaría accountctx.Middleware.
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

func createTestQueue(t *testing.T, svc *Service, name string) string {
	t.Helper()
	w := httptest.NewRecorder()
	svc.ServeHTTP(w, queryRequest("CreateQueue", url.Values{"QueueName": {name}}))
	if w.Code != http.StatusOK {
		t.Fatalf("CreateQueue: status = %d, body = %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "<QueueUrl>") {
		t.Fatalf("CreateQueue: esperaba QueueUrl en la respuesta, body = %s", w.Body.String())
	}
	return name
}

func TestCreateQueue_IsIdempotent(t *testing.T) {
	svc := newTestService(t)
	createTestQueue(t, svc, "q1")

	w := httptest.NewRecorder()
	svc.ServeHTTP(w, queryRequest("CreateQueue", url.Values{"QueueName": {"q1"}}))
	if w.Code != http.StatusOK {
		t.Fatalf("segundo CreateQueue: status = %d, body = %s", w.Code, w.Body.String())
	}

	w = httptest.NewRecorder()
	svc.ServeHTTP(w, queryRequest("ListQueues", url.Values{}))
	if strings.Count(w.Body.String(), "<QueueUrl>") != 1 {
		t.Fatalf("ListQueues tras CreateQueue duplicado: esperaba una sola cola, body = %s", w.Body.String())
	}
}

func TestSendAndReceiveMessage(t *testing.T) {
	svc := newTestService(t)
	createTestQueue(t, svc, "q1")

	w := httptest.NewRecorder()
	svc.ServeHTTP(w, queryRequest("SendMessage", url.Values{"QueueName": {"q1"}, "MessageBody": {"hola"}}))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "<MessageId>") {
		t.Fatalf("SendMessage: status = %d, body = %s", w.Code, w.Body.String())
	}

	w = httptest.NewRecorder()
	svc.ServeHTTP(w, queryRequest("ReceiveMessage", url.Values{"QueueName": {"q1"}}))
	if !strings.Contains(w.Body.String(), "<Body>hola</Body>") {
		t.Fatalf("ReceiveMessage: esperaba ver el body 'hola', body = %s", w.Body.String())
	}
}

func TestSendMessage_NonExistentQueueFails(t *testing.T) {
	svc := newTestService(t)
	w := httptest.NewRecorder()
	svc.ServeHTTP(w, queryRequest("SendMessage", url.Values{"QueueName": {"nope"}, "MessageBody": {"x"}}))
	if w.Code != http.StatusNotFound {
		t.Fatalf("SendMessage a cola inexistente: status = %d, esperaba 404", w.Code)
	}
}

func TestDeleteMessage_RemovesFromQueue(t *testing.T) {
	svc := newTestService(t)
	createTestQueue(t, svc, "q1")
	svc.ServeHTTP(httptest.NewRecorder(), queryRequest("SendMessage", url.Values{"QueueName": {"q1"}, "MessageBody": {"x"}}))

	w := httptest.NewRecorder()
	svc.ServeHTTP(w, queryRequest("ReceiveMessage", url.Values{"QueueName": {"q1"}}))
	receipt := extractBetween(w.Body.String(), "<ReceiptHandle>", "</ReceiptHandle>")
	if receipt == "" {
		t.Fatalf("no se pudo extraer ReceiptHandle, body = %s", w.Body.String())
	}

	w = httptest.NewRecorder()
	svc.ServeHTTP(w, queryRequest("DeleteMessage", url.Values{"QueueName": {"q1"}, "ReceiptHandle": {receipt}}))
	if w.Code != http.StatusOK {
		t.Fatalf("DeleteMessage: status = %d", w.Code)
	}

	w = httptest.NewRecorder()
	svc.ServeHTTP(w, queryRequest("ReceiveMessage", url.Values{"QueueName": {"q1"}}))
	if strings.Contains(w.Body.String(), "<Message>") {
		t.Fatalf("ReceiveMessage tras DeleteMessage: esperaba cola vacía, body = %s", w.Body.String())
	}
}

func TestPurgeQueue_RemovesAllMessages(t *testing.T) {
	svc := newTestService(t)
	createTestQueue(t, svc, "q1")
	svc.ServeHTTP(httptest.NewRecorder(), queryRequest("SendMessage", url.Values{"QueueName": {"q1"}, "MessageBody": {"a"}}))
	svc.ServeHTTP(httptest.NewRecorder(), queryRequest("SendMessage", url.Values{"QueueName": {"q1"}, "MessageBody": {"b"}}))

	w := httptest.NewRecorder()
	svc.ServeHTTP(w, queryRequest("PurgeQueue", url.Values{"QueueName": {"q1"}}))
	if w.Code != http.StatusOK {
		t.Fatalf("PurgeQueue: status = %d", w.Code)
	}

	w = httptest.NewRecorder()
	svc.ServeHTTP(w, queryRequest("ReceiveMessage", url.Values{"QueueName": {"q1"}, "MaxNumberOfMessages": {"10"}}))
	if strings.Contains(w.Body.String(), "<Message>") {
		t.Fatalf("ReceiveMessage tras PurgeQueue: esperaba cola vacía, body = %s", w.Body.String())
	}
}

func TestDeleteQueue_AlsoRemovesMessages(t *testing.T) {
	svc := newTestService(t)
	createTestQueue(t, svc, "q1")
	svc.ServeHTTP(httptest.NewRecorder(), queryRequest("SendMessage", url.Values{"QueueName": {"q1"}, "MessageBody": {"a"}}))

	w := httptest.NewRecorder()
	svc.ServeHTTP(w, queryRequest("DeleteQueue", url.Values{"QueueName": {"q1"}}))
	if w.Code != http.StatusOK {
		t.Fatalf("DeleteQueue: status = %d", w.Code)
	}

	w = httptest.NewRecorder()
	svc.ServeHTTP(w, queryRequest("GetQueueUrl", url.Values{"QueueName": {"q1"}}))
	if w.Code != http.StatusNotFound {
		t.Fatalf("GetQueueUrl tras DeleteQueue: status = %d, esperaba 404", w.Code)
	}
}

// TestQueueAttributes_SetThenGet cubre SetQueueAttributes seguido de
// GetQueueAttributes, y verifica que QueueArn y
// ApproximateNumberOfMessages se calculen además de los atributos
// custom seteados -- el bug histórico (ver createQueue) era justamente
// que estos cálculos no se reflejaban y rompía el waiter de Terraform.
func TestQueueAttributes_SetThenGet(t *testing.T) {
	svc := newTestService(t)
	createTestQueue(t, svc, "q1")
	svc.ServeHTTP(httptest.NewRecorder(), queryRequest("SendMessage", url.Values{"QueueName": {"q1"}, "MessageBody": {"a"}}))

	w := httptest.NewRecorder()
	svc.ServeHTTP(w, queryRequest("SetQueueAttributes", url.Values{
		"QueueName":         {"q1"},
		"Attribute.1.Name":  {"VisibilityTimeout"},
		"Attribute.1.Value": {"45"},
	}))
	if w.Code != http.StatusOK {
		t.Fatalf("SetQueueAttributes: status = %d, body = %s", w.Code, w.Body.String())
	}

	w = httptest.NewRecorder()
	svc.ServeHTTP(w, queryRequest("GetQueueAttributes", url.Values{"QueueName": {"q1"}}))
	body := w.Body.String()
	if !strings.Contains(body, "<Name>VisibilityTimeout</Name>") || !strings.Contains(body, "<Value>45</Value>") {
		t.Fatalf("GetQueueAttributes: esperaba ver VisibilityTimeout=45, body = %s", body)
	}
	if !strings.Contains(body, "<Name>QueueArn</Name>") {
		t.Fatalf("GetQueueAttributes: esperaba QueueArn calculado, body = %s", body)
	}
	if !strings.Contains(body, "<Name>ApproximateNumberOfMessages</Name>") {
		t.Fatalf("GetQueueAttributes: esperaba ApproximateNumberOfMessages calculado, body = %s", body)
	}
	if !strings.Contains(body, "<Value>1</Value>") {
		t.Fatalf("GetQueueAttributes: esperaba ApproximateNumberOfMessages=1 (un mensaje enviado), body = %s", body)
	}
}

func TestSendMessageBatch_AndDeleteMessageBatch(t *testing.T) {
	svc := newTestService(t)
	createTestQueue(t, svc, "q1")

	w := httptest.NewRecorder()
	svc.ServeHTTP(w, queryRequest("SendMessageBatch", url.Values{
		"QueueName":                                  {"q1"},
		"SendMessageBatchRequestEntry.1.Id":          {"e1"},
		"SendMessageBatchRequestEntry.1.MessageBody": {"primero"},
		"SendMessageBatchRequestEntry.2.Id":          {"e2"},
		"SendMessageBatchRequestEntry.2.MessageBody": {"segundo"},
	}))
	if w.Code != http.StatusOK {
		t.Fatalf("SendMessageBatch: status = %d, body = %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if strings.Count(body, "<SendMessageBatchResultEntry>") != 2 {
		t.Fatalf("SendMessageBatch: esperaba 2 entries, body = %s", body)
	}

	// Recibir todo y borrar en batch.
	w = httptest.NewRecorder()
	svc.ServeHTTP(w, queryRequest("ReceiveMessage", url.Values{"QueueName": {"q1"}, "MaxNumberOfMessages": {"10"}}))
	receipts := extractAllBetween(w.Body.String(), "<ReceiptHandle>", "</ReceiptHandle>")
	if len(receipts) != 2 {
		t.Fatalf("ReceiveMessage: esperaba 2 mensajes, body = %s", w.Body.String())
	}

	form := url.Values{"QueueName": {"q1"}}
	for i, r := range receipts {
		idx := i + 1
		form.Set("DeleteMessageBatchRequestEntry."+itoa(idx)+".Id", "e"+itoa(idx))
		form.Set("DeleteMessageBatchRequestEntry."+itoa(idx)+".ReceiptHandle", r)
	}
	w = httptest.NewRecorder()
	svc.ServeHTTP(w, queryRequest("DeleteMessageBatch", form))
	if w.Code != http.StatusOK {
		t.Fatalf("DeleteMessageBatch: status = %d, body = %s", w.Code, w.Body.String())
	}

	w = httptest.NewRecorder()
	svc.ServeHTTP(w, queryRequest("ReceiveMessage", url.Values{"QueueName": {"q1"}, "MaxNumberOfMessages": {"10"}}))
	if strings.Contains(w.Body.String(), "<Message>") {
		t.Fatalf("ReceiveMessage tras DeleteMessageBatch: esperaba cola vacía, body = %s", w.Body.String())
	}
}

// TestDeliverMessage_UsedByOtherServices cubre DeliverMessage, el atajo
// que usan SNS/EventBridge para entregar a una cola sin pasar por el
// protocolo público.
func TestDeliverMessage_UsedByOtherServices(t *testing.T) {
	svc := newTestService(t)
	createTestQueue(t, svc, "q1")

	id, err := svc.DeliverMessage("q1", "entregado-directo")
	if err != nil {
		t.Fatalf("DeliverMessage: %v", err)
	}
	if id == "" {
		t.Fatalf("DeliverMessage: esperaba un messageID no vacío")
	}

	w := httptest.NewRecorder()
	svc.ServeHTTP(w, queryRequest("ReceiveMessage", url.Values{"QueueName": {"q1"}}))
	if !strings.Contains(w.Body.String(), "<Body>entregado-directo</Body>") {
		t.Fatalf("ReceiveMessage: esperaba ver el mensaje entregado directo, body = %s", w.Body.String())
	}
}

func TestDeliverMessage_NonExistentQueueErrors(t *testing.T) {
	svc := newTestService(t)
	if _, err := svc.DeliverMessage("nope", "x"); err == nil {
		t.Fatalf("DeliverMessage a cola inexistente: esperaba error")
	}
}

func TestJSONProtocol_CreateAndSendMessage(t *testing.T) {
	svc := newTestService(t)

	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"QueueName":"jq1"}`))
	r.Header.Set("X-Amz-Target", "AmazonSQS.CreateQueue")
	w := httptest.NewRecorder()
	svc.ServeHTTP(w, r)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"QueueUrl"`) {
		t.Fatalf("CreateQueue (JSON): status = %d, body = %s", w.Code, w.Body.String())
	}

	r = httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"QueueUrl":"http://localhost:4566/000000000000/jq1","MessageBody":"hola-json"}`))
	r.Header.Set("X-Amz-Target", "AmazonSQS.SendMessage")
	w = httptest.NewRecorder()
	svc.ServeHTTP(w, r)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"MessageId"`) {
		t.Fatalf("SendMessage (JSON): status = %d, body = %s", w.Code, w.Body.String())
	}
}

func TestReset_ClearsQueuesAndMessages(t *testing.T) {
	svc := newTestService(t)
	createTestQueue(t, svc, "q1")
	svc.ServeHTTP(httptest.NewRecorder(), queryRequest("SendMessage", url.Values{"QueueName": {"q1"}, "MessageBody": {"a"}}))

	if err := svc.Reset(); err != nil {
		t.Fatalf("Reset: %v", err)
	}

	w := httptest.NewRecorder()
	svc.ServeHTTP(w, queryRequest("ListQueues", url.Values{}))
	if strings.Contains(w.Body.String(), "<QueueUrl>") {
		t.Fatalf("ListQueues tras Reset: esperaba ninguna cola, body = %s", w.Body.String())
	}
}

func extractBetween(body, open, close string) string {
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

func extractAllBetween(body, open, close string) []string {
	var out []string
	rest := body
	for {
		start := strings.Index(rest, open)
		if start == -1 {
			break
		}
		rest = rest[start+len(open):]
		end := strings.Index(rest, close)
		if end == -1 {
			break
		}
		out = append(out, rest[:end])
		rest = rest[end+len(close):]
	}
	return out
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	if neg {
		return "-" + string(b)
	}
	return string(b)
}

// Confirma que accountctx no es estrictamente necesario para estos tests
// (siempre usan FromContext con defaults), pero se referencia el paquete
// acá para dejar explícito que las URLs/ARNs generados usan
// accountctx.DefaultAccountID cuando no hay Authorization en la request.
var _ = accountctx.DefaultAccountID
