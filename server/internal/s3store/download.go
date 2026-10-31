package s3store

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// ErrTooLarge means an object is larger than the caller allowed.
var ErrTooLarge = errors.New("s3store: object is larger than allowed")

// Download streams the whole object at key into w (from the internal
// endpoint) and returns the number of bytes written. It stops with
// ErrTooLarge once more than maxBytes arrive, so a changed object cannot
// fill the disk. Cancelling ctx aborts the transfer.
func (s *Store) Download(ctx context.Context, key string, w io.Writer, maxBytes int64) (int64, error) {
	if key == "" {
		return 0, errors.New("s3store: empty key")
	}
	if maxBytes <= 0 {
		return 0, errors.New("s3store: maxBytes must be positive")
	}
	out, err := s.internal.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return 0, fmt.Errorf("s3store: get %s: %w", key, err)
	}
	defer func() { _ = out.Body.Close() }()
	if n := aws.ToInt64(out.ContentLength); n > maxBytes {
		return 0, fmt.Errorf("%w: %s is %d bytes (at most %d)", ErrTooLarge, key, n, maxBytes)
	}
	n, err := io.Copy(w, io.LimitReader(out.Body, maxBytes+1))
	if err != nil {
		return n, fmt.Errorf("s3store: read %s: %w", key, err)
	}
	if n > maxBytes {
		return n, fmt.Errorf("%w: %s (at most %d bytes)", ErrTooLarge, key, maxBytes)
	}
	return n, nil
}
