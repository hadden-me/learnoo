// Package clientip derives the client address used for per-address
// throttling (issue 46). By default it is the TCP peer address and every
// forwarding header is ignored. Only when the peer is inside a configured
// trusted-proxy range is a forwarding header read:
//
//   - X-Forwarded-For (default): the chain is walked from the right, skipping
//     trusted proxies; the first address that is not trusted is the client.
//     Entries to its left were written by the client and are never used.
//   - CF-Connecting-IP: the single address Cloudflare sets. Only safe when the
//     trusted proxy accepts connections from Cloudflare alone and passes the
//     header through unchanged (docs/DEPLOYMENT.md).
//
// Trusted ranges are configuration (for example the reverse proxy and the
// published Cloudflare ranges); nothing is hard-coded.
package clientip

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// Header names a forwarding header the Resolver can read.
type Header string

// Supported headers.
const (
	XForwardedFor  Header = "X-Forwarded-For"
	CFConnectingIP Header = "CF-Connecting-IP"
)

// maxHops bounds the X-Forwarded-For entries examined from the right.
const maxHops = 32

// Resolver derives client addresses. The zero value and nil trust nobody.
type Resolver struct {
	trusted []netip.Prefix
	header  Header
}

// New returns a Resolver trusting the given ranges and reading header from
// trusted peers. header must be XForwardedFor or CFConnectingIP.
func New(trusted []netip.Prefix, header Header) (*Resolver, error) {
	if header != XForwardedFor && header != CFConnectingIP {
		return nil, fmt.Errorf("unsupported client address header %q (want %s or %s)", header, XForwardedFor, CFConnectingIP)
	}
	for i, p := range trusted {
		q, err := checkRange(p)
		if err != nil {
			return nil, err
		}
		trusted[i] = q
	}
	return &Resolver{trusted: append([]netip.Prefix(nil), trusted...), header: header}, nil
}

// ParseTrusted parses a comma- or space-separated list of CIDR ranges or
// single addresses. An empty string means no trusted proxy. A range covering
// every address (0.0.0.0/0, ::/0) is refused, since it would let any client
// choose its own address.
func ParseTrusted(spec string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	for _, f := range strings.FieldsFunc(spec, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' || r == '\n' }) {
		var p netip.Prefix
		if strings.Contains(f, "/") {
			var err error
			if p, err = netip.ParsePrefix(f); err != nil {
				return nil, fmt.Errorf("trusted proxy range %q: %w", f, err)
			}
		} else {
			a, err := netip.ParseAddr(f)
			if err != nil || a.Zone() != "" {
				return nil, fmt.Errorf("trusted proxy address %q is not an IP address", f)
			}
			p = netip.PrefixFrom(a, a.BitLen())
		}
		if p.Addr().Zone() != "" {
			return nil, fmt.Errorf("trusted proxy range %q has a zone", f)
		}
		q, err := checkRange(p)
		if err != nil {
			return nil, fmt.Errorf("trusted proxy range %q: %w", f, err)
		}
		out = append(out, q)
	}
	return out, nil
}

// Header is the forwarding header read from trusted peers.
func (r *Resolver) Header() Header {
	if r == nil {
		return XForwardedFor
	}
	return r.header
}

// Narrowest prefix lengths accepted for a trusted range. A proxy range is
// small; anything broader is almost certainly a mistake that would let many
// clients choose their own throttle key.
const (
	minBits4 = 8
	minBits6 = 16
)

// checkRange masks p, unmaps IPv4-mapped ranges and refuses ranges broader
// than /8 (IPv4) or /16 (IPv6), including "everything" written as a mapped
// range (::ffff:0.0.0.0/96 and wider).
func checkRange(p netip.Prefix) (netip.Prefix, error) {
	if !p.IsValid() {
		return p, errors.New("invalid range")
	}
	p = p.Masked()
	if p.Addr().Is4In6() {
		if p.Bits() < 96 {
			return p, fmt.Errorf("range %v is broader than the IPv4-mapped space", p)
		}
		p = netip.PrefixFrom(p.Addr().Unmap(), p.Bits()-96).Masked()
	}
	if p.Addr().Is4() && p.Bits() < minBits4 || p.Addr().Is6() && p.Bits() < minBits6 {
		return p, fmt.Errorf("range %v is broader than /%d (IPv4) or /%d (IPv6)", p, minBits4, minBits6)
	}
	return p, nil
}

// Peer returns the normalized TCP peer address of remote (zone removed,
// IPv4-mapped IPv6 unmapped), or its host part unchanged if it is not an IP
// address. It is the client address when no trusted proxy applies.
func Peer(remote string) string {
	if a, ok := peerAddr(remote); ok {
		return a.String()
	}
	return peerHost(remote)
}

// Trusted reports how many ranges are trusted.
func (r *Resolver) Trusted() int {
	if r == nil {
		return 0
	}
	return len(r.trusted)
}

func (r *Resolver) isTrusted(a netip.Addr) bool {
	for _, p := range r.trusted {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// ClientAddr returns the client address of req as a normalized IP string.
// If RemoteAddr is not an IP address it is returned as is (tests, unusual
// listeners), and no header is read.
func (r *Resolver) ClientAddr(req *http.Request) string {
	peer, ok := peerAddr(req.RemoteAddr)
	if !ok {
		return peerHost(req.RemoteAddr)
	}
	if r == nil || len(r.trusted) == 0 || !r.isTrusted(peer) {
		return peer.String()
	}
	switch r.header {
	case CFConnectingIP:
		vals := req.Header.Values(string(CFConnectingIP))
		if len(vals) != 1 {
			return peer.String() // absent or ambiguous
		}
		a, ok := parseHop(vals[0])
		if !ok {
			return peer.String()
		}
		return a.String()
	default:
		return r.fromForwardedFor(req.Header.Values(string(XForwardedFor)), peer).String()
	}
}

// fromForwardedFor walks the chain right to left and returns the first
// address that is not trusted. When there is none (the chain is empty,
// malformed before an untrusted entry, too long, or made only of trusted
// addresses) it returns the nearest hop: the right-most entry if it is a
// valid trusted address, otherwise the peer. Entries further left are never
// returned unless they are the first untrusted one, so a broken or
// all-trusted chain cannot yield an address the client chose.
func (r *Resolver) fromForwardedFor(values []string, peer netip.Addr) netip.Addr {
	var hops []string
	for _, v := range values {
		hops = append(hops, strings.Split(v, ",")...)
	}
	nearest := peer
	for i, n := len(hops)-1, 0; i >= 0 && n < maxHops; i, n = i-1, n+1 {
		a, ok := parseHop(hops[i])
		if !ok {
			return nearest
		}
		if !r.isTrusted(a) {
			return a
		}
		if n == 0 {
			nearest = a
		}
	}
	return nearest
}

// parseHop accepts an IP address, optionally bracketed (IPv6) or with a
// port, as some proxies write. Zones are refused; IPv4-mapped IPv6
// addresses are unmapped.
func parseHop(s string) (netip.Addr, bool) {
	s = strings.TrimSpace(s)
	if s == "" || len(s) > 64 {
		return netip.Addr{}, false
	}
	if a, err := netip.ParseAddr(s); err == nil {
		return normalize(a)
	}
	if ap, err := netip.ParseAddrPort(s); err == nil {
		return normalize(ap.Addr())
	}
	if strings.HasPrefix(s, "[") && strings.HasSuffix(s, "]") {
		if a, err := netip.ParseAddr(s[1 : len(s)-1]); err == nil && a.Is6() {
			return normalize(a)
		}
	}
	return netip.Addr{}, false
}

func normalize(a netip.Addr) (netip.Addr, bool) {
	if a.Zone() != "" || !a.IsValid() {
		return netip.Addr{}, false
	}
	return a.Unmap(), true
}

func peerAddr(remote string) (netip.Addr, bool) {
	host := peerHost(remote)
	a, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}, false
	}
	return a.WithZone("").Unmap(), true
}

func peerHost(remote string) string {
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		return remote
	}
	return host
}

type ctxKey struct{}

// Middleware stores the resolved client address in the request context,
// where FromContext (and auth.ClientAddr) find it. Headers are not
// modified.
func Middleware(r *Resolver, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		next.ServeHTTP(w, req.WithContext(context.WithValue(req.Context(), ctxKey{}, r.ClientAddr(req))))
	})
}

// FromContext returns the address stored by Middleware.
func FromContext(ctx context.Context) (string, bool) {
	s, ok := ctx.Value(ctxKey{}).(string)
	return s, ok
}

// ErrConfig marks inconsistent settings.
var ErrConfig = errors.New("clientip: invalid configuration")

// FromEnv builds a Resolver from the LEARNOO_TRUSTED_PROXIES and
// LEARNOO_CLIENT_ADDRESS_HEADER values (empty header means X-Forwarded-For).
func FromEnv(trustedSpec, header string) (*Resolver, error) {
	trusted, err := ParseTrusted(trustedSpec)
	if err != nil {
		return nil, fmt.Errorf("LEARNOO_TRUSTED_PROXIES: %w", err)
	}
	h := XForwardedFor
	switch strings.ToLower(strings.TrimSpace(header)) {
	case "", "x-forwarded-for":
	case "cf-connecting-ip":
		h = CFConnectingIP
	default:
		return nil, fmt.Errorf("LEARNOO_CLIENT_ADDRESS_HEADER %q: want X-Forwarded-For or CF-Connecting-IP", header)
	}
	if h == CFConnectingIP && len(trusted) == 0 {
		return nil, fmt.Errorf("%w: LEARNOO_CLIENT_ADDRESS_HEADER=CF-Connecting-IP needs LEARNOO_TRUSTED_PROXIES", ErrConfig)
	}
	return New(trusted, h)
}
