// Package s3store is the API's client for the recordings bucket in Garage
// (S3-compatible). It is used by the ingest pipeline (upload), the retention
// job (delete) and playback (presigned GET URLs).
//
// There are two endpoints:
//
//   - InternalEndpoint is where the API itself talks to Garage (uploads,
//     deletes). In the all-in-containers setup this is http://garage:3900;
//     in development it is the public endpoint through Caddy.
//   - PublicEndpoint (S3_ENDPOINT, https://s3.<host>) is used only to sign
//     URLs that browsers fetch. A presigned URL signs the Host and the path,
//     so it must be signed for exactly the host the browser uses. Presigning
//     is pure computation; it never contacts the endpoint.
//
// Buckets are addressed path-style (https://s3.<host>/<bucket>/<key>), as
// Garage is configured without root_domain.
//
// The secret key is never logged or included in errors.
package s3store

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// Config configures a Store.
type Config struct {
	// InternalEndpoint is the base URL the API uses for its own requests
	// (scheme://host[:port], no path). Required.
	InternalEndpoint string
	// PublicEndpoint is the base URL browsers use; presigned URLs are signed
	// for it. Defaults to InternalEndpoint.
	PublicEndpoint string
	// Region is the S3 region Garage is configured with (s3_region).
	Region string
	// Bucket is the recordings bucket.
	Bucket string
	// AccessKeyID and SecretAccessKey are the API's Garage key.
	AccessKeyID     string
	SecretAccessKey string
	// HTTPClient overrides the HTTP client (tests). Optional.
	HTTPClient *http.Client
}

// MaxPresignTTL is the longest validity SigV4 allows for a presigned URL.
const MaxPresignTTL = 7 * 24 * time.Hour

// maxDeleteBatch is the S3 limit of keys per DeleteObjects request.
const maxDeleteBatch = 1000

// Store reads and writes objects in one bucket.
type Store struct {
	bucket   string
	internal *s3.Client
	presign  *s3.PresignClient
}

// New validates cfg and builds the clients. It does not contact Garage.
func New(cfg Config) (*Store, error) {
	if err := ValidateEndpoint(cfg.InternalEndpoint); err != nil {
		return nil, fmt.Errorf("s3store: internal endpoint: %w", err)
	}
	if cfg.PublicEndpoint == "" {
		cfg.PublicEndpoint = cfg.InternalEndpoint
	}
	if err := ValidateEndpoint(cfg.PublicEndpoint); err != nil {
		return nil, fmt.Errorf("s3store: public endpoint: %w", err)
	}
	switch {
	case cfg.Region == "":
		return nil, errors.New("s3store: region is required")
	case cfg.Bucket == "":
		return nil, errors.New("s3store: bucket is required")
	case cfg.AccessKeyID == "" || cfg.SecretAccessKey == "":
		return nil, errors.New("s3store: access key ID and secret are required")
	}

	creds := credentials.NewStaticCredentialsProvider(cfg.AccessKeyID, cfg.SecretAccessKey, "")
	newClient := func(endpoint string) *s3.Client {
		return s3.New(s3.Options{
			BaseEndpoint: aws.String(strings.TrimSuffix(endpoint, "/")),
			Region:       cfg.Region,
			Credentials:  creds,
			UsePathStyle: true,
			// Only send/verify checksums where the S3 API requires them
			// (DeleteObjects). Keeps requests compatible with Garage and
			// presigned URLs free of checksum parameters.
			RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired,
			ResponseChecksumValidation: aws.ResponseChecksumValidationWhenRequired,
			HTTPClient:                 httpClient(cfg.HTTPClient),
			RetryMaxAttempts:           3,
		})
	}
	return &Store{
		bucket:   cfg.Bucket,
		internal: newClient(cfg.InternalEndpoint),
		presign:  s3.NewPresignClient(newClient(cfg.PublicEndpoint)),
	}, nil
}

// httpClient returns c, or nil so the SDK uses its default (with timeouts).
func httpClient(c *http.Client) s3.HTTPClient {
	if c == nil {
		return nil
	}
	return c
}

// ValidateEndpoint checks that s is an http(s) base URL without userinfo,
// path, query or fragment. A path would break path-style addressing and the
// presigned URL signatures.
func ValidateEndpoint(s string) error {
	if s == "" {
		return errors.New("is required")
	}
	u, err := url.Parse(s)
	if err != nil {
		return errors.New("is not a valid URL")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return errors.New("scheme must be http or https")
	}
	if u.Host == "" || u.Hostname() == "" {
		return errors.New("host is required")
	}
	if u.User != nil {
		return errors.New("must not contain credentials")
	}
	if (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("must not have a path, query or fragment (use a subdomain, not a path prefix)")
	}
	return nil
}

// Bucket returns the bucket name.
func (s *Store) Bucket() string { return s.bucket }

// Put uploads body (size bytes) to key, replacing any existing object.
// body must be seekable so the SDK can sign the payload and retry.
func (s *Store) Put(ctx context.Context, key string, body io.ReadSeeker, size int64, contentType string) error {
	if key == "" {
		return errors.New("s3store: empty key")
	}
	_, err := s.internal.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(s.bucket),
		Key:           aws.String(key),
		Body:          body,
		ContentLength: aws.Int64(size),
		ContentType:   aws.String(contentType),
	})
	if err != nil {
		return fmt.Errorf("s3store: put %s: %w", key, err)
	}
	return nil
}

// Head returns the size of the object at key. exists is false (with a nil
// error) when there is no such object.
func (s *Store) Head(ctx context.Context, key string) (size int64, exists bool, err error) {
	out, err := s.internal.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		var nf *types.NotFound
		var nsk *types.NoSuchKey
		if errors.As(err, &nf) || errors.As(err, &nsk) {
			return 0, false, nil
		}
		return 0, false, fmt.Errorf("s3store: head %s: %w", key, err)
	}
	return aws.ToInt64(out.ContentLength), true, nil
}

// DeleteResult reports which keys DeleteObjects removed. S3 reports keys
// that did not exist as deleted.
type DeleteResult struct {
	Deleted []string
	// Failed maps a key to the error S3 returned for it.
	Failed map[string]error
}

// Delete removes keys with DeleteObjects (in batches of 1000). A request
// level error stops early and is returned together with what was deleted
// so far; per-key failures are reported in Failed. Callers should delete
// catalog rows only for keys in Deleted.
func (s *Store) Delete(ctx context.Context, keys []string) (DeleteResult, error) {
	res := DeleteResult{Failed: map[string]error{}}
	for start := 0; start < len(keys); start += maxDeleteBatch {
		batch := keys[start:min(start+maxDeleteBatch, len(keys))]
		ids := make([]types.ObjectIdentifier, len(batch))
		for i, k := range batch {
			ids[i] = types.ObjectIdentifier{Key: aws.String(k)}
		}
		out, err := s.internal.DeleteObjects(ctx, &s3.DeleteObjectsInput{
			Bucket: aws.String(s.bucket),
			Delete: &types.Delete{Objects: ids, Quiet: aws.Bool(false)},
		})
		if err != nil {
			return res, fmt.Errorf("s3store: delete objects: %w", err)
		}
		for _, d := range out.Deleted {
			res.Deleted = append(res.Deleted, aws.ToString(d.Key))
		}
		for _, e := range out.Errors {
			res.Failed[aws.ToString(e.Key)] = fmt.Errorf("%s: %s", aws.ToString(e.Code), aws.ToString(e.Message))
		}
	}
	return res, nil
}

// PresignGet returns a URL on the public endpoint that GETs key for ttl.
func (s *Store) PresignGet(ctx context.Context, key string, ttl time.Duration) (string, error) {
	if ttl <= 0 || ttl > MaxPresignTTL {
		return "", fmt.Errorf("s3store: presign ttl must be in (0, %s]", MaxPresignTTL)
	}
	req, err := s.presign.PresignGetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	}, s3.WithPresignExpires(ttl))
	if err != nil {
		return "", fmt.Errorf("s3store: presign %s: %w", key, err)
	}
	return req.URL, nil
}
