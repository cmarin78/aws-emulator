// Package dynamodb emula el subconjunto más usado de Amazon DynamoDB:
// CreateTable, PutItem, GetItem, DeleteItem y Scan, sobre el protocolo
// JSON 1.0 real (X-Amz-Target: DynamoDB_20120810.{Action}, body y
// respuesta JSON) — a diferencia de S3/SQS/IAM/STS, que usan el
// protocolo Query/XML clásico.
//
// No implementado en Fase 1: índices secundarios (GSI/LSI), Query con
// condiciones de key complejas (Query acá se comporta como un Scan con
// filtro simple sobre la partition key), transacciones, streams.
package dynamodb

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/cesarmarin/aws-emulator/internal/server"
	"github.com/cesarmarin/aws-emulator/internal/storage"
)

const (
	tablesBucket = "dynamodb.tables"
	itemsBucket  = "dynamodb.items"
)

// AttributeValue replica el shape de AttributeValue de la API real:
// un objeto con una sola clave de tipo ("S", "N", "B", "BOOL", "L", "M",
// "SS", "NS", "BS", "NULL"). Se modela como map[string]any para no tener
// que declarar cada variante — el emulador no valida tipos, solo
// persiste y devuelve lo que el cliente mandó.
type AttributeValue map[string]any

// Item es un mapa de nombre de atributo -> AttributeValue, igual que en
// la API real.
type Item map[string]AttributeValue

// Table es la metadata persistida de una tabla.
type Table struct {
	TableName    string `json:"tableName"`
	PartitionKey string `json:"partitionKey"`
	SortKey      string `json:"sortKey,omitempty"`
	Status       string `json:"status"`
}

// Service agrupa el estado del servicio DynamoDB.
type Service struct {
	db *storage.DB
}

// New crea el servicio DynamoDB.
func New(db *storage.DB) *Service {
	return &Service{db: db}
}

func (s *Service) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	target := r.Header.Get("X-Amz-Target")
	_, action, _ := strings.Cut(target, ".")

	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil && r.ContentLength != 0 {
		server.WriteJSONError(w, http.StatusBadRequest, "com.amazon.coral.validate#ValidationException",
			"no se pudo parsear el body JSON")
		return
	}

	switch action {
	case "CreateTable":
		s.createTable(w, body)
	case "DeleteTable":
		s.deleteTable(w, body)
	case "DescribeTable":
		s.describeTable(w, body)
	case "PutItem":
		s.putItem(w, body)
	case "GetItem":
		s.getItem(w, body)
	case "DeleteItem":
		s.deleteItem(w, body)
	case "Scan":
		s.scan(w, body)
	case "Query":
		s.scan(w, body) // Fase 1: Query se trata como Scan, ver comentario de paquete.
	default:
		server.WriteJSONError(w, http.StatusBadRequest, "com.amazon.coral.service#UnknownOperationException",
			"acción DynamoDB no soportada en este emulador: "+action)
	}
}

func keySchema(body map[string]any) (partitionKey, sortKey string) {
	schema, ok := body["KeySchema"].([]any)
	if !ok {
		return "", ""
	}
	for _, raw := range schema {
		entry, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		name, _ := entry["AttributeName"].(string)
		keyType, _ := entry["KeyType"].(string)
		switch keyType {
		case "HASH":
			partitionKey = name
		case "RANGE":
			sortKey = name
		}
	}
	return partitionKey, sortKey
}

func (s *Service) createTable(w http.ResponseWriter, body map[string]any) {
	name, _ := body["TableName"].(string)
	if name == "" {
		server.WriteJSONError(w, http.StatusBadRequest, "com.amazon.coral.validate#ValidationException", "TableName es requerido")
		return
	}
	pk, sk := keySchema(body)
	t := Table{TableName: name, PartitionKey: pk, SortKey: sk, Status: "ACTIVE"}
	if err := s.db.Put(tablesBucket, name, t); err != nil {
		server.WriteJSONError(w, http.StatusInternalServerError, "InternalServerError", err.Error())
		return
	}
	server.WriteJSON(w, http.StatusOK, map[string]any{
		"TableDescription": tableDescription(t),
	})
}

func tableDescription(t Table) map[string]any {
	keySchema := []map[string]string{{"AttributeName": t.PartitionKey, "KeyType": "HASH"}}
	if t.SortKey != "" {
		keySchema = append(keySchema, map[string]string{"AttributeName": t.SortKey, "KeyType": "RANGE"})
	}
	return map[string]any{
		"TableName":   t.TableName,
		"TableStatus": t.Status,
		"KeySchema":   keySchema,
	}
}

func (s *Service) deleteTable(w http.ResponseWriter, body map[string]any) {
	name, _ := body["TableName"].(string)
	var t Table
	found, err := s.db.Get(tablesBucket, name, &t)
	if err != nil {
		server.WriteJSONError(w, http.StatusInternalServerError, "InternalServerError", err.Error())
		return
	}
	if !found {
		server.WriteJSONError(w, http.StatusBadRequest, "com.amazonaws.dynamodb.v20120810#ResourceNotFoundException",
			"la tabla no existe: "+name)
		return
	}
	if err := s.db.DeletePrefix(itemsBucket, name+"/"); err != nil {
		server.WriteJSONError(w, http.StatusInternalServerError, "InternalServerError", err.Error())
		return
	}
	if err := s.db.Delete(tablesBucket, name); err != nil {
		server.WriteJSONError(w, http.StatusInternalServerError, "InternalServerError", err.Error())
		return
	}
	server.WriteJSON(w, http.StatusOK, map[string]any{"TableDescription": tableDescription(t)})
}

func (s *Service) describeTable(w http.ResponseWriter, body map[string]any) {
	name, _ := body["TableName"].(string)
	var t Table
	found, err := s.db.Get(tablesBucket, name, &t)
	if err != nil {
		server.WriteJSONError(w, http.StatusInternalServerError, "InternalServerError", err.Error())
		return
	}
	if !found {
		server.WriteJSONError(w, http.StatusBadRequest, "com.amazonaws.dynamodb.v20120810#ResourceNotFoundException",
			"la tabla no existe: "+name)
		return
	}
	server.WriteJSON(w, http.StatusOK, map[string]any{"Table": tableDescription(t)})
}

func itemKey(table string, pkValue, skValue string) string {
	if skValue == "" {
		return table + "/" + pkValue
	}
	return table + "/" + pkValue + "/" + skValue
}

// attrScalar extrae el valor escalar (S o N, como string) de un
// AttributeValue, suficiente para construir la clave de persistencia.
func attrScalar(av AttributeValue) string {
	if s, ok := av["S"].(string); ok {
		return s
	}
	if n, ok := av["N"].(string); ok {
		return n
	}
	return ""
}

func (s *Service) tableFor(name string) (Table, bool) {
	var t Table
	found, _ := s.db.Get(tablesBucket, name, &t)
	return t, found
}

func (s *Service) putItem(w http.ResponseWriter, body map[string]any) {
	name, _ := body["TableName"].(string)
	t, found := s.tableFor(name)
	if !found {
		server.WriteJSONError(w, http.StatusBadRequest, "com.amazonaws.dynamodb.v20120810#ResourceNotFoundException",
			"la tabla no existe: "+name)
		return
	}
	itemRaw, _ := body["Item"].(map[string]any)
	item := toItem(itemRaw)

	pk := attrScalar(item[t.PartitionKey])
	sk := ""
	if t.SortKey != "" {
		sk = attrScalar(item[t.SortKey])
	}
	if err := s.db.Put(itemsBucket, itemKey(name, pk, sk), item); err != nil {
		server.WriteJSONError(w, http.StatusInternalServerError, "InternalServerError", err.Error())
		return
	}
	server.WriteJSON(w, http.StatusOK, map[string]any{})
}

func toItem(raw map[string]any) Item {
	item := Item{}
	for k, v := range raw {
		if m, ok := v.(map[string]any); ok {
			item[k] = AttributeValue(m)
		}
	}
	return item
}

func (s *Service) getItem(w http.ResponseWriter, body map[string]any) {
	name, _ := body["TableName"].(string)
	t, found := s.tableFor(name)
	if !found {
		server.WriteJSONError(w, http.StatusBadRequest, "com.amazonaws.dynamodb.v20120810#ResourceNotFoundException",
			"la tabla no existe: "+name)
		return
	}
	keyRaw, _ := body["Key"].(map[string]any)
	key := toItem(keyRaw)
	pk := attrScalar(key[t.PartitionKey])
	sk := ""
	if t.SortKey != "" {
		sk = attrScalar(key[t.SortKey])
	}

	var item Item
	found, err := s.db.Get(itemsBucket, itemKey(name, pk, sk), &item)
	if err != nil {
		server.WriteJSONError(w, http.StatusInternalServerError, "InternalServerError", err.Error())
		return
	}
	if !found {
		server.WriteJSON(w, http.StatusOK, map[string]any{})
		return
	}
	server.WriteJSON(w, http.StatusOK, map[string]any{"Item": item})
}

func (s *Service) deleteItem(w http.ResponseWriter, body map[string]any) {
	name, _ := body["TableName"].(string)
	t, found := s.tableFor(name)
	if !found {
		server.WriteJSONError(w, http.StatusBadRequest, "com.amazonaws.dynamodb.v20120810#ResourceNotFoundException",
			"la tabla no existe: "+name)
		return
	}
	keyRaw, _ := body["Key"].(map[string]any)
	key := toItem(keyRaw)
	pk := attrScalar(key[t.PartitionKey])
	sk := ""
	if t.SortKey != "" {
		sk = attrScalar(key[t.SortKey])
	}
	if err := s.db.Delete(itemsBucket, itemKey(name, pk, sk)); err != nil {
		server.WriteJSONError(w, http.StatusInternalServerError, "InternalServerError", err.Error())
		return
	}
	server.WriteJSON(w, http.StatusOK, map[string]any{})
}

func (s *Service) scan(w http.ResponseWriter, body map[string]any) {
	name, _ := body["TableName"].(string)
	if _, found := s.tableFor(name); !found {
		server.WriteJSONError(w, http.StatusBadRequest, "com.amazonaws.dynamodb.v20120810#ResourceNotFoundException",
			"la tabla no existe: "+name)
		return
	}
	var items []Item
	_ = s.db.List(itemsBucket, name+"/", func(_ string, raw []byte) error {
		var item Item
		if err := json.Unmarshal(raw, &item); err == nil {
			items = append(items, item)
		}
		return nil
	})
	server.WriteJSON(w, http.StatusOK, map[string]any{
		"Items": items,
		"Count": len(items),
		"ScannedCount": len(items),
	})
}
