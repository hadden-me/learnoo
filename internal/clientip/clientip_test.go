package clientip

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
)

// Documentation addresses only (RFC 5737, RFC 3849).

func mustResolver(t *testing.T, spec string, h Header) *Resolver {
	t.Helper()
	trusted, err := ParseTrusted(spec)
	if err != nil {
		t.Fatal(err)
	}
	r, err := New(trusted, h)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func request(peer string, headers map[string][]string) *http.Request {
	req := httptest.NewRequest(http.MethodGet, "http://learnoo.test/", nil)
	req.RemoteAddr = peer
	for k, vs := range headers {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	return req
}

func xff(vs ...string) map[string][]string { return map[string][]string{"X-Forwarded-For": vs} }

func TestParseTrusted(t *testing.T) {
	ok := map[string]int{
		"":                                    0,
		"  ":                                  0,
		"192.0.2.1":                           1,
		"192.0.2.0/24, 198.51.100.0/24":       2,
		"2001:db8::/32 192.0.2.7\n10.0.0.0/8": 3,
		"2001:db8::1":                         1,
		"::ffff:10.0.0.0/104":                 1,
	}
	for spec, n := range ok {
		got, err := ParseTrusted(spec)
		if err != nil || len(got) != n {
			t.Errorf("%q: %v %v", spec, got, err)
		}
	}
	if got, _ := ParseTrusted("::ffff:10.0.0.0/104"); got[0].String() != "10.0.0.0/8" {
		t.Errorf("mapped range not unmapped: %v", got)
	}
	for _, edge := range []string{"10.0.0.0/8", "::ffff:10.0.0.0/104", "2001:db8::/16"} {
		if _, err := ParseTrusted(edge); err != nil {
			t.Errorf("%q refused: %v", edge, err)
		}
	}
	if got, _ := ParseTrusted("192.0.2.77/24"); got[0].String() != "192.0.2.0/24" {
		t.Errorf("range not masked: %v", got)
	}
	for _, bad := range []string{"0.0.0.0/0", "::/0", "::ffff:0.0.0.0/96", "::ffff:0.0.0.0/97", "::ffff:0.0.0.0/103",
		"0.0.0.0/1", "10.0.0.0/7", "128.0.0.0/2", "2001::/15", "::/1", "::ffff:10.0.0.0/103", "nope", "192.0.2.0/33", "fe80::1%eth0", "192.0.2.1:80", "example.test"} {
		if _, err := ParseTrusted(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	for _, p := range []netip.Prefix{netip.MustParsePrefix("0.0.0.0/0"), netip.MustParsePrefix("10.0.0.0/7"),
		netip.MustParsePrefix("2001::/15"), netip.MustParsePrefix("::ffff:0.0.0.0/97"), {}} {
		if _, err := New([]netip.Prefix{p}, XForwardedFor); err == nil {
			t.Errorf("New accepted %v", p)
		}
	}
	if r, err := New([]netip.Prefix{netip.MustParsePrefix("::ffff:10.0.0.0/104")}, XForwardedFor); err != nil ||
		r.ClientAddr(request("10.1.2.3:1", xff("198.51.100.7"))) != "198.51.100.7" {
		t.Errorf("mapped range passed to New not unmapped: %v", err)
	}
	if _, err := New(nil, "X-Real-IP"); err == nil {
		t.Error("unsupported header accepted")
	}
}

func TestFromEnv(t *testing.T) {
	for _, tc := range []struct {
		trusted, header string
		ok              bool
	}{
		{"", "", true},
		{"192.0.2.1", "", true},
		{"192.0.2.1", "x-forwarded-for", true},
		{"192.0.2.1", "CF-Connecting-IP", true},
		{"", "CF-Connecting-IP", false},
		{"192.0.2.1", "X-Real-IP", false},
		{"0.0.0.0/0", "", false},
	} {
		if _, err := FromEnv(tc.trusted, tc.header); (err == nil) != tc.ok {
			t.Errorf("%q %q: %v", tc.trusted, tc.header, err)
		}
	}
}

func TestNoTrustedProxiesIgnoresHeaders(t *testing.T) {
	for _, r := range []*Resolver{nil, mustResolver(t, "", XForwardedFor)} {
		req := request("192.0.2.10:4000", map[string][]string{
			"X-Forwarded-For":  {"198.51.100.1"},
			"Cf-Connecting-Ip": {"198.51.100.2"},
			"X-Real-Ip":        {"198.51.100.3"},
			"Forwarded":        {"for=198.51.100.4"},
		})
		if got := r.ClientAddr(req); got != "192.0.2.10" {
			t.Fatalf("got %q", got)
		}
	}
}

func TestUntrustedPeerCannotSpoof(t *testing.T) {
	r := mustResolver(t, "10.0.0.0/8", XForwardedFor)
	req := request("192.0.2.10:4000", xff("198.51.100.1", "10.0.0.5"))
	if got := r.ClientAddr(req); got != "192.0.2.10" {
		t.Fatalf("spoofed from untrusted peer: %q", got)
	}
	cf := mustResolver(t, "10.0.0.0/8", CFConnectingIP)
	req = request("192.0.2.10:4000", map[string][]string{"CF-Connecting-IP": {"198.51.100.1"}})
	if got := cf.ClientAddr(req); got != "192.0.2.10" {
		t.Fatalf("CF header from untrusted peer: %q", got)
	}
}

func TestForwardedForChains(t *testing.T) {
	r := mustResolver(t, "10.0.0.0/8, 203.0.113.0/24", XForwardedFor)
	for name, tc := range map[string]struct {
		peer    string
		headers map[string][]string
		want    string
	}{
		"single hop":                    {"10.0.0.2:1", xff("198.51.100.7"), "198.51.100.7"},
		"client prepends a spoof":       {"10.0.0.2:1", xff("1.2.3.4, 198.51.100.7"), "198.51.100.7"},
		"through two trusted hops":      {"10.0.0.2:1", xff("6.6.6.6, 198.51.100.7, 203.0.113.9"), "198.51.100.7"},
		"split over header lines":       {"10.0.0.2:1", xff("6.6.6.6, 198.51.100.7", "203.0.113.9"), "198.51.100.7"},
		"spoofed trusted address":       {"10.0.0.2:1", xff("10.9.9.9, 198.51.100.7"), "198.51.100.7"},
		"all hops trusted: nearest":     {"10.0.0.2:1", xff("203.0.113.4, 10.0.0.3"), "10.0.0.3"},
		"client-chosen trusted entry":   {"10.0.0.2:1", xff("10.66.66.66, 203.0.113.9"), "203.0.113.9"},
		"no header":                     {"10.0.0.2:1", nil, "10.0.0.2"},
		"empty header":                  {"10.0.0.2:1", xff(""), "10.0.0.2"},
		"spaces and empty entries":      {"10.0.0.2:1", xff(" 198.51.100.7 ,"), "10.0.0.2"},
		"with a port":                   {"10.0.0.2:1", xff("198.51.100.7:5555"), "198.51.100.7"},
		"IPv4-mapped IPv6 peer":         {"[::ffff:10.0.0.2]:1", xff("198.51.100.7"), "198.51.100.7"},
		"IPv4-mapped client":            {"10.0.0.2:1", xff("::ffff:198.51.100.7"), "198.51.100.7"},
		"CF header ignored in XFF mode": {"10.0.0.2:1", map[string][]string{"CF-Connecting-IP": {"198.51.100.8"}}, "10.0.0.2"},
	} {
		if got := r.ClientAddr(request(tc.peer, tc.headers)); got != tc.want {
			t.Errorf("%s: got %q, want %q", name, got, tc.want)
		}
	}
}

func TestForwardedForIPv6(t *testing.T) {
	r := mustResolver(t, "2001:db8:aaaa::/48", XForwardedFor)
	for name, tc := range map[string]struct {
		peer, header, want string
	}{
		"plain":          {"[2001:db8:aaaa::1]:443", "2001:db8:1::7", "2001:db8:1::7"},
		"bracketed":      {"[2001:db8:aaaa::1]:443", "[2001:db8:1::7]", "2001:db8:1::7"},
		"bracketed port": {"[2001:db8:aaaa::1]:443", "[2001:db8:1::7]:8080", "2001:db8:1::7"},
		"chain":          {"[2001:db8:aaaa::1]:443", "2001:db8:1::66, 2001:db8:1::7, 2001:db8:aaaa::2", "2001:db8:1::7"},
		"mixed families": {"[2001:db8:aaaa::1]:443", "198.51.100.7", "198.51.100.7"},
		"zone refused":   {"[2001:db8:aaaa::1]:443", "fe80::1%eth0", "2001:db8:aaaa::1"},
		"untrusted peer": {"[2001:db8:bbbb::1]:443", "2001:db8:1::7", "2001:db8:bbbb::1"},
	} {
		if got := r.ClientAddr(request(tc.peer, xff(tc.header))); got != tc.want {
			t.Errorf("%s: got %q, want %q", name, got, tc.want)
		}
	}
}

// A malformed entry stops the walk at the nearest trusted hop: the result
// is never an address the client chose.
func TestForwardedForMalformed(t *testing.T) {
	r := mustResolver(t, "10.0.0.0/8, 203.0.113.0/24", XForwardedFor)
	for _, h := range []string{
		"not-an-ip", "198.51.100.7 extra", "unknown", "198.51.100.256", "[198.51.100.7]",
		"_hidden", "198.51.100.7;", strings.Repeat("1", 100), "\x00",
	} {
		if got := r.ClientAddr(request("10.0.0.2:1", xff(h))); got != "10.0.0.2" {
			t.Errorf("%q: got %q", h, got)
		}
	}
	if got := r.ClientAddr(request("10.0.0.2:1", xff("198.51.100.7, garbage, 203.0.113.9"))); got != "203.0.113.9" {
		t.Errorf("malformed middle hop: got %q", got)
	}
	// Bounded work on a very long chain of trusted hops.
	long := strings.TrimSuffix(strings.Repeat("10.1.1.1, ", 10000), ", ")
	if got := r.ClientAddr(request("10.0.0.2:1", xff("198.51.100.7, "+long))); got != "10.1.1.1" {
		t.Errorf("long chain: got %q", got)
	}
}

func TestCFConnectingIP(t *testing.T) {
	r := mustResolver(t, "10.0.0.0/8", CFConnectingIP)
	cf := func(vs ...string) map[string][]string { return map[string][]string{"CF-Connecting-IP": vs} }
	for name, tc := range map[string]struct {
		headers map[string][]string
		want    string
	}{
		"IPv4":        {cf("198.51.100.7"), "198.51.100.7"},
		"IPv6":        {cf("2001:db8::7"), "2001:db8::7"},
		"absent":      {nil, "10.0.0.2"},
		"malformed":   {cf("198.51.100.7, 1.2.3.4"), "10.0.0.2"},
		"repeated":    {cf("198.51.100.7", "1.2.3.4"), "10.0.0.2"},
		"XFF ignored": {xff("198.51.100.9"), "10.0.0.2"},
	} {
		if got := r.ClientAddr(request("10.0.0.2:1", tc.headers)); got != tc.want {
			t.Errorf("%s: got %q, want %q", name, got, tc.want)
		}
	}
}

func TestPeerNormalization(t *testing.T) {
	for remote, want := range map[string]string{
		"192.0.2.1:80":          "192.0.2.1",
		"[::ffff:192.0.2.1]:80": "192.0.2.1",
		"[fe80::1%eth0]:80":     "fe80::1",
		"[2001:DB8::1]:80":      "2001:db8::1",
		"pipe":                  "pipe",
		"not-an-ip:80":          "not-an-ip",
	} {
		if got := Peer(remote); got != want {
			t.Errorf("%q: got %q, want %q", remote, got, want)
		}
	}
}

func TestNonIPRemoteAddrIsKept(t *testing.T) {
	r := mustResolver(t, "10.0.0.0/8", XForwardedFor)
	if got := r.ClientAddr(request("pipe", xff("198.51.100.7"))); got != "pipe" {
		t.Fatalf("got %q", got)
	}
}

func TestMiddlewareStoresAddress(t *testing.T) {
	r := mustResolver(t, "10.0.0.0/8", XForwardedFor)
	var got string
	var ok bool
	h := Middleware(r, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		got, ok = FromContext(req.Context())
		if req.Header.Get("X-Forwarded-For") != "198.51.100.7" {
			t.Error("header modified")
		}
	}))
	h.ServeHTTP(httptest.NewRecorder(), request("10.0.0.2:1", xff("198.51.100.7")))
	if !ok || got != "198.51.100.7" {
		t.Fatalf("got %q %v", got, ok)
	}
	if _, ok := FromContext(request("10.0.0.2:1", nil).Context()); ok {
		t.Fatal("address without middleware")
	}
}
