// Package dynamodb emula el subconjunto más usado de Amazon DynamoDB:
// CreateTable, PutItem, GetItem, DeleteItem, Scan y Query (con
// KeyConditionExpression real), sobre el protocolo JSON 1.0 real
// (X-Amz-Target: DynamoDB_20120810.{Action}, body y respuesta JSON) — a
// diferencia de S3/SQS/IAM/STS, que usan el protocolo Query/XML clásico.
//
// CreateTable acepta GlobalSecondaryIndexes/LocalSecondaryIndexes y los
// persiste en la metadata de la tabla; Query puede filtrar por ellos via
// IndexName. No hay almacenamiento separado por índice — como el
// emulador escanea todos los items de la tabla y filtra en memoria por
// los atributos pk/sk del índice, no hace falta mantener una proyección
// física aparte.
//
// No implementado en Fase 2: transacciones, streams, proyecciones
// parciales de GSI/LSI (siempre se proyecta ALL).
package dynamodb

import (
	"encoding/json"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/cesarmarin/aws-emulator/internal/accountctx"
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

// SecondaryIndex es la forma persistida de un GSI o LSI: solo lo
// necesario para que Query pueda resolver pk/sk al filtrar por IndexName.
type SecondaryIndex struct {
	IndexName    string `json:"indexName"`
	PartitionKey string `json:"partitionKey"`
	SortKey      string `json:"sortKey,omitempty"`
}

// Table es la metadata persistida de una tabla.
type Table struct {
	TableName    string           `json:"tableName"`
	Arn          string           `json:"arn"`
	PartitionKey string           `json:"partitionKey"`
	SortKey      string           `json:"sortKey,omitempty"`
	Status       string           `json:"status"`
	GSIs         []SecondaryIndex `json:"gsis,omitempty"`
	LSIs         []SecondaryIndex `json:"lsis,omitempty"`
}

// tableArn construye el ARN de una tabla a partir del account ID resuelto
// por credencial (ver internal/accountctx). Se computa una sola vez en
// createTable y se persiste en Table.Arn -- antes de esta fase,
// tableDescription recalculaba un ARN con un account ID hardcodeado en
// cada llamada (create/delete/describe); ahora sigue el mismo patrón que
// el resto de los servicios (roleArn, queueArn, topicArn, etc.): se calcula
// una vez al crear el recurso.
func tableArn(accountID, name string) string {
	return "arn:aws:dynamodb:us-east-1:" + accountID + ":table/" + name
}

func findIndex(t Table, name string) (SecondaryIndex, bool) {
	for _, idx := range t.GSIs {
		if idx.IndexName == name {
			return idx, true
		}
	}
	for _, idx := range t.LSIs {
		if idx.IndexName == name {
			return idx, true
		}
	}
	return SecondaryIndex{}, false
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

	accountID, _ := accountctx.FromContext(r.Context())

	switch action {
	case "CreateTable":
		s.createTable(w, body, accountID)
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
		s.query(w, body)
	case "DescribeTimeToLive":
		s.describeTimeToLive(w, body)
	case "ListTagsOfResource":
		s.listTagsOfResource(w, body)
	default:
		server.WriteJSONError(w, http.StatusBadRequest, "com.amazon.coral.service#UnknownOperationException",
			"acción DynamoDB no soportada en este emulador: "+action)
	}
}

// Reset limpia todo el estado persistido de DynamoDB (tablas e items).
// Implementa server.Resettable.
func (s *Service) Reset() error {
	return s.db.Reset(tablesBucket, itemsBucket)
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

// secondaryIndexes parsea GlobalSecondaryIndexes/LocalSecondaryIndexes del
// body de CreateTable: cada entrada tiene IndexName y su propio KeySchema,
// con la misma forma HASH/RANGE que la tabla.
func secondaryIndexes(raw any) []SecondaryIndex {
	list, ok := raw.([]any)
	if !ok {
		return nil
	}
	var out []SecondaryIndex
	for _, item := range list {
		entry, ok := item.(map[string]any)
		if !ok {
			continue
		}
		name, _ := entry["IndexName"].(string)
		pk, sk := keySchema(entry)
		out = append(out, SecondaryIndex{IndexName: name, PartitionKey: pk, SortKey: sk})
	}
	return out
}

func (s *Service) createTable(w http.ResponseWriter, body map[string]any, accountID string) {
	name, _ := body["TableName"].(string)
	if name == "" {
		server.WriteJSONError(w, http.StatusBadRequest, "com.amazon.coral.validate#ValidationException", "TableName es requerido")
		return
	}
	pk, sk := keySchema(body)
	t := Table{
		TableName:    name,
		Arn:          tableArn(accountID, name),
		PartitionKey: pk,
		SortKey:      sk,
		Status:       "ACTIVE",
		GSIs:         secondaryIndexes(body["GlobalSecondaryIndexes"]),
		LSIs:         secondaryIndexes(body["LocalSecondaryIndexes"]),
	}
	if err := s.db.Put(tablesBucket, name, t); err != nil {
		server.WriteJSONError(w, http.StatusInternalServerError, "InternalServerError", err.Error())
		return
	}
	server.WriteJSON(w, http.StatusOK, map[string]any{
		"TableDescription": tableDescription(t),
	})
}

func indexKeySchema(idx SecondaryIndex) []map[string]string {
	ks := []map[string]string{{"AttributeName": idx.PartitionKey, "KeyType": "HASH"}}
	if idx.SortKey != "" {
		ks = append(ks, map[string]string{"AttributeName": idx.SortKey, "KeyType": "RANGE"})
	}
	return ks
}

func tableDescription(t Table) map[string]any {
	keySchema := []map[string]string{{"AttributeName": t.PartitionKey, "KeyType": "HASH"}}
	if t.SortKey != "" {
		keySchema = append(keySchema, map[string]string{"AttributeName": t.SortKey, "KeyType": "RANGE"})
	}
	desc := map[string]any{
		"TableName":   t.TableName,
		"TableStatus": t.Status,
		"KeySchema":   keySchema,
		// TableArn: el provider de Terraform lee este campo para popular el
		// atributo "arn" de aws_dynamodb_table; sin él, terraform apply no
		// falla (es opcional en el wire) pero el output queda en "" --
		// encontrado vía terraform/aws-smoke-test, ver ROADMAP.md. Se computa
		// una vez en createTable (ver tableArn) y se persiste en t.Arn, en
		// vez de recalcularse acá con un account ID hardcodeado como antes
		// de internal/accountctx.
		"TableArn": t.Arn,
	}
	if len(t.GSIs) > 0 {
		var gsis []map[string]any
		for _, idx := range t.GSIs {
			gsis = append(gsis, map[string]any{"IndexName": idx.IndexName, "KeySchema": indexKeySchema(idx)})
		}
		desc["GlobalSecondaryIndexes"] = gsis
	}
	if len(t.LSIs) > 0 {
		var lsis []map[string]any
		for _, idx := range t.LSIs {
			lsis = append(lsis, map[string]any{"IndexName": idx.IndexName, "KeySchema": indexKeySchema(idx)})
		}
		desc["LocalSecondaryIndexes"] = lsis
	}
	return desc
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

// describeTimeToLive: este emulador no implementa TTL en absoluto (sin
// UpdateTimeToLive ni expiración real de items), así que siempre devuelve
// DISABLED. Existe únicamente para que clientes reales que refrescan el
// estado completo de una tabla (p. ej. el provider de Terraform en su
// Read, no solo en su Create) no fallen con UnknownOperationException —
// encontrado vía terraform/aws-smoke-test, ver ROADMAP.md.
// listTagsOfResource: este emulador no implementa tags de DynamoDB (no hay
// TagResource/UntagResource) -- el provider de Terraform llama a esto
// durante el Read de aws_dynamodb_table para refrescar tags_all, así que
// con validar que la tabla exista y devolver una lista vacía alcanza para
// no romper el apply, mismo patrón que sns/sqs/events.listTagsForResource.
// Encontrado vía terraform/aws-smoke-test, ver ROADMAP.md.
func (s *Service) listTagsOfResource(w http.ResponseWriter, body map[string]any) {
	arn, _ := body["ResourceArn"].(string)
	name := arn
	if i := strings.LastIndex(arn, "/"); i != -1 {
		name = arn[i+1:]
	}
	var t Table
	found, err := s.db.Get(tablesBucket, name, &t)
	if err != nil {
		server.WriteJSONError(w, http.StatusInternalServerError, "InternalServerError", err.Error())
		return
	}
	if !found {
		server.WriteJSONError(w, http.StatusBadRequest, "com.amazonaws.dynamodb.v20120810#ResourceNotFoundException",
			"la tabla no existe: "+arn)
		return
	}
	server.WriteJSON(w, http.StatusOK, map[string]any{"Tags": []map[string]string{}})
}

func (s *Service) describeTimeToLive(w http.ResponseWriter, body map[string]any) {
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
	server.WriteJSON(w, http.StatusOK, map[string]any{
		"TimeToLiveDescription": map[string]any{"TimeToLiveStatus": "DISABLED"},
	})
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

// --- Query con KeyConditionExpression real ---

var (
	reEqual      = regexp.MustCompile(`^\s*([#\w]+)\s*=\s*(:\w+)\s*$`)
	reCompare    = regexp.MustCompile(`^\s*([#\w]+)\s*(<=|>=|<|>)\s*(:\w+)\s*$`)
	reBetween    = regexp.MustCompile(`(?i)^\s*([#\w]+)\s+BETWEEN\s+(:\w+)\s+AND\s+(:\w+)\s*$`)
	reBeginsWith = regexp.MustCompile(`(?i)^\s*begins_with\(\s*([#\w]+)\s*,\s*(:\w+)\s*\)\s*$`)
)

// keyCondition es el resultado de parsear un KeyConditionExpression: la
// condición de igualdad obligatoria sobre la partition key, más una
// condición opcional sobre la sort key.
type keyCondition struct {
	pkName  string
	pkValue AttributeValue

	hasSK  bool
	skName string
	skOp   string // "=", "<", "<=", ">", ">=", "BETWEEN", "begins_with"
	skLow  AttributeValue
	skHigh AttributeValue // solo usado con BETWEEN
}

func resolveAttrName(raw string, names map[string]string) string {
	if strings.HasPrefix(raw, "#") {
		if n, ok := names[raw]; ok {
			return n
		}
	}
	return raw
}

// parseKeyCondition intenta extraer la condición de partition key (siempre
// requerida, siempre "=") y, si está presente, la condición de sort key
// (=, <, <=, >, >=, BETWEEN ... AND ..., begins_with(...)) de un
// KeyConditionExpression. Devuelve ok=false si la expresión no matchea
// ninguno de los patrones soportados, en cuyo caso el caller debería
// degradar a un comportamiento tipo Scan en vez de fallar la request.
func parseKeyCondition(expr string, values map[string]AttributeValue, names map[string]string) (keyCondition, bool) {
	var kc keyCondition
	parts := splitAnd(expr)
	if len(parts) == 0 || len(parts) > 2 {
		return kc, false
	}

	pkMatch := reEqual.FindStringSubmatch(parts[0])
	if pkMatch == nil {
		return kc, false
	}
	kc.pkName = resolveAttrName(pkMatch[1], names)
	pkVal, ok := values[pkMatch[2]]
	if !ok {
		return kc, false
	}
	kc.pkValue = pkVal

	if len(parts) == 1 {
		return kc, true
	}

	skExpr := parts[1]
	switch {
	case reBetween.MatchString(skExpr):
		m := reBetween.FindStringSubmatch(skExpr)
		low, okLow := values[m[2]]
		high, okHigh := values[m[3]]
		if !okLow || !okHigh {
			return kc, false
		}
		kc.hasSK, kc.skName, kc.skOp, kc.skLow, kc.skHigh = true, resolveAttrName(m[1], names), "BETWEEN", low, high
	case reBeginsWith.MatchString(skExpr):
		m := reBeginsWith.FindStringSubmatch(skExpr)
		val, ok := values[m[2]]
		if !ok {
			return kc, false
		}
		kc.hasSK, kc.skName, kc.skOp, kc.skLow = true, resolveAttrName(m[1], names), "begins_with", val
	case reCompare.MatchString(skExpr):
		m := reCompare.FindStringSubmatch(skExpr)
		val, ok := values[m[3]]
		if !ok {
			return kc, false
		}
		kc.hasSK, kc.skName, kc.skOp, kc.skLow = true, resolveAttrName(m[1], names), m[2], val
	case reEqual.MatchString(skExpr):
		m := reEqual.FindStringSubmatch(skExpr)
		val, ok := values[m[2]]
		if !ok {
			return kc, false
		}
		kc.hasSK, kc.skName, kc.skOp, kc.skLow = true, resolveAttrName(m[1], names), "=", val
	default:
		return kc, false
	}
	return kc, true
}

// splitAnd separa un KeyConditionExpression en sus hasta dos cláusulas
// (partition key y, opcionalmente, sort key), cortando solo en el primer
// "AND" de nivel superior (fuera de los paréntesis de begins_with(...)).
//
// Importante: se corta una sola vez, no en cada ocurrencia de "AND". La
// cláusula de sort key puede ser "sk BETWEEN :lo AND :hi", que tiene su
// propio "AND" interno -- una versión anterior de esta función cortaba en
// todas las ocurrencias, así que "pk = :pk AND sk BETWEEN :lo AND :hi"
// quedaba partido en 3 trozos en vez de 2; parseKeyCondition descartaba
// la condición entera por no matchear ningún patrón soportado y
// degradaba silenciosamente a un Scan sin filtrar, rompiendo cualquier
// Query que combinara una condición de partition key con un BETWEEN en
// la sort key. Detectado al escribir tests de Query en Fase 7.
func splitAnd(expr string) []string {
	depth := 0
	upper := strings.ToUpper(expr)
	for i := 0; i+5 <= len(expr); i++ {
		switch expr[i] {
		case '(':
			depth++
		case ')':
			depth--
		}
		if depth == 0 && upper[i:i+5] == " AND " {
			return []string{strings.TrimSpace(expr[:i]), strings.TrimSpace(expr[i+5:])}
		}
	}
	return []string{strings.TrimSpace(expr)}
}

// attrNumeric intenta interpretar un AttributeValue numérico ("N") como
// float64 para poder comparar; devuelve ok=false si no es numérico.
func attrNumeric(av AttributeValue) (float64, bool) {
	n, ok := av["N"].(string)
	if !ok {
		return 0, false
	}
	f, err := strconv.ParseFloat(n, 64)
	if err != nil {
		return 0, false
	}
	return f, true
}

func compareAttr(item AttributeValue, op string, ref, ref2 AttributeValue) bool {
	switch op {
	case "=":
		return attrScalar(item) == attrScalar(ref)
	case "begins_with":
		return strings.HasPrefix(attrScalar(item), attrScalar(ref))
	case "BETWEEN":
		if iv, ok := attrNumeric(item); ok {
			lo, _ := attrNumeric(ref)
			hi, _ := attrNumeric(ref2)
			return iv >= lo && iv <= hi
		}
		s, lo, hi := attrScalar(item), attrScalar(ref), attrScalar(ref2)
		return s >= lo && s <= hi
	case "<", "<=", ">", ">=":
		if iv, ok := attrNumeric(item); ok {
			rv, _ := attrNumeric(ref)
			return numericCompare(iv, op, rv)
		}
		return stringCompare(attrScalar(item), op, attrScalar(ref))
	}
	return false
}

func numericCompare(a float64, op string, b float64) bool {
	switch op {
	case "<":
		return a < b
	case "<=":
		return a <= b
	case ">":
		return a > b
	case ">=":
		return a >= b
	}
	return false
}

func stringCompare(a string, op string, b string) bool {
	switch op {
	case "<":
		return a < b
	case "<=":
		return a <= b
	case ">":
		return a > b
	case ">=":
		return a >= b
	}
	return false
}

func toAttributeValues(raw map[string]any) map[string]AttributeValue {
	out := map[string]AttributeValue{}
	for k, v := range raw {
		if m, ok := v.(map[string]any); ok {
			out[k] = AttributeValue(m)
		}
	}
	return out
}

func toStringMap(raw map[string]any) map[string]string {
	out := map[string]string{}
	for k, v := range raw {
		if s, ok := v.(string); ok {
			out[k] = s
		}
	}
	return out
}

func (s *Service) query(w http.ResponseWriter, body map[string]any) {
	name, _ := body["TableName"].(string)
	t, found := s.tableFor(name)
	if !found {
		server.WriteJSONError(w, http.StatusBadRequest, "com.amazonaws.dynamodb.v20120810#ResourceNotFoundException",
			"la tabla no existe: "+name)
		return
	}

	pkName, skName := t.PartitionKey, t.SortKey
	if idxName, _ := body["IndexName"].(string); idxName != "" {
		idx, ok := findIndex(t, idxName)
		if !ok {
			server.WriteJSONError(w, http.StatusBadRequest, "com.amazonaws.dynamodb.v20120810#ResourceNotFoundException",
				"el índice no existe: "+idxName)
			return
		}
		pkName, skName = idx.PartitionKey, idx.SortKey
	}

	expr, _ := body["KeyConditionExpression"].(string)
	valuesRaw, _ := body["ExpressionAttributeValues"].(map[string]any)
	namesRaw, _ := body["ExpressionAttributeNames"].(map[string]any)
	values := toAttributeValues(valuesRaw)
	names := toStringMap(namesRaw)

	kc, ok := parseKeyCondition(expr, values, names)

	var items []Item
	_ = s.db.List(itemsBucket, name+"/", func(_ string, raw []byte) error {
		var item Item
		if err := json.Unmarshal(raw, &item); err != nil {
			return nil
		}
		if ok {
			if pkName != kc.pkName || attrScalar(item[pkName]) != attrScalar(kc.pkValue) {
				return nil
			}
			if kc.hasSK && skName != "" {
				if !compareAttr(item[skName], kc.skOp, kc.skLow, kc.skHigh) {
					return nil
				}
			}
		}
		items = append(items, item)
		return nil
	})

	server.WriteJSON(w, http.StatusOK, map[string]any{
		"Items":        items,
		"Count":        len(items),
		"ScannedCount": len(items),
	})
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
		"Items":        items,
		"Count":        len(items),
		"ScannedCount": len(items),
	})
}
