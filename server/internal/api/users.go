package api

import (
	"context"

	"github.com/takiren/vms-demo/server/internal/api/gen"
	"github.com/takiren/vms-demo/server/internal/auth"
)

// User management. Only admins reach these handlers: the role requirement
// (x-min-role: admin) is enforced by the authorization middleware.

// ListUsers returns all users.
func (h *Handlers) ListUsers(ctx context.Context, _ gen.ListUsersRequestObject) (gen.ListUsersResponseObject, error) {
	users, err := h.auth.ListUsers(ctx)
	if err != nil {
		return nil, authError(err)
	}
	out := gen.UserList{Users: make([]gen.User, len(users))}
	for i, u := range users {
		out.Users[i] = toAPIUser(u)
	}
	return gen.ListUsers200JSONResponse(out), nil
}

// CreateUser creates a user.
func (h *Handlers) CreateUser(ctx context.Context, req gen.CreateUserRequestObject) (gen.CreateUserResponseObject, error) {
	u, err := h.auth.CreateUser(ctx, req.Body.Username, req.Body.Password, auth.Role(req.Body.Role))
	if err != nil {
		return nil, authError(err)
	}
	return gen.CreateUser201JSONResponse(toAPIUser(u)), nil
}

// GetUser returns one user.
func (h *Handlers) GetUser(ctx context.Context, req gen.GetUserRequestObject) (gen.GetUserResponseObject, error) {
	u, err := h.auth.GetUser(ctx, req.Id)
	if err != nil {
		return nil, authError(err)
	}
	return gen.GetUser200JSONResponse(toAPIUser(u)), nil
}

// UpdateUser changes a user's role and/or password.
func (h *Handlers) UpdateUser(ctx context.Context, req gen.UpdateUserRequestObject) (gen.UpdateUserResponseObject, error) {
	var upd auth.UserUpdate
	if req.Body.Role != nil {
		r := auth.Role(*req.Body.Role)
		upd.Role = &r
	}
	upd.Password = req.Body.Password
	u, err := h.auth.UpdateUser(ctx, req.Id, upd)
	if err != nil {
		return nil, authError(err)
	}
	return gen.UpdateUser200JSONResponse(toAPIUser(u)), nil
}

// DeleteUser deletes a user other than the caller.
func (h *Handlers) DeleteUser(ctx context.Context, req gen.DeleteUserRequestObject) (gen.DeleteUserResponseObject, error) {
	p, err := principal(ctx)
	if err != nil {
		return nil, err
	}
	if err := h.auth.DeleteUser(ctx, p.User.ID, req.Id); err != nil {
		return nil, authError(err)
	}
	return gen.DeleteUser204Response{}, nil
}
