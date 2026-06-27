// Migraciones de esquema sobre datos ya persistidos en BoltDB.
//
// Esto NO es lo mismo que "agregar un campo nuevo a un struct": Go/encoding-
// json ya maneja eso solo (un campo que no estaba en el JSON viejo se
// deserializa con su valor cero). Una migración hace falta cuando ese valor
// cero no es aceptable -- por ejemplo, internal/services/dynamodb agregó un
// campo Table.Arn en la misma fase que internal/accountctx (ver ROADMAP.md);
// las filas escritas por un binario anterior a ese cambio no tienen ese
// campo en absoluto, así que sin backfill quedarían con Arn == "" para
// siempre, rompiendo cualquier cliente real que dependa de TableArn después
// de actualizar el emulador sin borrar .aws-emulator-data/.
//
// Diseño: cada paquete de servicio que necesite una migración se registra a
// sí mismo en su propio init() (ver internal/services/dynamodb/migrations.go
// para el caso real). storage no puede importar internal/services/* -- los
// servicios ya importan storage, así que sería un ciclo -- por eso el
// registro es por el lado inverso: el registro global vive acá, y los
// servicios se anotan en él.
package storage

import (
	"encoding/binary"
	"fmt"
	"sort"

	bolt "go.etcd.io/bbolt"
)

// metaBucket guarda metadata interna del motor de storage. Separado de los
// buckets de datos de cada servicio (todos prefijados con su propio nombre,
// p. ej. "dynamodb.tables") para no colisionar nunca con ellos.
const metaBucket = "_meta"

// schemaVersionKey persiste qué migraciones ya se aplicaron sobre este
// archivo de datos. Un archivo creado antes de que existiera este mecanismo
// (cualquier .db de las Fases 1-6) no tiene esta key -- se trata como
// versión 0, y todas las migraciones registradas se aplican en orden la
// primera vez que se abre con un binario que ya las conoce.
const schemaVersionKey = "schemaVersion"

// Migration es un paso de evolución de esquema. Apply corre dentro de la
// misma transacción de escritura de BoltDB que el bump de versión, así que
// un fallo a mitad de camino no deja el archivo en un estado parcialmente
// migrado: o se aplica entera y queda registrada, o no se aplica nada.
type Migration struct {
	// Version es el número de versión de esquema al que esta migración
	// lleva la base de datos. Deben registrarse sin saltos (1, 2, 3, ...)
	// -- Open() las aplica ordenadas ascendentemente por Version,
	// independientemente del orden en que los init() de cada paquete
	// hayan corrido (Go no garantiza ese orden entre paquetes hermanos).
	Version int
	// Description es una línea humana de qué hace, usada en logs y en el
	// error si Apply falla.
	Description string
	// Apply hace el trabajo real de la migración sobre el *bolt.Tx de
	// escritura ya abierto -- no debe abrir su propia transacción.
	Apply func(tx *bolt.Tx) error
}

// registry acumula las migraciones registradas por cada paquete de
// servicio. No es safe para concurrencia: todas las llamadas a
// RegisterMigration deben pasar durante init() de paquete, antes de que
// cualquier goroutine llame a storage.Open.
var registry []Migration

// RegisterMigration agrega una migración al registro global. Pensado para
// llamarse desde un init() de paquete de servicio (import side-effect),
// nunca en runtime después de que el servidor ya está aceptando requests.
func RegisterMigration(m Migration) {
	registry = append(registry, m)
}

// migrate aplica, en orden ascendente de versión, todas las migraciones
// registradas con Version mayor a la actualmente persistida. Cada
// migración es su propia transacción de escritura (junto con el update de
// schemaVersion) -- si una migración falla, las anteriores ya quedaron
// confirmadas y la base queda en una versión intermedia válida, lista para
// reintentar solo desde ahí en el próximo Open.
func (d *DB) migrate() error {
	sorted := append([]Migration(nil), registry...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Version < sorted[j].Version })

	for _, m := range sorted {
		current, err := d.schemaVersion()
		if err != nil {
			return err
		}
		if m.Version <= current {
			continue
		}
		if err := d.bolt.Update(func(tx *bolt.Tx) error {
			if err := m.Apply(tx); err != nil {
				return fmt.Errorf("migración de esquema %d (%s): %w", m.Version, m.Description, err)
			}
			return setSchemaVersion(tx, m.Version)
		}); err != nil {
			return err
		}
	}
	return nil
}

func (d *DB) schemaVersion() (int, error) {
	var version int
	err := d.bolt.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(metaBucket))
		if b == nil {
			return nil
		}
		data := b.Get([]byte(schemaVersionKey))
		if data == nil {
			return nil
		}
		version = int(binary.BigEndian.Uint64(data))
		return nil
	})
	return version, err
}

func setSchemaVersion(tx *bolt.Tx, version int) error {
	b, err := tx.CreateBucketIfNotExists([]byte(metaBucket))
	if err != nil {
		return err
	}
	buf := make([]byte, 8)
	binary.BigEndian.PutUint64(buf, uint64(version))
	return b.Put([]byte(schemaVersionKey), buf)
}

// SchemaVersion expone la versión de esquema actualmente persistida en esta
// base de datos. Pensado para diagnóstico (p. ej. el log de arranque en
// cmd/aws-emulator/main.go) y para tests.
func (d *DB) SchemaVersion() (int, error) {
	return d.schemaVersion()
}

// LatestSchemaVersion devuelve la versión más alta entre las migraciones
// registradas (0 si no hay ninguna registrada todavía). Sirve para que el
// log de arranque pueda mostrar "versión actual / versión más nueva
// conocida por este binario" sin que main.go tenga que llevar su propia
// cuenta manual.
func LatestSchemaVersion() int {
	latest := 0
	for _, m := range registry {
		if m.Version > latest {
			latest = m.Version
		}
	}
	return latest
}
