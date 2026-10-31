package camera_test

import (
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/takiren/vms-demo/server/internal/camera"
)

func TestValidateRTSPURL(t *testing.T) {
	ok := []string{
		"rtsp://192.168.1.10/stream1",
		"rtsp://cam.local:554/Streaming/Channels/101?transportmode=unicast",
		"rtsps://cam.example:322/live",
		"rtsp://[fe80::1]:554/h264",
		"rtsp://cam/path@with-at-in-path",
		"rtsp://cam/a%20b",
	}
	for _, u := range ok {
		if err := camera.ValidateRTSPURL("main_url", u); err != nil {
			t.Errorf("%s: %v", u, err)
		}
	}
	bad := []struct{ url, msg string }{
		{"", "is required"},
		{"http://cam/stream", "must start with rtsp:// or rtsps://"},
		{"RTSP://cam/stream", "must start with rtsp:// or rtsps://"},
		{"rtmp://cam/stream", "must start with rtsp:// or rtsps://"},
		{"rtsp://admin:hunter2@cam/stream", "must not contain credentials"},
		{"rtsp://admin@cam/stream", "must not contain credentials"},
		{"rtsp://a:b@c@cam/stream", "must not contain credentials"},
		{"rtsp:///stream", "must have a host"},
		{"rtsp://:554/stream", "must have a host"},
		{"rtsp://cam:99999/stream", "invalid port"},
		{"rtsp://cam:0/stream", "invalid port"},
		{"rtsp://cam/a b", "printable ASCII"},
		{"rtsp://cam/\x00", "printable ASCII"},
		{"rtsp://cam/日本", "printable ASCII"},
		{"rtsp://cam/stream#frag", "fragment"},
		{"rtsp://cam/" + strings.Repeat("a", camera.MaxURLLen), "at most"},
		{"rtsp://cam:abc/", "not a valid URL"},
	}
	for _, tt := range bad {
		err := camera.ValidateRTSPURL("main_url", tt.url)
		var verr *camera.ValidationError
		if !errors.As(err, &verr) || !strings.Contains(err.Error(), tt.msg) || !strings.HasPrefix(err.Error(), "main_url") {
			t.Errorf("%q: err = %v, want %q", tt.url, err, tt.msg)
			continue
		}
		if strings.Contains(err.Error(), "hunter2") {
			t.Errorf("error leaks the password: %v", err)
		}
	}
}

func TestValidateName(t *testing.T) {
	for _, n := range []string{"front door", "玄関", strings.Repeat("あ", 100)} {
		if err := camera.ValidateName(n); err != nil {
			t.Errorf("%q: %v", n, err)
		}
	}
	for _, n := range []string{"", " lead", "trail ", "tab\there", strings.Repeat("a", 101), "\xff"} {
		if err := camera.ValidateName(n); err == nil {
			t.Errorf("%q accepted", n)
		}
	}
}

func TestValidateCredentials(t *testing.T) {
	good := []camera.Credentials{{Username: "admin", Password: ""}, {Username: "u", Password: "p@ss:w/rd?#%"}}
	for _, c := range good {
		if err := camera.ValidateCredentials(c); err != nil {
			t.Errorf("%v", err)
		}
	}
	bad := []camera.Credentials{
		{Username: "", Password: "x"},
		{Username: strings.Repeat("u", 256)},
		{Username: "u", Password: strings.Repeat("p", 256)},
		{Username: "u\n", Password: "x"},
		{Username: "u", Password: "x\x00"},
	}
	for _, c := range bad {
		if err := camera.ValidateCredentials(c); err == nil {
			t.Errorf("accepted %d/%d bytes", len(c.Username), len(c.Password))
		}
	}
	if err := camera.ValidateRetentionDays(0); err == nil {
		t.Error("retention 0 accepted")
	}
	if err := camera.ValidateRetentionDays(7); err != nil {
		t.Error(err)
	}
}

func TestWithCredentials(t *testing.T) {
	tests := []struct {
		url   string
		creds *camera.Credentials
		want  string
	}{
		{"rtsp://cam/s", nil, "rtsp://cam/s"},
		{"rtsp://cam:554/s?x=1", &camera.Credentials{Username: "admin", Password: "pw"}, "rtsp://admin:pw@cam:554/s?x=1"},
		{"rtsps://cam/s", &camera.Credentials{Username: "a@b", Password: "p:@/?#%"}, "rtsps://a%40b:p%3A%40%2F%3F%23%25@cam/s"},
		{"rtsp://cam/a%2Fb", &camera.Credentials{Username: "u", Password: ""}, "rtsp://u:@cam/a%2Fb"},
	}
	for _, tt := range tests {
		if got := camera.WithCredentials(tt.url, tt.creds); got != tt.want {
			t.Errorf("WithCredentials(%q) = %q, want %q", tt.url, got, tt.want)
		}
	}
}

func TestCredentialsNeverPrint(t *testing.T) {
	c := camera.Credentials{Username: "admin", Password: "hunter2"}
	var b strings.Builder
	fmt.Fprintf(&b, "%v %+v %#v %s", c, c, c, c)
	slog.New(slog.NewTextHandler(&b, nil)).Info("x", slog.Any("creds", c))
	if strings.Contains(b.String(), "hunter2") || strings.Contains(b.String(), "admin") {
		t.Errorf("credentials printed: %s", b.String())
	}
}
