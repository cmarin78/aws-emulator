// Package sns emula el subconjunto más usado de Amazon SNS: tópicos,
// suscripciones y Publish, con el protocolo Query/XML clásico (a
// diferencia de SQS, botocore todavía manda SNS como form-urlencoded
// clásico, no JSON — confirmado con `aws sns create-topic --debug`, ver
// ROADMAP.md). Solo se soporta el protocolo de suscripción "sqs": Publish
// entrega el mensaje directamente en la cola SQS correspondiente vía
// sqs.Service.DeliverMessage, igual que haría una integración real
// SNS->SQS. No se implementan otros protocolos (http/https/email/lambda),
// filtros de mensaje, ni FIFO topics.
package sns

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/cesarmarin/aws-emulator/internal/accountctx"
	"github.com/cesarmarin/aws-emulator/internal/router"
	"github.com/cesarmarin/aws-emulator/internal/server"
	"github.com/cesarmarin/aws-emulator/internal/services/sqs"
	"github.com/cesarmarin/aws-emulator/internal/storage"
)

const (
	topicsBucket        = "sns.topics"
	subscriptionsBucket = "sns.subscriptions"
)

// Service agrupa el estado del servicio SNS. Depende de *sqs.Service (no
// solo de storage.DB) porque Publish necesita entregar mensajes a colas
// SQS suscriptas reusando su lógica de persistencia (DeliverMessage), en
// vez de escribir directamente en los buckets internos de SQS.
type Service struct {
	db  *storage.DB
	sqs *sqs.Service
}

// New crea el servicio SNS. sqsSvc puede ser nil si no se necesita la
// integración Publish->SQS (p. ej. en tests unitarios de solo tópicos).
func New(db *storage.DB, sqsSvc *sqs.Service) *Service {
	return &Service{db: db, sqs: sqsSvc}
}

// Topic es la forma persistida de un tópico SNS.
type Topic struct {
	Name       string    `json:"name"`
	Arn        string    `json:"arn"`
	CreateDate time.Time `json:"createDate"`
}

// Subscription es la forma persistida de una suscripción a un tópico.
type Subscription struct {
	SubscriptionArn string `json:"subscriptionArn"`
	TopicArn        string `json:"topicArn"`
	Protocol        string `json:"protocol"`
	Endpoint        string `json:"endpoint"`
}

func topicArn(accountID, name string) string {
	return "arn:aws:sns:us-east-1:" + accountID + ":" + name
}

// topicNameFromArn devuelve el último segmento de un ARN de tópico
// (arn:aws:sns:region:account:name -> name).
func topicNameFromArn(arn string) string {
	if i := strings.LastIndex(arn, ":"); i != -1 {
		return arn[i+1:]
	}
	return arn
}

func randomID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (s *Service) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	req := router.FromHTTPRequest(r)
	action := req.Action
	if action == "" {
		action = r.URL.Query().Get("Action")
	}
	form := formValues(r)
	accountID, _ := accountctx.FromContext(r.Context())

	switch action {
	case "CreateTopic":
		s.createTopic(w, form, accountID)
	case "ListTopics":
		s.listTopics(w)
	case "DeleteTopic":
		s.deleteTopic(w, form)
	case "Subscribe":
		s.subscribe(w, form)
	case "Unsubscribe":
		s.unsubscribe(w, form)
	case "ListSubscriptionsByTopic":
		s.listSubscriptionsByTopic(w, form)
	case "Publish":
		s.publish(w, form)
	case "GetTopicAttributes":
		s.getTopicAttributes(w, form, accountID)
	case "SetTopicAttributes":
		s.setTopicAttributes(w, form)
	case "ListTagsForResource":
		s.listTagsForResource(w, form)
	case "GetSubscriptionAttributes":
		s.getSubscriptionAttributes(w, form, accountID)
	default:
		server.WriteXMLError(w, http.StatusBadRequest, "InvalidAction",
			"acción SNS no soportada en este emulador: "+action)
	}
}

// Reset limpia todo el estado persistido de SNS (tópicos y suscripciones).
// Implementa server.Resettable.
func (s *Service) Reset() error {
	return s.db.Reset(topicsBucket, subscriptionsBucket)
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

// --- tópicos ---

type createTopicResponse struct {
	XMLName xml.Name          `xml:"CreateTopicResponse"`
	Result  createTopicResult `xml:"CreateTopicResult"`
}
type createTopicResult struct {
	TopicArn string `xml:"TopicArn"`
}

func (s *Service) createTopic(w http.ResponseWriter, form map[string]string, accountID string) {
	name := form["Name"]
	if name == "" {
		server.WriteXMLError(w, http.StatusBadRequest, "ValidationError", "Name es requerido")
		return
	}
	var existing Topic
	if found, _ := s.db.Get(topicsBucket, name, &existing); found {
		server.WriteXML(w, http.StatusOK, createTopicResponse{Result: createTopicResult{TopicArn: existing.Arn}})
		return
	}
	t := Topic{Name: name, Arn: topicArn(accountID, name), CreateDate: time.Now().UTC()}
	if err := s.db.Put(topicsBucket, name, t); err != nil {
		server.WriteXMLError(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	server.WriteXML(w, http.StatusOK, createTopicResponse{Result: createTopicResult{TopicArn: t.Arn}})
}

type listTopicsResponse struct {
	XMLName xml.Name         `xml:"ListTopicsResponse"`
	Result  listTopicsResult `xml:"ListTopicsResult"`
}
type listTopicsResult struct {
	Topics []topicXML `xml:"Topics>member"`
}
type topicXML struct {
	TopicArn string `xml:"TopicArn"`
}

func (s *Service) listTopics(w http.ResponseWriter) {
	var topics []Topic
	_ = s.db.List(topicsBucket, "", func(_ string, raw []byte) error {
		var t Topic
		if err := unmarshal(raw, &t); err == nil {
			topics = append(topics, t)
		}
		return nil
	})
	sort.Slice(topics, func(i, j int) bool { return topics[i].Name < topics[j].Name })
	out := make([]topicXML, 0, len(topics))
	for _, t := range topics {
		out = append(out, topicXML{TopicArn: t.Arn})
	}
	server.WriteXML(w, http.StatusOK, listTopicsResponse{Result: listTopicsResult{Topics: out}})
}

type deleteTopicResponse struct {
	XMLName xml.Name `xml:"DeleteTopicResponse"`
}

func (s *Service) deleteTopic(w http.ResponseWriter, form map[string]string) {
	arn := form["TopicArn"]
	name := topicNameFromArn(arn)
	if err := s.db.DeletePrefix(subscriptionsBucket, arn+":"); err != nil {
		server.WriteXMLError(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	if err := s.db.Delete(topicsBucket, name); err != nil {
		server.WriteXMLError(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	server.WriteXML(w, http.StatusOK, deleteTopicResponse{})
}

// getTopicAttributes: el provider real de Terraform llama a esto durante
// su Read (no solo en su Create) para refrescar el estado completo del
// tópico -- encontrado vía terraform/aws-smoke-test, ver ROADMAP.md. Solo
// se exponen los atributos mínimos que el emulador realmente modela
// (TopicArn/Owner); el resto de los atributos reales de SNS (Policy,
// DeliveryPolicy, etc.) no se implementan, así que no se incluyen.
type getTopicAttributesResponse struct {
	XMLName xml.Name                 `xml:"GetTopicAttributesResponse"`
	Result  getTopicAttributesResult `xml:"GetTopicAttributesResult"`
}
type getTopicAttributesResult struct {
	Attributes attributeEntries `xml:"Attributes"`
}
type attributeEntries struct {
	Entries []attributeEntry `xml:"entry"`
}
type attributeEntry struct {
	Key   string `xml:"key"`
	Value string `xml:"value"`
}

func (s *Service) getTopicAttributes(w http.ResponseWriter, form map[string]string, accountID string) {
	arn := form["TopicArn"]
	name := topicNameFromArn(arn)
	var t Topic
	found, err := s.db.Get(topicsBucket, name, &t)
	if err != nil {
		server.WriteXMLError(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	if !found {
		server.WriteXMLError(w, http.StatusNotFound, "NotFound", "el tópico no existe: "+arn)
		return
	}
	server.WriteXML(w, http.StatusOK, getTopicAttributesResponse{
		Result: getTopicAttributesResult{Attributes: attributeEntries{Entries: []attributeEntry{
			{Key: "TopicArn", Value: t.Arn},
			{Key: "Owner", Value: accountID},
			// Policy: el provider de Terraform no solo parsea este atributo
			// como JSON en su Read de aws_sns_topic -- también camina su
			// campo "Statement" buscando ARNs/account IDs de principals
			// válidos (findTopicAttributesWithValidAWSPrincipalsByARN ->
			// tfiam.PolicyHasValidAWSPrincipals). Un objeto JSON vacío "{}"
			// no tiene "Statement", así que ese código cae en su rama
			// default sobre un valor nil y rompe con "parsing policy:
			// unexpected result: (<nil>) "<nil>"". Un documento de policy
			// mínimo pero válido (con Statement como lista vacía) evita esa
			// rama sin que el emulador necesite evaluar políticas
			// realmente. Encontrado vía terraform/aws-smoke-test, ver
			// ROADMAP.md.
			{Key: "Policy", Value: `{"Version":"2012-10-17","Statement":[]}`},
		}}},
	})
}

// setTopicAttributes: este emulador no modela ningún atributo real de
// tópico (Policy, DeliveryPolicy, FirehoseSuccessFeedbackSampleRate,
// etc.) -- el provider de Terraform manda SetTopicAttributes para varios
// de estos durante el Create/Update de aws_sns_topic, así que con solo
// validar que el tópico exista y devolver éxito alcanza para no romper el
// apply, igual que setQueueAttributes en SQS (con la diferencia de que ahí
// sí persistimos los valores porque GetQueueAttributes los expone de
// vuelta). Encontrado vía terraform/aws-smoke-test, ver ROADMAP.md.
type setTopicAttributesResponse struct {
	XMLName xml.Name `xml:"SetTopicAttributesResponse"`
}

func (s *Service) setTopicAttributes(w http.ResponseWriter, form map[string]string) {
	arn := form["TopicArn"]
	name := topicNameFromArn(arn)
	if found, _ := s.db.Get(topicsBucket, name, &Topic{}); !found {
		server.WriteXMLError(w, http.StatusNotFound, "NotFound", "el tópico no existe: "+arn)
		return
	}
	server.WriteXML(w, http.StatusOK, setTopicAttributesResponse{})
}

// listTagsForResource: este emulador no implementa tags de SNS (no hay
// TagResource/UntagResource) -- el provider de Terraform llama a esto
// durante el Read de aws_sns_topic para refrescar tags_all, así que con
// devolver una lista vacía alcanza para no romper el apply, mismo patrón
// que events/logs/ssm.listTagsForResource. Encontrado vía
// terraform/aws-smoke-test, ver ROADMAP.md.
type listTagsForResourceResponse struct {
	XMLName xml.Name                  `xml:"ListTagsForResourceResponse"`
	Result  listTagsForResourceResult `xml:"ListTagsForResourceResult"`
}
type listTagsForResourceResult struct {
	Tags []tagXML `xml:"Tags>member"`
}
type tagXML struct {
	Key   string `xml:"Key"`
	Value string `xml:"Value"`
}

func (s *Service) listTagsForResource(w http.ResponseWriter, form map[string]string) {
	arn := form["ResourceArn"]
	name := topicNameFromArn(arn)
	if found, _ := s.db.Get(topicsBucket, name, &Topic{}); !found {
		server.WriteXMLError(w, http.StatusNotFound, "ResourceNotFoundException", "el tópico no existe: "+arn)
		return
	}
	server.WriteXML(w, http.StatusOK, listTagsForResourceResponse{Result: listTagsForResourceResult{Tags: []tagXML{}}})
}

// --- suscripciones ---

type subscribeResponse struct {
	XMLName xml.Name        `xml:"SubscribeResponse"`
	Result  subscribeResult `xml:"SubscribeResult"`
}
type subscribeResult struct {
	SubscriptionArn string `xml:"SubscriptionArn"`
}

func (s *Service) subscribe(w http.ResponseWriter, form map[string]string) {
	topicArnParam := form["TopicArn"]
	protocol := form["Protocol"]
	endpoint := form["Endpoint"]
	if topicArnParam == "" || protocol == "" || endpoint == "" {
		server.WriteXMLError(w, http.StatusBadRequest, "ValidationError",
			"TopicArn, Protocol y Endpoint son requeridos")
		return
	}
	subArn := topicArnParam + ":" + randomID()
	sub := Subscription{SubscriptionArn: subArn, TopicArn: topicArnParam, Protocol: protocol, Endpoint: endpoint}
	if err := s.db.Put(subscriptionsBucket, subArn, sub); err != nil {
		server.WriteXMLError(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	server.WriteXML(w, http.StatusOK, subscribeResponse{Result: subscribeResult{SubscriptionArn: subArn}})
}

// getSubscriptionAttributes: el provider de Terraform, después de
// Subscribe, hace polling de esto para esperar a que la suscripción se
// confirme (aws_sns_topic_subscription espera PendingConfirmation="false")
// -- relevante para protocolos como email/http donde la confirmación es
// asincrónica y requiere que el endpoint visite un link. Este emulador
// solo soporta el protocolo "sqs" (ver comentario del paquete), que en
// AWS real se autoconfirma inmediatamente sin esperar nada, así que basta
// devolver PendingConfirmation="false" siempre para no romper el waiter.
// Encontrado vía terraform/aws-smoke-test, ver ROADMAP.md.
type getSubscriptionAttributesResponse struct {
	XMLName xml.Name                        `xml:"GetSubscriptionAttributesResponse"`
	Result  getSubscriptionAttributesResult `xml:"GetSubscriptionAttributesResult"`
}
type getSubscriptionAttributesResult struct {
	Attributes attributeEntries `xml:"Attributes"`
}

func (s *Service) getSubscriptionAttributes(w http.ResponseWriter, form map[string]string, accountID string) {
	arn := form["SubscriptionArn"]
	var sub Subscription
	if found, _ := s.db.Get(subscriptionsBucket, arn, &sub); !found {
		server.WriteXMLError(w, http.StatusNotFound, "NotFound", "la suscripción no existe: "+arn)
		return
	}
	server.WriteXML(w, http.StatusOK, getSubscriptionAttributesResponse{
		Result: getSubscriptionAttributesResult{Attributes: attributeEntries{Entries: []attributeEntry{
			{Key: "SubscriptionArn", Value: sub.SubscriptionArn},
			{Key: "TopicArn", Value: sub.TopicArn},
			{Key: "Protocol", Value: sub.Protocol},
			{Key: "Endpoint", Value: sub.Endpoint},
			{Key: "PendingConfirmation", Value: "false"},
			{Key: "Owner", Value: accountID},
			{Key: "RawMessageDelivery", Value: "false"},
		}}},
	})
}

type unsubscribeResponse struct {
	XMLName xml.Name `xml:"UnsubscribeResponse"`
}

func (s *Service) unsubscribe(w http.ResponseWriter, form map[string]string) {
	arn := form["SubscriptionArn"]
	if err := s.db.Delete(subscriptionsBucket, arn); err != nil {
		server.WriteXMLError(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	server.WriteXML(w, http.StatusOK, unsubscribeResponse{})
}

type listSubscriptionsByTopicResponse struct {
	XMLName xml.Name                       `xml:"ListSubscriptionsByTopicResponse"`
	Result  listSubscriptionsByTopicResult `xml:"ListSubscriptionsByTopicResult"`
}
type listSubscriptionsByTopicResult struct {
	Subscriptions []subscriptionXML `xml:"Subscriptions>member"`
}
type subscriptionXML struct {
	SubscriptionArn string `xml:"SubscriptionArn"`
	TopicArn        string `xml:"TopicArn"`
	Protocol        string `xml:"Protocol"`
	Endpoint        string `xml:"Endpoint"`
}

func (s *Service) listSubscriptionsByTopic(w http.ResponseWriter, form map[string]string) {
	arn := form["TopicArn"]
	var out []subscriptionXML
	_ = s.db.List(subscriptionsBucket, arn+":", func(_ string, raw []byte) error {
		var sub Subscription
		if err := unmarshal(raw, &sub); err == nil {
			out = append(out, subscriptionXML{
				SubscriptionArn: sub.SubscriptionArn,
				TopicArn:        sub.TopicArn,
				Protocol:        sub.Protocol,
				Endpoint:        sub.Endpoint,
			})
		}
		return nil
	})
	server.WriteXML(w, http.StatusOK,
		listSubscriptionsByTopicResponse{Result: listSubscriptionsByTopicResult{Subscriptions: out}})
}

// --- publish ---

type publishResponse struct {
	XMLName xml.Name      `xml:"PublishResponse"`
	Result  publishResult `xml:"PublishResult"`
}
type publishResult struct {
	MessageId string `xml:"MessageId"`
}

// publish entrega el mensaje a cada suscripción "sqs" del tópico. El
// Endpoint de una suscripción sqs es el ARN de la cola
// (arn:aws:sqs:region:account:queueName); se extrae el nombre y se
// entrega vía sqs.Service.DeliverMessage. Otros protocolos de suscripción
// (http/https/email/lambda) no están implementados: se ignoran
// silenciosamente, igual que ministack hace con integraciones no
// soportadas, para no romper Publish cuando el tópico tiene una mezcla de
// suscripciones soportadas y no soportadas.
func (s *Service) publish(w http.ResponseWriter, form map[string]string) {
	arn := form["TopicArn"]
	message := form["Message"]
	if arn == "" {
		server.WriteXMLError(w, http.StatusBadRequest, "ValidationError", "TopicArn es requerido")
		return
	}
	id := s.deliverToSubscribers(arn, message)
	server.WriteXML(w, http.StatusOK, publishResponse{Result: publishResult{MessageId: id}})
}

// PublishMessage publica message al tópico identificado por name (no ARN
// completo) y devuelve el MessageId generado. Expuesto para que otros
// servicios (EventBridge con un target SNS) puedan publicar sin pasar por
// el protocolo HTTP, igual que sqs.Service.DeliverMessage.
func (s *Service) PublishMessage(topicName, message string) (messageID string, err error) {
	var t Topic
	if found, _ := s.db.Get(topicsBucket, topicName, &t); !found {
		return "", fmt.Errorf("el tópico no existe: %s", topicName)
	}
	return s.deliverToSubscribers(t.Arn, message), nil
}

// deliverToSubscribers entrega message a cada suscripción "sqs" del
// tópico (ver comentario de publish() sobre protocolos no soportados) y
// devuelve un MessageId nuevo.
//
// Importante: primero junta las suscripciones que matchean en un slice y
// recién después, ya cerrado el db.List/View de lectura, llama a
// sqs.DeliverMessage (que hace su propio db.Get/Put). Bolt prohíbe abrir
// una transacción de escritura desde el mismo goroutine mientras una de
// lectura sigue abierta — entrar en deadlock haciendo Put() dentro del
// callback de List() fue justamente el primer bug real que apareció al
// probar esto contra el AWS CLI real.
func (s *Service) deliverToSubscribers(topicArnVal, message string) string {
	if s.sqs != nil {
		var queueNames []string
		_ = s.db.List(subscriptionsBucket, topicArnVal+":", func(_ string, raw []byte) error {
			var sub Subscription
			if err := unmarshal(raw, &sub); err != nil || sub.Protocol != "sqs" {
				return nil
			}
			queueNames = append(queueNames, topicNameFromArn(sub.Endpoint))
			return nil
		})
		for _, queueName := range queueNames {
			_, _ = s.sqs.DeliverMessage(queueName, message)
		}
	}
	return randomID()
}
