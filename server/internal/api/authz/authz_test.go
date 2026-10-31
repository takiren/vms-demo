package authz_test

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"

	"github.com/takiren/vms-demo/server/internal/api/authz"
	"github.com/takiren/vms-demo/server/internal/api/gen"
	"github.com/takiren/vms-demo/server/internal/auth"
)

// expected is the decided policy of every operation in api/openapi.yaml.
// Adding an operation without adding it here fails the test, so every
// operation's access rule is reviewed.
var expected = map[string]string{
	"getHealthz":        "public",
	"login":             "public",
	"logout":            "public",
	"getMe":             "viewer",
	"changeOwnPassword": "viewer",
	"listUsers":         "admin",
	"createUser":        "admin",
	"getUser":           "admin",
	"updateUser":        "admin",
	"deleteUser":        "admin",
	"listCameras":       "viewer",
	"createCamera":      "admin",
	"getCamera":         "admin",
	"updateCamera":      "admin",
	"deleteCamera":      "admin",

	"mediamtxSegmentComplete": "hookToken",
}

func TestEverySpecOperationHasADecidedPolicy(t *testing.T) {
	expectedByName := map[string]string{}
	for op, p := range expected {
		expectedByName[authz.StrictName(op)] = p
	}
	spec, err := gen.GetSpec()
	if err != nil {
		t.Fatal(err)
	}
	table, err := authz.Build(spec, "/api")
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	seen := map[string]bool{}
	for route, p := range table.Policies() {
		got := string(p.MinRole)
		if p.Public {
			got = "public"
		}
		if p.HookToken {
			got = "hookToken"
		}
		want, ok := expectedByName[authz.StrictName(p.OperationID)]
		if !ok {
			t.Errorf("%s (%s) has no decided policy in this test: add it to expected", route, p.OperationID)
			continue
		}
		if got != want {
			t.Errorf("%s (%s): policy %s, want %s", route, p.OperationID, got, want)
		}
		seen[authz.StrictName(p.OperationID)] = true

		byOp, ok := table.ForOperation(authz.StrictName(p.OperationID))
		if !ok || byOp != p {
			t.Errorf("%s: ForOperation(%s) = %+v, %v", route, authz.StrictName(p.OperationID), byOp, ok)
		}
	}
	for op := range expected {
		if !seen[authz.StrictName(op)] {
			t.Errorf("operation %s is expected but not in the spec", op)
		}
	}

	// Public operations must be the ones we intend: anything sensitive
	// added by mistake shows up here.
	if p, ok := table.ForRoute(http.MethodGet, "/api/users/:id"); !ok || p.MinRole != auth.RoleAdmin {
		t.Errorf("route lookup with Echo path params: %+v, %v", p, ok)
	}
}

func TestCheck(t *testing.T) {
	admin := &auth.User{Role: auth.RoleAdmin}
	viewer := &auth.User{Role: auth.RoleViewer}
	odd := &auth.User{Role: "root"}
	tests := []struct {
		p    authz.Policy
		u    *auth.User
		want error
	}{
		{authz.Policy{Public: true}, nil, nil},
		{authz.Policy{Public: true}, viewer, nil},
		{authz.Policy{MinRole: auth.RoleViewer}, nil, authz.ErrUnauthenticated},
		{authz.Policy{MinRole: auth.RoleViewer}, viewer, nil},
		{authz.Policy{MinRole: auth.RoleViewer}, admin, nil},
		{authz.Policy{MinRole: auth.RoleAdmin}, viewer, authz.ErrForbidden},
		{authz.Policy{MinRole: auth.RoleAdmin}, admin, nil},
		{authz.Policy{MinRole: auth.RoleAdmin}, nil, authz.ErrUnauthenticated},
		{authz.Policy{MinRole: auth.RoleViewer}, odd, authz.ErrForbidden},
		{authz.Policy{}, admin, authz.ErrForbidden}, // zero policy denies
		// Sessions never satisfy the hook token scheme.
		{authz.Policy{HookToken: true}, admin, authz.ErrUnauthenticated},
		{authz.Policy{HookToken: true}, nil, authz.ErrUnauthenticated},
	}
	for i, tt := range tests {
		if err := tt.p.Check(tt.u); !errors.Is(err, tt.want) {
			t.Errorf("%d: Check = %v, want %v", i, err, tt.want)
		}
	}
	if authz.StatusFor(authz.ErrUnauthenticated) != http.StatusUnauthorized ||
		authz.StatusFor(authz.ErrForbidden) != http.StatusForbidden ||
		authz.StatusFor(errors.New("x")) != http.StatusForbidden {
		t.Error("StatusFor is wrong")
	}
}

const specHeader = `openapi: 3.0.3
info: {title: t, version: "1"}
security:
  - cookieAuth: []
components:
  securitySchemes:
    cookieAuth: {type: apiKey, in: cookie, name: s}
    other: {type: apiKey, in: header, name: X-Key}
    hookToken: {type: apiKey, in: header, name: X-VMS-Hook-Token}
paths:
  /x:
    get:
      responses: {"200": {description: ok}}
`

func build(t *testing.T, op string) error {
	t.Helper()
	return buildAt(t, "/x", op)
}

func buildAt(t *testing.T, path, op string) error {
	t.Helper()
	spec := strings.Replace(specHeader, "  /x:", "  "+path+":", 1)
	doc, err := openapi3.NewLoader().LoadFromData([]byte(strings.Replace(spec, "      responses:", op+"\n      responses:", 1)))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	_, err = authz.Build(doc, "/api")
	return err
}

func TestBuildRejectsUndecidedOperations(t *testing.T) {
	tests := []struct {
		name, op string
		ok       bool
	}{
		{"default security with role", "      operationId: getX\n      x-min-role: viewer", true},
		{"default security without role", "      operationId: getX", false},
		{"unknown role", "      operationId: getX\n      x-min-role: root", false},
		{"non-string role", "      operationId: getX\n      x-min-role: 1", false},
		{"public", "      operationId: getX\n      security: []", true},
		{"public with role", "      operationId: getX\n      security: []\n      x-min-role: admin", false},
		{"other scheme", "      operationId: getX\n      security: [{other: []}]\n      x-min-role: admin", false},
		{"alternative schemes", "      operationId: getX\n      security: [{cookieAuth: []}, {other: []}]\n      x-min-role: admin", false},
		{"combined schemes", "      operationId: getX\n      security: [{cookieAuth: [], other: []}]\n      x-min-role: admin", false},
		{"scopes", "      operationId: getX\n      security: [{cookieAuth: [a]}]\n      x-min-role: admin", false},
		{"missing operationId", "      x-min-role: admin", false},
		{"non-camel operationId", "      operationId: get-x\n      x-min-role: admin", false},
		{"upper camel operationId", "      operationId: GetX\n      x-min-role: admin", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := build(t, tt.op)
			if (err == nil) != tt.ok {
				t.Errorf("Build = %v, want ok=%v", err, tt.ok)
			}
		})
	}
}

func TestBuildInternalOperations(t *testing.T) {
	tests := []struct {
		name, path, op string
		ok             bool
	}{
		{"hook token under internal", "/internal/hook", "      operationId: hook\n      security: [{hookToken: []}]", true},
		{"hook token outside internal", "/hook", "      operationId: hook\n      security: [{hookToken: []}]", false},
		{"hook token with role", "/internal/hook", "      operationId: hook\n      security: [{hookToken: []}]\n      x-min-role: admin", false},
		{"hook token with scopes", "/internal/hook", "      operationId: hook\n      security: [{hookToken: [a]}]", false},
		{"hook token or cookie", "/internal/hook", "      operationId: hook\n      security: [{hookToken: []}, {cookieAuth: []}]", false},
		{"cookie under internal", "/internal/hook", "      operationId: hook\n      x-min-role: admin", false},
		{"public under internal", "/internal/hook", "      operationId: hook\n      security: []", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := buildAt(t, tt.path, tt.op)
			if (err == nil) != tt.ok {
				t.Errorf("Build = %v, want ok=%v", err, tt.ok)
			}
		})
	}
}

func TestStrictName(t *testing.T) {
	if authz.StrictName("getHealthz") != "GetHealthz" || authz.StrictName("") != "" {
		t.Error("StrictName is wrong")
	}
}
