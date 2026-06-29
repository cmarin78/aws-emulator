package dynamodb

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

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

// jsonRequest construye una request del protocolo JSON 1.0 real de
// DynamoDB: X-Amz-Target: DynamoDB_20120810.{Action} + body JSON.
func jsonRequest(action string, body map[string]any) *http.Request {
	raw, _ := json.Marshal(body)
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(string(raw)))
	r.Header.Set("X-Amz-Target", "DynamoDB_20120810."+action)
	r.Header.Set("Content-Type", "application/x-amz-json-1.0")
	return r
}

func doDynamo(svc *Service, action string, body map[string]any) (*httptest.ResponseRecorder, map[string]any) {
	w := httptest.NewRecorder()
	svc.ServeHTTP(w, jsonRequest(action, body))
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w, out
}

func strAttr(v string) map[string]any { return map[string]any{"S": v} }
func numAttr(v string) map[string]any { return map[string]any{"N": v} }

func createSimpleTable(t *testing.T, svc *Service, name string) {
	t.Helper()
	w, _ := doDynamo(svc, "CreateTable", map[string]any{
		"TableName": name,
		"KeySchema": []any{
			map[string]any{"AttributeName": "pk", "KeyType": "HASH"},
			map[string]any{"AttributeName": "sk", "KeyType": "RANGE"},
		},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("CreateTable: status = %d, body = %s", w.Code, w.Body.String())
	}
}

func TestCreateTable_WithArnAndKeySchema(t *testing.T) {
	svc := newTestService(t)
	w, out := doDynamo(svc, "CreateTable", map[string]any{
		"TableName": "t1",
		"KeySchema": []any{
			map[string]any{"AttributeName": "pk", "KeyType": "HASH"},
		},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("CreateTable: status = %d, body = %s", w.Code, w.Body.String())
	}
	desc, _ := out["TableDescription"].(map[string]any)
	if desc["TableName"] != "t1" {
		t.Fatalf("TableDescription.TableName = %v", desc["TableName"])
	}
	arn, _ := desc["TableArn"].(string)
	if !strings.Contains(arn, "table/t1") {
		t.Fatalf("TableArn = %q, esperaba que contenga table/t1", arn)
	}
}

func TestCreateTable_RequiresTableName(t *testing.T) {
	svc := newTestService(t)
	w, _ := doDynamo(svc, "CreateTable", map[string]any{})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("CreateTable sin TableName: status = %d, esperaba 400", w.Code)
	}
}

func TestDescribeTable_NotFound(t *testing.T) {
	svc := newTestService(t)
	w, _ := doDynamo(svc, "DescribeTable", map[string]any{"TableName": "nope"})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("DescribeTable inexistente: status = %d, esperaba 400 (ResourceNotFoundException)", w.Code)
	}
}

func TestPutGetDeleteItem_Lifecycle(t *testing.T) {
	svc := newTestService(t)
	createSimpleTable(t, svc, "t1")

	w, _ := doDynamo(svc, "PutItem", map[string]any{
		"TableName": "t1",
		"Item": map[string]any{
			"pk":   strAttr("user1"),
			"sk":   strAttr("profile"),
			"name": strAttr("alice"),
		},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("PutItem: status = %d, body = %s", w.Code, w.Body.String())
	}

	w, out := doDynamo(svc, "GetItem", map[string]any{
		"TableName": "t1",
		"Key": map[string]any{
			"pk": strAttr("user1"),
			"sk": strAttr("profile"),
		},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("GetItem: status = %d, body = %s", w.Code, w.Body.String())
	}
	item, ok := out["Item"].(map[string]any)
	if !ok {
		t.Fatalf("GetItem: esperaba Item en la respuesta, body = %s", w.Body.String())
	}
	name, _ := item["name"].(map[string]any)
	if name["S"] != "alice" {
		t.Fatalf("GetItem: name.S = %v, esperaba alice", name["S"])
	}

	w, _ = doDynamo(svc, "DeleteItem", map[string]any{
		"TableName": "t1",
		"Key": map[string]any{
			"pk": strAttr("user1"),
			"sk": strAttr("profile"),
		},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("DeleteItem: status = %d", w.Code)
	}

	w, out = doDynamo(svc, "GetItem", map[string]any{
		"TableName": "t1",
		"Key": map[string]any{
			"pk": strAttr("user1"),
			"sk": strAttr("profile"),
		},
	})
	if _, found := out["Item"]; found {
		t.Fatalf("GetItem tras DeleteItem: esperaba sin Item, body = %s", w.Body.String())
	}
}

func TestPutItem_TableMustExist(t *testing.T) {
	svc := newTestService(t)
	w, _ := doDynamo(svc, "PutItem", map[string]any{
		"TableName": "nope",
		"Item":      map[string]any{"pk": strAttr("x")},
	})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("PutItem sobre tabla inexistente: status = %d, esperaba 400", w.Code)
	}
}

func TestScan_ReturnsAllItemsInTable(t *testing.T) {
	svc := newTestService(t)
	createSimpleTable(t, svc, "t1")
	doDynamo(svc, "PutItem", map[string]any{
		"TableName": "t1",
		"Item":      map[string]any{"pk": strAttr("a"), "sk": strAttr("1")},
	})
	doDynamo(svc, "PutItem", map[string]any{
		"TableName": "t1",
		"Item":      map[string]any{"pk": strAttr("b"), "sk": strAttr("1")},
	})

	w, out := doDynamo(svc, "Scan", map[string]any{"TableName": "t1"})
	if w.Code != http.StatusOK {
		t.Fatalf("Scan: status = %d, body = %s", w.Code, w.Body.String())
	}
	if count, _ := out["Count"].(float64); count != 2 {
		t.Fatalf("Scan Count = %v, esperaba 2", out["Count"])
	}
}

func TestQuery_EqualityOnPartitionKeyOnly(t *testing.T) {
	svc := newTestService(t)
	createSimpleTable(t, svc, "t1")
	doDynamo(svc, "PutItem", map[string]any{
		"TableName": "t1",
		"Item":      map[string]any{"pk": strAttr("user1"), "sk": strAttr("a")},
	})
	doDynamo(svc, "PutItem", map[string]any{
		"TableName": "t1",
		"Item":      map[string]any{"pk": strAttr("user1"), "sk": strAttr("b")},
	})
	doDynamo(svc, "PutItem", map[string]any{
		"TableName": "t1",
		"Item":      map[string]any{"pk": strAttr("user2"), "sk": strAttr("c")},
	})

	w, out := doDynamo(svc, "Query", map[string]any{
		"TableName":              "t1",
		"KeyConditionExpression": "pk = :pk",
		"ExpressionAttributeValues": map[string]any{
			":pk": strAttr("user1"),
		},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("Query: status = %d, body = %s", w.Code, w.Body.String())
	}
	if count, _ := out["Count"].(float64); count != 2 {
		t.Fatalf("Query Count = %v, esperaba 2 items para user1, body = %s", out["Count"], w.Body.String())
	}
}

func TestQuery_SortKeyComparisonAndBeginsWith(t *testing.T) {
	svc := newTestService(t)
	createSimpleTable(t, svc, "t1")
	for _, sk := range []string{"2020", "2021", "2022", "other"} {
		doDynamo(svc, "PutItem", map[string]any{
			"TableName": "t1",
			"Item":      map[string]any{"pk": strAttr("u"), "sk": strAttr(sk)},
		})
	}

	w, out := doDynamo(svc, "Query", map[string]any{
		"TableName":              "t1",
		"KeyConditionExpression": "pk = :pk AND begins_with(sk, :prefix)",
		"ExpressionAttributeValues": map[string]any{
			":pk":     strAttr("u"),
			":prefix": strAttr("20"),
		},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("Query begins_with: status = %d, body = %s", w.Code, w.Body.String())
	}
	if count, _ := out["Count"].(float64); count != 3 {
		t.Fatalf("Query begins_with Count = %v, esperaba 3, body = %s", out["Count"], w.Body.String())
	}

	w, out = doDynamo(svc, "Query", map[string]any{
		"TableName":              "t1",
		"KeyConditionExpression": "pk = :pk AND sk > :ref",
		"ExpressionAttributeValues": map[string]any{
			":pk":  strAttr("u"),
			":ref": strAttr("2020"),
		},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("Query sk > ref: status = %d, body = %s", w.Code, w.Body.String())
	}
	if count, _ := out["Count"].(float64); count != 3 {
		t.Fatalf("Query sk > ref Count = %v, esperaba 3 (2021,2022,other), body = %s", out["Count"], w.Body.String())
	}
}

func TestQuery_BetweenOnNumericSortKey(t *testing.T) {
	svc := newTestService(t)
	createSimpleTable(t, svc, "t1")
	for _, n := range []string{"1", "5", "10", "20"} {
		doDynamo(svc, "PutItem", map[string]any{
			"TableName": "t1",
			"Item":      map[string]any{"pk": strAttr("u"), "sk": numAttr(n)},
		})
	}

	w, out := doDynamo(svc, "Query", map[string]any{
		"TableName":              "t1",
		"KeyConditionExpression": "pk = :pkv AND sk BETWEEN :lo AND :hi",
		"ExpressionAttributeValues": map[string]any{
			":pkv": strAttr("u"),
			":lo":  numAttr("1"),
			":hi":  numAttr("10"),
		},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("Query BETWEEN: status = %d, body = %s", w.Code, w.Body.String())
	}
	if count, _ := out["Count"].(float64); count != 3 {
		t.Fatalf("Query BETWEEN Count = %v, esperaba 3 (1,5,10), body = %s", out["Count"], w.Body.String())
	}
}

func TestQuery_WithExpressionAttributeNames(t *testing.T) {
	svc := newTestService(t)
	createSimpleTable(t, svc, "t1")
	doDynamo(svc, "PutItem", map[string]any{
		"TableName": "t1",
		"Item":      map[string]any{"pk": strAttr("u"), "sk": strAttr("a")},
	})

	w, out := doDynamo(svc, "Query", map[string]any{
		"TableName":              "t1",
		"KeyConditionExpression": "#p = :pk",
		"ExpressionAttributeNames": map[string]any{
			"#p": "pk",
		},
		"ExpressionAttributeValues": map[string]any{
			":pk": strAttr("u"),
		},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("Query con #p alias: status = %d, body = %s", w.Code, w.Body.String())
	}
	if count, _ := out["Count"].(float64); count != 1 {
		t.Fatalf("Query con alias Count = %v, esperaba 1, body = %s", out["Count"], w.Body.String())
	}
}

// TestQuery_OnGlobalSecondaryIndex cubre el caso central de Fase 2 para
// DynamoDB: crear una tabla con un GSI y consultarla por IndexName,
// resolviendo pk/sk del índice (no de la tabla base) -- ver findIndex.
func TestQuery_OnGlobalSecondaryIndex(t *testing.T) {
	svc := newTestService(t)
	w, _ := doDynamo(svc, "CreateTable", map[string]any{
		"TableName": "t1",
		"KeySchema": []any{
			map[string]any{"AttributeName": "pk", "KeyType": "HASH"},
			map[string]any{"AttributeName": "sk", "KeyType": "RANGE"},
		},
		"GlobalSecondaryIndexes": []any{
			map[string]any{
				"IndexName": "gsi1",
				"KeySchema": []any{
					map[string]any{"AttributeName": "gsiPk", "KeyType": "HASH"},
				},
			},
		},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("CreateTable con GSI: status = %d, body = %s", w.Code, w.Body.String())
	}

	doDynamo(svc, "PutItem", map[string]any{
		"TableName": "t1",
		"Item": map[string]any{
			"pk":    strAttr("p1"),
			"sk":    strAttr("s1"),
			"gsiPk": strAttr("shared"),
		},
	})
	doDynamo(svc, "PutItem", map[string]any{
		"TableName": "t1",
		"Item": map[string]any{
			"pk":    strAttr("p2"),
			"sk":    strAttr("s2"),
			"gsiPk": strAttr("shared"),
		},
	})
	doDynamo(svc, "PutItem", map[string]any{
		"TableName": "t1",
		"Item": map[string]any{
			"pk":    strAttr("p3"),
			"sk":    strAttr("s3"),
			"gsiPk": strAttr("other"),
		},
	})

	w, out := doDynamo(svc, "Query", map[string]any{
		"TableName":              "t1",
		"IndexName":              "gsi1",
		"KeyConditionExpression": "gsiPk = :v",
		"ExpressionAttributeValues": map[string]any{
			":v": strAttr("shared"),
		},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("Query sobre GSI: status = %d, body = %s", w.Code, w.Body.String())
	}
	if count, _ := out["Count"].(float64); count != 2 {
		t.Fatalf("Query sobre GSI Count = %v, esperaba 2 items con gsiPk=shared, body = %s", out["Count"], w.Body.String())
	}
}

func TestQuery_UnknownIndexFails(t *testing.T) {
	svc := newTestService(t)
	createSimpleTable(t, svc, "t1")
	w, _ := doDynamo(svc, "Query", map[string]any{
		"TableName":              "t1",
		"IndexName":              "nope",
		"KeyConditionExpression": "pk = :pk",
		"ExpressionAttributeValues": map[string]any{
			":pk": strAttr("x"),
		},
	})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("Query con IndexName inexistente: status = %d, esperaba 400", w.Code)
	}
}

func TestDeleteTable_AlsoRemovesItems(t *testing.T) {
	svc := newTestService(t)
	createSimpleTable(t, svc, "t1")
	doDynamo(svc, "PutItem", map[string]any{
		"TableName": "t1",
		"Item":      map[string]any{"pk": strAttr("a"), "sk": strAttr("1")},
	})

	w, _ := doDynamo(svc, "DeleteTable", map[string]any{"TableName": "t1"})
	if w.Code != http.StatusOK {
		t.Fatalf("DeleteTable: status = %d, body = %s", w.Code, w.Body.String())
	}

	w, _ = doDynamo(svc, "DescribeTable", map[string]any{"TableName": "t1"})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("DescribeTable tras DeleteTable: status = %d, esperaba 400", w.Code)
	}
}

func TestDescribeTimeToLive_AlwaysDisabled(t *testing.T) {
	svc := newTestService(t)
	createSimpleTable(t, svc, "t1")
	w, out := doDynamo(svc, "DescribeTimeToLive", map[string]any{"TableName": "t1"})
	if w.Code != http.StatusOK {
		t.Fatalf("DescribeTimeToLive: status = %d, body = %s", w.Code, w.Body.String())
	}
	ttl, _ := out["TimeToLiveDescription"].(map[string]any)
	if ttl["TimeToLiveStatus"] != "DISABLED" {
		t.Fatalf("TimeToLiveStatus = %v, esperaba DISABLED", ttl["TimeToLiveStatus"])
	}
}

func TestListTagsOfResource_EmptyButTableMustExist(t *testing.T) {
	svc := newTestService(t)
	createSimpleTable(t, svc, "t1")

	w, out := doDynamo(svc, "ListTagsOfResource", map[string]any{
		"ResourceArn": "arn:aws:dynamodb:us-east-1:000000000000:table/t1",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("ListTagsOfResource: status = %d, body = %s", w.Code, w.Body.String())
	}
	tags, _ := out["Tags"].([]any)
	if len(tags) != 0 {
		t.Fatalf("Tags = %v, esperaba vacío", tags)
	}

	w, _ = doDynamo(svc, "ListTagsOfResource", map[string]any{
		"ResourceArn": "arn:aws:dynamodb:us-east-1:000000000000:table/nope",
	})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("ListTagsOfResource sobre tabla inexistente: status = %d, esperaba 400", w.Code)
	}
}

func TestServeHTTP_UnknownActionFails(t *testing.T) {
	svc := newTestService(t)
	w, _ := doDynamo(svc, "TransactWriteItems", map[string]any{})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("acción no soportada: status = %d, esperaba 400", w.Code)
	}
}

func TestReset_ClearsTablesAndItems(t *testing.T) {
	svc := newTestService(t)
	createSimpleTable(t, svc, "t1")
	doDynamo(svc, "PutItem", map[string]any{
		"TableName": "t1",
		"Item":      map[string]any{"pk": strAttr("a"), "sk": strAttr("1")},
	})

	if err := svc.Reset(); err != nil {
		t.Fatalf("Reset: %v", err)
	}

	w, _ := doDynamo(svc, "DescribeTable", map[string]any{"TableName": "t1"})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("DescribeTable tras Reset: status = %d, esperaba 400 (tabla ya no existe)", w.Code)
	}
}
