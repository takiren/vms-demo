package camera

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/takiren/vms-demo/server/internal/db/sqlc"
	"github.com/takiren/vms-demo/server/internal/mediamtx"
	"github.com/takiren/vms-demo/server/internal/sealbox"
)

// Service is the database-backed camera store.
type Service struct {
	pool     *pgxpool.Pool
	q        *sqlc.Queries
	box      *sealbox.Box
	logger   *slog.Logger
	onChange func()
}

// NewService returns a Service. key is VMS_SECRET_KEY (32 bytes).
func NewService(pool *pgxpool.Pool, key []byte, logger *slog.Logger) (*Service, error) {
	box, err := sealbox.New(key)
	if err != nil {
		return nil, err
	}
	return &Service{pool: pool, q: sqlc.New(pool), box: box, logger: logger, onChange: func() {}}, nil
}

var _ mediamtx.CameraSource = (*Service)(nil)

// SetOnChange sets the hook called after every committed change (the
// MediaMTX reconciler's Trigger). It must not block. Call it before serving
// requests.
func (s *Service) SetOnChange(f func()) {
	if f != nil {
		s.onChange = f
	}
}

func toCamera(c sqlc.Camera) Camera {
	return Camera{
		ID:             c.ID,
		Name:           c.Name,
		MainURL:        c.MainUrl,
		SubURL:         c.SubUrl,
		HasCredentials: len(c.CredentialsEnc) > 0,
		RetentionDays:  int(c.RetentionDays),
		Enabled:        c.Enabled,
		CreatedAt:      c.CreatedAt,
		UpdatedAt:      c.UpdatedAt,
	}
}

// List returns all cameras ordered by name.
func (s *Service) List(ctx context.Context) ([]Camera, error) {
	rows, err := s.q.ListCameras(ctx)
	if err != nil {
		return nil, fmt.Errorf("camera: list: %w", err)
	}
	out := make([]Camera, len(rows))
	for i, r := range rows {
		out[i] = toCamera(r)
	}
	return out, nil
}

// Get returns one camera.
func (s *Service) Get(ctx context.Context, id uuid.UUID) (Camera, error) {
	row, err := s.q.GetCamera(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return Camera{}, ErrNotFound
	}
	if err != nil {
		return Camera{}, fmt.Errorf("camera: get: %w", err)
	}
	return toCamera(row), nil
}

// Create validates and stores a new camera.
func (s *Service) Create(ctx context.Context, in Create) (Camera, error) {
	if err := ValidateName(in.Name); err != nil {
		return Camera{}, err
	}
	if err := ValidateRTSPURL("main_url", in.MainURL); err != nil {
		return Camera{}, err
	}
	if in.SubURL != nil {
		if err := ValidateRTSPURL("sub_url", *in.SubURL); err != nil {
			return Camera{}, err
		}
	}
	retention := DefaultRetention
	if in.RetentionDays != nil {
		retention = *in.RetentionDays
	}
	if err := ValidateRetentionDays(retention); err != nil {
		return Camera{}, err
	}
	enabled := true
	if in.Enabled != nil {
		enabled = *in.Enabled
	}
	id, err := uuid.NewV7()
	if err != nil {
		return Camera{}, fmt.Errorf("camera: new id: %w", err)
	}
	var enc []byte
	if in.Credentials != nil {
		if enc, err = s.seal(id, *in.Credentials); err != nil {
			return Camera{}, err
		}
	}
	row, err := s.q.CreateCameraWithID(ctx, sqlc.CreateCameraWithIDParams{
		ID:             id,
		Name:           in.Name,
		MainUrl:        in.MainURL,
		SubUrl:         in.SubURL,
		CredentialsEnc: enc,
		RetentionDays:  int32(retention), //nolint:gosec // G115: bounded by ValidateRetentionDays
		Enabled:        enabled,
	})
	if err != nil {
		return Camera{}, dbError("create", err)
	}
	s.onChange()
	return toCamera(row), nil
}

// Update changes a camera. Unset fields are kept; credentials are kept
// unless replaced or removed.
func (s *Service) Update(ctx context.Context, id uuid.UUID, in Update) (Camera, error) {
	if in.SubURL != nil && in.RemoveSubURL {
		return Camera{}, invalid("sub_url and remove_sub_url cannot be used together")
	}
	if in.Credentials != nil && in.RemoveCreds {
		return Camera{}, invalid("credentials and remove_credentials cannot be used together")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Camera{}, fmt.Errorf("camera: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	q := s.q.WithTx(tx)

	cur, err := q.GetCameraForUpdate(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return Camera{}, ErrNotFound
	}
	if err != nil {
		return Camera{}, fmt.Errorf("camera: get for update: %w", err)
	}
	p := sqlc.UpdateCameraParams{
		ID:             cur.ID,
		Name:           cur.Name,
		MainUrl:        cur.MainUrl,
		SubUrl:         cur.SubUrl,
		CredentialsEnc: cur.CredentialsEnc,
		RetentionDays:  cur.RetentionDays,
		Enabled:        cur.Enabled,
	}
	if in.Name != nil {
		if err := ValidateName(*in.Name); err != nil {
			return Camera{}, err
		}
		p.Name = *in.Name
	}
	if in.MainURL != nil {
		if err := ValidateRTSPURL("main_url", *in.MainURL); err != nil {
			return Camera{}, err
		}
		p.MainUrl = *in.MainURL
	}
	switch {
	case in.SubURL != nil:
		if err := ValidateRTSPURL("sub_url", *in.SubURL); err != nil {
			return Camera{}, err
		}
		p.SubUrl = in.SubURL
	case in.RemoveSubURL:
		p.SubUrl = nil
	}
	switch {
	case in.Credentials != nil:
		if p.CredentialsEnc, err = s.seal(id, *in.Credentials); err != nil {
			return Camera{}, err
		}
	case in.RemoveCreds:
		p.CredentialsEnc = nil
	}
	if in.RetentionDays != nil {
		if err := ValidateRetentionDays(*in.RetentionDays); err != nil {
			return Camera{}, err
		}
		p.RetentionDays = int32(*in.RetentionDays) //nolint:gosec // G115: bounded by ValidateRetentionDays
	}
	if in.Enabled != nil {
		p.Enabled = *in.Enabled
	}
	row, err := q.UpdateCamera(ctx, p)
	if err != nil {
		return Camera{}, dbError("update", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Camera{}, dbError("commit", err)
	}
	s.onChange()
	return toCamera(row), nil
}

// Delete removes a camera. It fails with ErrHasRecordings while the camera
// has recordings.
func (s *Service) Delete(ctx context.Context, id uuid.UUID) error {
	n, err := s.q.DeleteCamera(ctx, id)
	if err != nil {
		return dbError("delete", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	s.onChange()
	return nil
}

// EnabledCameraSources returns the MediaMTX sources of the enabled cameras,
// with the credentials decrypted and put into the URLs. A camera whose
// credentials cannot be decrypted is returned as Unavailable (and logged,
// without secrets), so its existing MediaMTX paths are left alone.
func (s *Service) EnabledCameraSources(ctx context.Context) ([]mediamtx.Camera, error) {
	rows, err := s.q.ListEnabledCameras(ctx)
	if err != nil {
		return nil, fmt.Errorf("camera: list enabled: %w", err)
	}
	out := make([]mediamtx.Camera, 0, len(rows))
	for _, r := range rows {
		c := mediamtx.Camera{ID: r.ID}
		var creds *Credentials
		if len(r.CredentialsEnc) > 0 {
			cr, err := s.open(r.ID, r.CredentialsEnc)
			if err != nil {
				s.logger.Error("cannot decrypt camera credentials (was VMS_SECRET_KEY changed?)",
					slog.String("camera_id", r.ID.String()), slog.String("error", err.Error()))
				c.Unavailable = true
				out = append(out, c)
				continue
			}
			creds = &cr
		}
		c.MainSource = WithCredentials(r.MainUrl, creds)
		if r.SubUrl != nil {
			c.SubSource = WithCredentials(*r.SubUrl, creds)
		}
		out = append(out, c)
	}
	return out, nil
}

func (s *Service) seal(id uuid.UUID, c Credentials) ([]byte, error) {
	if err := ValidateCredentials(c); err != nil {
		return nil, err
	}
	pt, err := json.Marshal(c) //nolint:gosec // G117: marshaled only to be encrypted right below
	if err != nil {
		return nil, fmt.Errorf("camera: encode credentials: %w", err)
	}
	enc, err := s.box.Seal(pt, credentialsAAD(id))
	clear(pt)
	if err != nil {
		return nil, fmt.Errorf("camera: seal credentials: %w", err)
	}
	return enc, nil
}

func (s *Service) open(id uuid.UUID, enc []byte) (Credentials, error) {
	pt, err := s.box.Open(enc, credentialsAAD(id))
	if err != nil {
		return Credentials{}, err
	}
	defer clear(pt)
	var c Credentials
	if err := json.Unmarshal(pt, &c); err != nil {
		return Credentials{}, errors.New("camera: decrypted credentials are not valid JSON")
	}
	return c, nil
}

// PostgreSQL error codes.
const (
	pgUniqueViolation     = "23505"
	pgForeignKeyViolation = "23503"
	pgRestrictViolation   = "23001"
	pgCheckViolation      = "23514"
)

func dbError(op string, err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case pgUniqueViolation:
			return ErrNameTaken
		case pgForeignKeyViolation, pgRestrictViolation:
			return ErrHasRecordings
		case pgCheckViolation:
			// Validation should have caught it; do not echo the row.
			return invalid("the camera does not satisfy a database constraint (%s)", pgErr.ConstraintName)
		}
	}
	return fmt.Errorf("camera: %s: %w", op, err)
}
