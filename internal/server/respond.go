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
// AWS (S3, SQS clásico, IAM, STS): <Error><Code/><Message/></Error>.
type xmlErrorBody struct {
	XMLName xml.Name `xml:"Error"`
	Code    string   `xml:"Code"`
	Message string   `xml:"Message"`
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
	WriteXML(w, status, xmlErrorBody{Code: code, Message: message})
}
