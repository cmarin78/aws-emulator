// Package events emula el subconjunto más usado de Amazon EventBridge:
// el bus de eventos default, PutEvents, y reglas (PutRule/PutTargets) con
// matching de patrón simplificado, sobre el protocolo JSON real
// (X-Amz-Target: AWSEvents.{Action}, Content-Type:
// application/x-amz-json-1.1) — confirmado con `aws events put-events
// --debug`, ver ROADMAP.md.
//
// Solo existe el bus "default" (no se soporta CreateEventBus). El
// matching de patrón es deliberadamente simple: un EventPattern es un
// objeto JSON donde cada clave mapea a un array de valores permitidos
// (el "value match" más común de EventBridge); se evalúa contra
// source/detail-type del evento y, con un nivel de anidamiento, contra
// las claves de detail. No se soportan patrones avanzados (prefix,
// numeric, anything-but, exists, $or, etc.).
//
// Los targets de una regla solo pueden ser una cola SQS o un tópico SNS
// (arn:aws:sqs:... / arn:aws:sns:...); otros tipos de target (Lambda,
// Step Functions, etc.) se ignoran silenciosamente al entregar, igual
// que sns.Publish ignora protocolos de suscripción no soportados.
package events

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/cesarmarin/aws-emulator/internal/server"
	"github.com/cesarmarin/aws-emulator/internal/services/sns"
	"github.com/cesarmarin/aws-emulator/internal/services/sqs"
	"github.com/cesarmarin/aws-emulator/internal/storage"
)

const (
	rulesBucket   = "events.rules"
	targetsBucket = "events.targets"
	accountID     = "000000000000"
	defaultBus    = "default"
)

// Service agrupa el estado del servicio EventBridge. Depende de
// *sqs.Service y *sns.Service para poder entregar a targets SQS/SNS sin
// pasar por HTTP, igual que sns.Service depende de *sqs.Service.
type Service struct {
	db  *storage.DB
	sqs *sqs.Service
	sns *sns.Service
}

// New crea el servicio EventBridge. sqsSvc/snsSvc pueden ser nil si no se
// necesita la entrega a esos targets (p. ej. tests que solo ejercitan
// PutRule/ListRules).
func New(db *storage.DB, sqsSvc *sqs.Service, snsSvc *sns.Service) *Service {
	return &Service{db: db, sqs: sqsSvc, sns: snsSvc}
}

// Rule es la forma persistida de una regla del bus default.
type Rule struct {
	Name         string `json:"name"`
	Arn          string `json:"arn"`
	EventPattern string `json:"eventPattern"`
	State        string `json:"state"`
}

// Target es la forma persistida de un target asociado a una regla.
type Target struct {
	RuleName string `json:"ruleName"`
	Id       string `json:"id"`
	Arn      string `json:"arn"`
}

func ruleArn(name string) string {
	return "arn:aws:events:us-east-1:" + accountID + ":rule/" + defaultBus + "/" + name
}

func (s *Service) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	target := r.Header.Get("X-Amz-Target")
	_, action, _ := strings.Cut(target, ".")

	body, _ := decodeJSONBody(r)

	switch action {
	case "PutEvents":
		s.putEvents(w, body)
	case "PutRule":
		s.putRule(w, body)
	case "DeleteRule":
		s.deleteRule(w, body)
	case "ListRules":
		s.listRules(w, body)
	case "PutTargets":
		s.putTargets(w, body)
	case "RemoveTargets":
		s.removeTargets(w, body)
	case "ListTargetsByRule":
		s.listTargetsByRule(w, body)
	default:
		server.WriteJSONError(w, http.StatusBadRequest, "InvalidAction",
			"acción EventBridge no soportada en este emulador: "+action)
	}
}

// Reset limpia todo el estado persistido de EventBridge (reglas y
// targets). Implementa server.Resettable.
func (s *Service) Reset() error {
	return s.db.Reset(rulesBucket, targetsBucket)
}

func decodeJSONBody(r *http.Request) (map[string]any, error) {
	defer r.Body.Close()
	out := map[string]any{}
	if r.ContentLength == 0 {
		return out, nil
	}
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(&out); err != nil {
		return out, err
	}
	return out, nil
}

// --- PutEvents ---

type putEventsEntryResult struct {
	EventId string `json:"EventId"`
}

func (s *Service) putEvents(w http.ResponseWriter, body map[string]any) {
	entriesRaw, _ := body["Entries"].([]any)
	results := make([]putEventsEntryResult, 0, len(entriesRaw))

	rules := s.loadEnabledRules()

	for _, e := range entriesRaw {
		entry, _ := e.(map[string]any)
		source, _ := entry["Source"].(string)
		detailType, _ := entry["DetailType"].(string)
		detailStr, _ := entry["Detail"].(string)

		event := map[string]any{
			"source":      source,
			"detail-type": detailType,
		}
		var detail map[string]any
		if detailStr != "" {
			_ = json.Unmarshal([]byte(detailStr), &detail)
		}
		event["detail"] = detail

		eventID := randomID()
		results = append(results, putEventsEntryResult{EventId: eventID})

		envelope, _ := json.Marshal(map[string]any{
			"id":          eventID,
			"source":      source,
			"detail-type": detailType,
			"detail":      detail,
			"time":        time.Now().UTC().Format(time.RFC3339),
		})

		for _, rule := range rules {
			if !matchesPattern(rule.EventPattern, event) {
				continue
			}
			s.deliverToTargets(rule.Name, string(envelope))
		}
	}

	server.WriteJSON(w, http.StatusOK, map[string]any{
		"FailedEntryCount": 0,
		"Entries":          results,
	})
}

func (s *Service) loadEnabledRules() []Rule {
	var rules []Rule
	_ = s.db.List(rulesBucket, "", func(_ string, raw []byte) error {
		var rule Rule
		if err := json.Unmarshal(raw, &rule); err == nil && rule.State != "DISABLED" {
			rules = append(rules, rule)
		}
		return nil
	})
	return rules
}

// deliverToTargets junta primero los targets de la regla en un slice y
// recién después de cerrado el db.List/View de lectura llama a
// sqs.DeliverMessage/sns.PublishMessage (ambos hacen su propio db.Get/Put
// sobre el mismo *storage.DB). Igual que en sns.deliverToSubscribers,
// llamar a esos métodos desde dentro del callback de List() deadlockea
// Bolt (transacción de escritura abierta desde el mismo goroutine que
// todavía tiene una de lectura abierta).
func (s *Service) deliverToTargets(ruleName, message string) {
	var targets []Target
	_ = s.db.List(targetsBucket, ruleName+"/", func(_ string, raw []byte) error {
		var t Target
		if err := json.Unmarshal(raw, &t); err == nil {
			targets = append(targets, t)
		}
		return nil
	})
	for _, t := range targets {
		switch {
		case strings.Contains(t.Arn, ":sqs:"):
			if s.sqs != nil {
				queueName := arnResourceName(t.Arn)
				_, _ = s.sqs.DeliverMessage(queueName, message)
			}
		case strings.Contains(t.Arn, ":sns:"):
			if s.sns != nil {
				topicName := arnResourceName(t.Arn)
				_, _ = s.sns.PublishMessage(topicName, message)
			}
		}
	}
}

func arnResourceName(arn string) string {
	if i := strings.LastIndex(arn, ":"); i != -1 {
		return arn[i+1:]
	}
	return arn
}

// matchesPattern implementa el subconjunto "value match" de
// EventPattern: pattern es un objeto JSON {"campo": ["valor1","valor2"]}
// evaluado contra event (con un nivel de anidamiento para "detail"). Un
// pattern vacío o inválido matchea todo (igual que no tener
// EventPattern en una regla real).
func matchesPattern(pattern string, event map[string]any) bool {
	if strings.TrimSpace(pattern) == "" {
		return true
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(pattern), &parsed); err != nil {
		return true
	}
	return matchFields(parsed, event)
}

func matchFields(pattern map[string]any, event map[string]any) bool {
	for key, want := range pattern {
		got, ok := event[key]
		if !ok {
			return false
		}
		switch w := want.(type) {
		case []any:
			if !matchAnyValue(w, got) {
				return false
			}
		case map[string]any:
			gotMap, ok := got.(map[string]any)
			if !ok || !matchFields(w, gotMap) {
				return false
			}
		default:
			return false
		}
	}
	return true
}

func matchAnyValue(allowed []any, got any) bool {
	gotStr, isStr := got.(string)
	for _, v := range allowed {
		if isStr {
			if vs, ok := v.(string); ok && vs == gotStr {
				return true
			}
		} else if v == got {
			return true
		}
	}
	return false
}

func randomID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// --- reglas ---

func (s *Service) putRule(w http.ResponseWriter, body map[string]any) {
	name, _ := body["Name"].(string)
	if name == "" {
		server.WriteJSONError(w, http.StatusBadRequest, "ValidationException", "Name es requerido")
		return
	}
	pattern, _ := body["EventPattern"].(string)
	state, _ := body["State"].(string)
	if state == "" {
		state = "ENABLED"
	}
	rule := Rule{Name: name, Arn: ruleArn(name), EventPattern: pattern, State: state}
	if err := s.db.Put(rulesBucket, name, rule); err != nil {
		server.WriteJSONError(w, http.StatusInternalServerError, "InternalFailure", err.Error())
		return
	}
	server.WriteJSON(w, http.StatusOK, map[string]any{"RuleArn": rule.Arn})
}

func (s *Service) deleteRule(w http.ResponseWriter, body map[string]any) {
	name, _ := body["Name"].(string)
	if err := s.db.DeletePrefix(targetsBucket, name+"/"); err != nil {
		server.WriteJSONError(w, http.StatusInternalServerError, "InternalFailure", err.Error())
		return
	}
	if err := s.db.Delete(rulesBucket, name); err != nil {
		server.WriteJSONError(w, http.StatusInternalServerError, "InternalFailure", err.Error())
		return
	}
	server.WriteJSON(w, http.StatusOK, map[string]any{})
}

func (s *Service) listRules(w http.ResponseWriter, _ map[string]any) {
	var rules []Rule
	_ = s.db.List(rulesBucket, "", func(_ string, raw []byte) error {
		var rule Rule
		if err := json.Unmarshal(raw, &rule); err == nil {
			rules = append(rules, rule)
		}
		return nil
	})
	out := make([]map[string]any, 0, len(rules))
	for _, r := range rules {
		out = append(out, map[string]any{
			"Name":         r.Name,
			"Arn":          r.Arn,
			"EventPattern": r.EventPattern,
			"State":        r.State,
		})
	}
	server.WriteJSON(w, http.StatusOK, map[string]any{"Rules": out})
}

// --- targets ---

func (s *Service) putTargets(w http.ResponseWriter, body map[string]any) {
	ruleName, _ := body["Rule"].(string)
	targetsRaw, _ := body["Targets"].([]any)
	if ruleName == "" {
		server.WriteJSONError(w, http.StatusBadRequest, "ValidationException", "Rule es requerido")
		return
	}
	for _, tr := range targetsRaw {
		tm, _ := tr.(map[string]any)
		id, _ := tm["Id"].(string)
		arn, _ := tm["Arn"].(string)
		if id == "" {
			id = randomID()
		}
		target := Target{RuleName: ruleName, Id: id, Arn: arn}
		_ = s.db.Put(targetsBucket, ruleName+"/"+id, target)
	}
	server.WriteJSON(w, http.StatusOK, map[string]any{"FailedEntryCount": 0, "FailedEntries": []any{}})
}

func (s *Service) removeTargets(w http.ResponseWriter, body map[string]any) {
	ruleName, _ := body["Rule"].(string)
	idsRaw, _ := body["Ids"].([]any)
	for _, idAny := range idsRaw {
		id, _ := idAny.(string)
		_ = s.db.Delete(targetsBucket, ruleName+"/"+id)
	}
	server.WriteJSON(w, http.StatusOK, map[string]any{"FailedEntryCount": 0, "FailedEntries": []any{}})
}

func (s *Service) listTargetsByRule(w http.ResponseWriter, body map[string]any) {
	ruleName, _ := body["Rule"].(string)
	var out []map[string]any
	_ = s.db.List(targetsBucket, ruleName+"/", func(_ string, raw []byte) error {
		var t Target
		if err := json.Unmarshal(raw, &t); err == nil {
			out = append(out, map[string]any{"Id": t.Id, "Arn": t.Arn})
		}
		return nil
	})
	server.WriteJSON(w, http.StatusOK, map[string]any{"Targets": out})
}
