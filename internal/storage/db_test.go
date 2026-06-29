package storage

import (
	"errors"
	"path/filepath"
	"testing"
)

type widget struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

func TestPutGet_RoundTrip(t *testing.T) {
	withRegistry(t, nil)
	db := openTestDB(t)

	in := widget{Name: "bolt", Count: 3}
	if err := db.Put("widgets", "w1", in); err != nil {
		t.Fatalf("Put: %v", err)
	}

	var out widget
	found, err := db.Get("widgets", "w1", &out)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !found {
		t.Fatalf("esperaba found=true")
	}
	if out != in {
		t.Fatalf("Get devolvió %+v, esperaba %+v", out, in)
	}
}

func TestGet_NotFound(t *testing.T) {
	withRegistry(t, nil)
	db := openTestDB(t)

	var out widget
	found, err := db.Get("widgets", "missing", &out)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if found {
		t.Fatalf("esperaba found=false para una key/bucket que no existen")
	}
}

func TestPutRawGetRaw_RoundTrip(t *testing.T) {
	withRegistry(t, nil)
	db := openTestDB(t)

	data := []byte{0x00, 0x01, 0xFF, 'h', 'i'}
	if err := db.PutRaw("blobs", "b1", data); err != nil {
		t.Fatalf("PutRaw: %v", err)
	}

	got, found, err := db.GetRaw("blobs", "b1")
	if err != nil {
		t.Fatalf("GetRaw: %v", err)
	}
	if !found {
		t.Fatalf("esperaba found=true")
	}
	if string(got) != string(data) {
		t.Fatalf("GetRaw = %v, esperaba %v", got, data)
	}
}

func TestDelete(t *testing.T) {
	withRegistry(t, nil)
	db := openTestDB(t)

	if err := db.Put("widgets", "w1", widget{Name: "x"}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := db.Delete("widgets", "w1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	var out widget
	found, err := db.Get("widgets", "w1", &out)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if found {
		t.Fatalf("esperaba que la key ya no exista después de Delete")
	}
}

func TestDelete_MissingKeyOrBucketDoesNotError(t *testing.T) {
	withRegistry(t, nil)
	db := openTestDB(t)

	if err := db.Delete("does-not-exist", "k"); err != nil {
		t.Fatalf("Delete sobre bucket inexistente no debería fallar: %v", err)
	}
	if err := db.Put("widgets", "w1", widget{Name: "x"}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := db.Delete("widgets", "missing-key"); err != nil {
		t.Fatalf("Delete sobre key inexistente no debería fallar: %v", err)
	}
}

func TestList_PrefixScan(t *testing.T) {
	withRegistry(t, nil)
	db := openTestDB(t)

	for _, name := range []string{"order/1", "order/2", "user/1"} {
		if err := db.Put("items", name, widget{Name: name}); err != nil {
			t.Fatalf("Put(%q): %v", name, err)
		}
	}

	var seen []string
	err := db.List("items", "order/", func(key string, raw []byte) error {
		seen = append(seen, key)
		return nil
	})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(seen) != 2 {
		t.Fatalf("List devolvió %v, esperaba 2 entradas con prefijo order/", seen)
	}
}

func TestList_EmptyPrefixListsEverything(t *testing.T) {
	withRegistry(t, nil)
	db := openTestDB(t)

	for _, name := range []string{"a", "b", "c"} {
		if err := db.Put("items", name, widget{Name: name}); err != nil {
			t.Fatalf("Put(%q): %v", name, err)
		}
	}

	count := 0
	err := db.List("items", "", func(key string, raw []byte) error {
		count++
		return nil
	})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if count != 3 {
		t.Fatalf("List con prefijo vacío devolvió %d entradas, esperaba 3", count)
	}
}

func TestList_MissingBucketIsNoOp(t *testing.T) {
	withRegistry(t, nil)
	db := openTestDB(t)

	called := false
	err := db.List("does-not-exist", "", func(key string, raw []byte) error {
		called = true
		return nil
	})
	if err != nil {
		t.Fatalf("List sobre bucket inexistente no debería fallar: %v", err)
	}
	if called {
		t.Fatalf("List no debería invocar fn si el bucket no existe")
	}
}

func TestList_PropagatesCallbackError(t *testing.T) {
	withRegistry(t, nil)
	db := openTestDB(t)

	if err := db.Put("items", "a", widget{Name: "a"}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	sentinel := errors.New("boom")
	err := db.List("items", "", func(key string, raw []byte) error {
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("List = %v, esperaba que propague el error del callback", err)
	}
}

func TestDeletePrefix(t *testing.T) {
	withRegistry(t, nil)
	db := openTestDB(t)

	for _, name := range []string{"order/1", "order/2", "user/1"} {
		if err := db.Put("items", name, widget{Name: name}); err != nil {
			t.Fatalf("Put(%q): %v", name, err)
		}
	}

	if err := db.DeletePrefix("items", "order/"); err != nil {
		t.Fatalf("DeletePrefix: %v", err)
	}

	var remaining []string
	err := db.List("items", "", func(key string, raw []byte) error {
		remaining = append(remaining, key)
		return nil
	})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(remaining) != 1 || remaining[0] != "user/1" {
		t.Fatalf("tras DeletePrefix quedaron %v, esperaba solo [user/1]", remaining)
	}
}

func TestReset_ClearsGivenBucketsOnly(t *testing.T) {
	withRegistry(t, nil)
	db := openTestDB(t)

	if err := db.Put("buckets.a", "k1", widget{Name: "a"}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := db.Put("buckets.b", "k1", widget{Name: "b"}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	if err := db.Reset("buckets.a"); err != nil {
		t.Fatalf("Reset: %v", err)
	}

	var out widget
	found, err := db.Get("buckets.a", "k1", &out)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if found {
		t.Fatalf("buckets.a debería haber quedado vacío después de Reset")
	}
	found, err = db.Get("buckets.b", "k1", &out)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !found {
		t.Fatalf("Reset no debería haber tocado buckets.b")
	}
}

func TestEnsureBucket(t *testing.T) {
	withRegistry(t, nil)
	db := openTestDB(t)

	if err := db.EnsureBucket("fresh.bucket"); err != nil {
		t.Fatalf("EnsureBucket: %v", err)
	}
	// Idempotente: llamarlo de nuevo no debe fallar.
	if err := db.EnsureBucket("fresh.bucket"); err != nil {
		t.Fatalf("EnsureBucket (segunda vez): %v", err)
	}

	count := 0
	if err := db.List("fresh.bucket", "", func(key string, raw []byte) error {
		count++
		return nil
	}); err != nil {
		t.Fatalf("List: %v", err)
	}
	if count != 0 {
		t.Fatalf("un bucket recién asegurado debería estar vacío, tiene %d entradas", count)
	}
}

func TestOpen_CreatesParentDirectory(t *testing.T) {
	withRegistry(t, nil)
	path := filepath.Join(t.TempDir(), "nested", "dir", "state.db")

	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open debería crear el directorio padre, falló: %v", err)
	}
	defer db.Close()
}
