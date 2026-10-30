// Package auth implements users, password hashing, sessions and the login
// rate limit. The design is in docs/design.md「認証」.
//
// HTTP concerns (cookies on requests, CSRF, authorization per operation) live
// in internal/api; this package only provides the building blocks and the
// database-backed Service.
package auth

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// Role is a user's role.
type Role string

// Roles. admin can do everything; viewer can only view.
const (
	RoleAdmin  Role = "admin"
	RoleViewer Role = "viewer"
)

// Valid reports whether r is a known role.
func (r Role) Valid() bool { return r == RoleAdmin || r == RoleViewer }

// Satisfies reports whether a user with role r may perform an operation
// that requires at least min. Unknown roles satisfy nothing.
func (r Role) Satisfies(minRole Role) bool {
	switch minRole {
	case RoleViewer:
		return r == RoleAdmin || r == RoleViewer
	case RoleAdmin:
		return r == RoleAdmin
	default:
		return false
	}
}

// User is a user without the password hash. Nothing that leaves this package
// carries the hash.
type User struct {
	ID        uuid.UUID
	Username  string
	Role      Role
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Principal is the authenticated caller of a request: the user and the
// session the request came with.
type Principal struct {
	User      User
	SessionID uuid.UUID
}

type principalKey struct{}

// WithPrincipal returns a context that carries p.
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, p)
}

// PrincipalFrom returns the authenticated caller, if any.
func PrincipalFrom(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(principalKey{}).(Principal)
	return p, ok
}

// UserFrom returns the logged-in user, if any. The authentication middleware
// puts it into the request context.
func UserFrom(ctx context.Context) (User, bool) {
	p, ok := PrincipalFrom(ctx)
	return p.User, ok
}
