package s3store_test

import (
	"bytes"
	"context"
	"flag"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/takiren/vms-demo/server/internal/s3store"
	"github.com/takiren/vms-demo/server/internal/testutil/garagetest"
)

func TestMain(m *testing.M) {
	flag.Parse()
	if err := garagetest.Start(context.Background()); err != nil {
		log.Fatal(err)
	}
	code := m.Run()
	garagetest.Stop()
	os.Exit(code)
}

func validConfig() s3store.Config {
	return s3store.Config{
		InternalEndpoint: "http://garage:3900",
		PublicEndpoint:   "https://s3.vms.example",
		Region:           "garage",
		Bucket:           "recordings",
		AccessKeyID:      "GKtest",
		SecretAccessKey:  "very-secret-value",
	}
}

func TestValidateEndpoint(t *testing.T) {
	for _, tc := range []struct {
		in string
		ok bool
	}{
		{"http://garage:3900", true},
		{"https://s3.vms.example", true},
		{"https://s3.vms.example/", true},
		{"", false},
		{"s3.vms.example", false},
		{"ftp://s3.vms.example", false},
		{"https://s3.vms.example/garage", false},
		{"https://user:pw@s3.vms.example", false},
		{"https://s3.vms.example?x=1", false},
		{"https://s3.vms.example#f", false},
		{"https://:443", false},
		{"http://[::1", false},
	} {
		if err := s3store.ValidateEndpoint(tc.in); (err == nil) != tc.ok {
			t.Errorf("ValidateEndpoint(%q) = %v, want ok=%v", tc.in, err, tc.ok)
		}
	}
}

func TestNewValidates(t *testing.T) {
	mods := map[string]func(*s3store.Config){
		"internal":  func(c *s3store.Config) { c.InternalEndpoint = "garage:3900" },
		"public":    func(c *s3store.Config) { c.PublicEndpoint = "https://s3.example/prefix" },
		"region":    func(c *s3store.Config) { c.Region = "" },
		"bucket":    func(c *s3store.Config) { c.Bucket = "" },
		"keyid":     func(c *s3store.Config) { c.AccessKeyID = "" },
		"keysecret": func(c *s3store.Config) { c.SecretAccessKey = "" },
	}
	for name, mod := range mods {
		cfg := validConfig()
		mod(&cfg)
		if _, err := s3store.New(cfg); err == nil {
			t.Errorf("%s: New accepted an invalid config", name)
		} else if strings.Contains(err.Error(), "very-secret-value") {
			t.Errorf("%s: error leaks the secret: %v", name, err)
		}
	}
	cfg := validConfig()
	cfg.PublicEndpoint = ""
	if _, err := s3store.New(cfg); err != nil {
		t.Errorf("public endpoint should default to internal: %v", err)
	}
}

// Presigning is offline: it must be signed for the public host, path-style,
// and never contain the secret.
func TestPresignUsesPublicEndpoint(t *testing.T) {
	s, err := s3store.New(validConfig())
	if err != nil {
		t.Fatal(err)
	}
	raw, err := s.PresignGet(t.Context(), "cam/2026/01/02/03/1767322800000.mp4", 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if u.Scheme != "https" || u.Host != "s3.vms.example" {
		t.Errorf("presigned URL host = %s://%s, want https://s3.vms.example", u.Scheme, u.Host)
	}
	if u.Path != "/recordings/cam/2026/01/02/03/1767322800000.mp4" {
		t.Errorf("path = %q, want path-style /recordings/<key>", u.Path)
	}
	q := u.Query()
	if q.Get("X-Amz-Expires") != "300" || !strings.Contains(q.Get("X-Amz-Credential"), "/garage/s3/aws4_request") {
		t.Errorf("unexpected query: %v", q)
	}
	if strings.Contains(raw, "very-secret-value") {
		t.Error("presigned URL contains the secret key")
	}
	for _, ttl := range []time.Duration{0, -time.Second, s3store.MaxPresignTTL + time.Second} {
		if _, err := s.PresignGet(t.Context(), "k", ttl); err == nil {
			t.Errorf("ttl %s accepted", ttl)
		}
	}
	if s.Bucket() != "recordings" {
		t.Errorf("Bucket() = %q", s.Bucket())
	}
}

func TestPutWithoutKey(t *testing.T) {
	s, err := s3store.New(validConfig())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Put(t.Context(), "", bytes.NewReader(nil), 0, "video/mp4"); err == nil {
		t.Error("empty key accepted")
	}
}

func TestGarageRoundTrip(t *testing.T) {
	cfg := garagetest.Config(t)
	s, err := s3store.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	body := []byte("fmp4 segment bytes")
	key := "0190/2026/01/02/03/1767322800000.mp4"
	if err := s.Put(ctx, key, bytes.NewReader(body), int64(len(body)), "video/mp4"); err != nil {
		t.Fatalf("Put: %v", err)
	}
	// Overwriting is allowed (retries re-upload the same key).
	if err := s.Put(ctx, key, bytes.NewReader(body), int64(len(body)), "video/mp4"); err != nil {
		t.Fatalf("Put again: %v", err)
	}
	size, ok, err := s.Head(ctx, key)
	if err != nil || !ok || size != int64(len(body)) {
		t.Fatalf("Head = %d, %v, %v", size, ok, err)
	}
	if _, ok, err := s.Head(ctx, "missing"); err != nil || ok {
		t.Fatalf("Head(missing) = %v, %v", ok, err)
	}

	u, err := s.PresignGet(ctx, key, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	got := httpGet(t, u)
	if !bytes.Equal(got, body) {
		t.Fatalf("GET presigned = %q", got)
	}

	res, err := s.Delete(ctx, []string{key, "never-existed"})
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if len(res.Failed) != 0 || len(res.Deleted) != 2 {
		t.Fatalf("Delete result = %+v", res)
	}
	if _, ok, err := s.Head(ctx, key); err != nil || ok {
		t.Fatalf("object still exists after delete: %v, %v", ok, err)
	}
}

func TestGarageDown(t *testing.T) {
	cfg := validConfig()
	cfg.InternalEndpoint = "http://127.0.0.1:1"
	s, err := s3store.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	err = s.Put(ctx, "k", bytes.NewReader([]byte("x")), 1, "video/mp4")
	if err == nil {
		t.Fatal("Put to a closed port succeeded")
	}
	if strings.Contains(err.Error(), cfg.SecretAccessKey) {
		t.Errorf("error leaks the secret: %v", err)
	}
	if _, err := s.Delete(ctx, []string{"k"}); err == nil {
		t.Error("Delete to a closed port succeeded")
	}
	if _, _, err := s.Head(ctx, "k"); err == nil {
		t.Error("Head to a closed port succeeded")
	}
}

func httpGet(t *testing.T, u string) []byte {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, u, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %d: %s", resp.StatusCode, b)
	}
	return b
}
