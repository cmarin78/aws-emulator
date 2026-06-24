// Package router detecta a qué servicio de AWS apunta una request HTTP
// entrante, replicando la lógica de ministack/core/router.py (el emulador
// Python hermano de este proyecto) en Go idiomático.
//
// AWS multiplexa decenas de servicios sobre un único endpoint (un solo
// puerto, normalmente 4566) y no hay un único campo confiable para saber
// el destino: cada protocolo (JSON 1.0/1.1, Query/XML, REST) marca el
// servicio de una forma distinta. detect_service en ministack usa, en
// orden de confiabilidad decreciente:
//
//  1. X-Amz-Target (servicios JSON: DynamoDB, SSM, Logs, ...)
//  2. Authorization: credential scope (AWS4-HMAC-SHA256 .../{region}/{service}/aws4_request)
//  3. Action= en query params o body (servicios Query/XML: SQS, SNS, IAM, STS, CloudWatch)
//  4. Host header (sqs.<region>.amazonaws.com, dynamodb.<region>.amazonaws.com, ...)
//  5. Path patterns (Lambda /2015-03-31/..., ECS /clusters, ...)
//  6. S3 como fallback final (sin Action, sin target, host no reconocido)
//
// Solo se porta el subconjunto de servicios que este proyecto implementa
// en Fase 1 (s3, sqs, dynamodb, iam, sts) más los patrones de host/acción
// más comunes, para no arrastrar las ~50 entradas de servicios no
// implementados todavía. Ver ROADMAP.md para el plan de fases siguientes
// — agregar un servicio nuevo implica sumar sus patrones acá Y su
// Service en internal/services/.
package router

import "regexp"

// ServicePattern describe cómo reconocer un servicio por distintas señales
// de la request. Cualquier campo puede ir vacío si esa señal no aplica.
type ServicePattern struct {
	// TargetPrefixes: prefijos de X-Amz-Target que identifican al servicio
	// (p. ej. "DynamoDB_20120810" para DynamoDB).
	TargetPrefixes []string
	// HostPatterns: regexps sobre el header Host.
	HostPatterns []*regexp.Regexp
	// PathPatterns: regexps sobre el path de la URL.
	PathPatterns []*regexp.Regexp
}

// servicePatterns es el equivalente Go de SERVICE_PATTERNS en router.py.
// El orden importa para los casos ambiguos (igual que en Python, donde
// dynamodbstreams se evalúa antes que dynamodb por la misma razón
// documentada ahí: el host "streams.dynamodb." también matchea "dynamodb.").
var servicePatterns = []struct {
	Name    string
	Pattern ServicePattern
}{
	{"dynamodbstreams", ServicePattern{
		TargetPrefixes: []string{"DynamoDBStreams_20120810"},
		HostPatterns:   []*regexp.Regexp{regexp.MustCompile(`streams\.dynamodb\.`)},
	}},
	{"dynamodb", ServicePattern{
		TargetPrefixes: []string{"DynamoDB_20120810"},
		HostPatterns:   []*regexp.Regexp{regexp.MustCompile(`dynamodb\.`)},
	}},
	{"sqs", ServicePattern{
		TargetPrefixes: []string{"AmazonSQS"},
		HostPatterns:   []*regexp.Regexp{regexp.MustCompile(`sqs\.`)},
		PathPatterns:   []*regexp.Regexp{regexp.MustCompile(`/queue/`)},
	}},
	{"iam", ServicePattern{
		HostPatterns: []*regexp.Regexp{regexp.MustCompile(`iam\.`)},
	}},
	{"sts", ServicePattern{
		TargetPrefixes: []string{"AWSSecurityTokenService"},
		HostPatterns:   []*regexp.Regexp{regexp.MustCompile(`sts\.`)},
	}},
	{"s3", ServicePattern{
		HostPatterns: []*regexp.Regexp{regexp.MustCompile(`s3[.\-]`), regexp.MustCompile(`\.s3\.`)},
		// Nota: a diferencia de ministack/router.py, acá no se declara un
		// PathPattern para S3 porque el RE2 de Go (a diferencia del motor de
		// Python) no soporta lookahead negativo, y S3 de todos modos se
		// salta explícitamente en el paso 5 de DetectService — siempre actúa
		// como fallback final (paso 6), nunca por path pattern.
	}},
}

// credentialScopeRe extrae el componente {service} de un header
// Authorization AWS4-HMAC-SHA256: "Credential={key}/{date}/{region}/{service}/aws4_request".
var credentialScopeRe = regexp.MustCompile(`Credential=[^/]+/[^/]+/[^/]+/([^/]+)/`)

// actionServiceMap es el equivalente reducido de action_service_map en
// router.py: qué servicio implementa cada Action de la API Query/XML.
// Solo cubre las acciones de los servicios que este proyecto ya
// implementa; sumar más entradas a medida que se agreguen servicios.
var actionServiceMap = map[string]string{
	// SQS
	"SendMessage": "sqs", "ReceiveMessage": "sqs", "DeleteMessage": "sqs",
	"CreateQueue": "sqs", "DeleteQueue": "sqs", "ListQueues": "sqs",
	"GetQueueUrl": "sqs", "GetQueueAttributes": "sqs", "SetQueueAttributes": "sqs",
	"PurgeQueue": "sqs", "SendMessageBatch": "sqs", "DeleteMessageBatch": "sqs",
	"ChangeMessageVisibility": "sqs", "TagQueue": "sqs", "UntagQueue": "sqs",
	"ListQueueTags": "sqs",
	// IAM
	"CreateRole": "iam", "GetRole": "iam", "ListRoles": "iam", "DeleteRole": "iam",
	"CreateUser": "iam", "GetUser": "iam", "ListUsers": "iam", "DeleteUser": "iam",
	"CreatePolicy": "iam", "GetPolicy": "iam", "DeletePolicy": "iam", "ListPolicies": "iam",
	"AttachRolePolicy": "iam", "DetachRolePolicy": "iam", "ListAttachedRolePolicies": "iam",
	"PutRolePolicy": "iam", "GetRolePolicy": "iam", "DeleteRolePolicy": "iam",
	"CreateAccessKey": "iam", "ListAccessKeys": "iam", "DeleteAccessKey": "iam",
	"TagRole": "iam", "UntagRole": "iam",
	// STS
	"GetCallerIdentity": "sts", "AssumeRole": "sts", "GetSessionToken": "sts",
	"AssumeRoleWithWebIdentity": "sts",
}

// credentialScopeMap traduce el nombre de servicio que aparece en el
// credential scope de SigV4 (que no siempre coincide con el nombre interno
// del servicio) al nombre interno usado en este proyecto.
var credentialScopeMap = map[string]string{
	"dynamodb": "dynamodb",
	"sqs":      "sqs",
	"iam":      "iam",
	"sts":      "sts",
	"s3":       "s3",
}

// Request agrupa las señales de una request HTTP relevantes para
// detección de servicio, desacoplado de net/http para que sea trivial
// testear DetectService con tablas de casos sin levantar un servidor.
type Request struct {
	Method        string
	Path          string
	Host          string
	Target        string // header X-Amz-Target
	Authorization string
	Action        string // de query params o, si el body ya fue parseado, de form values
}

// DetectService replica detect_service() de router.py: devuelve el nombre
// interno del servicio (p. ej. "s3", "dynamodb") o "" si ninguna señal
// matchea, en cuyo caso el caller debe responder 404.
func DetectService(req Request) string {
	// 1. X-Amz-Target (más confiable para servicios JSON).
	if req.Target != "" {
		for _, sp := range servicePatterns {
			for _, prefix := range sp.Pattern.TargetPrefixes {
				if hasPrefix(req.Target, prefix) {
					return sp.Name
				}
			}
		}
	}

	// 2. Authorization: credential scope.
	if req.Authorization != "" {
		if m := credentialScopeRe.FindStringSubmatch(req.Authorization); m != nil {
			scope := m[1]
			for _, sp := range servicePatterns {
				if sp.Name == scope {
					return sp.Name
				}
			}
			if mapped, ok := credentialScopeMap[scope]; ok {
				return mapped
			}
		}
	}

	// 3. Action= (servicios Query/XML).
	if req.Action != "" {
		if svc, ok := actionServiceMap[req.Action]; ok {
			return svc
		}
	}

	// 4. Host header.
	if req.Host != "" {
		for _, sp := range servicePatterns {
			for _, re := range sp.Pattern.HostPatterns {
				if re.MatchString(req.Host) {
					return sp.Name
				}
			}
		}
	}

	// 5. Path patterns (excluyendo s3, que es el fallback final del paso 6).
	if req.Path != "" {
		for _, sp := range servicePatterns {
			if sp.Name == "s3" {
				continue
			}
			for _, re := range sp.Pattern.PathPatterns {
				if re.MatchString(req.Path) {
					return sp.Name
				}
			}
		}
	}

	// 6. Fallback: S3.
	return "s3"
}

func hasPrefix(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}
