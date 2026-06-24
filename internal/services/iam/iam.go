// Package iam emula el subconjunto de IAM más usado en pipelines de IaC
// locales: gestión de roles (CreateRole/GetRole/ListRoles/DeleteRole),
// políticas inline de rol (PutRolePolicy/GetRolePolicy/DeleteRolePolicy/
// ListRolePolicies), usuarios (CreateUser/GetUser/ListUsers/DeleteUser) y
// access keys (CreateAccessKey/ListAccessKeys/DeleteAccessKey). No valida
// políticas ni hace enforcement de permisos — el objetivo es que
// Terraform/CDK/boto3 puedan crear y leer recursos IAM sin que el emulador
// rechace la llamada, no replicar el modelo de autorización real de AWS.
package iam

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/cesarmarin/aws-emulator/internal/router"
	"github.com/cesarmarin/aws-emulator/internal/server"
	"github.com/cesarmarin/aws-emulator/internal/storage"
)

const (
	rolesBucket      = "iam.roles"
	policiesBucket   = "iam.policies"
	usersBucket      = "iam.users"
	accessKeysBucket = "iam.accesskeys"
)

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

func userArn(name string) string {
	return "arn:aws:iam::" + accountID + ":user/" + name
}

func randomID(prefix string, n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return strings.ToUpper(prefix + hex.EncodeToString(b))
}

// RolePolicy es la forma persistida de una política inline de rol.
type RolePolicy struct {
	RoleName       string `json:"roleName"`
	PolicyName     string `json:"policyName"`
	PolicyDocument string `json:"policyDocument"`
}

func policyKey(roleName, policyName string) string {
	return roleName + "/" + policyName
}

// User es la forma persistida de un usuario IAM.
type User struct {
	UserName   string    `json:"userName"`
	Arn        string    `json:"arn"`
	CreateDate time.Time `json:"createDate"`
	Path       string    `json:"path"`
}

// AccessKey es la forma persistida de una access key de usuario.
type AccessKey struct {
	UserName        string    `json:"userName"`
	AccessKeyId     string    `json:"accessKeyId"`
	SecretAccessKey string    `json:"secretAccessKey"`
	Status          string    `json:"status"`
	CreateDate      time.Time `json:"createDate"`
}

func accessKeyKey(userName, accessKeyId string) string {
	return userName + "/" + accessKeyId
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
	case "PutRolePolicy":
		s.putRolePolicy(w, form)
	case "GetRolePolicy":
		s.getRolePolicy(w, form)
	case "DeleteRolePolicy":
		s.deleteRolePolicy(w, form)
	case "ListRolePolicies":
		s.listRolePolicies(w, form)
	case "CreateUser":
		s.createUser(w, form)
	case "GetUser":
		s.getUser(w, form)
	case "ListUsers":
		s.listUsers(w)
	case "DeleteUser":
		s.deleteUser(w, form)
	case "CreateAccessKey":
		s.createAccessKey(w, form)
	case "ListAccessKeys":
		s.listAccessKeys(w, form)
	case "DeleteAccessKey":
		s.deleteAccessKey(w, form)
	default:
		server.WriteXMLError(w, http.StatusBadRequest, "InvalidAction",
			"acción IAM no soportada en este emulador: "+action)
	}
}

// Reset limpia todo el estado persistido de IAM (roles, políticas inline,
// usuarios y access keys). Implementa server.Resettable.
func (s *Service) Reset() error {
	return s.db.Reset(rolesBucket, policiesBucket, usersBucket, accessKeysBucket)
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
	XMLName xml.Name         `xml:"CreateRoleResponse"`
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

type deleteRoleResponse struct {
	XMLName xml.Name `xml:"DeleteRoleResponse"`
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
	// botocore exige al menos un elemento raíz válido en la respuesta XML,
	// aunque la operación no tenga datos que devolver -- un body
	// completamente vacío rompe el parser ("no element found").
	server.WriteXML(w, http.StatusOK, deleteRoleResponse{})
}

// --- políticas inline de rol ---

type putRolePolicyResponse struct {
	XMLName xml.Name `xml:"PutRolePolicyResponse"`
}

func (s *Service) putRolePolicy(w http.ResponseWriter, form map[string]string) {
	roleName, policyName := form["RoleName"], form["PolicyName"]
	if roleName == "" || policyName == "" {
		server.WriteXMLError(w, http.StatusBadRequest, "ValidationError", "RoleName y PolicyName son requeridos")
		return
	}
	if found, _ := s.db.Get(rolesBucket, roleName, &Role{}); !found {
		server.WriteXMLError(w, http.StatusNotFound, "NoSuchEntity", "el rol no existe: "+roleName)
		return
	}
	p := RolePolicy{RoleName: roleName, PolicyName: policyName, PolicyDocument: form["PolicyDocument"]}
	if err := s.db.Put(policiesBucket, policyKey(roleName, policyName), p); err != nil {
		server.WriteXMLError(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	server.WriteXML(w, http.StatusOK, putRolePolicyResponse{})
}

type getRolePolicyResponse struct {
	XMLName xml.Name            `xml:"GetRolePolicyResponse"`
	Result  getRolePolicyResult `xml:"GetRolePolicyResult"`
}
type getRolePolicyResult struct {
	RoleName       string `xml:"RoleName"`
	PolicyName     string `xml:"PolicyName"`
	PolicyDocument string `xml:"PolicyDocument"`
}

func (s *Service) getRolePolicy(w http.ResponseWriter, form map[string]string) {
	roleName, policyName := form["RoleName"], form["PolicyName"]
	var p RolePolicy
	found, err := s.db.Get(policiesBucket, policyKey(roleName, policyName), &p)
	if err != nil {
		server.WriteXMLError(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	if !found {
		server.WriteXMLError(w, http.StatusNotFound, "NoSuchEntity",
			"la política inline no existe: "+roleName+"/"+policyName)
		return
	}
	server.WriteXML(w, http.StatusOK, getRolePolicyResponse{Result: getRolePolicyResult{
		RoleName: p.RoleName, PolicyName: p.PolicyName, PolicyDocument: p.PolicyDocument,
	}})
}

type deleteRolePolicyResponse struct {
	XMLName xml.Name `xml:"DeleteRolePolicyResponse"`
}

func (s *Service) deleteRolePolicy(w http.ResponseWriter, form map[string]string) {
	roleName, policyName := form["RoleName"], form["PolicyName"]
	if err := s.db.Delete(policiesBucket, policyKey(roleName, policyName)); err != nil {
		server.WriteXMLError(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	server.WriteXML(w, http.StatusOK, deleteRolePolicyResponse{})
}

type listRolePoliciesResponse struct {
	XMLName xml.Name               `xml:"ListRolePoliciesResponse"`
	Result  listRolePoliciesResult `xml:"ListRolePoliciesResult"`
}
type listRolePoliciesResult struct {
	PolicyNames []string `xml:"PolicyNames>member"`
	IsTruncated bool     `xml:"IsTruncated"`
}

func (s *Service) listRolePolicies(w http.ResponseWriter, form map[string]string) {
	roleName := form["RoleName"]
	var names []string
	_ = s.db.List(policiesBucket, roleName+"/", func(_ string, raw []byte) error {
		var p RolePolicy
		if err := json.Unmarshal(raw, &p); err == nil {
			names = append(names, p.PolicyName)
		}
		return nil
	})
	sort.Strings(names)
	server.WriteXML(w, http.StatusOK, listRolePoliciesResponse{Result: listRolePoliciesResult{PolicyNames: names}})
}

// --- usuarios ---

type userXML struct {
	UserName   string `xml:"UserName"`
	UserId     string `xml:"UserId"`
	Arn        string `xml:"Arn"`
	Path       string `xml:"Path"`
	CreateDate string `xml:"CreateDate"`
}

func toUserXML(u User) userXML {
	path := u.Path
	if path == "" {
		path = "/"
	}
	return userXML{
		UserName:   u.UserName,
		UserId:     "AIDA" + u.UserName,
		Arn:        u.Arn,
		Path:       path,
		CreateDate: u.CreateDate.UTC().Format(time.RFC3339),
	}
}

type createUserResponse struct {
	XMLName xml.Name         `xml:"CreateUserResponse"`
	Result  createUserResult `xml:"CreateUserResult"`
}
type createUserResult struct {
	User userXML `xml:"User"`
}

func (s *Service) createUser(w http.ResponseWriter, form map[string]string) {
	name := form["UserName"]
	if name == "" {
		server.WriteXMLError(w, http.StatusBadRequest, "ValidationError", "UserName es requerido")
		return
	}
	u := User{UserName: name, Arn: userArn(name), CreateDate: time.Now().UTC(), Path: form["Path"]}
	if err := s.db.Put(usersBucket, name, u); err != nil {
		server.WriteXMLError(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	server.WriteXML(w, http.StatusOK, createUserResponse{Result: createUserResult{User: toUserXML(u)}})
}

type getUserResponse struct {
	XMLName xml.Name      `xml:"GetUserResponse"`
	Result  getUserResult `xml:"GetUserResult"`
}
type getUserResult struct {
	User userXML `xml:"User"`
}

func (s *Service) getUser(w http.ResponseWriter, form map[string]string) {
	name := form["UserName"]
	var u User
	found, err := s.db.Get(usersBucket, name, &u)
	if err != nil {
		server.WriteXMLError(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	if !found {
		server.WriteXMLError(w, http.StatusNotFound, "NoSuchEntity", "el usuario no existe: "+name)
		return
	}
	server.WriteXML(w, http.StatusOK, getUserResponse{Result: getUserResult{User: toUserXML(u)}})
}

type listUsersResponse struct {
	XMLName xml.Name        `xml:"ListUsersResponse"`
	Result  listUsersResult `xml:"ListUsersResult"`
}
type listUsersResult struct {
	Users       []userXML `xml:"Users>member"`
	IsTruncated bool      `xml:"IsTruncated"`
}

func (s *Service) listUsers(w http.ResponseWriter) {
	var users []userXML
	_ = s.db.List(usersBucket, "", func(_ string, raw []byte) error {
		var u User
		if err := json.Unmarshal(raw, &u); err == nil {
			users = append(users, toUserXML(u))
		}
		return nil
	})
	sort.Slice(users, func(i, j int) bool { return users[i].UserName < users[j].UserName })
	server.WriteXML(w, http.StatusOK, listUsersResponse{Result: listUsersResult{Users: users}})
}

type deleteUserResponse struct {
	XMLName xml.Name `xml:"DeleteUserResponse"`
}

func (s *Service) deleteUser(w http.ResponseWriter, form map[string]string) {
	name := form["UserName"]
	if found, _ := s.db.Get(usersBucket, name, &User{}); !found {
		server.WriteXMLError(w, http.StatusNotFound, "NoSuchEntity", "el usuario no existe: "+name)
		return
	}
	if err := s.db.DeletePrefix(accessKeysBucket, name+"/"); err != nil {
		server.WriteXMLError(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	if err := s.db.Delete(usersBucket, name); err != nil {
		server.WriteXMLError(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	server.WriteXML(w, http.StatusOK, deleteUserResponse{})
}

// --- access keys ---

type accessKeyXML struct {
	UserName        string `xml:"UserName"`
	AccessKeyId     string `xml:"AccessKeyId"`
	SecretAccessKey string `xml:"SecretAccessKey,omitempty"`
	Status          string `xml:"Status"`
	CreateDate      string `xml:"CreateDate"`
}

type createAccessKeyResponse struct {
	XMLName xml.Name              `xml:"CreateAccessKeyResponse"`
	Result  createAccessKeyResult `xml:"CreateAccessKeyResult"`
}
type createAccessKeyResult struct {
	AccessKey accessKeyXML `xml:"AccessKey"`
}

func (s *Service) createAccessKey(w http.ResponseWriter, form map[string]string) {
	userName := form["UserName"]
	if userName == "" {
		server.WriteXMLError(w, http.StatusBadRequest, "ValidationError", "UserName es requerido")
		return
	}
	if found, _ := s.db.Get(usersBucket, userName, &User{}); !found {
		server.WriteXMLError(w, http.StatusNotFound, "NoSuchEntity", "el usuario no existe: "+userName)
		return
	}
	k := AccessKey{
		UserName:        userName,
		AccessKeyId:     randomID("AKIA", 8),
		SecretAccessKey: randomID("", 20),
		Status:          "Active",
		CreateDate:      time.Now().UTC(),
	}
	if err := s.db.Put(accessKeysBucket, accessKeyKey(userName, k.AccessKeyId), k); err != nil {
		server.WriteXMLError(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	server.WriteXML(w, http.StatusOK, createAccessKeyResponse{Result: createAccessKeyResult{AccessKey: accessKeyXML{
		UserName:        k.UserName,
		AccessKeyId:     k.AccessKeyId,
		SecretAccessKey: k.SecretAccessKey,
		Status:          k.Status,
		CreateDate:      k.CreateDate.Format(time.RFC3339),
	}}})
}

type listAccessKeysResponse struct {
	XMLName xml.Name             `xml:"ListAccessKeysResponse"`
	Result  listAccessKeysResult `xml:"ListAccessKeysResult"`
}
type listAccessKeysResult struct {
	AccessKeyMetadata []accessKeyXML `xml:"AccessKeyMetadata>member"`
	IsTruncated       bool           `xml:"IsTruncated"`
}

func (s *Service) listAccessKeys(w http.ResponseWriter, form map[string]string) {
	userName := form["UserName"]
	var out []accessKeyXML
	_ = s.db.List(accessKeysBucket, userName+"/", func(_ string, raw []byte) error {
		var k AccessKey
		if err := json.Unmarshal(raw, &k); err == nil {
			out = append(out, accessKeyXML{
				UserName:    k.UserName,
				AccessKeyId: k.AccessKeyId,
				Status:      k.Status,
				CreateDate:  k.CreateDate.Format(time.RFC3339),
			})
		}
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return out[i].AccessKeyId < out[j].AccessKeyId })
	server.WriteXML(w, http.StatusOK, listAccessKeysResponse{Result: listAccessKeysResult{AccessKeyMetadata: out}})
}

type deleteAccessKeyResponse struct {
	XMLName xml.Name `xml:"DeleteAccessKeyResponse"`
}

func (s *Service) deleteAccessKey(w http.ResponseWriter, form map[string]string) {
	userName, keyID := form["UserName"], form["AccessKeyId"]
	if err := s.db.Delete(accessKeysBucket, accessKeyKey(userName, keyID)); err != nil {
		server.WriteXMLError(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	server.WriteXML(w, http.StatusOK, deleteAccessKeyResponse{})
}
