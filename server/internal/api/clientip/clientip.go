// Package clientip determines the client address of a request behind
// trusted reverse proxies (Caddy).
//
// X-Forwarded-For is only believed when the direct peer is a trusted proxy.
// The header is then read from right to left, skipping trusted proxies; the
// first untrusted address is the client. Anything a client puts at the left
// end of the header is therefore ignored.
package clientip

import (
	"context"
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// Resolver extracts client addresses.
type Resolver struct {
	trusted []netip.Prefix
}

// New returns a Resolver that trusts X-Forwarded-For only from peers in
// trusted. With no prefixes, the direct peer is always the client.
func New(trusted []netip.Prefix) *Resolver {
	return &Resolver{trusted: trusted}
}

func (r *Resolver) isTrusted(a netip.Addr) bool {
	for _, p := range r.trusted {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// ClientIP returns the client address of req, or the zero Addr if even the
// peer address cannot be parsed.
func (r *Resolver) ClientIP(req *http.Request) netip.Addr {
	peer := parsePeer(req.RemoteAddr)
	if !peer.IsValid() || !r.isTrusted(peer) {
		return peer
	}

	var hops []string
	for _, v := range req.Header.Values("X-Forwarded-For") {
		hops = append(hops, strings.Split(v, ",")...)
	}
	client := peer
	for i := len(hops) - 1; i >= 0; i-- {
		a, err := netip.ParseAddr(strings.TrimSpace(hops[i]))
		if err != nil {
			// A malformed entry: stop at the last address we could trust
			// to have been written by a proxy we trust.
			return client
		}
		client = a.Unmap()
		if !r.isTrusted(client) {
			return client
		}
	}
	return client
}

func parsePeer(remoteAddr string) netip.Addr {
	if ap, err := netip.ParseAddrPort(remoteAddr); err == nil {
		return ap.Addr().Unmap()
	}
	host := remoteAddr
	if h, _, err := net.SplitHostPort(remoteAddr); err == nil {
		host = h
	}
	a, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}
	}
	return a.Unmap()
}

type ctxKey struct{}

// NewContext returns a context carrying the client address.
func NewContext(ctx context.Context, a netip.Addr) context.Context {
	return context.WithValue(ctx, ctxKey{}, a)
}

// FromContext returns the client address stored by NewContext.
func FromContext(ctx context.Context) netip.Addr {
	a, _ := ctx.Value(ctxKey{}).(netip.Addr)
	return a
}
