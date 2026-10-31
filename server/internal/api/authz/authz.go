// Package authz builds the per-operation authorization policy from the
// OpenAPI document and checks requests against it.
//
// Rules (docs/design.md「認可」), all checked when the table is built so a
// mistake in api/openapi.yaml stops the server from starting:
//
//   - The effective security of an operation is its own `security`, or the
//     document's when the operation has none. The document default is
//     cookieAuth, so operations are authenticated unless they opt out.
//   - `security: []` makes an operation public. It must not have x-min-role.
//   - `security: [cookieAuth: []]` requires a session and an explicit
//     `x-min-role` of admin or viewer (viewer = any logged-in user).
//   - Internal schemes (InternalSchemes: `mediamtxAuth` for the MediaMTX
//     auth callback, `hookToken` for the segment-complete hook) are for server-to-server operations: only
//     operations under /internal/ may use them, with no scopes and without
//     x-min-role. Sessions never satisfy them; the middleware checks the
//     scheme's own credentials.
//   - Every operation under /internal/ (no CSRF check) must use exactly one
//     internal scheme: never cookieAuth, never public.
//   - Any other security requirement is rejected.
//
// Operations missing from the table are denied.
package authz

import (
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/getkin/kin-openapi/openapi3"

	"github.com/takiren/vms-demo/server/internal/auth"
)

// SchemeCookie is the security scheme name for the session cookie.
const SchemeCookie = "cookieAuth"

// SchemeHookToken is the security scheme name for the MediaMTX hook's
// shared secret (X-VMS-Hook-Token).
const SchemeHookToken = "hookToken"

// SchemeMediaMTX is the security scheme of the MediaMTX auth callback
// (Basic auth with VMS_MEDIAMTX_AUTH_SECRET).
const SchemeMediaMTX = "mediamtxAuth"

// InternalSchemes are the server-to-server security schemes. Only
// operations under InternalPrefix may use them, and those operations must
// use exactly one of them. Add a scheme here (and its check in the
// middleware) to allow it.
var InternalSchemes = map[string]bool{
	SchemeMediaMTX:  true,
	SchemeHookToken: true,
}

// InternalPrefix is the path prefix (relative to the base path) of the
// server-to-server operations.
const InternalPrefix = "/internal/"

// ExtMinRole is the operation extension naming the minimum role.
const ExtMinRole = "x-min-role"

// Policy is the access rule of one operation.
type Policy struct {
	// OperationID is the operationId in the OpenAPI document (as loaded;
	// the spec embedded by oapi-codegen has it upper-cased).
	OperationID string
	// Public operations need no session.
	Public bool
	// MinRole is the least role allowed (for cookie operations).
	MinRole auth.Role
	// Scheme is the security scheme (SchemeCookie or one of
	// InternalSchemes); empty for public operations.
	Scheme string
}

// Internal reports whether the operation uses an internal scheme. Sessions
// never satisfy it: the middleware checks the scheme's credentials instead.
func (p Policy) Internal() bool { return InternalSchemes[p.Scheme] }

// MediaMTX reports whether the operation is the MediaMTX auth callback.
func (p Policy) MediaMTX() bool { return p.Scheme == SchemeMediaMTX }

// Errors returned by Check.
var (
	ErrUnauthenticated = errors.New("authentication required")
	ErrForbidden       = errors.New("insufficient role")
)

// Check reports whether a caller (nil when not logged in) may use the
// operation.
func (p Policy) Check(u *auth.User) error {
	if p.Public {
		return nil
	}
	if p.Internal() || u == nil {
		return ErrUnauthenticated
	}
	if !u.Role.Satisfies(p.MinRole) {
		return ErrForbidden
	}
	return nil
}

// Table holds the policy of every operation.
type Table struct {
	byRoute     map[string]Policy // "METHOD /api/path/:param" as Echo registers it
	byOperation map[string]Policy // generated (strict-server) operation name
}

var (
	operationIDPattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9]*$`)
	pathParamPattern   = regexp.MustCompile(`\{([^}/]+)\}`)
)

// Build derives the table from doc. basePath is the prefix routes are
// registered under (the servers entry, "/api").
func Build(doc *openapi3.T, basePath string) (*Table, error) {
	t := &Table{byRoute: map[string]Policy{}, byOperation: map[string]Policy{}}
	var errs []error
	for path, item := range doc.Paths.Map() {
		for method, op := range item.Operations() {
			p, err := policyFor(doc, path, op)
			if err != nil {
				errs = append(errs, fmt.Errorf("%s %s: %w", method, path, err))
				continue
			}
			route := RouteKey(method, basePath+pathParamPattern.ReplaceAllString(path, ":$1"))
			t.byRoute[route] = p
			t.byOperation[StrictName(op.OperationID)] = p
		}
	}
	if len(errs) > 0 {
		return nil, fmt.Errorf("authz: %w", errors.Join(errs...))
	}
	return t, nil
}

func policyFor(doc *openapi3.T, path string, op *openapi3.Operation) (Policy, error) {
	if !operationIDPattern.MatchString(op.OperationID) {
		return Policy{}, fmt.Errorf("operationId %q must be camelCase letters and digits", op.OperationID)
	}
	p := Policy{OperationID: op.OperationID}

	security := doc.Security
	if op.Security != nil {
		security = *op.Security
	}
	rawRole, hasRole := op.Extensions[ExtMinRole]
	internal := strings.HasPrefix(path, InternalPrefix)

	switch {
	case len(security) == 0:
		if internal {
			return Policy{}, fmt.Errorf("operation %s: %s operations must use an internal scheme, not be public", op.OperationID, InternalPrefix)
		}
		if hasRole {
			return Policy{}, fmt.Errorf("public operation %s must not have %s", op.OperationID, ExtMinRole)
		}
		p.Public = true
		return p, nil
	case len(security) == 1 && len(security[0]) == 1:
		scheme, scopes := onlyScheme(security[0])
		if InternalSchemes[scheme] {
			switch {
			case len(scopes) != 0:
				return Policy{}, fmt.Errorf("operation %s: %s takes no scopes", op.OperationID, scheme)
			case !internal:
				return Policy{}, fmt.Errorf("operation %s: %s is only for %s operations", op.OperationID, scheme, InternalPrefix)
			case hasRole:
				return Policy{}, fmt.Errorf("operation %s: %s operations must not have %s", op.OperationID, scheme, ExtMinRole)
			}
			p.Scheme = scheme
			return p, nil
		}
		if scheme != SchemeCookie || len(scopes) != 0 {
			return Policy{}, fmt.Errorf("operation %s: only %s or an internal scheme without scopes is supported", op.OperationID, SchemeCookie)
		}
		if internal {
			// /internal/ skips the CSRF check, so a session must not work there.
			return Policy{}, fmt.Errorf("operation %s: %s operations must not use %s", op.OperationID, InternalPrefix, SchemeCookie)
		}
	default:
		return Policy{}, fmt.Errorf("operation %s: unsupported security requirement", op.OperationID)
	}

	role, _ := rawRole.(string)
	if !hasRole || !auth.Role(role).Valid() {
		return Policy{}, fmt.Errorf("authenticated operation %s needs %s: admin or viewer", op.OperationID, ExtMinRole)
	}
	p.MinRole = auth.Role(role)
	p.Scheme = SchemeCookie
	return p, nil
}

// onlyScheme returns the scheme of a requirement with exactly one scheme.
func onlyScheme(req openapi3.SecurityRequirement) (string, []string) {
	for name, scopes := range req {
		return name, scopes
	}
	return "", nil
}

// RouteKey is the lookup key for an Echo route.
func RouteKey(method, path string) string { return strings.ToUpper(method) + " " + path }

// StrictName is the operation name oapi-codegen passes to strict-server
// middleware: the operationId with its first letter upper-cased (Build only
// accepts camelCase IDs of letters and digits, for which this is exact).
// The spec embedded by oapi-codegen already has upper-cased IDs.
func StrictName(operationID string) string {
	if operationID == "" {
		return ""
	}
	return strings.ToUpper(operationID[:1]) + operationID[1:]
}

// ForRoute returns the policy of the Echo route (method and registered
// path pattern).
func (t *Table) ForRoute(method, path string) (Policy, bool) {
	p, ok := t.byRoute[RouteKey(method, path)]
	return p, ok
}

// ForOperation returns the policy by strict-server operation name.
func (t *Table) ForOperation(name string) (Policy, bool) {
	p, ok := t.byOperation[name]
	return p, ok
}

// Policies returns every policy, keyed by route.
func (t *Table) Policies() map[string]Policy {
	out := make(map[string]Policy, len(t.byRoute))
	for k, v := range t.byRoute {
		out[k] = v
	}
	return out
}

// StatusFor maps a Check error to an HTTP status.
func StatusFor(err error) int {
	switch {
	case errors.Is(err, ErrUnauthenticated):
		return http.StatusUnauthorized
	case errors.Is(err, ErrForbidden):
		return http.StatusForbidden
	default:
		return http.StatusForbidden
	}
}
