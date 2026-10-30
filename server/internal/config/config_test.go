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
		config.EnvHost:        "vms.example",
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
	if cfg.PublicOrigin != "https://vms.example" {
		t.Errorf("PublicOrigin = %q", cfg.PublicOrigin)
	}
	if len(cfg.TrustedProxies) != 0 {
		t.Errorf("TrustedProxies = %v, want none by default", cfg.TrustedProxies)
	}
	if cfg.SessionIdleTimeout != 12*time.Hour || cfg.SessionMaxLifetime != 7*24*time.Hour {
		t.Errorf("session durations = %v, %v", cfg.SessionIdleTimeout, cfg.SessionMaxLifetime)
	}
	if cfg.AdminUsername != "" || cfg.AdminPassword.IsSet() {
		t.Errorf("admin = %q, %v; want unset", cfg.AdminUsername, cfg.AdminPassword.IsSet())
	}
}

func TestLoadAuthSettings(t *testing.T) {
	cfg, err := config.Load(env(map[string]string{
		config.EnvHost:               "VMS.Example:8443",
		config.EnvTrustedProxies:     " 172.16.0.0/12, 10.0.0.5 ,::1, ::ffff:192.0.2.0/120",
		config.EnvSessionIdleTimeout: "30m",
		config.EnvSessionMaxLifetime: "24h",
		config.EnvAdminUsername:      "root.admin",
		config.EnvAdminPassword:      " a long admin password ",
	}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.PublicOrigin != "https://vms.example:8443" {
		t.Errorf("PublicOrigin = %q", cfg.PublicOrigin)
	}
	want := []string{"172.16.0.0/12", "10.0.0.5/32", "::1/128", "192.0.2.0/24"}
	if fmt.Sprint(cfg.TrustedProxies) != fmt.Sprint(want) {
		t.Errorf("TrustedProxies = %v, want %v", cfg.TrustedProxies, want)
	}
	if cfg.SessionIdleTimeout != 30*time.Minute || cfg.SessionMaxLifetime != 24*time.Hour {
		t.Errorf("durations = %v %v", cfg.SessionIdleTimeout, cfg.SessionMaxLifetime)
	}
	if cfg.AdminUsername != "root.admin" || cfg.AdminPassword.Value() != " a long admin password " {
		t.Errorf("admin = %q (password must not be trimmed)", cfg.AdminUsername)
	}

	cfg, err = config.Load(env(map[string]string{config.EnvPublicOrigin: "https://VMS.example.com/"}))
	if err != nil || cfg.PublicOrigin != "https://vms.example.com" {
		t.Errorf("override: %q, %v", cfg.PublicOrigin, err)
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
		{"missing host", map[string]string{config.EnvHost: "<unset>"}, "VMS_HOST: is required"},
		{"host with scheme", map[string]string{config.EnvHost: "https://vms.example"}, "VMS_HOST: must be a host name"},
		{"host with path", map[string]string{config.EnvHost: "vms.example/x"}, "VMS_HOST: must be a host name"},
		{"http origin", map[string]string{config.EnvPublicOrigin: "http://vms.example"}, "VMS_PUBLIC_ORIGIN: must be an https origin"},
		{"origin with path", map[string]string{config.EnvPublicOrigin: "https://vms.example/app"}, "VMS_PUBLIC_ORIGIN: must be an https origin"},
		{"bad proxy", map[string]string{config.EnvTrustedProxies: "10.0.0.0/33"}, "VMS_TRUSTED_PROXIES: invalid CIDR"},
		{"bad proxy ip", map[string]string{config.EnvTrustedProxies: "caddy"}, "VMS_TRUSTED_PROXIES: invalid IP address"},
		{"bad idle", map[string]string{config.EnvSessionIdleTimeout: "1d"}, "VMS_SESSION_IDLE_TIMEOUT: must be a duration"},
		{"idle too short", map[string]string{config.EnvSessionIdleTimeout: "10s"}, "VMS_SESSION_IDLE_TIMEOUT: must be between"},
		{"max too long", map[string]string{config.EnvSessionMaxLifetime: "9000h"}, "VMS_SESSION_MAX_LIFETIME: must be between"},
		{"idle exceeds max", map[string]string{config.EnvSessionIdleTimeout: "48h", config.EnvSessionMaxLifetime: "24h"}, "VMS_SESSION_IDLE_TIMEOUT: must not exceed"},
		{"admin user without password", map[string]string{config.EnvAdminUsername: "admin"}, "must be set together"},
		{"admin password without user", map[string]string{config.EnvAdminPassword: testPassword + "-long-enough"}, "must be set together"},
		{"admin placeholder", map[string]string{config.EnvAdminUsername: "admin", config.EnvAdminPassword: "CHANGE_ME"}, "VMS_ADMIN_PASSWORD: is still the CHANGE_ME placeholder"},
		{"admin weak password", map[string]string{config.EnvAdminUsername: "admin", config.EnvAdminPassword: testPassword}, "VMS_ADMIN_PASSWORD: password must be 12 to 128"},
		{"admin bad username", map[string]string{config.EnvAdminUsername: "ad min", config.EnvAdminPassword: testPassword + "-long-enough"}, "VMS_ADMIN_USERNAME: must be"},
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
	cfg, err := config.Load(env(map[string]string{
		config.EnvAdminUsername: "admin",
		config.EnvAdminPassword: testPassword + "-admin",
	}))
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
		"admin pw":     fmt.Sprintf("%v %+v %#v %s", cfg.AdminPassword, cfg.AdminPassword, cfg.AdminPassword, cfg.AdminPassword),
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
