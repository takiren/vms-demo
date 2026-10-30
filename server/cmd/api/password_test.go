package main

import (
	"strings"
	"testing"
)

func TestReadLine(t *testing.T) {
	tests := map[string]string{
		"secret password\n":       "secret password",
		"secret password\r\n":     "secret password",
		"secret password":         "secret password",
		" spaced \nsecond line\n": " spaced ",
	}
	for in, want := range tests {
		got, err := readLine(strings.NewReader(in))
		if err != nil || got != want {
			t.Errorf("readLine(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"", "\n"} {
		if _, err := readLine(strings.NewReader(in)); err == nil {
			t.Errorf("readLine(%q) succeeded", in)
		}
	}
}

func TestRunUsage(t *testing.T) {
	if err := run([]string{"bogus"}); err == nil || !strings.Contains(err.Error(), "usage") {
		t.Errorf("unknown command: %v", err)
	}
	if err := run([]string{"reset-password"}); err == nil {
		t.Error("missing username accepted")
	}
	if err := run([]string{"create-admin", "a", "b"}); err == nil {
		t.Error("extra argument accepted")
	}
}
