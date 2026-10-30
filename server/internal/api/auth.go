package api

import (
	"context"
	"errors"
	"math"
	"net/http"

	"github.com/google/uuid"

	"github.com/takiren/vms-demo/server/internal/api/apierror"
	"github.com/takiren/vms-demo/server/internal/api/clientip"
	"github.com/takiren/vms-demo/server/internal/api/gen"
	"github.com/takiren/vms-demo/server/internal/auth"
)

// authError maps auth errors to client-facing API errors. Messages never
// say whether a username exists.
func authError(err error) error {
	switch {
	case errors.Is(err, auth.ErrInvalidCredentials):
		return apierror.New(http.StatusUnauthorized, "invalid username or password")
	case errors.Is(err, auth.ErrUnauthenticated):
		return apierror.New(http.StatusUnauthorized, "authentication required")
	case errors.Is(err, auth.ErrWrongPassword):
		return apierror.New(http.StatusBadRequest, "current password is incorrect")
	case errors.Is(err, auth.ErrWeakPassword),
		errors.Is(err, auth.ErrInvalidUsername),
		errors.Is(err, auth.ErrInvalidRole):
		return apierror.Wrap(http.StatusBadRequest, err.Error(), err)
	case errors.Is(err, auth.ErrUserNotFound):
		return apierror.New(http.StatusNotFound, "user not found")
	case errors.Is(err, auth.ErrUsernameTaken):
		return apierror.New(http.StatusConflict, "username is already taken")
	case errors.Is(err, auth.ErrLastAdmin):
		return apierror.New(http.StatusConflict, "cannot remove or demote the last admin")
	case errors.Is(err, auth.ErrDeleteSelf):
		return apierror.New(http.StatusConflict, "you cannot delete your own account")
	case errors.Is(err, auth.ErrBusy):
		return apierror.Wrap(http.StatusServiceUnavailable, "", err)
	default:
		return apierror.Internal(err)
	}
}

// retryAfterSeconds rounds up so clients never retry too early.
func retryAfterSeconds(e *auth.RateLimitedError) int {
	return max(1, int(math.Ceil(e.RetryAfter.Seconds())))
}

func tooManyRequests() gen.Error {
	return gen.Error{Error: gen.ErrorBody{
		Code:    gen.ErrorCodeTooManyRequests,
		Message: "too many failed attempts; try again later",
	}}
}

func toAPIUser(u auth.User) gen.User {
	return gen.User{
		Id:        u.ID,
		Username:  u.Username,
		Role:      gen.Role(u.Role),
		CreatedAt: u.CreatedAt,
		UpdatedAt: u.UpdatedAt,
	}
}

// principal returns the caller. The authorization middleware guarantees it
// for authenticated operations; the error is a safety net.
func principal(ctx context.Context) (auth.Principal, error) {
	p, ok := auth.PrincipalFrom(ctx)
	if !ok {
		return auth.Principal{}, apierror.New(http.StatusUnauthorized, "authentication required")
	}
	return p, nil
}

// Login verifies credentials and sets a new session cookie.
func (h *Handlers) Login(ctx context.Context, req gen.LoginRequestObject) (gen.LoginResponseObject, error) {
	var previous *uuid.UUID
	if p, ok := auth.PrincipalFrom(ctx); ok {
		previous = &p.SessionID
	}
	u, token, err := h.auth.Login(ctx, req.Body.Username, req.Body.Password, clientip.FromContext(ctx), previous)
	if rl, ok := errors.AsType[*auth.RateLimitedError](err); ok {
		return gen.Login429JSONResponse{
			Body:    tooManyRequests(),
			Headers: gen.Login429ResponseHeaders{RetryAfter: retryAfterSeconds(rl)},
		}, nil
	}
	if err != nil {
		return nil, authError(err)
	}
	return gen.Login200JSONResponse{
		Body:    toAPIUser(u),
		Headers: gen.Login200ResponseHeaders{SetCookie: auth.SessionCookie(token)},
	}, nil
}

// Logout deletes the caller's session (if any) and clears the cookie.
func (h *Handlers) Logout(ctx context.Context, _ gen.LogoutRequestObject) (gen.LogoutResponseObject, error) {
	if p, ok := auth.PrincipalFrom(ctx); ok {
		if err := h.auth.Logout(ctx, p.SessionID); err != nil {
			return nil, apierror.Internal(err)
		}
	}
	return gen.Logout204Response{Headers: gen.Logout204ResponseHeaders{SetCookie: auth.ClearSessionCookie()}}, nil
}

// GetMe returns the logged-in user.
func (h *Handlers) GetMe(ctx context.Context, _ gen.GetMeRequestObject) (gen.GetMeResponseObject, error) {
	p, err := principal(ctx)
	if err != nil {
		return nil, err
	}
	return gen.GetMe200JSONResponse(toAPIUser(p.User)), nil
}

// ChangeOwnPassword changes the caller's password and issues a new session.
func (h *Handlers) ChangeOwnPassword(ctx context.Context, req gen.ChangeOwnPasswordRequestObject) (gen.ChangeOwnPasswordResponseObject, error) {
	p, err := principal(ctx)
	if err != nil {
		return nil, err
	}
	token, err := h.auth.ChangeOwnPassword(ctx, p.User.ID, req.Body.CurrentPassword, req.Body.NewPassword)
	if rl, ok := errors.AsType[*auth.RateLimitedError](err); ok {
		return gen.ChangeOwnPassword429JSONResponse{
			Body:    tooManyRequests(),
			Headers: gen.ChangeOwnPassword429ResponseHeaders{RetryAfter: retryAfterSeconds(rl)},
		}, nil
	}
	if err != nil {
		return nil, authError(err)
	}
	return gen.ChangeOwnPassword204Response{
		Headers: gen.ChangeOwnPassword204ResponseHeaders{SetCookie: auth.SessionCookie(token)},
	}, nil
}
