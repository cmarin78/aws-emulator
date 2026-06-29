package iam

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cesarmarin/aws-emulator/internal/accountctx"
	"github.com/cesarmarin/aws-emulator/internal/storage"
)

func newTestService(t *testing.T) *Service {
	t.Helper()
	db, err := storage.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return New(db)
}

func actionRequest(action string, form url.Values) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/?Action="+action, strings.NewReader(""))
	q := r.URL.Query()
	for k, v := range form {
		for _, val := range v {
			q.Add(k, val)
		}
	}
	r.URL.RawQuery = q.Encode()
	return r
}

// withDefaultIdentity simula lo que dejaría accountctx.Middleware -- los
// handlers de IAM leen accountID vía accountctx.FromContext, así que sin
// esto siempre verían el default igual, pero lo dejamos explícito para
// que el test sea legible y resistente a futuros cambios de default.
func withDefaultIdentity(r *http.Request) *http.Request {
	ctx := context.Background()
	return r.WithContext(ctx)
}

func doAction(svc *Service, action string, form url.Values) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	svc.ServeHTTP(w, withDefaultIdentity(actionRequest(action, form)))
	return w
}

func TestCreateAndGetRole(t *testing.T) {
	svc := newTestService(t)

	w := doAction(svc, "CreateRole", url.Values{
		"RoleName":                 {"my-role"},
		"AssumeRolePolicyDocument": {`{"Version":"2012-10-17"}`},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("CreateRole: status = %d, body = %s", w.Code, w.Body.String())
	}
	if want := accountctx.DefaultAccountID; !strings.Contains(w.Body.String(), "arn:aws:iam::"+want+":role/my-role") {
		t.Fatalf("CreateRole: esperaba ver el ARN del rol, body = %s", w.Body.String())
	}

	w = doAction(svc, "GetRole", url.Values{"RoleName": {"my-role"}})
	if !strings.Contains(w.Body.String(), "<RoleName>my-role</RoleName>") {
		t.Fatalf("GetRole: status = %d, body = %s", w.Code, w.Body.String())
	}
}

func TestGetRole_NotFound(t *testing.T) {
	svc := newTestService(t)
	w := doAction(svc, "GetRole", url.Values{"RoleName": {"nope"}})
	if w.Code != http.StatusNotFound {
		t.Fatalf("GetRole inexistente: status = %d, esperaba 404", w.Code)
	}
}

func TestListRoles_SortedByName(t *testing.T) {
	svc := newTestService(t)
	doAction(svc, "CreateRole", url.Values{"RoleName": {"zeta"}})
	doAction(svc, "CreateRole", url.Values{"RoleName": {"alpha"}})

	w := doAction(svc, "ListRoles", nil)
	body := w.Body.String()
	if strings.Index(body, "alpha") > strings.Index(body, "zeta") {
		t.Fatalf("ListRoles: esperaba orden alfabético, body = %s", body)
	}
}

func TestDeleteRole(t *testing.T) {
	svc := newTestService(t)
	doAction(svc, "CreateRole", url.Values{"RoleName": {"r1"}})

	w := doAction(svc, "DeleteRole", url.Values{"RoleName": {"r1"}})
	if w.Code != http.StatusOK {
		t.Fatalf("DeleteRole: status = %d", w.Code)
	}

	w = doAction(svc, "GetRole", url.Values{"RoleName": {"r1"}})
	if w.Code != http.StatusNotFound {
		t.Fatalf("GetRole tras DeleteRole: status = %d, esperaba 404", w.Code)
	}
}

func TestDeleteRole_NotFound(t *testing.T) {
	svc := newTestService(t)
	w := doAction(svc, "DeleteRole", url.Values{"RoleName": {"nope"}})
	if w.Code != http.StatusNotFound {
		t.Fatalf("DeleteRole inexistente: status = %d, esperaba 404", w.Code)
	}
}

func TestRolePolicy_PutGetListDelete(t *testing.T) {
	svc := newTestService(t)
	doAction(svc, "CreateRole", url.Values{"RoleName": {"r1"}})

	w := doAction(svc, "PutRolePolicy", url.Values{
		"RoleName":       {"r1"},
		"PolicyName":     {"p1"},
		"PolicyDocument": {`{"Statement":[]}`},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("PutRolePolicy: status = %d, body = %s", w.Code, w.Body.String())
	}

	w = doAction(svc, "GetRolePolicy", url.Values{"RoleName": {"r1"}, "PolicyName": {"p1"}})
	if !strings.Contains(w.Body.String(), "<PolicyName>p1</PolicyName>") {
		t.Fatalf("GetRolePolicy: body = %s", w.Body.String())
	}

	w = doAction(svc, "ListRolePolicies", url.Values{"RoleName": {"r1"}})
	if !strings.Contains(w.Body.String(), "p1") {
		t.Fatalf("ListRolePolicies: esperaba ver p1, body = %s", w.Body.String())
	}

	w = doAction(svc, "DeleteRolePolicy", url.Values{"RoleName": {"r1"}, "PolicyName": {"p1"}})
	if w.Code != http.StatusOK {
		t.Fatalf("DeleteRolePolicy: status = %d", w.Code)
	}
	w = doAction(svc, "GetRolePolicy", url.Values{"RoleName": {"r1"}, "PolicyName": {"p1"}})
	if w.Code != http.StatusNotFound {
		t.Fatalf("GetRolePolicy tras delete: status = %d, esperaba 404", w.Code)
	}
}

func TestPutRolePolicy_RoleMustExist(t *testing.T) {
	svc := newTestService(t)
	w := doAction(svc, "PutRolePolicy", url.Values{"RoleName": {"nope"}, "PolicyName": {"p1"}})
	if w.Code != http.StatusNotFound {
		t.Fatalf("PutRolePolicy sobre rol inexistente: status = %d, esperaba 404", w.Code)
	}
}

// TestListAttachedRolePolicies_AlwaysEmpty y
// TestListInstanceProfilesForRole_AlwaysEmpty cubren los dos stubs que el
// provider de Terraform necesita durante el ciclo de vida de un rol (Read
// y Delete respectivamente) -- ver comentarios en iam.go.
func TestListAttachedRolePolicies_AlwaysEmpty(t *testing.T) {
	svc := newTestService(t)
	w := doAction(svc, "ListAttachedRolePolicies", url.Values{"RoleName": {"whatever"}})
	if w.Code != http.StatusOK || strings.Contains(w.Body.String(), "<member>") {
		t.Fatalf("ListAttachedRolePolicies: status = %d, body = %s", w.Code, w.Body.String())
	}
}

func TestListInstanceProfilesForRole_AlwaysEmpty(t *testing.T) {
	svc := newTestService(t)
	w := doAction(svc, "ListInstanceProfilesForRole", url.Values{"RoleName": {"whatever"}})
	if w.Code != http.StatusOK || strings.Contains(w.Body.String(), "<member>") {
		t.Fatalf("ListInstanceProfilesForRole: status = %d, body = %s", w.Code, w.Body.String())
	}
}

func TestCreateAndGetUser(t *testing.T) {
	svc := newTestService(t)
	w := doAction(svc, "CreateUser", url.Values{"UserName": {"alice"}})
	if w.Code != http.StatusOK {
		t.Fatalf("CreateUser: status = %d, body = %s", w.Code, w.Body.String())
	}

	w = doAction(svc, "GetUser", url.Values{"UserName": {"alice"}})
	if !strings.Contains(w.Body.String(), "<UserName>alice</UserName>") {
		t.Fatalf("GetUser: body = %s", w.Body.String())
	}
}

func TestListUsers_SortedByName(t *testing.T) {
	svc := newTestService(t)
	doAction(svc, "CreateUser", url.Values{"UserName": {"zeta"}})
	doAction(svc, "CreateUser", url.Values{"UserName": {"alpha"}})

	w := doAction(svc, "ListUsers", nil)
	body := w.Body.String()
	if strings.Index(body, "alpha") > strings.Index(body, "zeta") {
		t.Fatalf("ListUsers: esperaba orden alfabético, body = %s", body)
	}
}

func TestDeleteUser_AlsoRemovesAccessKeys(t *testing.T) {
	svc := newTestService(t)
	doAction(svc, "CreateUser", url.Values{"UserName": {"alice"}})
	doAction(svc, "CreateAccessKey", url.Values{"UserName": {"alice"}})

	w := doAction(svc, "DeleteUser", url.Values{"UserName": {"alice"}})
	if w.Code != http.StatusOK {
		t.Fatalf("DeleteUser: status = %d, body = %s", w.Code, w.Body.String())
	}

	w = doAction(svc, "ListAccessKeys", url.Values{"UserName": {"alice"}})
	if strings.Contains(w.Body.String(), "<AccessKeyId>") {
		t.Fatalf("ListAccessKeys tras DeleteUser: esperaba ninguna key, body = %s", w.Body.String())
	}
}

func TestDeleteUser_NotFound(t *testing.T) {
	svc := newTestService(t)
	w := doAction(svc, "DeleteUser", url.Values{"UserName": {"nope"}})
	if w.Code != http.StatusNotFound {
		t.Fatalf("DeleteUser inexistente: status = %d, esperaba 404", w.Code)
	}
}

func TestAccessKey_CreateListDelete(t *testing.T) {
	svc := newTestService(t)
	doAction(svc, "CreateUser", url.Values{"UserName": {"alice"}})

	w := doAction(svc, "CreateAccessKey", url.Values{"UserName": {"alice"}})
	if w.Code != http.StatusOK {
		t.Fatalf("CreateAccessKey: status = %d, body = %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "<AccessKeyId>AKIA") {
		t.Fatalf("CreateAccessKey: esperaba un AccessKeyId con prefijo AKIA, body = %s", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "<SecretAccessKey>") {
		t.Fatalf("CreateAccessKey: esperaba SecretAccessKey en la respuesta, body = %s", w.Body.String())
	}
	keyID := extractTag(w.Body.String(), "AccessKeyId")

	w = doAction(svc, "ListAccessKeys", url.Values{"UserName": {"alice"}})
	if !strings.Contains(w.Body.String(), keyID) {
		t.Fatalf("ListAccessKeys: esperaba ver %q, body = %s", keyID, w.Body.String())
	}
	// ListAccessKeys no debe devolver el secreto.
	if strings.Contains(w.Body.String(), "<SecretAccessKey>") {
		t.Fatalf("ListAccessKeys: no debería incluir SecretAccessKey, body = %s", w.Body.String())
	}

	w = doAction(svc, "DeleteAccessKey", url.Values{"UserName": {"alice"}, "AccessKeyId": {keyID}})
	if w.Code != http.StatusOK {
		t.Fatalf("DeleteAccessKey: status = %d", w.Code)
	}
	w = doAction(svc, "ListAccessKeys", url.Values{"UserName": {"alice"}})
	if strings.Contains(w.Body.String(), keyID) {
		t.Fatalf("ListAccessKeys tras delete: no debería ver %q, body = %s", keyID, w.Body.String())
	}
}

func TestCreateAccessKey_UserMustExist(t *testing.T) {
	svc := newTestService(t)
	w := doAction(svc, "CreateAccessKey", url.Values{"UserName": {"nope"}})
	if w.Code != http.StatusNotFound {
		t.Fatalf("CreateAccessKey sobre usuario inexistente: status = %d, esperaba 404", w.Code)
	}
}

func TestReset_ClearsAllIAMState(t *testing.T) {
	svc := newTestService(t)
	doAction(svc, "CreateRole", url.Values{"RoleName": {"r1"}})
	doAction(svc, "CreateUser", url.Values{"UserName": {"alice"}})

	if err := svc.Reset(); err != nil {
		t.Fatalf("Reset: %v", err)
	}

	w := doAction(svc, "ListRoles", nil)
	if strings.Contains(w.Body.String(), "<RoleName>") {
		t.Fatalf("ListRoles tras Reset: esperaba vacío, body = %s", w.Body.String())
	}
	w = doAction(svc, "ListUsers", nil)
	if strings.Contains(w.Body.String(), "<UserName>") {
		t.Fatalf("ListUsers tras Reset: esperaba vacío, body = %s", w.Body.String())
	}
}

func extractTag(body, tag string) string {
	open := "<" + tag + ">"
	close := "</" + tag + ">"
	start := strings.Index(body, open)
	if start == -1 {
		return ""
	}
	start += len(open)
	end := strings.Index(body[start:], close)
	if end == -1 {
		return ""
	}
	return body[start : start+end]
}
