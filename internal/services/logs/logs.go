// Package logs emula el subconjunto más usado de Amazon CloudWatch Logs:
// grupos y streams de log, PutLogEvents y FilterLogEvents, sobre el
// protocolo JSON real (X-Amz-Target: Logs_20140328.{Action}, Content-Type:
// application/x-amz-json-1.1) — confirmado con `aws logs create-log-group
// --debug`, ver ROADMAP.md.
//
// CloudWatch Logs dejó de requerir SequenceToken en PutLogEvents desde
// 2021 (el campo sigue existiendo en la API por compatibilidad, pero ya no
// se valida); este emulador lo ignora por completo y siempre devuelve un
// nextSequenceToken nuevo, en vez de implementar la lógica de
// encadenamiento estricta de versiones viejas de la API.
//
// FilterLogEvents soporta filtrado por startTime/endTime y, si se pasa
// filterPattern, un match por substring literal — no se implementa la
// sintaxis real de patrones de CloudWatch Logs Insights (términos,
// negación, JSON fields, etc.), siguiendo el mismo criterio de
// "simplificado a propósito" que el matching de EventBridge.
package logs

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/cesarmarin/aws-emulator/internal/server"
	"github.com/cesarmarin/aws-emulator/internal/storage"
)

const (
	groupsBucket  = "logs.groups"
	streamsBucket = "logs.streams"
	eventsBucket  = "logs.events"
	accountID     = "000000000000"
)

// Service agrupa el estado del servicio CloudWatch Logs.
type Service struct {
	db *storage.DB
}

// New crea el servicio CloudWatch Logs.
func New(db *storage.DB) *Service {
	return &Service{db: db}
}

// LogGroup es la forma persistida de un grupo de log.
type LogGroup struct {
	Name         string `json:"name"`
	Arn          string `json:"arn"`
	CreationTime int64  `json:"creationTime"`
}

// LogStream es la forma persistida de un stream de log dentro de un grupo.
type LogStream struct {
	LogGroupName        string `json:"logGroupName"`
	Name                string `json:"name"`
	Arn                 string `json:"arn"`
	CreationTime        int64  `json:"creationTime"`
	FirstEventTimestamp int64  `json:"firstEventTimestamp"`
	LastEventTimestamp  int64  `json:"lastEventTimestamp"`
	LastIngestionTime   int64  `json:"lastIngestionTime"`
	UploadSequenceToken string `json:"uploadSequenceToken"`
}

// LogEvent es un evento individual dentro de un stream.
type LogEvent struct {
	Timestamp     int64  `json:"timestamp"`
	Message       string `json:"message"`
	IngestionTime int64  `json:"ingestionTime"`
}

func groupArn(name string) string {
	return "arn:aws:logs:us-east-1:" + accountID + ":log-group:" + name
}

func streamArn(groupName, streamName string) string {
	return groupArn(groupName) + ":log-stream:" + streamName
}

func streamKey(groupName, streamName string) string {
	return groupName + "|" + streamName
}

func nowMillis() int64 {
	return time.Now().UTC().UnixMilli()
}

func (s *Service) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	target := r.Header.Get("X-Amz-Target")
	_, action, _ := strings.Cut(target, ".")

	body, _ := decodeJSONBody(r)

	switch action {
	case "CreateLogGroup":
		s.createLogGroup(w, body)
	case "DeleteLogGroup":
		s.deleteLogGroup(w, body)
	case "DescribeLogGroups":
		s.describeLogGroups(w, body)
	case "CreateLogStream":
		s.createLogStream(w, body)
	case "DeleteLogStream":
		s.deleteLogStream(w, body)
	case "DescribeLogStreams":
		s.describeLogStreams(w, body)
	case "PutLogEvents":
		s.putLogEvents(w, body)
	case "GetLogEvents":
		s.getLogEvents(w, body)
	case "FilterLogEvents":
		s.filterLogEvents(w, body)
	case "ListTagsForResource":
		s.listTagsForResource(w, body)
	default:
		server.WriteJSONError(w, http.StatusBadRequest, "InvalidAction",
			"acción CloudWatch Logs no soportada en este emulador: "+action)
	}
}

// Reset limpia todo el estado persistido de CloudWatch Logs. Implementa
// server.Resettable.
func (s *Service) Reset() error {
	return s.db.Reset(groupsBucket, streamsBucket, eventsBucket)
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

// --- grupos ---

func (s *Service) createLogGroup(w http.ResponseWriter, body map[string]any) {
	name, _ := body["logGroupName"].(string)
	if name == "" {
		server.WriteJSONError(w, http.StatusBadRequest, "ValidationException", "logGroupName es requerido")
		return
	}
	var existing LogGroup
	if found, _ := s.db.Get(groupsBucket, name, &existing); found {
		server.WriteJSONError(w, http.StatusBadRequest, "ResourceAlreadyExistsException",
			"el grupo de log ya existe: "+name)
		return
	}
	g := LogGroup{Name: name, Arn: groupArn(name), CreationTime: nowMillis()}
	if err := s.db.Put(groupsBucket, name, g); err != nil {
		server.WriteJSONError(w, http.StatusInternalServerError, "InternalFailure", err.Error())
		return
	}
	server.WriteJSON(w, http.StatusOK, map[string]any{})
}

func (s *Service) deleteLogGroup(w http.ResponseWriter, body map[string]any) {
	name, _ := body["logGroupName"].(string)
	if err := s.db.DeletePrefix(streamsBucket, name+"|"); err != nil {
		server.WriteJSONError(w, http.StatusInternalServerError, "InternalFailure", err.Error())
		return
	}
	if err := s.db.DeletePrefix(eventsBucket, name+"|"); err != nil {
		server.WriteJSONError(w, http.StatusInternalServerError, "InternalFailure", err.Error())
		return
	}
	if err := s.db.Delete(groupsBucket, name); err != nil {
		server.WriteJSONError(w, http.StatusInternalServerError, "InternalFailure", err.Error())
		return
	}
	server.WriteJSON(w, http.StatusOK, map[string]any{})
}

func (s *Service) describeLogGroups(w http.ResponseWriter, body map[string]any) {
	prefix, _ := body["logGroupNamePrefix"].(string)
	var groups []LogGroup
	_ = s.db.List(groupsBucket, "", func(_ string, raw []byte) error {
		var g LogGroup
		if err := json.Unmarshal(raw, &g); err == nil && strings.HasPrefix(g.Name, prefix) {
			groups = append(groups, g)
		}
		return nil
	})
	sort.Slice(groups, func(i, j int) bool { return groups[i].Name < groups[j].Name })
	out := make([]map[string]any, 0, len(groups))
	for _, g := range groups {
		out = append(out, map[string]any{
			"logGroupName": g.Name,
			"arn":          g.Arn,
			"creationTime": g.CreationTime,
		})
	}
	server.WriteJSON(w, http.StatusOK, map[string]any{"logGroups": out})
}

// listTagsForResource: este emulador no implementa tags en absoluto para
// log groups (no hay TagResource/UntagResource). Siempre devuelve un mapa
// vacío -- existe para que clientes reales que refrescan el estado
// completo de un log group (p. ej. el provider de Terraform en su Read)
// no fallen con un error desconocido. Encontrado vía
// terraform/aws-smoke-test, ver ROADMAP.md.
func (s *Service) listTagsForResource(w http.ResponseWriter, _ map[string]any) {
	server.WriteJSON(w, http.StatusOK, map[string]any{"tags": map[string]string{}})
}

// --- streams ---

func (s *Service) createLogStream(w http.ResponseWriter, body map[string]any) {
	groupName, _ := body["logGroupName"].(string)
	streamName, _ := body["logStreamName"].(string)
	if groupName == "" || streamName == "" {
		server.WriteJSONError(w, http.StatusBadRequest, "ValidationException",
			"logGroupName y logStreamName son requeridos")
		return
	}
	key := streamKey(groupName, streamName)
	var existing LogStream
	if found, _ := s.db.Get(streamsBucket, key, &existing); found {
		server.WriteJSONError(w, http.StatusBadRequest, "ResourceAlreadyExistsException",
			"el stream de log ya existe: "+streamName)
		return
	}
	st := LogStream{LogGroupName: groupName, Name: streamName, Arn: streamArn(groupName, streamName), CreationTime: nowMillis()}
	if err := s.db.Put(streamsBucket, key, st); err != nil {
		server.WriteJSONError(w, http.StatusInternalServerError, "InternalFailure", err.Error())
		return
	}
	server.WriteJSON(w, http.StatusOK, map[string]any{})
}

func (s *Service) deleteLogStream(w http.ResponseWriter, body map[string]any) {
	groupName, _ := body["logGroupName"].(string)
	streamName, _ := body["logStreamName"].(string)
	key := streamKey(groupName, streamName)
	if err := s.db.Delete(eventsBucket, key); err != nil {
		server.WriteJSONError(w, http.StatusInternalServerError, "InternalFailure", err.Error())
		return
	}
	if err := s.db.Delete(streamsBucket, key); err != nil {
		server.WriteJSONError(w, http.StatusInternalServerError, "InternalFailure", err.Error())
		return
	}
	server.WriteJSON(w, http.StatusOK, map[string]any{})
}

func (s *Service) describeLogStreams(w http.ResponseWriter, body map[string]any) {
	groupName, _ := body["logGroupName"].(string)
	prefix, _ := body["logStreamNamePrefix"].(string)
	var streams []LogStream
	_ = s.db.List(streamsBucket, groupName+"|", func(_ string, raw []byte) error {
		var st LogStream
		if err := json.Unmarshal(raw, &st); err == nil && strings.HasPrefix(st.Name, prefix) {
			streams = append(streams, st)
		}
		return nil
	})
	sort.Slice(streams, func(i, j int) bool { return streams[i].Name < streams[j].Name })
	out := make([]map[string]any, 0, len(streams))
	for _, st := range streams {
		out = append(out, map[string]any{
			"logStreamName":       st.Name,
			"arn":                 st.Arn,
			"creationTime":        st.CreationTime,
			"firstEventTimestamp": st.FirstEventTimestamp,
			"lastEventTimestamp":  st.LastEventTimestamp,
			"lastIngestionTime":   st.LastIngestionTime,
			"uploadSequenceToken": st.UploadSequenceToken,
		})
	}
	server.WriteJSON(w, http.StatusOK, map[string]any{"logStreams": out})
}

// --- eventos ---

func (s *Service) putLogEvents(w http.ResponseWriter, body map[string]any) {
	groupName, _ := body["logGroupName"].(string)
	streamName, _ := body["logStreamName"].(string)
	eventsRaw, _ := body["logEvents"].([]any)
	if groupName == "" || streamName == "" {
		server.WriteJSONError(w, http.StatusBadRequest, "ValidationException",
			"logGroupName y logStreamName son requeridos")
		return
	}
	key := streamKey(groupName, streamName)

	var stream LogStream
	if found, _ := s.db.Get(streamsBucket, key, &stream); !found {
		server.WriteJSONError(w, http.StatusBadRequest, "ResourceNotFoundException",
			"el stream de log no existe: "+streamName)
		return
	}

	var existing []LogEvent
	_, _ = s.db.Get(eventsBucket, key, &existing)

	now := nowMillis()
	for _, e := range eventsRaw {
		em, _ := e.(map[string]any)
		ts, _ := em["timestamp"].(float64)
		msg, _ := em["message"].(string)
		ev := LogEvent{Timestamp: int64(ts), Message: msg, IngestionTime: now}
		existing = append(existing, ev)
		if stream.FirstEventTimestamp == 0 || ev.Timestamp < stream.FirstEventTimestamp {
			stream.FirstEventTimestamp = ev.Timestamp
		}
		if ev.Timestamp > stream.LastEventTimestamp {
			stream.LastEventTimestamp = ev.Timestamp
		}
	}
	stream.LastIngestionTime = now
	stream.UploadSequenceToken = randomToken()

	if err := s.db.Put(eventsBucket, key, existing); err != nil {
		server.WriteJSONError(w, http.StatusInternalServerError, "InternalFailure", err.Error())
		return
	}
	if err := s.db.Put(streamsBucket, key, stream); err != nil {
		server.WriteJSONError(w, http.StatusInternalServerError, "InternalFailure", err.Error())
		return
	}
	server.WriteJSON(w, http.StatusOK, map[string]any{"nextSequenceToken": stream.UploadSequenceToken})
}

func (s *Service) getLogEvents(w http.ResponseWriter, body map[string]any) {
	groupName, _ := body["logGroupName"].(string)
	streamName, _ := body["logStreamName"].(string)
	key := streamKey(groupName, streamName)

	var events []LogEvent
	_, _ = s.db.Get(eventsBucket, key, &events)
	sort.Slice(events, func(i, j int) bool { return events[i].Timestamp < events[j].Timestamp })

	out := make([]map[string]any, 0, len(events))
	for _, e := range events {
		out = append(out, map[string]any{
			"timestamp":     e.Timestamp,
			"message":       e.Message,
			"ingestionTime": e.IngestionTime,
		})
	}
	token := "f/000000000000000000000000000000000000000000000000000000"
	server.WriteJSON(w, http.StatusOK, map[string]any{
		"events":            out,
		"nextForwardToken":  token,
		"nextBackwardToken": token,
	})
}

func (s *Service) filterLogEvents(w http.ResponseWriter, body map[string]any) {
	groupName, _ := body["logGroupName"].(string)
	startTime, hasStart := body["startTime"].(float64)
	endTime, hasEnd := body["endTime"].(float64)
	filterPattern, _ := body["filterPattern"].(string)

	var streamNames []string
	if namesRaw, ok := body["logStreamNames"].([]any); ok && len(namesRaw) > 0 {
		for _, n := range namesRaw {
			if ns, ok := n.(string); ok {
				streamNames = append(streamNames, ns)
			}
		}
	} else {
		_ = s.db.List(streamsBucket, groupName+"|", func(k string, _ []byte) error {
			_, name, _ := strings.Cut(k, "|")
			streamNames = append(streamNames, name)
			return nil
		})
	}

	var out []map[string]any
	searched := make([]map[string]any, 0, len(streamNames))
	for _, streamName := range streamNames {
		key := streamKey(groupName, streamName)
		var events []LogEvent
		_, _ = s.db.Get(eventsBucket, key, &events)
		searched = append(searched, map[string]any{"logStreamName": streamName})
		for _, e := range events {
			if hasStart && float64(e.Timestamp) < startTime {
				continue
			}
			if hasEnd && float64(e.Timestamp) >= endTime {
				continue
			}
			if filterPattern != "" && !strings.Contains(e.Message, filterPattern) {
				continue
			}
			out = append(out, map[string]any{
				"logStreamName": streamName,
				"timestamp":     e.Timestamp,
				"message":       e.Message,
				"ingestionTime": e.IngestionTime,
				"eventId":       randomToken(),
			})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i]["timestamp"].(int64) < out[j]["timestamp"].(int64)
	})

	server.WriteJSON(w, http.StatusOK, map[string]any{
		"events":             out,
		"searchedLogStreams": searched,
	})
}

func randomToken() string {
	buf := make([]byte, 16)
	_, _ = rand.Read(buf)
	return "seq-" + hex.EncodeToString(buf)
}
