package config_test

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/takiren/vms-demo/server/internal/config"
)

const (
	testPassword = "s3cr3t-pw"
	testDBURL    = "postgres://vms:" + testPassword + "@db.example:5432/vms?sslmode=disable"
)

var testKey = base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x42}, 32))

func env(overrides map[string]string) func(string) (string, bool) {
	m := map[string]string{
		config.EnvDatabaseURL: testDBURL,
		config.EnvSecretKey:   testKey,
	}
	for k, v := range overrides {
		if v == "<unset>" {
			delete(m, k)
			continue
		}
		m[k] = v
	}
	return func(k string) (string, bool) {
		v, ok := m[k]
		return v, ok
	}
}

func TestLoadDefaults(t *testing.T) {
	cfg, err := config.Load(env(nil))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ListenAddr != config.DefaultListenAddr {
		t.Errorf("ListenAddr = %q, want %q", cfg.ListenAddr, config.DefaultListenAddr)
	}
	if cfg.LogLevel != slog.LevelInfo {
		t.Errorf("LogLevel = %v, want INFO", cfg.LogLevel)
	}
	if cfg.ShutdownTimeout != config.DefaultShutdownTimeout {
		t.Errorf("ShutdownTimeout = %v", cfg.ShutdownTimeout)
	}
	if cfg.DatabaseURL.Value() != testDBURL {
		t.Errorf("DatabaseURL.Value() = %q", cfg.DatabaseURL.Value())
	}
	if !bytes.Equal(cfg.SecretKey.Bytes(), bytes.Repeat([]byte{0x42}, 32)) {
		t.Errorf("SecretKey.Bytes() mismatch")
	}
}

func TestLoadOverrides(t *testing.T) {
	cfg, err := config.Load(env(map[string]string{
		config.EnvListenAddr:      "127.0.0.1:9000",
		config.EnvLogLevel:        "debug",
		config.EnvShutdownTimeout: " 30s ",
	}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ListenAddr != "127.0.0.1:9000" || cfg.LogLevel != slog.LevelDebug || cfg.ShutdownTimeout != 30*time.Second {
		t.Errorf("unexpected config: %+v", cfg)
	}
}

func TestLoadErrors(t *testing.T) {
	tests := []struct {
		name    string
		env     map[string]string
		wantMsg string
	}{
		{"missing database url", map[string]string{config.EnvDatabaseURL: "<unset>"}, "DATABASE_URL: is required"},
		{"bad scheme", map[string]string{config.EnvDatabaseURL: "mysql://u:" + testPassword + "@h/db"}, "DATABASE_URL: scheme must be"},
		{"no host", map[string]string{config.EnvDatabaseURL: "postgres:///db"}, "DATABASE_URL: host is required"},
		{"unparsable url", map[string]string{config.EnvDatabaseURL: "postgres://u:" + testPassword + "@h:bad port/db"}, "DATABASE_URL: is not a valid URL"},
		{"placeholder password", map[string]string{config.EnvDatabaseURL: "postgres://vms:CHANGE_ME@localhost:5432/vms"}, "DATABASE_URL: password is still the CHANGE_ME placeholder"},
		{"missing key", map[string]string{config.EnvSecretKey: "<unset>"}, "VMS_SECRET_KEY: is required"},
		{"placeholder key", map[string]string{config.EnvSecretKey: "CHANGE_ME"}, "VMS_SECRET_KEY: is still the CHANGE_ME placeholder"},
		{"key not base64", map[string]string{config.EnvSecretKey: "not base64!!"}, "VMS_SECRET_KEY: must be standard base64"},
		{"key too short", map[string]string{config.EnvSecretKey: base64.StdEncoding.EncodeToString([]byte("short"))}, "VMS_SECRET_KEY: must decode to 32 bytes, got 5"},
		{"bad listen addr", map[string]string{config.EnvListenAddr: "8080"}, "VMS_LISTEN_ADDR: must be host:port"},
		{"bad log level", map[string]string{config.EnvLogLevel: "verbose"}, "VMS_LOG_LEVEL: must be one of"},
		{"bad timeout", map[string]string{config.EnvShutdownTimeout: "ten"}, "VMS_SHUTDOWN_TIMEOUT: must be a duration"},
		{"non-positive timeout", map[string]string{config.EnvShutdownTimeout: "0s"}, "VMS_SHUTDOWN_TIMEOUT: must be positive"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := config.Load(env(tt.env))
			if err == nil {
				t.Fatal("Load succeeded, want error")
			}
			if !strings.Contains(err.Error(), tt.wantMsg) {
				t.Errorf("error = %q, want it to contain %q", err, tt.wantMsg)
			}
			if strings.Contains(err.Error(), testPassword) {
				t.Errorf("error leaks the database password: %q", err)
			}
		})
	}
}

func TestLoadReportsAllErrors(t *testing.T) {
	_, err := config.Load(env(map[string]string{
		config.EnvDatabaseURL: "<unset>",
		config.EnvSecretKey:   "<unset>",
		config.EnvLogLevel:    "nope",
	}))
	if err == nil {
		t.Fatal("Load succeeded, want error")
	}
	for _, name := range []string{config.EnvDatabaseURL, config.EnvSecretKey, config.EnvLogLevel} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("error %q does not mention %s", err, name)
		}
	}
}

func TestSecretsAreRedacted(t *testing.T) {
	cfg, err := config.Load(env(nil))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	var buf bytes.Buffer
	slog.New(slog.NewJSONHandler(&buf, nil)).Info("cfg", slog.Any("config", cfg))
	outputs := map[string]string{
		"slog":         buf.String(),
		"%v":           fmt.Sprintf("%v", cfg),
		"%+v":          fmt.Sprintf("%+v", cfg),
		"%#v":          fmt.Sprintf("%#v", cfg),
		"Sprint url":   fmt.Sprint(cfg.DatabaseURL),
		"Sprint key":   fmt.Sprint(cfg.SecretKey),
		"%#v url, key": fmt.Sprintf("%#v %#v", cfg.DatabaseURL, cfg.SecretKey),
	}
	for name, out := range outputs {
		if strings.Contains(out, testPassword) {
			t.Errorf("%s leaks the database password: %s", name, out)
		}
		if strings.Contains(out, testKey) {
			t.Errorf("%s leaks the secret key: %s", name, out)
		}
	}
	if !strings.Contains(buf.String(), "db.example:5432") {
		t.Errorf("slog output should still show the database host: %s", buf.String())
	}
}

func TestSecretKeyBytesIsACopy(t *testing.T) {
	cfg, err := config.Load(env(nil))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	b := cfg.SecretKey.Bytes()
	b[0] = 0
	if cfg.SecretKey.Bytes()[0] != 0x42 {
		t.Error("mutating Bytes() changed the key")
	}
}

func TestDatabaseURLRedactsQueryParams(t *testing.T) {
	tests := []struct {
		name, url string
		keep      []string
	}{
		{"password param", "postgres://vms@db:5432/vms?sslmode=require&password=" + testPassword, []string{"sslmode=require", "password=xxxxx"}},
		{"sslpassword param", "postgres://vms@db:5432/vms?sslpassword=" + testPassword + "&sslkey=%2Fk.pem", []string{"sslpassword=xxxxx", "sslkey=%2Fk.pem"}},
		{"case-insensitive", "postgres://vms@db:5432/vms?PassWord=" + testPassword, []string{"PassWord=xxxxx"}},
		{"userinfo and param", "postgres://vms:" + testPassword + "@db:5432/vms?password=" + testPassword, []string{"vms:xxxxx@db", "password=xxxxx"}},
		{"unparsable query", "postgres://vms@db:5432/vms?password=" + testPassword + "%zz", []string{"?xxxxx"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := config.Load(env(map[string]string{config.EnvDatabaseURL: tt.url}))
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.DatabaseURL.Value() != tt.url {
				t.Errorf("Value() = %q, want the original URL", cfg.DatabaseURL.Value())
			}
			var buf bytes.Buffer
			slog.New(slog.NewJSONHandler(&buf, nil)).Info("cfg", slog.Any("config", cfg))
			for name, out := range map[string]string{
				"String": cfg.DatabaseURL.String(),
				"%#v":    fmt.Sprintf("%#v", cfg),
				"slog":   buf.String(),
			} {
				if strings.Contains(out, testPassword) {
					t.Errorf("%s leaks the password: %s", name, out)
				}
			}
			for _, k := range tt.keep {
				if !strings.Contains(cfg.DatabaseURL.String(), k) {
					t.Errorf("String() = %q, want it to contain %q", cfg.DatabaseURL.String(), k)
				}
			}
		})
	}
}
