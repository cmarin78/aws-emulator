// Package storage provee una capa de persistencia embebida (BoltDB) para
// que el emulador sea completamente portable: un único archivo de datos,
// sin dependencias externas (no requiere Postgres, Docker, etc.).
//
// Es deliberadamente una copia casi literal de internal/storage en
// azure-emulator/gcp-emulator: los tres emuladores comparten el mismo
// modelo de persistencia (buckets + key/value JSON), así que no hay
// motivo para reinventar la capa de storage por proveedor de nube.
package storage

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	bolt "go.etcd.io/bbolt"
)

// DB envuelve una base de datos BoltDB y expone operaciones genéricas
// de tipo key/value con buckets, usadas por todos los servicios emulados
// (S3, DynamoDB, SQS, IAM, etc.).
type DB struct {
	bolt *bolt.DB
}

// Open abre (o crea) el archivo de base de datos en la ruta indicada.
// BoltDB no crea el directorio padre por sí solo, así que se asegura
// aquí (p. ej. ".aws-emulator-data/") antes de abrir el archivo.
func Open(path string) (*DB, error) {
	if dir := filepath.Dir(path); dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("storage: no se pudo crear el directorio %q: %w", dir, err)
		}
	}
	b, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: 5 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("storage: no se pudo abrir la base de datos %q: %w", path, err)
	}
	return &DB{bolt: b}, nil
}

// Close cierra la base de datos.
func (d *DB) Close() error {
	return d.bolt.Close()
}

// EnsureBucket crea el bucket si no existe.
func (d *DB) EnsureBucket(bucket string) error {
	return d.bolt.Update(func(tx *bolt.Tx) error {
		_, err := tx.CreateBucketIfNotExists([]byte(bucket))
		return err
	})
}

// Put serializa value como JSON y lo guarda bajo bucket/key.
func (d *DB) Put(bucket, key string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("storage: error serializando valor: %w", err)
	}
	return d.bolt.Update(func(tx *bolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists([]byte(bucket))
		if err != nil {
			return err
		}
		return b.Put([]byte(key), data)
	})
}

// PutRaw guarda bytes crudos bajo bucket/key, sin pasar por JSON. Usado
// por S3 para el contenido binario de los objetos (igual que ministack
// guarda los blobs de Lambda como bytes sueltos, no como JSON).
func (d *DB) PutRaw(bucket, key string, value []byte) error {
	return d.bolt.Update(func(tx *bolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists([]byte(bucket))
		if err != nil {
			return err
		}
		return b.Put([]byte(key), value)
	})
}

// Get busca bucket/key y deserializa el resultado en out (puntero).
// Devuelve found=false si la clave no existe.
func (d *DB) Get(bucket, key string, out any) (found bool, err error) {
	err = d.bolt.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(bucket))
		if b == nil {
			return nil
		}
		data := b.Get([]byte(key))
		if data == nil {
			return nil
		}
		found = true
		return json.Unmarshal(data, out)
	})
	return found, err
}

// GetRaw busca bucket/key y devuelve los bytes crudos, sin deserializar.
func (d *DB) GetRaw(bucket, key string) (data []byte, found bool, err error) {
	err = d.bolt.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(bucket))
		if b == nil {
			return nil
		}
		raw := b.Get([]byte(key))
		if raw == nil {
			return nil
		}
		found = true
		data = append([]byte(nil), raw...)
		return nil
	})
	return data, found, err
}

// Delete elimina bucket/key. No falla si la clave no existe.
func (d *DB) Delete(bucket, key string) error {
	return d.bolt.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(bucket))
		if b == nil {
			return nil
		}
		return b.Delete([]byte(key))
	})
}

// List recorre todas las entradas de un bucket cuyo key tenga el prefijo
// dado (usar "" para listar todo) y llama a fn(key, rawJSON) por cada una.
// Si fn devuelve un error, List se detiene y propaga ese error.
func (d *DB) List(bucket, prefix string, fn func(key string, raw []byte) error) error {
	return d.bolt.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(bucket))
		if b == nil {
			return nil
		}
		c := b.Cursor()
		p := []byte(prefix)
		for k, v := c.Seek(p); k != nil && hasPrefix(k, p); k, v = c.Next() {
			if err := fn(string(k), v); err != nil {
				return err
			}
		}
		return nil
	})
}

// DeletePrefix elimina todas las entradas de un bucket cuyo key empiece
// con prefix. Usado por operaciones de "borrar todo lo relacionado a X"
// (p. ej. vaciar un bucket de S3 antes de eliminarlo).
func (d *DB) DeletePrefix(bucket, prefix string) error {
	return d.bolt.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(bucket))
		if b == nil {
			return nil
		}
		c := b.Cursor()
		p := []byte(prefix)
		var keys [][]byte
		for k, _ := c.Seek(p); k != nil && hasPrefix(k, p); k, _ = c.Next() {
			keys = append(keys, append([]byte(nil), k...))
		}
		for _, k := range keys {
			if err := b.Delete(k); err != nil {
				return err
			}
		}
		return nil
	})
}

// Reset vacía los buckets indicados (borra todas sus entradas, sin borrar
// el bucket en sí). Pensado para el endpoint administrativo
// POST /_aws-emulator/reset: cada servicio expone sus propios nombres de
// bucket y los limpia llamando a este método, en vez de tener storage
// conociendo de antemano qué buckets existen.
func (d *DB) Reset(buckets ...string) error {
	for _, bucket := range buckets {
		if err := d.DeletePrefix(bucket, ""); err != nil {
			return fmt.Errorf("storage: error reseteando bucket %q: %w", bucket, err)
		}
	}
	return nil
}

func hasPrefix(k, prefix []byte) bool {
	if len(prefix) == 0 {
		return true
	}
	if len(k) < len(prefix) {
		return false
	}
	for i := range prefix {
		if k[i] != prefix[i] {
			return false
		}
	}
	return true
}
