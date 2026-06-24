package router

import (
	"bytes"
	"io"
	"net/http"
	"regexp"
	"strings"
)

// FromHTTPRequest construye un Request a partir de un *http.Request real,
// extrayendo las señales de detección de servicio. Si el body es
// application/x-www-form-urlencoded (forma clásica de la API Query de AWS
// para SQS/IAM/STS cuando el cliente no manda Action por query string),
// lo parsea para sacar "Action" y luego restaura r.Body para que el
// handler del servicio pueda leerlo de nuevo.
func FromHTTPRequest(r *http.Request) Request {
	action := r.URL.Query().Get("Action")
	if action == "" && r.Method == http.MethodPost {
		if ct := r.Header.Get("Content-Type"); strings.HasPrefix(ct, "application/x-www-form-urlencoded") {
			body, err := io.ReadAll(r.Body)
			if err == nil {
				r.Body = io.NopCloser(bytes.NewReader(body))
				if vals, err := parseFormBody(body); err == nil {
					action = vals.Get("Action")
				}
			}
		}
	}

	return Request{
		Method:        r.Method,
		Path:          r.URL.Path,
		Host:          r.Host,
		Target:        r.Header.Get("X-Amz-Target"),
		Authorization: r.Header.Get("Authorization"),
		Action:        action,
	}
}

func parseFormBody(body []byte) (urlValuesGetter, error) {
	req, err := http.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
	if err != nil {
		return urlValuesGetter{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if err := req.ParseForm(); err != nil {
		return urlValuesGetter{}, err
	}
	return urlValuesGetter{req.PostForm}, nil
}

type urlValuesGetter struct {
	values map[string][]string
}

func (g urlValuesGetter) Get(key string) string {
	if v, ok := g.values[key]; ok && len(v) > 0 {
		return v[0]
	}
	return ""
}

// AccessKeyIDFromAuthorization extrae el access key id del header
// Authorization (Credential={accessKeyId}/{date}/{region}/{service}/aws4_request).
// Devuelve "" si no se pudo extraer (request sin firmar, p. ej. curl directo).
var accessKeyRe = regexp.MustCompile(`Credential=([^/]+)/`)

func AccessKeyIDFromAuthorization(authorization string) string {
	if m := accessKeyRe.FindStringSubmatch(authorization); m != nil {
		return m[1]
	}
	return ""
}
