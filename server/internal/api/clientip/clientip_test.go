package clientip_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"

	"github.com/takiren/vms-demo/server/internal/api/clientip"
)

func TestClientIP(t *testing.T) {
	trusted := []netip.Prefix{
		netip.MustParsePrefix("10.0.0.0/8"),
		netip.MustParsePrefix("fd00::/8"),
	}
	tests := []struct {
		name    string
		trusted []netip.Prefix
		remote  string
		xff     []string
		want    string
	}{
		{"no proxies trusted: peer wins", nil, "203.0.113.5:1234", []string{"198.51.100.1"}, "203.0.113.5"},
		{"untrusted peer: header ignored", trusted, "203.0.113.5:1234", []string{"198.51.100.1"}, "203.0.113.5"},
		{"trusted peer, no header", trusted, "10.0.0.2:1234", nil, "10.0.0.2"},
		{"trusted peer: rightmost untrusted", trusted, "10.0.0.2:1234", []string{"198.51.100.1"}, "198.51.100.1"},
		{"spoofed left entries are ignored", trusted, "10.0.0.2:1234", []string{"1.2.3.4, 198.51.100.1"}, "198.51.100.1"},
		{"chain of trusted proxies", trusted, "10.0.0.2:1234", []string{"198.51.100.1, 10.0.0.9, 10.1.1.1"}, "198.51.100.1"},
		{"multiple headers", trusted, "10.0.0.2:1234", []string{"1.2.3.4", "198.51.100.1, 10.0.0.9"}, "198.51.100.1"},
		{"all trusted: leftmost", trusted, "10.0.0.2:1234", []string{"10.0.0.7, 10.0.0.8"}, "10.0.0.7"},
		{"malformed entry: stop at last trusted hop", trusted, "10.0.0.2:1234", []string{"198.51.100.1, garbage"}, "10.0.0.2"},
		{"malformed after trusted hop", trusted, "10.0.0.2:1234", []string{"junk, 10.0.0.9"}, "10.0.0.9"},
		{"ipv6 peer", trusted, "[fd00::1]:443", []string{"2001:db8::7"}, "2001:db8::7"},
		{"ipv4-mapped peer", trusted, "[::ffff:10.0.0.2]:443", []string{"198.51.100.1"}, "198.51.100.1"},
		{"ipv4-mapped client is unmapped", trusted, "10.0.0.2:1", []string{"::ffff:198.51.100.1"}, "198.51.100.1"},
		{"spaces", trusted, "10.0.0.2:1", []string{"  198.51.100.1  ,  10.0.0.3 "}, "198.51.100.1"},
		{"bare peer address", nil, "203.0.113.5", nil, "203.0.113.5"},
		{"unparsable peer", trusted, "pipe", []string{"198.51.100.1"}, "invalid IP"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.RemoteAddr = tt.remote
			for _, v := range tt.xff {
				req.Header.Add("X-Forwarded-For", v)
			}
			got := clientip.New(tt.trusted).ClientIP(req)
			if got.String() != tt.want {
				t.Errorf("ClientIP = %s, want %s", got, tt.want)
			}
		})
	}
}

func TestContext(t *testing.T) {
	if a := clientip.FromContext(context.Background()); a.IsValid() {
		t.Errorf("empty context: %v", a)
	}
	want := netip.MustParseAddr("192.0.2.1")
	if a := clientip.FromContext(clientip.NewContext(context.Background(), want)); a != want {
		t.Errorf("FromContext = %v", a)
	}
}
