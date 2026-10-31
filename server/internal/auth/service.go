package auth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/takiren/vms-demo/server/internal/db/sqlc"
)

// Errors returned by Service. internal/api maps them to HTTP statuses.
var (
	// ErrInvalidCredentials: wrong username or password (indistinguishable).
	ErrInvalidCredentials = errors.New("auth: invalid username or password")
	// ErrUnauthenticated: no valid session.
	ErrUnauthenticated = errors.New("auth: not authenticated")
	// ErrWrongPassword: the current password given to change it is wrong.
	ErrWrongPassword = errors.New("auth: current password is incorrect")
	// ErrUserNotFound: no user with that ID or username.
	ErrUserNotFound = errors.New("auth: user not found")
	// ErrUsernameTaken: the username is already used (case-insensitively).
	ErrUsernameTaken = errors.New("auth: username is already taken")
	// ErrLastAdmin: the operation would leave no admin.
	ErrLastAdmin = errors.New("auth: cannot remove or demote the last admin")
	// ErrDeleteSelf: users cannot delete their own account.
	ErrDeleteSelf = errors.New("auth: cannot delete your own account")
	// ErrInvalidUsername: the username does not match the allowed pattern.
	ErrInvalidUsername = errors.New("auth: username must be 1-64 characters of A-Z a-z 0-9 . _ -")
	// ErrInvalidRole: unknown role.
	ErrInvalidRole = errors.New("auth: role must be admin or viewer")
)

// RateLimitedError means too many failed attempts; retry after RetryAfter.
type RateLimitedError struct{ RetryAfter time.Duration }

func (e *RateLimitedError) Error() string {
	return fmt.Sprintf("auth: too many failed attempts, retry after %s", e.RetryAfter.Round(time.Second))
}

// usernamePattern matches users.username in the database and Username in
// api/openapi.yaml.
var usernamePattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// ValidateUsername checks a username against the allowed pattern.
func ValidateUsername(name string) error {
	if !usernamePattern.MatchString(name) {
		return ErrInvalidUsername
	}
	return nil
}

// Login rate limits (docs/design.md「総当たり対策」).
const (
	userFailures   = 10
	ipFailures     = 30
	failureWindow  = 15 * time.Minute
	maxLimiterKeys = 100_000
)

// Defaults for SessionConfig.
const (
	DefaultIdleTimeout = 12 * time.Hour
	DefaultMaxLifetime = 7 * 24 * time.Hour
	maxTouchInterval   = time.Minute
)

// Hashing concurrency: at most this many argon2id computations at once
// (2 x 64 MiB with DefaultParams), waiting at most hashWait for a slot.
const (
	maxConcurrentHashes = 2
	hashWait            = 5 * time.Second
)

// Config configures a Service.
type Config struct {
	// IdleTimeout ends a session this long after its last use.
	IdleTimeout time.Duration
	// MaxLifetime ends a session this long after it was created, however
	// active it is.
	MaxLifetime time.Duration
	// Params are the argon2id parameters for new hashes. Zero means
	// DefaultParams.
	Params Params
	// Now is the clock for the rate limiters. nil means time.Now.
	Now func() time.Time
}

// Service is the database-backed authentication service.
type Service struct {
	pool       *pgxpool.Pool
	q          *sqlc.Queries
	hasher     *Hasher
	logger     *slog.Logger
	idle       time.Duration
	maxLife    time.Duration
	touchEvery time.Duration
	userLimit  *Limiter
	ipLimit    *Limiter
	dummyHash  string
}

// NewService returns a Service. It computes one password hash up front (the
// dummy hash verified for unknown usernames, so they take as long as known
// ones).
func NewService(ctx context.Context, pool *pgxpool.Pool, logger *slog.Logger, cfg Config) (*Service, error) {
	if cfg.IdleTimeout <= 0 {
		cfg.IdleTimeout = DefaultIdleTimeout
	}
	if cfg.MaxLifetime <= 0 {
		cfg.MaxLifetime = DefaultMaxLifetime
	}
	if cfg.IdleTimeout > cfg.MaxLifetime {
		return nil, errors.New("auth: idle timeout must not exceed the max lifetime")
	}
	if cfg.Params == (Params{}) {
		cfg.Params = DefaultParams
	}
	s := &Service{
		pool:       pool,
		q:          sqlc.New(pool),
		hasher:     NewHasher(cfg.Params, maxConcurrentHashes, hashWait),
		logger:     logger,
		idle:       cfg.IdleTimeout,
		maxLife:    cfg.MaxLifetime,
		touchEvery: min(maxTouchInterval, cfg.IdleTimeout/4),
		userLimit:  NewLimiter(userFailures, failureWindow, maxLimiterKeys, cfg.Now),
		ipLimit:    NewLimiter(ipFailures, failureWindow, maxLimiterKeys, cfg.Now),
	}
	tok, _ := newToken()
	h, err := s.hasher.Hash(ctx, tok)
	if err != nil {
		return nil, err
	}
	s.dummyHash = h
	return s, nil
}

func toUser(u sqlc.User) User {
	return User{ID: u.ID, Username: u.Username, Role: Role(u.Role), CreatedAt: u.CreatedAt, UpdatedAt: u.UpdatedAt}
}

func userKey(username string) string { return "u:" + strings.ToLower(username) }

func ipKey(ip netip.Addr) string {
	if !ip.IsValid() {
		return "ip:unknown"
	}
	return "ip:" + ip.Unmap().String()
}

func (s *Service) checkLimits(keys ...string) error {
	var wait time.Duration
	for _, k := range keys {
		wait = max(wait, s.userOrIPLimiter(k).RetryAfter(k))
	}
	if wait > 0 {
		return &RateLimitedError{RetryAfter: wait}
	}
	return nil
}

func (s *Service) userOrIPLimiter(key string) *Limiter {
	if strings.HasPrefix(key, "ip:") {
		return s.ipLimit
	}
	return s.userLimit
}

func (s *Service) fail(keys ...string) {
	for _, k := range keys {
		s.userOrIPLimiter(k).Fail(k)
	}
}

// Login verifies the credentials and creates a new session. It returns the
// user and the session token to put in the cookie. A wrong password and an
// unknown username both return ErrInvalidCredentials after the same amount
// of hashing work. Too many failures return *RateLimitedError.
//
// If previous is a session ID (the caller's old session), it is deleted.
func (s *Service) Login(ctx context.Context, username, password string, clientIP netip.Addr, previous *uuid.UUID) (User, string, error) {
	uk, ik := userKey(username), ipKey(clientIP)
	if err := s.checkLimits(uk, ik); err != nil {
		return User{}, "", err
	}

	row, err := s.q.GetUserByUsername(ctx, username)
	found := err == nil
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return User{}, "", fmt.Errorf("auth: get user: %w", err)
	}
	hash := s.dummyHash
	if found {
		hash = row.PasswordHash
	}
	ok, rehash, err := s.hasher.Verify(ctx, hash, password)
	if err != nil {
		return User{}, "", err
	}
	if !found || !ok {
		s.fail(uk, ik)
		return User{}, "", ErrInvalidCredentials
	}
	s.userLimit.Reset(uk)

	if rehash {
		hash = s.rehash(ctx, row.ID, hash, password)
	}

	token, err := s.createSession(ctx, row.ID, hash)
	if err != nil {
		return User{}, "", err
	}
	if previous != nil {
		if _, err := s.q.DeleteSession(ctx, *previous); err != nil {
			s.logger.WarnContext(ctx, "failed to delete previous session", slog.String("error", err.Error()))
		}
	}
	return toUser(row), token, nil
}

// rehash stores a hash with the current parameters and returns the hash
// that is now stored. Failures are logged, not returned: the login itself
// succeeded.
func (s *Service) rehash(ctx context.Context, id uuid.UUID, old, password string) string {
	h, err := s.hasher.Hash(ctx, password)
	if err != nil {
		s.logger.WarnContext(ctx, "password rehash failed", slog.String("error", err.Error()))
		return old
	}
	n, err := s.q.RehashUserPassword(ctx, sqlc.RehashUserPasswordParams{ID: id, OldHash: old, NewHash: h})
	if err != nil || n != 1 {
		if err != nil {
			s.logger.WarnContext(ctx, "password rehash failed", slog.String("error", err.Error()))
		}
		return old
	}
	return h
}

// createSession inserts a session for userID if the stored password hash is
// still pwHash (see CreateSession in queries/sessions.sql).
func (s *Service) createSession(ctx context.Context, userID uuid.UUID, pwHash string) (string, error) {
	token, tokenHash := newToken()
	_, err := s.q.CreateSession(ctx, sqlc.CreateSessionParams{
		UserID:       userID,
		TokenHash:    tokenHash,
		PasswordHash: pwHash,
		TtlSeconds:   s.idle.Seconds(),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		// The password changed (or the user was deleted) after we verified it.
		return "", ErrInvalidCredentials
	}
	if err != nil {
		return "", fmt.Errorf("auth: create session: %w", err)
	}
	return token, nil
}

// Authenticate resolves a session token from a cookie. It returns
// ErrUnauthenticated for malformed, unknown or expired tokens. It extends
// the session (sliding idle timeout, capped by the max lifetime) at most
// once per touch interval.
func (s *Service) Authenticate(ctx context.Context, token string) (Principal, error) {
	tokenHash, ok := hashToken(token)
	if !ok {
		return Principal{}, ErrUnauthenticated
	}
	row, err := s.q.GetSessionByTokenHash(ctx, tokenHash)
	if errors.Is(err, pgx.ErrNoRows) {
		return Principal{}, ErrUnauthenticated
	}
	if err != nil {
		return Principal{}, fmt.Errorf("auth: get session: %w", err)
	}
	if time.Since(row.Session.LastSeenAt) >= s.touchEvery {
		if _, err := s.q.TouchSession(ctx, sqlc.TouchSessionParams{
			ID:                 row.Session.ID,
			IdleSeconds:        s.idle.Seconds(),
			MaxLifetimeSeconds: s.maxLife.Seconds(),
		}); err != nil {
			// The session is still valid until its current expiry.
			s.logger.WarnContext(ctx, "failed to extend session", slog.String("error", err.Error()))
		}
	}
	return Principal{User: toUser(row.User), SessionID: row.Session.ID}, nil
}

// Logout deletes a session.
func (s *Service) Logout(ctx context.Context, sessionID uuid.UUID) error {
	if _, err := s.q.DeleteSession(ctx, sessionID); err != nil {
		return fmt.Errorf("auth: delete session: %w", err)
	}
	return nil
}

// ChangeOwnPassword changes the caller's password after checking the current
// one. It deletes all of the user's sessions and returns a new session token.
// Wrong current passwords count as login failures for the username.
func (s *Service) ChangeOwnPassword(ctx context.Context, userID uuid.UUID, current, newPassword string) (string, error) {
	if err := ValidatePassword(newPassword); err != nil {
		return "", err
	}
	u, err := s.q.GetUser(ctx, userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrUnauthenticated
	}
	if err != nil {
		return "", fmt.Errorf("auth: get user: %w", err)
	}
	uk := userKey(u.Username)
	if err := s.checkLimits(uk); err != nil {
		return "", err
	}
	ok, _, err := s.hasher.Verify(ctx, u.PasswordHash, current)
	if err != nil {
		return "", err
	}
	if !ok {
		s.fail(uk)
		return "", ErrWrongPassword
	}
	s.userLimit.Reset(uk)

	newHash, err := s.hasher.Hash(ctx, newPassword)
	if err != nil {
		return "", err
	}
	var token string
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := s.q.WithTx(tx)
		// Lock the row and make sure the password did not change meanwhile.
		locked, err := q.GetUserForUpdate(ctx, userID)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrUnauthenticated
		}
		if err != nil {
			return err
		}
		if locked.PasswordHash != u.PasswordHash {
			return ErrWrongPassword
		}
		if err := setPassword(ctx, q, userID, newHash); err != nil {
			return err
		}
		tok, tokHash := newToken()
		if _, err := q.CreateSession(ctx, sqlc.CreateSessionParams{
			UserID: userID, TokenHash: tokHash, PasswordHash: newHash, TtlSeconds: s.idle.Seconds(),
		}); err != nil {
			return fmt.Errorf("create session: %w", err)
		}
		token = tok
		return nil
	})
	if err != nil {
		return "", wrapInternal(err)
	}
	return token, nil
}

// setPassword stores a new hash and deletes every session of the user. It
// must run in a transaction.
func setPassword(ctx context.Context, q *sqlc.Queries, id uuid.UUID, hash string) error {
	if _, err := q.UpdateUserPassword(ctx, sqlc.UpdateUserPasswordParams{ID: id, PasswordHash: hash}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrUserNotFound
		}
		return fmt.Errorf("update password: %w", err)
	}
	if _, err := q.DeleteSessionsByUser(ctx, id); err != nil {
		return fmt.Errorf("delete sessions: %w", err)
	}
	return nil
}

// wrapInternal leaves the package's sentinel errors as they are and marks
// everything else as an auth failure.
func wrapInternal(err error) error {
	if err == nil {
		return nil
	}
	for _, e := range []error{
		ErrInvalidCredentials, ErrUnauthenticated, ErrWrongPassword, ErrUserNotFound,
		ErrUsernameTaken, ErrLastAdmin, ErrDeleteSelf, ErrWeakPassword, ErrBusy,
		ErrInvalidUsername, ErrInvalidRole,
	} {
		if errors.Is(err, e) {
			return err
		}
	}
	return fmt.Errorf("auth: %w", err)
}

// ListUsers returns all users ordered by username.
func (s *Service) ListUsers(ctx context.Context) ([]User, error) {
	rows, err := s.q.ListUsers(ctx)
	if err != nil {
		return nil, fmt.Errorf("auth: list users: %w", err)
	}
	users := make([]User, len(rows))
	for i, r := range rows {
		users[i] = toUser(r)
	}
	return users, nil
}

// GetUser returns one user.
func (s *Service) GetUser(ctx context.Context, id uuid.UUID) (User, error) {
	u, err := s.q.GetUser(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, ErrUserNotFound
	}
	if err != nil {
		return User{}, fmt.Errorf("auth: get user: %w", err)
	}
	return toUser(u), nil
}

// CreateUser creates a user. It returns ErrUsernameTaken if the username is
// used (case-insensitively).
func (s *Service) CreateUser(ctx context.Context, username, password string, role Role) (User, error) {
	if err := ValidateUsername(username); err != nil {
		return User{}, err
	}
	if !role.Valid() {
		return User{}, ErrInvalidRole
	}
	if err := ValidatePassword(password); err != nil {
		return User{}, err
	}
	hash, err := s.hasher.Hash(ctx, password)
	if err != nil {
		return User{}, err
	}
	u, err := s.q.CreateUser(ctx, sqlc.CreateUserParams{Username: username, PasswordHash: hash, Role: string(role)})
	if isUniqueViolation(err) {
		return User{}, ErrUsernameTaken
	}
	if err != nil {
		return User{}, fmt.Errorf("auth: create user: %w", err)
	}
	return toUser(u), nil
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// UserUpdate is a partial update; nil fields are left unchanged.
type UserUpdate struct {
	Role     *Role
	Password *string
}

// UpdateUser changes a user's role and/or password. Changing either deletes
// all of the user's sessions. Demoting the last admin returns ErrLastAdmin;
// the check holds under concurrency because all admin rows are locked first.
func (s *Service) UpdateUser(ctx context.Context, id uuid.UUID, upd UserUpdate) (User, error) {
	if upd.Role != nil && !upd.Role.Valid() {
		return User{}, ErrInvalidRole
	}
	var newHash string
	if upd.Password != nil {
		if err := ValidatePassword(*upd.Password); err != nil {
			return User{}, err
		}
		h, err := s.hasher.Hash(ctx, *upd.Password)
		if err != nil {
			return User{}, err
		}
		newHash = h
	}

	var out sqlc.User
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := s.q.WithTx(tx)
		admins, err := q.LockAdmins(ctx)
		if err != nil {
			return fmt.Errorf("lock admins: %w", err)
		}
		target, err := q.GetUserForUpdate(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrUserNotFound
		}
		if err != nil {
			return fmt.Errorf("get user: %w", err)
		}
		out = target
		changed := false
		if upd.Role != nil && Role(target.Role) != *upd.Role {
			if Role(target.Role) == RoleAdmin && len(admins) <= 1 {
				return ErrLastAdmin
			}
			if out, err = q.UpdateUserRole(ctx, sqlc.UpdateUserRoleParams{ID: id, Role: string(*upd.Role)}); err != nil {
				return fmt.Errorf("update role: %w", err)
			}
			changed = true
		}
		if newHash != "" {
			if out, err = q.UpdateUserPassword(ctx, sqlc.UpdateUserPasswordParams{ID: id, PasswordHash: newHash}); err != nil {
				return fmt.Errorf("update password: %w", err)
			}
			changed = true
		}
		if changed {
			if _, err := q.DeleteSessionsByUser(ctx, id); err != nil {
				return fmt.Errorf("delete sessions: %w", err)
			}
		}
		return nil
	})
	if err != nil {
		return User{}, wrapInternal(err)
	}
	return toUser(out), nil
}

// DeleteUser deletes a user and (by ON DELETE CASCADE) their sessions.
// actorID is the caller: users cannot delete themselves. Deleting the last
// admin returns ErrLastAdmin.
func (s *Service) DeleteUser(ctx context.Context, actorID, id uuid.UUID) error {
	if actorID == id {
		return ErrDeleteSelf
	}
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := s.q.WithTx(tx)
		admins, err := q.LockAdmins(ctx)
		if err != nil {
			return fmt.Errorf("lock admins: %w", err)
		}
		target, err := q.GetUserForUpdate(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrUserNotFound
		}
		if err != nil {
			return fmt.Errorf("get user: %w", err)
		}
		if Role(target.Role) == RoleAdmin && len(admins) <= 1 {
			return ErrLastAdmin
		}
		if _, err := q.DeleteUser(ctx, id); err != nil {
			return fmt.Errorf("delete user: %w", err)
		}
		return nil
	})
	return wrapInternal(err)
}

// BootstrapAdmin creates an admin with the given credentials only if there
// is no admin at all. It never modifies an existing user: if the username
// is taken (by a viewer), it returns ErrUsernameTaken and changes nothing.
// It reports whether it created the user.
func (s *Service) BootstrapAdmin(ctx context.Context, username, password string) (bool, error) {
	if err := ValidateUsername(username); err != nil {
		return false, err
	}
	if err := ValidatePassword(password); err != nil {
		return false, err
	}
	// Cheap check first, so a normal start does not compute a hash.
	n, err := s.q.CountUsersByRole(ctx, string(RoleAdmin))
	if err != nil {
		return false, fmt.Errorf("auth: count admins: %w", err)
	}
	if n > 0 {
		return false, nil
	}
	hash, err := s.hasher.Hash(ctx, password)
	if err != nil {
		return false, err
	}
	created := false
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := s.q.WithTx(tx)
		if err := q.LockAdminBootstrap(ctx); err != nil {
			return fmt.Errorf("lock: %w", err)
		}
		n, err := q.CountUsersByRole(ctx, string(RoleAdmin))
		if err != nil {
			return fmt.Errorf("count admins: %w", err)
		}
		if n > 0 {
			return nil
		}
		if _, err := q.GetUserByUsername(ctx, username); err == nil {
			return ErrUsernameTaken
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("get user: %w", err)
		}
		if _, err := q.CreateUser(ctx, sqlc.CreateUserParams{Username: username, PasswordHash: hash, Role: string(RoleAdmin)}); err != nil {
			if isUniqueViolation(err) {
				return ErrUsernameTaken
			}
			return fmt.Errorf("create user: %w", err)
		}
		created = true
		return nil
	})
	if err != nil {
		return false, wrapInternal(err)
	}
	return created, nil
}

// ResetPassword sets a user's password by username (for the CLI) and
// deletes all of their sessions.
func (s *Service) ResetPassword(ctx context.Context, username, password string) error {
	if err := ValidatePassword(password); err != nil {
		return err
	}
	u, err := s.q.GetUserByUsername(ctx, username)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrUserNotFound
	}
	if err != nil {
		return fmt.Errorf("auth: get user: %w", err)
	}
	hash, err := s.hasher.Hash(ctx, password)
	if err != nil {
		return err
	}
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		return setPassword(ctx, s.q.WithTx(tx), u.ID, hash)
	})
	return wrapInternal(err)
}

// CleanupInterval is how often RunCleanup deletes expired sessions.
const CleanupInterval = 10 * time.Minute

// RunCleanup deletes expired sessions every CleanupInterval until ctx is done.
func (s *Service) RunCleanup(ctx context.Context) {
	t := time.NewTicker(CleanupInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			n, err := s.q.DeleteExpiredSessions(ctx)
			if err != nil {
				if ctx.Err() == nil {
					s.logger.WarnContext(ctx, "failed to delete expired sessions", slog.String("error", err.Error()))
				}
				continue
			}
			if n > 0 {
				s.logger.InfoContext(ctx, "deleted expired sessions", slog.Int64("count", n))
			}
		}
	}
}
