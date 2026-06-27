// Package accountctx deriva una identidad (account ID de 12 dígitos +
// región) por request a partir de las credenciales que manda el cliente, y
// la expone a los servicios vía el contexto de la request -- ver
// ROADMAP.md, Fase 6, "real multi-tenancy".
//
// Alcance deliberado de esta pasada: cada credencial distinta (access key
// id distinto) obtiene un account ID estable y propio, reflejado en todos
// los ARNs que devuelve cada servicio y en STS GetCallerIdentity -- AWS
// real hace lo mismo (el account ID es función del par de credenciales, no
// un valor fijo). Lo que NO cambia en esta pasada es el almacenamiento:
// todas las credenciales siguen compartiendo el mismo BoltDB y los mismos
// buckets/keys -- aislar datos por cuenta es una migración de storage mucho
// más grande (re-anidar todas las claves de internal/storage bajo un
// prefijo de cuenta, en los ~12 servicios) que no se justifica para un
// emulador de desarrollo local, donde en la práctica un único proceso
// normalmente corre con un único par de credenciales de prueba. Si en el
// futuro hace falta aislamiento real de datos entre cuentas, este paquete
// es el punto de partida: ya tiene el account ID resuelto por request.
package accountctx

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"net/http"
	"regexp"
)

// DefaultAccountID es el account ID que se usa cuando la request no trae
// credenciales firmadas (curl directo sin Authorization, p. ej.) -- el
// mismo valor fijo que usaba cada servicio antes de esta fase.
const DefaultAccountID = "000000000000"

// DefaultRegion es la región que se usa cuando la request no trae
// credenciales firmadas o el credential scope no se pudo parsear.
const DefaultRegion = "us-east-1"

// credentialRe extrae el access key id y la región de un header
// Authorization AWS4-HMAC-SHA256:
// "Credential={accessKeyId}/{date}/{region}/{service}/aws4_request".
var credentialRe = regexp.MustCompile(`Credential=([^/]+)/[^/]+/([^/]+)/`)

// DeriveAccountID calcula un account ID de 12 dígitos estable a partir de
// un access key id, mismo enfoque que LocalStack: el mismo access key
// siempre devuelve el mismo account ID, y access keys distintos devuelven
// (con altísima probabilidad) account IDs distintos -- es un hash, no una
// asignación con garantía de cero colisiones, pero alcanza para separar
// identidades en desarrollo/test. Un access key vacío (request sin firmar)
// siempre devuelve DefaultAccountID para no romper el comportamiento previo
// a esta fase.
func DeriveAccountID(accessKeyID string) string {
	if accessKeyID == "" {
		return DefaultAccountID
	}
	sum := sha256.Sum256([]byte(accessKeyID))
	n := binary.BigEndian.Uint64(sum[:8]) % 1_000_000_000_000
	return fmt.Sprintf("%012d", n)
}

// FromAuthorization extrae el access key id y la región del header
// Authorization y devuelve el account ID derivado más la región (o los
// defaults si el header está vacío o no matchea el shape esperado).
func FromAuthorization(authorization string) (accountID, region string) {
	m := credentialRe.FindStringSubmatch(authorization)
	if m == nil {
		return DefaultAccountID, DefaultRegion
	}
	return DeriveAccountID(m[1]), m[2]
}

type ctxKey struct{}

type identity struct {
	accountID string
	region    string
}

// Middleware calcula la identidad (account ID + región) de cada request a
// partir de su header Authorization y la deja en el contexto para que
// cualquier servicio downstream la pueda leer con FromContext. Va en la
// cadena de middlewares de server.Handler(), antes del dispatch a
// servicios.
func Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		accountID, region := FromAuthorization(r.Header.Get("Authorization"))
		ctx := context.WithValue(r.Context(), ctxKey{}, identity{accountID: accountID, region: region})
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// FromContext devuelve el account ID y región resueltos por Middleware
// para esta request. Si Middleware no corrió (p. ej. un test que llama a
// un Service directamente sin pasar por server.Handler(), o que construye
// su propio *http.Request sin pasarlo por el handler raíz), devuelve los
// defaults -- nunca strings vacíos, para que los servicios puedan
// concatenar el resultado directamente en un ARN sin chequear el ok.
func FromContext(ctx context.Context) (accountID, region string) {
	id, ok := ctx.Value(ctxKey{}).(identity)
	if !ok {
		return DefaultAccountID, DefaultRegion
	}
	return id.accountID, id.region
}

// FromRequest es un atajo de FromAuthorization(r.Header.Get("Authorization"))
// para los casos en los que un servicio tiene el *http.Request a mano pero
// no quiere depender de que Middleware haya corrido (p. ej. tests que
// llaman ServeHTTP directamente). Servicios que sí corren detrás de
// server.Handler() pueden usar FromContext(r.Context()) indistintamente --
// da el mismo resultado, porque Middleware deriva la identidad de la misma
// forma.
func FromRequest(r *http.Request) (accountID, region string) {
	return FromAuthorization(r.Header.Get("Authorization"))
}
