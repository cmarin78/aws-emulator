package dynamodb

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/cesarmarin/aws-emulator/internal/accountctx"
	"github.com/cesarmarin/aws-emulator/internal/storage"
	bolt "go.etcd.io/bbolt"
)

// seedRawTable escribe una fila directamente con bbolt, sin pasar por
// storage.Open -- a propósito: storage.Open corre las migraciones
// pendientes apenas abre el archivo, así que si se usara para sembrar los
// datos "viejos", esa primera apertura (con el bucket todavía vacío)
// migraría y subiría la versión de esquema de inmediato, dejando nada
// pendiente para la segunda apertura que el test quiere usar para
// verificar la migración. Yendo directo a bbolt se simula fielmente un
// archivo escrito por un binario que nunca conoció este mecanismo.
func seedRawTable(t *testing.T, path, key string, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	b, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("bolt.Open: %v", err)
	}
	defer b.Close()
	err = b.Update(func(tx *bolt.Tx) error {
		bucket, err := tx.CreateBucketIfNotExists([]byte(tablesBucket))
		if err != nil {
			return err
		}
		return bucket.Put([]byte(key), data)
	})
	if err != nil {
		t.Fatalf("seed Update: %v", err)
	}
}

// oldShapeTable simula el JSON que un binario anterior a la Fase 6
// (multi-tenancy) habría persistido: sin el campo "arn" en absoluto. No se
// reusa el struct Table actual a propósito -- si se usara, Go siempre
// incluiría "arn":"" en el JSON serializado, que para encoding/json es
// indistinguible de "el campo no estaba" pero no prueba que la migración
// también funcione contra datos realmente viejos que nunca tuvieron la
// clave.
type oldShapeTable struct {
	TableName    string `json:"tableName"`
	PartitionKey string `json:"partitionKey"`
	Status       string `json:"status"`
}

func TestBackfillTableArn_MigratesOldData(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")

	old := oldShapeTable{TableName: "orders", PartitionKey: "id", Status: "ACTIVE"}
	seedRawTable(t, path, "orders", old)

	// El primer storage.Open de este archivo corre migrate() ->
	// backfillTableArn sobre el dato "viejo" sembrado arriba.
	db, err := storage.Open(path)
	if err != nil {
		t.Fatalf("Open (post-seed): %v", err)
	}
	defer db.Close()

	var migrated Table
	found, err := db.Get(tablesBucket, "orders", &migrated)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !found {
		t.Fatalf("esperaba encontrar la tabla 'orders' después de migrar")
	}

	want := tableArn(accountctx.DefaultAccountID, "orders")
	if migrated.Arn != want {
		t.Fatalf("Arn = %q, esperaba %q", migrated.Arn, want)
	}
	// El resto de los campos no debería haberse alterado.
	if migrated.TableName != "orders" || migrated.PartitionKey != "id" || migrated.Status != "ACTIVE" {
		t.Fatalf("la migración alteró campos que no debía: %+v", migrated)
	}

	version, err := db.SchemaVersion()
	if err != nil {
		t.Fatalf("SchemaVersion: %v", err)
	}
	if version < 1 {
		t.Fatalf("esperaba versión de esquema >= 1 tras migrar, obtuve %d", version)
	}
}

func TestBackfillTableArn_DoesNotOverwriteExistingArn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")

	custom := Table{TableName: "invoices", Arn: "arn:aws:dynamodb:us-east-1:123456789012:table/invoices", PartitionKey: "id", Status: "ACTIVE"}
	seedRawTable(t, path, "invoices", custom)

	db, err := storage.Open(path)
	if err != nil {
		t.Fatalf("Open (post-seed): %v", err)
	}
	defer db.Close()

	var got Table
	found, err := db.Get(tablesBucket, "invoices", &got)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !found {
		t.Fatalf("esperaba encontrar la tabla 'invoices'")
	}
	if got.Arn != custom.Arn {
		t.Fatalf("Arn = %q, la migración no debería tocar un Arn ya seteado (era %q)", got.Arn, custom.Arn)
	}
}

func TestBackfillTableArn_NoTablesBucket(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")

	// Una DB completamente nueva (sin ninguna tabla creada nunca) no debe
	// fallar al migrar -- el bucket dynamodb.tables ni siquiera existe.
	db, err := storage.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	version, err := db.SchemaVersion()
	if err != nil {
		t.Fatalf("SchemaVersion: %v", err)
	}
	if version < 1 {
		t.Fatalf("esperaba que la migración igual corra (y suba versión) aunque no haya tablas, obtuve %d", version)
	}
}
