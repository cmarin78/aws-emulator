// Package sqs emula el subconjunto más usado de Amazon SQS: colas y
// mensajes, con el protocolo Query/XML clásico (CreateQueue, SendMessage,
// ReceiveMessage, DeleteMessage, ListQueues, GetQueueUrl, DeleteQueue).
// No implementa colas FIFO, dead-letter queues, ni long polling real
// (ReceiveMessage no bloquea — devuelve inmediatamente lo que haya).
package sqs

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/xml"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/cesarmarin/aws-emulator/internal/server"
	"github.com/cesarmarin/aws-emulator/internal/storage"
)

const (
	queuesBucket   = "sqs.queues"
	messagesBucket = "sqs.messages"
	accountID      = "000000000000"
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
	ID           string `json:"id"`
	Queue        string `json:"queue"`
	Body         string `json:"body"`
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
	form := formValues(r)
	action := form["Action"]
	if action == "" {
		action = r.URL.Query().Get("Action")
	}

	switch action {
	case "CreateQueue":
		s.createQueue(w, form)
	case "ListQueues":
		s.listQueues(w)
	case "GetQueueUrl":
		s.getQueueURL(w, form)
	case "DeleteQueue":
		s.deleteQueue(w, form)
	case "SendMessage":
		s.sendMessage(w, form)
	case "ReceiveMessage":
		s.receiveMessage(w, form)
	case "DeleteMessage":
		s.deleteMessage(w, form)
	case "PurgeQueue":
		s.purgeQueue(w, form)
	default:
		server.WriteXMLError(w, http.StatusBadRequest, "InvalidAction",
			"acción SQS no soportada en este emulador: "+action)
	}
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

// --- colas ---

type createQueueResponse struct {
	XMLName xml.Name          `xml:"CreateQueueResponse"`
	Result  createQueueResult `xml:"CreateQueueResult"`
}
type createQueueResult struct {
	QueueUrl string `xml:"QueueUrl"`
}

func (s *Service) createQueue(w http.ResponseWriter, form map[string]string) {
	name := form["QueueName"]
	if name == "" {
		server.WriteXMLError(w, http.StatusBadRequest, "MissingParameter", "QueueName es requerido")
		return
	}
	var existing Queue
	if found, _ := s.db.Get(queuesBucket, name, &existing); found {
		server.WriteXML(w, http.StatusOK, createQueueResponse{Result: createQueueResult{QueueUrl: existing.URL}})
		return
	}
	q := Queue{Name: name, URL: queueURL(name), CreateDate: time.Now().UTC()}
	if err := s.db.Put(queuesBucket, name, q); err != nil {
		server.WriteXMLError(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	server.WriteXML(w, http.StatusOK, createQueueResponse{Result: createQueueResult{QueueUrl: q.URL}})
}

type listQueuesResponse struct {
	XMLName xml.Name         `xml:"ListQueuesResponse"`
	Result  listQueuesResult `xml:"ListQueuesResult"`
}
type listQueuesResult struct {
	QueueUrls []string `xml:"QueueUrl"`
}

func (s *Service) listQueues(w http.ResponseWriter) {
	var urls []string
	_ = s.db.List(queuesBucket, "", func(_ string, raw []byte) error {
		var q Queue
		if err := unmarshal(raw, &q); err == nil {
			urls = append(urls, q.URL)
		}
		return nil
	})
	sort.Strings(urls)
	server.WriteXML(w, http.StatusOK, listQueuesResponse{Result: listQueuesResult{QueueUrls: urls}})
}

type getQueueURLResponse struct {
	XMLName xml.Name          `xml:"GetQueueUrlResponse"`
	Result  getQueueURLResult `xml:"GetQueueUrlResult"`
}
type getQueueURLResult struct {
	QueueUrl string `xml:"QueueUrl"`
}

func (s *Service) getQueueURL(w http.ResponseWriter, form map[string]string) {
	name := form["QueueName"]
	var q Queue
	found, err := s.db.Get(queuesBucket, name, &q)
	if err != nil {
		server.WriteXMLError(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	if !found {
		server.WriteXMLError(w, http.StatusNotFound, "QueueDoesNotExist", "la cola no existe: "+name)
		return
	}
	server.WriteXML(w, http.StatusOK, getQueueURLResponse{Result: getQueueURLResult{QueueUrl: q.URL}})
}

func (s *Service) deleteQueue(w http.ResponseWriter, form map[string]string) {
	name := queueNameFromURLOrParam(form)
	if err := s.db.DeletePrefix(messagesBucket, name+"/"); err != nil {
		server.WriteXMLError(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	if err := s.db.Delete(queuesBucket, name); err != nil {
		server.WriteXMLError(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	server.WriteXML(w, http.StatusOK, nil)
}

func (s *Service) purgeQueue(w http.ResponseWriter, form map[string]string) {
	name := queueNameFromURLOrParam(form)
	if err := s.db.DeletePrefix(messagesBucket, name+"/"); err != nil {
		server.WriteXMLError(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	server.WriteXML(w, http.StatusOK, nil)
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
	XMLName xml.Name           `xml:"SendMessageResponse"`
	Result  sendMessageResult `xml:"SendMessageResult"`
}
type sendMessageResult struct {
	MessageId string `xml:"MessageId"`
	MD5OfBody string `xml:"MD5OfMessageBody"`
}

func (s *Service) sendMessage(w http.ResponseWriter, form map[string]string) {
	queue := queueNameFromURLOrParam(form)
	if found, _ := s.db.Get(queuesBucket, queue, &Queue{}); !found {
		server.WriteXMLError(w, http.StatusNotFound, "QueueDoesNotExist", "la cola no existe: "+queue)
		return
	}
	body := form["MessageBody"]
	id := randomID()
	msg := Message{ID: id, Queue: queue, Body: body, ReceiptHandle: randomID()}
	if err := s.db.Put(messagesBucket, queue+"/"+id, msg); err != nil {
		server.WriteXMLError(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	server.WriteXML(w, http.StatusOK, sendMessageResponse{Result: sendMessageResult{
		MessageId: id,
		MD5OfBody: md5Hex(body),
	}})
}

type receiveMessageResponse struct {
	XMLName xml.Name              `xml:"ReceiveMessageResponse"`
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

func (s *Service) receiveMessage(w http.ResponseWriter, form map[string]string) {
	queue := queueNameFromURLOrParam(form)
	maxMessages := 1
	if v := form["MaxNumberOfMessages"]; v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			maxMessages = n
		}
	}

	var out []messageXML
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
		}
		return nil
	})
	server.WriteXML(w, http.StatusOK, receiveMessageResponse{Result: receiveMessageResult{Messages: out}})
}

func (s *Service) deleteMessage(w http.ResponseWriter, form map[string]string) {
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
	server.WriteXML(w, http.StatusOK, nil)
}
