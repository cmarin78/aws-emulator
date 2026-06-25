package server

import (
	"encoding/json"
	"encoding/xml"
	"log"
	"net/http"
)

// WriteJSON serializa v como JSON con el status code dado. Usado por los
// servicios cuyo protocolo real es JSON (DynamoDB, SQS con JSON protocol,
// IAM con AWS Query/JSON híbrido en SDKs modernos, STS, etc.).
func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/x-amz-json-1.0")
	w.WriteHeader(status)
	if v == nil {
		return
	}
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("error escribiendo respuesta JSON: %v", err)
	}
}

// jsonError es la forma de error que esperan los SDKs para servicios JSON
// (DynamoDB, etc.): un campo "__type" con el shape ServiceName#ErrorCode.
type jsonError struct {
	Type    string `json:"__type"`
	Message string `json:"message"`
}

// WriteJSONError escribe un error en el shape JSON que validan los SDKs
// (botocore revisa "__type" para decidir la excepción Python a lanzar).
func WriteJSONError(w http.ResponseWriter, status int, errType, message string) {
	WriteJSON(w, status, jsonError{Type: errType, Message: message})
}

// xmlErrorBody replica el shape de error XML que devuelve la API Query de
// AWS (S3, SQS clásico, SNS, IAM, STS): un <Error> envuelto en
// <ErrorResponse>. El envoltorio no es cosmético -- el deserializador de
// errores del protocolo Query de aws-sdk-go-v2
// (awsAwsquery_deserializeError) busca específicamente el elemento
// <ErrorResponse><Error>...; sin él, no reconoce el documento como un
// error AWS y el SDK devuelve un genérico "UnknownError" en vez del Code
// real (p. ej. "InvalidAction"), aunque el body XML sea válido y aunque
// herramientas más permisivas como el AWS CLI (botocore) sí lo acepten sin
// el wrapper. Esto hacía parecer "UnknownError" misteriosos en
// SNS/SQS/IAM que en realidad eran errores normales (acción no soportada,
// recurso no encontrado, etc.) mal deserializados por el SDK real de Go.
// Encontrado vía terraform/aws-smoke-test (provider real, que usa
// aws-sdk-go-v2), ver ROADMAP.md.
type xmlErrorBody struct {
	XMLName xml.Name    `xml:"ErrorResponse"`
	Error   xmlErrorTag `xml:"Error"`
}
type xmlErrorTag struct {
	Type    string `xml:"Type"`
	Code    string `xml:"Code"`
	Message string `xml:"Message"`
}

// WriteXML serializa v como XML con el status code dado. Usado por los
// servicios "clásicos" de la API Query de AWS (S3, SQS, IAM, STS).
func WriteXML(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	if v == nil {
		return
	}
	if _, err := w.Write([]byte(xml.Header)); err != nil {
		log.Printf("error escribiendo XML header: %v", err)
		return
	}
	if err := xml.NewEncoder(w).Encode(v); err != nil {
		log.Printf("error escribiendo respuesta XML: %v", err)
	}
}

// WriteXMLError escribe un error en el shape XML <Error> de la API Query.
func WriteXMLError(w http.ResponseWriter, status int, code, message string) {
	WriteXML(w, status, xmlErrorBody{Error: xmlErrorTag{Type: "Sender", Code: code, Message: message}})
}

// restXMLErrorBody replica el shape de error que usa el protocolo REST-XML
// de AWS (S3): un <Error> SIN envoltorio -- a diferencia del protocolo
// Query (SNS/SQS/IAM/STS, ver xmlErrorBody arriba), el deserializador de
// errores REST-XML de aws-sdk-go-v2 espera <Error><Code/><Message/></Error>
// como elemento raíz. Envolverlo en <ErrorResponse> (como sí hay que hacer
// para Query) rompe la detección del Code real en S3 y el SDK cae de
// nuevo al "NotFound"/"UnknownError" genérico. Encontrado vía
// terraform/aws-smoke-test (GetBucketPolicy en un bucket sin policy
// debía deserializar como NoSuchBucketPolicy, no como NotFound genérico),
// ver ROADMAP.md.
type restXMLErrorBody struct {
	XMLName xml.Name `xml:"Error"`
	Code    string   `xml:"Code"`
	Message string   `xml:"Message"`
}

// WriteRESTXMLError escribe un error en el shape REST-XML <Error> (S3),
// distinto del <ErrorResponse><Error> del protocolo Query (ver
// WriteXMLError).
func WriteRESTXMLError(w http.ResponseWriter, status int, code, message string) {
	WriteXML(w, status, restXMLErrorBody{Code: code, Message: message})
}
