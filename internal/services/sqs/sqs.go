// Package sqs emula el subconjunto más usado de Amazon SQS: colas y
// mensajes, con el protocolo Query/XML clásico (CreateQueue, SendMessage,
// ReceiveMessage, DeleteMessage, ListQueues, GetQueueUrl, DeleteQueue),
// más atributos de cola (GetQueueAttributes/SetQueueAttributes) y
// operaciones batch (SendMessageBatch/DeleteMessageBatch). No implementa
// colas FIFO, dead-letter queues, ni long polling real (ReceiveMessage no
// bloquea — devuelve inmediatamente lo que haya).
package sqs

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/cesarmarin/aws-emulator/internal/server"
	"github.com/cesarmarin/aws-emulator/internal/storage"
)

const (
	queuesBucket     = "sqs.queues"
	messagesBucket   = "sqs.messages"
	attributesBucket = "sqs.attributes"
	accountID        = "000000000000"
)

// Service agrupa el estado del servicio SQS.
type Service struct {
	db *storage.DB
}

// New crea el servicio SQS.
func New(db *storage.DB) *Service {
	return &Service{db: db}
}

// Queue es la forma persistida de una cola.
type Queue struct {
	Name       string    `json:"name"`
	URL        string    `json:"url"`
	CreateDate time.Time `json:"createDate"`
}

// Message es la forma persistida de un mensaje en cola.
type Message struct {
	ID            string `json:"id"`
	Queue         string `json:"queue"`
	Body          string `json:"body"`
	ReceiptHandle string `json:"receiptHandle"`
}

func queueURL(name string) string {
	return "http://localhost:4566/" + accountID + "/" + name
}

func randomID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (s *Service) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var form map[string]string
	var action string

	// botocore migró SQS al protocolo JSON (Content-Type
	// application/x-amz-json-1.0, X-Amz-Target: AmazonSQS.<Action>) hace
	// varias versiones, manteniendo los mismos nombres de parámetro que el
	// protocolo Query clásico vía el header x-amzn-query-mode -- la AWS CLI
	// actual ya no manda form-encoded para SQS. Si vemos esa señal,
	// parseamos JSON y traducimos al mismo formato `form` que ya usan todos
	// los handlers, en vez de duplicar cada handler en dos protocolos.
	if target := r.Header.Get("X-Amz-Target"); target != "" {
		action = jsonActionFromTarget(target)
		form = formFromJSONBody(r, action)
	} else {
		form = formValues(r)
		action = form["Action"]
		if action == "" {
			action = r.URL.Query().Get("Action")
		}
	}

	// Modo de respuesta: si la request llegó como JSON (X-Amz-Target
	// presente), la respuesta también debe ir en JSON -- botocore configura
	// su parser de respuesta según el protocolo con el que mandó la
	// request, así que devolver XML acá (aunque sea XML válido) hace que
	// falle el parseo y los campos lleguen vacíos/None al cliente.
	useJSON := r.Header.Get("X-Amz-Target") != ""

	switch action {
	case "CreateQueue":
		s.createQueue(w, form, useJSON)
	case "ListQueues":
		s.listQueues(w, useJSON)
	case "GetQueueUrl":
		s.getQueueURL(w, form, useJSON)
	case "DeleteQueue":
		s.deleteQueue(w, form, useJSON)
	case "SendMessage":
		s.sendMessage(w, form, useJSON)
	case "ReceiveMessage":
		s.receiveMessage(w, form, useJSON)
	case "DeleteMessage":
		s.deleteMessage(w, form, useJSON)
	case "PurgeQueue":
		s.purgeQueue(w, form, useJSON)
	case "GetQueueAttributes":
		s.getQueueAttributes(w, form, useJSON)
	case "SetQueueAttributes":
		s.setQueueAttributes(w, form, useJSON)
	case "SendMessageBatch":
		s.sendMessageBatch(w, form, useJSON)
	case "DeleteMessageBatch":
		s.deleteMessageBatch(w, form, useJSON)
	default:
		writeError(w, useJSON, http.StatusBadRequest, "InvalidAction",
			"acción SQS no soportada en este emulador: "+action)
	}
}

// writeResult responde con xmlVal (protocolo Query/XML clásico) o jsonVal
// (protocolo AmazonSQS JSON 1.0, usado por la AWS CLI/SDKs modernos) según
// useJSON. Los dos protocolos tienen shapes distintos para el mismo dato
// (JSON es plano, XML envuelve en un elemento "<Accion>Result"), por eso
// cada handler construye ambas representaciones en vez de reusar un único
// struct con tags xml+json.
func writeResult(w http.ResponseWriter, useJSON bool, status int, xmlVal, jsonVal any) {
	if useJSON {
		server.WriteJSON(w, status, jsonVal)
		return
	}
	server.WriteXML(w, status, xmlVal)
}

// writeError responde un error en el shape correspondiente al protocolo de
// la request. En JSON usamos el mismo convenio "com.amazonaws.sqs#<Code>"
// que ya usa el servicio DynamoDB para sus errores JSON.
func writeError(w http.ResponseWriter, useJSON bool, status int, code, message string) {
	if useJSON {
		server.WriteJSONError(w, status, "com.amazonaws.sqs#"+code, message)
		return
	}
	server.WriteXMLError(w, status, code, message)
}

// Reset limpia todo el estado persistido de SQS (colas, mensajes y
// atributos de cola). Implementa server.Resettable.
func (s *Service) Reset() error {
	return s.db.Reset(queuesBucket, messagesBucket, attributesBucket)
}

// indexedEntries agrupa parámetros de la forma "<prefix>.<n>.<campo>"
// (índices 1..N contiguos, como manda botocore para listas en el
// protocolo Query) en una lista de mapas campo->valor. Se detiene en el
// primer índice sin ningún campo presente. Sirve tanto para entries de
// batch (".1.Id", ".1.MessageBody", ...) como para listas de atributos
// (Attribute.1.Name / Attribute.1.Value).
func indexedEntries(form map[string]string, prefix string) []map[string]string {
	var entries []map[string]string
	for i := 1; ; i++ {
		p := prefix + "." + strconv.Itoa(i) + "."
		entry := map[string]string{}
		for k, v := range form {
			if strings.HasPrefix(k, p) {
				entry[strings.TrimPrefix(k, p)] = v
			}
		}
		if len(entry) == 0 {
			break
		}
		entries = append(entries, entry)
	}
	return entries
}

func formValues(r *http.Request) map[string]string {
	out := map[string]string{}
	for k, v := range r.URL.Query() {
		if len(v) > 0 {
			out[k] = v[0]
		}
	}
	if err := r.ParseForm(); err == nil {
		for k, v := range r.PostForm {
			if len(v) > 0 {
				out[k] = v[0]
			}
		}
	}
	return out
}

// jsonActionFromTarget extrae el nombre de acción de un header
// X-Amz-Target con forma "AmazonSQS.<Action>" (p. ej. "CreateQueue" de
// "AmazonSQS.CreateQueue").
func jsonActionFromTarget(target string) string {
	if i := strings.LastIndex(target, "."); i != -1 {
		return target[i+1:]
	}
	return target
}

// batchEntryPrefix mapea cada acción batch al nombre de parámetro que usa
// el protocolo Query clásico para su lista de entries (que es distinto
// del nombre del campo JSON, siempre "Entries", por lo que no se puede
// derivar genéricamente).
var batchEntryPrefix = map[string]string{
	"SendMessageBatch":   "SendMessageBatchRequestEntry",
	"DeleteMessageBatch": "DeleteMessageBatchRequestEntry",
}

// formFromJSONBody traduce un body JSON (protocolo AmazonSQS JSON 1.0) al
// mismo formato `form` plano que usan los handlers existentes (escritos
// originalmente para el protocolo Query), para no duplicar cada operación
// en dos protocolos. Los nombres de campo escalares son iguales en ambos
// protocolos (QueueUrl, MessageBody, etc.); solo las listas/mapas
// necesitan traducción explícita (Attributes -> Attribute.N.Name/Value,
// Entries -> <Prefix>.N.<campo>).
func formFromJSONBody(r *http.Request, action string) map[string]string {
	out := map[string]string{}
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		return out
	}
	for k, v := range body {
		switch k {
		case "Attributes":
			attrs, ok := v.(map[string]any)
			if !ok {
				continue
			}
			names := make([]string, 0, len(attrs))
			for name := range attrs {
				names = append(names, name)
			}
			sort.Strings(names)
			for i, name := range names {
				idx := strconv.Itoa(i + 1)
				out["Attribute."+idx+".Name"] = name
				out["Attribute."+idx+".Value"] = jsonScalarToString(attrs[name])
			}
		case "Entries":
			entries, ok := v.([]any)
			if !ok {
				continue
			}
			prefix := batchEntryPrefix[action]
			if prefix == "" {
				continue
			}
			for i, raw := range entries {
				entry, ok := raw.(map[string]any)
				if !ok {
					continue
				}
				idx := strconv.Itoa(i + 1)
				for field, fv := range entry {
					out[prefix+"."+idx+"."+field] = jsonScalarToString(fv)
				}
			}
		default:
			out[k] = jsonScalarToString(v)
		}
	}
	return out
}

// jsonScalarToString convierte un valor decodificado de JSON (string,
// float64, bool) a su representación de texto equivalente a la que
// llegaría como parámetro de form en el protocolo Query.
func jsonScalarToString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case float64:
		if t == float64(int64(t)) {
			return strconv.FormatInt(int64(t), 10)
		}
		return fmt.Sprintf("%g", t)
	case bool:
		return strconv.FormatBool(t)
	default:
		return fmt.Sprintf("%v", t)
	}
}

// --- colas ---

type createQueueResponse struct {
	XMLName xml.Name          `xml:"CreateQueueResponse"`
	Result  createQueueResult `xml:"CreateQueueResult"`
}
type createQueueResult struct {
	QueueUrl string `xml:"QueueUrl"`
}
type createQueueJSON struct {
	QueueUrl string `json:"QueueUrl"`
}

func (s *Service) createQueue(w http.ResponseWriter, form map[string]string, useJSON bool) {
	name := form["QueueName"]
	if name == "" {
		writeError(w, useJSON, http.StatusBadRequest, "MissingParameter", "QueueName es requerido")
		return
	}
	var existing Queue
	if found, _ := s.db.Get(queuesBucket, name, &existing); found {
		writeResult(w, useJSON, http.StatusOK,
			createQueueResponse{Result: createQueueResult{QueueUrl: existing.URL}},
			createQueueJSON{QueueUrl: existing.URL})
		return
	}
	q := Queue{Name: name, URL: queueURL(name), CreateDate: time.Now().UTC()}
	if err := s.db.Put(queuesBucket, name, q); err != nil {
		writeError(w, useJSON, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	writeResult(w, useJSON, http.StatusOK,
		createQueueResponse{Result: createQueueResult{QueueUrl: q.URL}},
		createQueueJSON{QueueUrl: q.URL})
}

type listQueuesResponse struct {
	XMLName xml.Name         `xml:"ListQueuesResponse"`
	Result  listQueuesResult `xml:"ListQueuesResult"`
}
type listQueuesResult struct {
	QueueUrls []string `xml:"QueueUrl"`
}
type listQueuesJSON struct {
	QueueUrls []string `json:"QueueUrls"`
}

func (s *Service) listQueues(w http.ResponseWriter, useJSON bool) {
	var urls []string
	_ = s.db.List(queuesBucket, "", func(_ string, raw []byte) error {
		var q Queue
		if err := unmarshal(raw, &q); err == nil {
			urls = append(urls, q.URL)
		}
		return nil
	})
	sort.Strings(urls)
	writeResult(w, useJSON, http.StatusOK,
		listQueuesResponse{Result: listQueuesResult{QueueUrls: urls}},
		listQueuesJSON{QueueUrls: urls})
}

type getQueueURLResponse struct {
	XMLName xml.Name          `xml:"GetQueueUrlResponse"`
	Result  getQueueURLResult `xml:"GetQueueUrlResult"`
}
type getQueueURLResult struct {
	QueueUrl string `xml:"QueueUrl"`
}

func (s *Service) getQueueURL(w http.ResponseWriter, form map[string]string, useJSON bool) {
	name := form["QueueName"]
	var q Queue
	found, err := s.db.Get(queuesBucket, name, &q)
	if err != nil {
		writeError(w, useJSON, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	if !found {
		writeError(w, useJSON, http.StatusNotFound, "QueueDoesNotExist", "la cola no existe: "+name)
		return
	}
	writeResult(w, useJSON, http.StatusOK,
		getQueueURLResponse{Result: getQueueURLResult{QueueUrl: q.URL}},
		createQueueJSON{QueueUrl: q.URL})
}

type deleteQueueResponse struct {
	XMLName xml.Name `xml:"DeleteQueueResponse"`
}

func (s *Service) deleteQueue(w http.ResponseWriter, form map[string]string, useJSON bool) {
	name := queueNameFromURLOrParam(form)
	if err := s.db.DeletePrefix(messagesBucket, name+"/"); err != nil {
		writeError(w, useJSON, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	if err := s.db.Delete(queuesBucket, name); err != nil {
		writeError(w, useJSON, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	writeResult(w, useJSON, http.StatusOK, deleteQueueResponse{}, struct{}{})
}

type purgeQueueResponse struct {
	XMLName xml.Name `xml:"PurgeQueueResponse"`
}

func (s *Service) purgeQueue(w http.ResponseWriter, form map[string]string, useJSON bool) {
	name := queueNameFromURLOrParam(form)
	if err := s.db.DeletePrefix(messagesBucket, name+"/"); err != nil {
		writeError(w, useJSON, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	writeResult(w, useJSON, http.StatusOK, purgeQueueResponse{}, struct{}{})
}

// queueNameFromURLOrParam acepta tanto QueueUrl (forma que mandan los SDKs
// para casi todas las operaciones de data plane) como QueueName, y
// devuelve el último segmento del path como nombre de cola.
func queueNameFromURLOrParam(form map[string]string) string {
	if name := form["QueueName"]; name != "" {
		return name
	}
	url := form["QueueUrl"]
	for i := len(url) - 1; i >= 0; i-- {
		if url[i] == '/' {
			return url[i+1:]
		}
	}
	return url
}

// --- mensajes ---

type sendMessageResponse struct {
	XMLName xml.Name          `xml:"SendMessageResponse"`
	Result  sendMessageResult `xml:"SendMessageResult"`
}
type sendMessageResult struct {
	MessageId string `xml:"MessageId"`
	MD5OfBody string `xml:"MD5OfMessageBody"`
}
type sendMessageJSON struct {
	MessageId string `json:"MessageId"`
	MD5OfBody string `json:"MD5OfMessageBody"`
}

func (s *Service) sendMessage(w http.ResponseWriter, form map[string]string, useJSON bool) {
	queue := queueNameFromURLOrParam(form)
	if found, _ := s.db.Get(queuesBucket, queue, &Queue{}); !found {
		writeError(w, useJSON, http.StatusNotFound, "QueueDoesNotExist", "la cola no existe: "+queue)
		return
	}
	body := form["MessageBody"]
	id := randomID()
	msg := Message{ID: id, Queue: queue, Body: body, ReceiptHandle: randomID()}
	if err := s.db.Put(messagesBucket, queue+"/"+id, msg); err != nil {
		writeError(w, useJSON, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	writeResult(w, useJSON, http.StatusOK,
		sendMessageResponse{Result: sendMessageResult{MessageId: id, MD5OfBody: md5Hex(body)}},
		sendMessageJSON{MessageId: id, MD5OfBody: md5Hex(body)})
}

type receiveMessageResponse struct {
	XMLName xml.Name             `xml:"ReceiveMessageResponse"`
	Result  receiveMessageResult `xml:"ReceiveMessageResult"`
}
type receiveMessageResult struct {
	Messages []messageXML `xml:"Message"`
}
type messageXML struct {
	MessageId     string `xml:"MessageId"`
	ReceiptHandle string `xml:"ReceiptHandle"`
	Body          string `xml:"Body"`
	MD5OfBody     string `xml:"MD5OfBody"`
}
type messageJSON struct {
	MessageId     string `json:"MessageId"`
	ReceiptHandle string `json:"ReceiptHandle"`
	Body          string `json:"Body"`
	MD5OfBody     string `json:"MD5OfBody"`
}
type receiveMessageJSON struct {
	Messages []messageJSON `json:"Messages"`
}

func (s *Service) receiveMessage(w http.ResponseWriter, form map[string]string, useJSON bool) {
	queue := queueNameFromURLOrParam(form)
	maxMessages := 1
	if v := form["MaxNumberOfMessages"]; v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			maxMessages = n
		}
	}

	var out []messageXML
	var outJSON []messageJSON
	_ = s.db.List(messagesBucket, queue+"/", func(_ string, raw []byte) error {
		if len(out) >= maxMessages {
			return nil
		}
		var m Message
		if err := unmarshal(raw, &m); err == nil {
			out = append(out, messageXML{
				MessageId:     m.ID,
				ReceiptHandle: m.ReceiptHandle,
				Body:          m.Body,
				MD5OfBody:     md5Hex(m.Body),
			})
			outJSON = append(outJSON, messageJSON{
				MessageId:     m.ID,
				ReceiptHandle: m.ReceiptHandle,
				Body:          m.Body,
				MD5OfBody:     md5Hex(m.Body),
			})
		}
		return nil
	})
	writeResult(w, useJSON, http.StatusOK,
		receiveMessageResponse{Result: receiveMessageResult{Messages: out}},
		receiveMessageJSON{Messages: outJSON})
}

func (s *Service) deleteMessage(w http.ResponseWriter, form map[string]string, useJSON bool) {
	queue := queueNameFromURLOrParam(form)
	receipt := form["ReceiptHandle"]

	var toDelete string
	_ = s.db.List(messagesBucket, queue+"/", func(key string, raw []byte) error {
		var m Message
		if err := unmarshal(raw, &m); err == nil && m.ReceiptHandle == receipt {
			toDelete = key
		}
		return nil
	})
	if toDelete != "" {
		_ = s.db.Delete(messagesBucket, toDelete)
	}
	writeResult(w, useJSON, http.StatusOK, deleteMessageResponse{}, struct{}{})
}

type deleteMessageResponse struct {
	XMLName xml.Name `xml:"DeleteMessageResponse"`
}

// --- atributos de cola ---

func queueArn(name string) string {
	return "arn:aws:sqs:us-east-1:" + accountID + ":" + name
}

type getQueueAttributesResponse struct {
	XMLName xml.Name                 `xml:"GetQueueAttributesResponse"`
	Result  getQueueAttributesResult `xml:"GetQueueAttributesResult"`
}
type getQueueAttributesResult struct {
	Attributes []attributeXML `xml:"Attribute"`
}
type attributeXML struct {
	Name  string `xml:"Name"`
	Value string `xml:"Value"`
}
type getQueueAttributesJSON struct {
	Attributes map[string]string `json:"Attributes"`
}

func (s *Service) getQueueAttributes(w http.ResponseWriter, form map[string]string, useJSON bool) {
	queue := queueNameFromURLOrParam(form)
	if found, _ := s.db.Get(queuesBucket, queue, &Queue{}); !found {
		writeError(w, useJSON, http.StatusNotFound, "QueueDoesNotExist", "la cola no existe: "+queue)
		return
	}
	var stored map[string]string
	_, _ = s.db.Get(attributesBucket, queue, &stored)

	var count int
	_ = s.db.List(messagesBucket, queue+"/", func(string, []byte) error { count++; return nil })

	attrsMap := map[string]string{
		"QueueArn":                    queueArn(queue),
		"ApproximateNumberOfMessages": strconv.Itoa(count),
	}
	for k, v := range stored {
		attrsMap[k] = v
	}
	out := make([]attributeXML, 0, len(attrsMap))
	for k, v := range attrsMap {
		out = append(out, attributeXML{Name: k, Value: v})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	writeResult(w, useJSON, http.StatusOK,
		getQueueAttributesResponse{Result: getQueueAttributesResult{Attributes: out}},
		getQueueAttributesJSON{Attributes: attrsMap})
}

func (s *Service) setQueueAttributes(w http.ResponseWriter, form map[string]string, useJSON bool) {
	queue := queueNameFromURLOrParam(form)
	if found, _ := s.db.Get(queuesBucket, queue, &Queue{}); !found {
		writeError(w, useJSON, http.StatusNotFound, "QueueDoesNotExist", "la cola no existe: "+queue)
		return
	}
	var stored map[string]string
	_, _ = s.db.Get(attributesBucket, queue, &stored)
	if stored == nil {
		stored = map[string]string{}
	}
	for _, e := range indexedEntries(form, "Attribute") {
		if name, ok := e["Name"]; ok {
			stored[name] = e["Value"]
		}
	}
	if err := s.db.Put(attributesBucket, queue, stored); err != nil {
		writeError(w, useJSON, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	writeResult(w, useJSON, http.StatusOK, setQueueAttributesResponse{}, struct{}{})
}

type setQueueAttributesResponse struct {
	XMLName xml.Name `xml:"SetQueueAttributesResponse"`
}

// --- operaciones batch ---

type sendMessageBatchResponse struct {
	XMLName xml.Name               `xml:"SendMessageBatchResponse"`
	Result  sendMessageBatchResult `xml:"SendMessageBatchResult"`
}
type sendMessageBatchResult struct {
	Entries []sendMessageBatchResultEntry `xml:"SendMessageBatchResultEntry"`
}
type sendMessageBatchResultEntry struct {
	Id        string `xml:"Id"`
	MessageId string `xml:"MessageId"`
	MD5OfBody string `xml:"MD5OfMessageBody"`
}

type sendMessageBatchEntryJSON struct {
	Id        string `json:"Id"`
	MessageId string `json:"MessageId"`
	MD5OfBody string `json:"MD5OfMessageBody"`
}
type sendMessageBatchJSON struct {
	Successful []sendMessageBatchEntryJSON `json:"Successful"`
}

func (s *Service) sendMessageBatch(w http.ResponseWriter, form map[string]string, useJSON bool) {
	queue := queueNameFromURLOrParam(form)
	if found, _ := s.db.Get(queuesBucket, queue, &Queue{}); !found {
		writeError(w, useJSON, http.StatusNotFound, "QueueDoesNotExist", "la cola no existe: "+queue)
		return
	}
	var results []sendMessageBatchResultEntry
	var resultsJSON []sendMessageBatchEntryJSON
	for _, e := range indexedEntries(form, "SendMessageBatchRequestEntry") {
		body := e["MessageBody"]
		id := randomID()
		msg := Message{ID: id, Queue: queue, Body: body, ReceiptHandle: randomID()}
		if err := s.db.Put(messagesBucket, queue+"/"+id, msg); err != nil {
			writeError(w, useJSON, http.StatusInternalServerError, "InternalError", err.Error())
			return
		}
		results = append(results, sendMessageBatchResultEntry{Id: e["Id"], MessageId: id, MD5OfBody: md5Hex(body)})
		resultsJSON = append(resultsJSON, sendMessageBatchEntryJSON{Id: e["Id"], MessageId: id, MD5OfBody: md5Hex(body)})
	}
	writeResult(w, useJSON, http.StatusOK,
		sendMessageBatchResponse{Result: sendMessageBatchResult{Entries: results}},
		sendMessageBatchJSON{Successful: resultsJSON})
}

type deleteMessageBatchResponse struct {
	XMLName xml.Name                 `xml:"DeleteMessageBatchResponse"`
	Result  deleteMessageBatchResult `xml:"DeleteMessageBatchResult"`
}
type deleteMessageBatchResult struct {
	Entries []deleteMessageBatchResultEntry `xml:"DeleteMessageBatchResultEntry"`
}
type deleteMessageBatchResultEntry struct {
	Id string `xml:"Id"`
}
type deleteMessageBatchEntryJSON struct {
	Id string `json:"Id"`
}
type deleteMessageBatchJSON struct {
	Successful []deleteMessageBatchEntryJSON `json:"Successful"`
}

func (s *Service) deleteMessageBatch(w http.ResponseWriter, form map[string]string, useJSON bool) {
	queue := queueNameFromURLOrParam(form)
	var results []deleteMessageBatchResultEntry
	var resultsJSON []deleteMessageBatchEntryJSON
	for _, e := range indexedEntries(form, "DeleteMessageBatchRequestEntry") {
		receipt := e["ReceiptHandle"]
		var toDelete string
		_ = s.db.List(messagesBucket, queue+"/", func(key string, raw []byte) error {
			var m Message
			if err := unmarshal(raw, &m); err == nil && m.ReceiptHandle == receipt {
				toDelete = key
			}
			return nil
		})
		if toDelete != "" {
			_ = s.db.Delete(messagesBucket, toDelete)
		}
		results = append(results, deleteMessageBatchResultEntry{Id: e["Id"]})
		resultsJSON = append(resultsJSON, deleteMessageBatchEntryJSON{Id: e["Id"]})
	}
	writeResult(w, useJSON, http.StatusOK,
		deleteMessageBatchResponse{Result: deleteMessageBatchResult{Entries: results}},
		deleteMessageBatchJSON{Successful: resultsJSON})
}
