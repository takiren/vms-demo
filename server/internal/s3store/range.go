package s3store

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// MaxRangeLength bounds GetRange. It is meant for headers (the init section
// of a segment), not for whole objects.
const MaxRangeLength = 16 << 20

// GetRange reads up to length bytes of the object at key starting at offset
// (an HTTP Range request on the internal endpoint). It returns fewer bytes
// when the object is shorter.
func (s *Store) GetRange(ctx context.Context, key string, offset, length int64) ([]byte, error) {
	if key == "" {
		return nil, errors.New("s3store: empty key")
	}
	if offset < 0 || length <= 0 || length > MaxRangeLength {
		return nil, fmt.Errorf("s3store: range length must be in (0, %d]", MaxRangeLength)
	}
	out, err := s.internal.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
		Range:  aws.String(fmt.Sprintf("bytes=%d-%d", offset, offset+length-1)),
	})
	if err != nil {
		return nil, fmt.Errorf("s3store: get %s: %w", key, err)
	}
	defer func() { _ = out.Body.Close() }()
	// Never trust the server to honor the range: read at most length bytes.
	b, err := io.ReadAll(io.LimitReader(out.Body, length))
	if err != nil {
		return nil, fmt.Errorf("s3store: read %s: %w", key, err)
	}
	return b, nil
}
