package s3store_test

import (
	"bytes"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/takiren/vms-demo/server/internal/s3store"
	"github.com/takiren/vms-demo/server/internal/testutil/garagetest"
)

func TestGetRangeValidates(t *testing.T) {
	s, err := s3store.New(validConfig())
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		key         string
		off, length int64
	}{
		{"", 0, 1}, {"k", -1, 1}, {"k", 0, 0}, {"k", 0, s3store.MaxRangeLength + 1},
	} {
		if _, err := s.GetRange(t.Context(), tc.key, tc.off, tc.length); err == nil {
			t.Errorf("GetRange(%q, %d, %d) accepted", tc.key, tc.off, tc.length)
		}
	}
}

// HLS byte ranges rely on presigned GETs honoring Range: the header is not
// part of the signature.
func TestGarageRanges(t *testing.T) {
	s, err := s3store.New(garagetest.Config(t))
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	body := []byte("0123456789abcdefghij")
	key := "range/2026/01/02/03/1767322800000.mp4"
	if err := s.Put(ctx, key, bytes.NewReader(body), int64(len(body)), "video/mp4"); err != nil {
		t.Fatal(err)
	}

	got, err := s.GetRange(ctx, key, 5, 4)
	if err != nil || string(got) != "5678" {
		t.Fatalf("GetRange = %q, %v", got, err)
	}
	// Past the end: what is there.
	got, err = s.GetRange(ctx, key, 15, 100)
	if err != nil || string(got) != "fghij" {
		t.Fatalf("GetRange past the end = %q, %v", got, err)
	}
	if _, err := s.GetRange(ctx, "missing", 0, 10); err == nil {
		t.Error("GetRange(missing) succeeded")
	}

	u, err := s.PresignGet(ctx, key, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	req.Header.Set("Range", "bytes=10-14")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusPartialContent || string(b) != "abcde" || resp.Header.Get("Content-Range") != "bytes 10-14/20" {
		t.Fatalf("presigned GET with Range: %d %q %q", resp.StatusCode, b, resp.Header.Get("Content-Range"))
	}
}
