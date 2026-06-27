// Migración de esquema (ver internal/storage/migrate.go para el mecanismo
// general): backfill de Table.Arn para tablas creadas con binarios
// anteriores a internal/accountctx (Fase 6 -- multi-tenancy, ver
// ROADMAP.md).
//
// Antes de esa fase, el struct Table no tenía campo Arn en absoluto: el
// ARN se computaba al vuelo dentro de tableDescription a partir de un
// accountID hardcodeado. Una fila persistida por ese binario viejo, leída
// con el binario actual, deserializa con Arn == "" -- el campo nuevo
// simplemente no estaba en el JSON guardado, y encoding/json no tiene
// forma de inventarlo solo. Sin este backfill, TableArn aparecería vacío
// para esas tablas después de actualizar el emulador sin borrar
// .aws-emulator-data/, rompiendo cualquier referencia que un cliente real
// (p. ej. el provider de Terraform) ya le hubiera hecho a ese ARN.
package dynamodb

import (
	"encoding/json"

	"github.com/cesarmarin/aws-emulator/internal/accountctx"
	"github.com/cesarmarin/aws-emulator/internal/storage"
	bolt "go.etcd.io/bbolt"
)

func init() {
	storage.RegisterMigration(storage.Migration{
		Version:     1,
		Description: "dynamodb: backfill Table.Arn para tablas creadas antes de accountctx",
		Apply:       backfillTableArn,
	})
}

// backfillTableArn recorre dynamodb.tables y completa Arn en cualquier
// tabla que no lo tenga, usando accountctx.DefaultAccountID -- el único
// account ID que pudo haber existido antes de que el routing por
// credencial existiera, así que es el valor correcto para datos viejos
// (no uno inventado).
//
// Junta primero las filas a actualizar en un slice y recién después hace
// los Put: mutar un bucket de BoltDB en el medio de iterar su Cursor es
// el mismo anti-patrón ya documentado en ROADMAP.md para
// DeletePrefix/sns.deliverToSubscribers -- evitarlo por las dudas, aunque
// acá sería Put y no Delete, es más simple no depender de qué garantías
// exactas da bbolt sobre eso.
func backfillTableArn(tx *bolt.Tx) error {
	b := tx.Bucket([]byte(tablesBucket))
	if b == nil {
		return nil // no hay ninguna tabla persistida todavía, nada que migrar
	}

	type pendingUpdate struct {
		key  []byte
		data []byte
	}
	var updates []pendingUpdate

	c := b.Cursor()
	for k, v := c.First(); k != nil; k, v = c.Next() {
		var t Table
		if err := json.Unmarshal(v, &t); err != nil {
			return err
		}
		if t.Arn != "" {
			continue
		}
		t.Arn = tableArn(accountctx.DefaultAccountID, t.TableName)
		data, err := json.Marshal(t)
		if err != nil {
			return err
		}
		updates = append(updates, pendingUpdate{key: append([]byte(nil), k...), data: data})
	}

	for _, u := range updates {
		if err := b.Put(u.key, u.data); err != nil {
			return err
		}
	}
	return nil
}
