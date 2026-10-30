package api

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/labstack/echo/v5"

	"github.com/takiren/vms-demo/server/internal/api/authz"
	"github.com/takiren/vms-demo/server/internal/api/gen"
	"github.com/takiren/vms-demo/server/internal/auth"
)

func policyTable(t *testing.T) *authz.Table {
	t.Helper()
	spec, err := gen.GetSpec()
	if err != nil {
		t.Fatal(err)
	}
	table, err := authz.Build(spec, BasePath)
	if err != nil {
		t.Fatal(err)
	}
	return table
}

// Every generated strict handler method must have a policy by the name
// strict-server middleware receives, or strictAuthorize denies it.
func TestEveryStrictOperationHasAPolicy(t *testing.T) {
	table := policyTable(t)
	iface := reflect.TypeFor[gen.StrictServerInterface]()
	for i := range iface.NumMethod() {
		name := iface.Method(i).Name
		if _, ok := table.ForOperation(name); !ok {
			t.Errorf("strict operation %s has no policy", name)
		}
	}
}

func TestStrictAuthorize(t *testing.T) {
	table := policyTable(t)
	called := false
	handler := func(*echo.Context, any) (any, error) { called = true; return "ok", nil }
	run := func(op string, u *auth.User) error {
		called = false
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		if u != nil {
			req = req.WithContext(auth.WithPrincipal(req.Context(), auth.Principal{User: *u}))
		}
		c := echo.New().NewContext(req, httptest.NewRecorder())
		_, err := strictAuthorize(table)(handler, op)(c, nil)
		return err
	}
	admin := &auth.User{Role: auth.RoleAdmin}
	viewer := &auth.User{Role: auth.RoleViewer}

	if err := run("NotInTheSpec", admin); err == nil || called {
		t.Error("unknown operation was allowed")
	}
	if err := run("DeleteUser", viewer); echo.StatusCode(err) != http.StatusForbidden || called {
		t.Errorf("viewer DeleteUser: %v", err)
	}
	if err := run("DeleteUser", nil); echo.StatusCode(err) != http.StatusUnauthorized || called {
		t.Errorf("anonymous DeleteUser: %v", err)
	}
	if err := run("DeleteUser", admin); err != nil || !called {
		t.Errorf("admin DeleteUser: %v", err)
	}
	if err := run("GetHealthz", nil); err != nil || !called {
		t.Errorf("anonymous GetHealthz: %v", err)
	}
}

func TestAuthErrorMapping(t *testing.T) {
	tests := []struct {
		err    error
		status int
	}{
		{auth.ErrInvalidCredentials, 401},
		{auth.ErrUnauthenticated, 401},
		{auth.ErrWrongPassword, 400},
		{auth.ErrWeakPassword, 400},
		{auth.ErrInvalidUsername, 400},
		{auth.ErrInvalidRole, 400},
		{auth.ErrUserNotFound, 404},
		{auth.ErrUsernameTaken, 409},
		{auth.ErrLastAdmin, 409},
		{auth.ErrDeleteSelf, 409},
		{auth.ErrBusy, 503},
		{errors.New("boom"), 500},
	}
	for _, tt := range tests {
		if got := echo.StatusCode(authError(tt.err)); got != tt.status {
			t.Errorf("authError(%v) status = %d, want %d", tt.err, got, tt.status)
		}
	}
}
