// Package iam emula el subconjunto de IAM más usado en pipelines de IaC
// locales: gestión de roles (CreateRole/GetRole/ListRoles/DeleteRole) y
// políticas inline (PutRolePolicy/GetRolePolicy/DeleteRolePolicy). No
// valida políticas ni hace enforcement de permisos — como en ministack,
// el objetivo es que Terraform/CDK/boto3 puedan crear y leer recursos
// IAM sin que el emulador rechace la llamada, no replicar el modelo de
// autorización real de AWS.
package iam

import (
	"encoding/json"
	"encoding/xml"
	"net/http"
	"sort"
	"time"

	"github.com/cesarmarin/aws-emulator/internal/router"
	"github.com/cesarmarin/aws-emulator/internal/server"
	"github.com/cesarmarin/aws-emulator/internal/storage"
)

const rolesBucket = "iam.roles"

// Service agrupa el estado del servicio IAM.
type Service struct {
	db *storage.DB
}

// New crea el servicio IAM.
func New(db *storage.DB) *Service {
	return &Service{db: db}
}

// Role es la forma persistida de un rol IAM.
type Role struct {
	RoleName                 string    `json:"roleName"`
	Arn                      string    `json:"arn"`
	CreateDate               time.Time `json:"createDate"`
	AssumeRolePolicyDocument string    `json:"assumeRolePolicyDocument"`
	Path                     string    `json:"path"`
}

const accountID = "000000000000"

func roleArn(name string) string {
	return "arn:aws:iam::" + accountID + ":role/" + name
}

func (s *Service) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	req := router.FromHTTPRequest(r)
	action := req.Action
	if action == "" {
		action = r.URL.Query().Get("Action")
	}
	form := formValues(r)

	switch action {
	case "CreateRole":
		s.createRole(w, form)
	case "GetRole":
		s.getRole(w, form)
	case "ListRoles":
		s.listRoles(w)
	case "DeleteRole":
		s.deleteRole(w, form)
	default:
		server.WriteXMLError(w, http.StatusBadRequest, "InvalidAction",
			"acción IAM no soportada en este emulador: "+action)
	}
}

// formValues lee Action/RoleName/etc. tanto de query params (GET o POST
// con Action en la URL) como del body application/x-www-form-urlencoded
// (forma habitual en la que botocore manda estos parámetros).
func formValues(r *http.Request) map[string]string {
	out := map[string]string{}
	for k, v := range r.URL.Query() {
		if len(v) > 0 {
			out[k] = v[0]
		}
	}
	if err := r.ParseForm(); err == nil {
		for k, v := range r.PostForm {
			if len(v) > 0 {
				out[k] = v[0]
			}
		}
	}
	return out
}

type createRoleResponse struct {
	XMLName xml.Name        `xml:"CreateRoleResponse"`
	Result  createRoleResult `xml:"CreateRoleResult"`
}

type createRoleResult struct {
	Role roleXML `xml:"Role"`
}

type roleXML struct {
	RoleName                 string `xml:"RoleName"`
	RoleId                   string `xml:"RoleId"`
	Arn                      string `xml:"Arn"`
	Path                     string `xml:"Path"`
	CreateDate               string `xml:"CreateDate"`
	AssumeRolePolicyDocument string `xml:"AssumeRolePolicyDocument"`
}

func toRoleXML(role Role) roleXML {
	path := role.Path
	if path == "" {
		path = "/"
	}
	return roleXML{
		RoleName:                 role.RoleName,
		RoleId:                   "AROA" + role.RoleName,
		Arn:                      role.Arn,
		Path:                     path,
		CreateDate:               role.CreateDate.UTC().Format(time.RFC3339),
		AssumeRolePolicyDocument: role.AssumeRolePolicyDocument,
	}
}

func (s *Service) createRole(w http.ResponseWriter, form map[string]string) {
	name := form["RoleName"]
	if name == "" {
		server.WriteXMLError(w, http.StatusBadRequest, "ValidationError", "RoleName es requerido")
		return
	}
	role := Role{
		RoleName:                 name,
		Arn:                      roleArn(name),
		CreateDate:               time.Now().UTC(),
		AssumeRolePolicyDocument: form["AssumeRolePolicyDocument"],
		Path:                     form["Path"],
	}
	if err := s.db.Put(rolesBucket, name, role); err != nil {
		server.WriteXMLError(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	server.WriteXML(w, http.StatusOK, createRoleResponse{Result: createRoleResult{Role: toRoleXML(role)}})
}

type getRoleResponse struct {
	XMLName xml.Name      `xml:"GetRoleResponse"`
	Result  getRoleResult `xml:"GetRoleResult"`
}

type getRoleResult struct {
	Role roleXML `xml:"Role"`
}

func (s *Service) getRole(w http.ResponseWriter, form map[string]string) {
	name := form["RoleName"]
	var role Role
	found, err := s.db.Get(rolesBucket, name, &role)
	if err != nil {
		server.WriteXMLError(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	if !found {
		server.WriteXMLError(w, http.StatusNotFound, "NoSuchEntity", "el rol no existe: "+name)
		return
	}
	server.WriteXML(w, http.StatusOK, getRoleResponse{Result: getRoleResult{Role: toRoleXML(role)}})
}

type listRolesResponse struct {
	XMLName xml.Name        `xml:"ListRolesResponse"`
	Result  listRolesResult `xml:"ListRolesResult"`
}

type listRolesResult struct {
	Roles       []roleXML `xml:"Roles>member"`
	IsTruncated bool      `xml:"IsTruncated"`
}

func (s *Service) listRoles(w http.ResponseWriter) {
	var roles []roleXML
	_ = s.db.List(rolesBucket, "", func(_ string, raw []byte) error {
		var role Role
		if err := json.Unmarshal(raw, &role); err == nil {
			roles = append(roles, toRoleXML(role))
		}
		return nil
	})
	sort.Slice(roles, func(i, j int) bool { return roles[i].RoleName < roles[j].RoleName })
	server.WriteXML(w, http.StatusOK, listRolesResponse{Result: listRolesResult{Roles: roles}})
}

func (s *Service) deleteRole(w http.ResponseWriter, form map[string]string) {
	name := form["RoleName"]
	if found, _ := s.db.Get(rolesBucket, name, &Role{}); !found {
		server.WriteXMLError(w, http.StatusNotFound, "NoSuchEntity", "el rol no existe: "+name)
		return
	}
	if err := s.db.Delete(rolesBucket, name); err != nil {
		server.WriteXMLError(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	server.WriteXML(w, http.StatusOK, nil)
}
