package storage

import (
	"path/filepath"
	"testing"

	bolt "go.etcd.io/bbolt"
)

// withRegistry reemplaza el registro global de migraciones por el dado
// durante el test, y lo restaura al terminar -- el registro es package-
// level porque RegisterMigration está pensado para llamarse desde init(),
// no para inyectarse por parámetro, así que los tests necesitan este
// snapshot/restore para no pisarse entre sí ni con migraciones reales que
// algún otro archivo de este mismo paquete llegue a registrar.
func withRegistry(t *testing.T, migrations []Migration) {
	t.Helper()
	original := registry
	registry = append([]Migration(nil), migrations...)
	t.Cleanup(func() { registry = original })
}

func openTestDB(t *testing.T) *DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestSchemaVersion_FreshDBNoMigrations(t *testing.T) {
	withRegistry(t, nil)
	db := openTestDB(t)

	version, err := db.SchemaVersion()
	if err != nil {
		t.Fatalf("SchemaVersion: %v", err)
	}
	if version != 0 {
		t.Fatalf("esperaba versión 0 en una DB nueva sin migraciones registradas, obtuve %d", version)
	}
}

func TestMigrate_AppliesAndBumpsVersion(t *testing.T) {
	applied := 0
	withRegistry(t, []Migration{
		{
			Version:     1,
			Description: "marca un valor de prueba",
			Apply: func(tx *bolt.Tx) error {
				applied++
				b, err := tx.CreateBucketIfNotExists([]byte("test.bucket"))
				if err != nil {
					return err
				}
				return b.Put([]byte("marker"), []byte("v1"))
			},
		},
	})

	db := openTestDB(t)

	version, err := db.SchemaVersion()
	if err != nil {
		t.Fatalf("SchemaVersion: %v", err)
	}
	if version != 1 {
		t.Fatalf("esperaba versión 1 después de Open, obtuve %d", version)
	}
	if applied != 1 {
		t.Fatalf("esperaba que Apply corriera exactamente 1 vez, corrió %d", applied)
	}

	data, found, err := db.GetRaw("test.bucket", "marker")
	if err != nil {
		t.Fatalf("GetRaw: %v", err)
	}
	if !found {
		t.Fatalf("esperaba que la migración hubiera escrito el marker")
	}
	if string(data) != "v1" {
		t.Fatalf("marker = %q, esperaba %q", data, "v1")
	}
}

// TestMigrate_AppliesInVersionOrder verifica que las migraciones se
// aplican ordenadas por Version, no por el orden en que fueron pasadas al
// registro -- esto importa porque Go no garantiza el orden de ejecución
// de init() entre paquetes hermanos, así que el motor no puede asumir que
// el slice ya viene ordenado.
func TestMigrate_AppliesInVersionOrder(t *testing.T) {
	var order []int
	withRegistry(t, []Migration{
		{Version: 3, Description: "tercera", Apply: func(tx *bolt.Tx) error {
			order = append(order, 3)
			return nil
		}},
		{Version: 1, Description: "primera", Apply: func(tx *bolt.Tx) error {
			order = append(order, 1)
			return nil
		}},
		{Version: 2, Description: "segunda", Apply: func(tx *bolt.Tx) error {
			order = append(order, 2)
			return nil
		}},
	})

	db := openTestDB(t)

	version, err := db.SchemaVersion()
	if err != nil {
		t.Fatalf("SchemaVersion: %v", err)
	}
	if version != 3 {
		t.Fatalf("esperaba versión final 3, obtuve %d", version)
	}
	if len(order) != 3 || order[0] != 1 || order[1] != 2 || order[2] != 3 {
		t.Fatalf("orden de aplicación = %v, esperaba [1 2 3]", order)
	}
}

// TestMigrate_NotReappliedOnSecondOpen simula el caso real: cerrar la DB y
// volver a abrirla (como hace un restart del binario) no debe volver a
// correr una migración ya aplicada.
func TestMigrate_NotReappliedOnSecondOpen(t *testing.T) {
	applied := 0
	migrations := []Migration{
		{
			Version:     1,
			Description: "cuenta cuántas veces corre",
			Apply: func(tx *bolt.Tx) error {
				applied++
				return nil
			},
		},
	}
	withRegistry(t, migrations)

	path := filepath.Join(t.TempDir(), "test.db")

	db1, err := Open(path)
	if err != nil {
		t.Fatalf("primer Open: %v", err)
	}
	if err := db1.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if applied != 1 {
		t.Fatalf("esperaba 1 aplicación tras el primer Open, obtuve %d", applied)
	}

	db2, err := Open(path)
	if err != nil {
		t.Fatalf("segundo Open: %v", err)
	}
	t.Cleanup(func() { _ = db2.Close() })

	if applied != 1 {
		t.Fatalf("esperaba que la migración NO se reaplique en un segundo Open, total de aplicaciones = %d", applied)
	}
	version, err := db2.SchemaVersion()
	if err != nil {
		t.Fatalf("SchemaVersion: %v", err)
	}
	if version != 1 {
		t.Fatalf("versión tras el segundo Open = %d, esperaba 1", version)
	}
}

// TestMigrate_OnlyAppliesPendingVersions simula actualizar el binario de a
// pasos: una DB que ya está en versión 1 (porque fue abierta con un
// binario que solo conocía la migración 1) y luego se abre con un binario
// que además conoce la migración 2 -- solo la 2 debe correr.
func TestMigrate_OnlyAppliesPendingVersions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")

	v1Applied := 0
	withRegistry(t, []Migration{
		{Version: 1, Description: "v1", Apply: func(tx *bolt.Tx) error {
			v1Applied++
			return nil
		}},
	})
	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open (binario viejo): %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if v1Applied != 1 {
		t.Fatalf("v1Applied = %d, esperaba 1", v1Applied)
	}

	v1AppliedAgain := 0
	v2Applied := 0
	withRegistry(t, []Migration{
		{Version: 1, Description: "v1", Apply: func(tx *bolt.Tx) error {
			v1AppliedAgain++
			return nil
		}},
		{Version: 2, Description: "v2", Apply: func(tx *bolt.Tx) error {
			v2Applied++
			return nil
		}},
	})
	db2, err := Open(path)
	if err != nil {
		t.Fatalf("Open (binario nuevo): %v", err)
	}
	t.Cleanup(func() { _ = db2.Close() })

	if v1AppliedAgain != 0 {
		t.Fatalf("v1 no debería reaplicarse, corrió %d veces", v1AppliedAgain)
	}
	if v2Applied != 1 {
		t.Fatalf("v2 debería aplicarse exactamente 1 vez, corrió %d", v2Applied)
	}
	version, err := db2.SchemaVersion()
	if err != nil {
		t.Fatalf("SchemaVersion: %v", err)
	}
	if version != 2 {
		t.Fatalf("versión final = %d, esperaba 2", version)
	}
}

func TestLatestSchemaVersion(t *testing.T) {
	withRegistry(t, []Migration{
		{Version: 1, Description: "a", Apply: func(tx *bolt.Tx) error { return nil }},
		{Version: 4, Description: "b", Apply: func(tx *bolt.Tx) error { return nil }},
		{Version: 2, Description: "c", Apply: func(tx *bolt.Tx) error { return nil }},
	})
	if got := LatestSchemaVersion(); got != 4 {
		t.Fatalf("LatestSchemaVersion() = %d, esperaba 4", got)
	}
}

func TestLatestSchemaVersion_Empty(t *testing.T) {
	withRegistry(t, nil)
	if got := LatestSchemaVersion(); got != 0 {
		t.Fatalf("LatestSchemaVersion() sin migraciones = %d, esperaba 0", got)
	}
}
