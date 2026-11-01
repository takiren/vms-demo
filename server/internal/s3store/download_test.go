package s3store_test

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/takiren/vms-demo/server/internal/s3store"
	"github.com/takiren/vms-demo/server/internal/testutil/garagetest"
)

func TestDownloadValidates(t *testing.T) {
	s, err := s3store.New(validConfig())
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if _, err := s.Download(t.Context(), "", &buf, 1); err == nil {
		t.Error("empty key accepted")
	}
	if _, err := s.Download(t.Context(), "k", &buf, 0); err == nil {
		t.Error("maxBytes 0 accepted")
	}
}

func TestGarageDownload(t *testing.T) {
	s, err := s3store.New(garagetest.Config(t))
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	body := bytes.Repeat([]byte("0123456789"), 1000)
	key := "download/2026/01/02/03/1767322800000.mp4"
	if err := s.Put(ctx, key, bytes.NewReader(body), int64(len(body)), "video/mp4"); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	n, err := s.Download(ctx, key, &buf, int64(len(body)))
	if err != nil || n != int64(len(body)) || !bytes.Equal(buf.Bytes(), body) {
		t.Fatalf("Download = %d, %v", n, err)
	}
	buf.Reset()
	if _, err := s.Download(ctx, key, &buf, int64(len(body))-1); !errors.Is(err, s3store.ErrTooLarge) {
		t.Errorf("over the limit: %v", err)
	}
	if buf.Len() > 0 {
		t.Errorf("wrote %d bytes of an object over the limit", buf.Len())
	}
	if _, err := s.Download(ctx, "missing", &buf, 10); err == nil || !strings.Contains(err.Error(), "missing") {
		t.Errorf("missing object: %v", err)
	}
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := s.Download(cctx, key, &buf, int64(len(body))); err == nil {
		t.Error("canceled context accepted")
	}
}
