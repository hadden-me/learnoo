package server_test

import (
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/hadden-me/learnoo/internal/auth"
	"github.com/hadden-me/learnoo/internal/clientip"
)

// Per-address throttles behind a simulated reverse proxy (issue 46). The
// handler is wrapped as cmd/server wraps it. Documentation addresses only.

const proxyPeer = "10.0.0.2"

// behindProxy installs the client address middleware with trustedSpec.
func (e *env) behindProxy(trustedSpec string, header clientip.Header) {
	e.t.Helper()
	trusted, err := clientip.ParseTrusted(trustedSpec)
	if err != nil {
		e.t.Fatal(err)
	}
	r, err := clientip.New(trusted, header)
	if err != nil {
		e.t.Fatal(err)
	}
	e.h = clientip.Middleware(r, e.h)
}

// viaProxy is a request relayed by the proxy for client: the proxy appends
// the client's address to whatever X-Forwarded-For the client sent.
func viaProxy(client string, clientSent ...string) reqOpt {
	return func(r *http.Request) {
		r.RemoteAddr = proxyPeer + ":40000"
		r.Header.Set("X-Forwarded-For", strings.Join(append(clientSent, client), ", "))
	}
}

func TestLoginThrottleBehindTrustedProxy(t *testing.T) {
	e := newEnv(t)
	e.behindProxy("10.0.0.0/8", clientip.XForwardedFor)
	attacker := "198.51.100.66"
	for i := 0; i < e.cfg.AddressLimit; i++ {
		// Prepending a different address each time does not help.
		spoof := "203.0.113." + strconv.Itoa(i+1)
		w := e.login("user"+strconv.Itoa(i)+"@example.test", "wrong-password-123", viaProxy(attacker, spoof))
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: %d", i+1, w.Code)
		}
	}
	if w := e.login(testEmail, testPassword, viaProxy(attacker, "203.0.113.200")); w.Code != http.StatusTooManyRequests {
		t.Fatalf("attacker over the limit: %d", w.Code)
	}
	// Another client behind the same proxy still signs in.
	if w := e.login(testEmail, testPassword, viaProxy("198.51.100.7")); w.Code != http.StatusOK {
		t.Fatalf("other client behind the proxy: %d %s", w.Code, w.Body)
	}
	// IPv6 clients are keyed separately too.
	if w := e.login(testEmail, testPassword, viaProxy("2001:db8::7")); w.Code != http.StatusOK {
		t.Fatalf("IPv6 client: %d", w.Code)
	}
}

func TestForwardedHeadersIgnoredWithoutTrustedProxies(t *testing.T) {
	e := newEnv(t)
	e.behindProxy("", clientip.XForwardedFor) // the default configuration
	for i := 0; i < e.cfg.AddressLimit; i++ {
		w := e.login("user"+strconv.Itoa(i)+"@example.test", "wrong-password-123", viaProxy("198.51.100."+strconv.Itoa(i+1)))
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: %d", i+1, w.Code)
		}
	}
	// Every request counted against the peer: the header was not used.
	if w := e.login(testEmail, testPassword, viaProxy("198.51.100.200")); w.Code != http.StatusTooManyRequests {
		t.Fatalf("forwarded header honoured without trusted proxies: %d", w.Code)
	}
}

func TestUntrustedPeerCannotSpoofThrottleKey(t *testing.T) {
	e := newEnv(t)
	e.behindProxy("10.0.0.0/8", clientip.XForwardedFor)
	direct := func(spoof string) reqOpt {
		return func(r *http.Request) {
			r.RemoteAddr = "192.0.2.99:40000" // not a trusted proxy
			r.Header.Set("X-Forwarded-For", spoof)
			r.Header.Set("CF-Connecting-IP", spoof)
		}
	}
	for i := 0; i < e.cfg.AddressLimit; i++ {
		w := e.login("user"+strconv.Itoa(i)+"@example.test", "wrong-password-123", direct("198.51.100."+strconv.Itoa(i+1)))
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: %d", i+1, w.Code)
		}
	}
	if w := e.login(testEmail, testPassword, direct("198.51.100.250")); w.Code != http.StatusTooManyRequests {
		t.Fatalf("spoofed header changed the key: %d", w.Code)
	}
}

func TestInvitationThrottleBehindTrustedProxy(t *testing.T) {
	f := newReader(t)
	f.behindProxy("10.0.0.0/8", clientip.XForwardedFor)
	bad := "/invite/" + strings.Repeat("B", 43)
	for i := range f.cfg.InvitationLimit {
		if w := f.page(apiUser{}, http.MethodGet, bad, nil, viaProxy("198.51.100.77")); w.Code != http.StatusNotFound {
			t.Fatalf("attempt %d: %d", i, w.Code)
		}
	}
	wantPage(t, "throttled client", f.page(apiUser{}, http.MethodGet, bad, nil, viaProxy("198.51.100.77", "203.0.113.1")),
		http.StatusTooManyRequests, "Trop de tentatives")
	if w := f.page(apiUser{}, http.MethodGet, bad, nil, viaProxy("198.51.100.78")); w.Code != http.StatusNotFound {
		t.Fatalf("other client behind the proxy: %d", w.Code)
	}
}

func TestErrorReportThrottleBehindTrustedProxy(t *testing.T) {
	f := newReaderConfig(t, func(c *auth.Config) {
		c.ErrorReportAccountLimit = 10
		c.ErrorReportAddressLimit = 2
	})
	f.behindProxy("10.0.0.0/8", clientip.CFConnectingIP)
	p := f.publishNew("TEST Leçon derrière le proxy", "fr", nil)
	other := f.invitedStudent("test-student-two@example.test", "TEST Deux")
	path := lessonPath(p.publicID)
	cf := func(client string) reqOpt {
		return func(r *http.Request) {
			r.RemoteAddr = proxyPeer + ":40000"
			r.Header.Set("CF-Connecting-IP", client)
		}
	}
	wantRedirect(t, "first", f.fileReport(f.student, p, "Erreur 1.", cf("198.51.100.50")), path+"/reported")
	wantRedirect(t, "second", f.fileReport(f.student, p, "Erreur 2.", cf("198.51.100.50")), path+"/reported")
	wantPage(t, "address limit", f.fileReport(other, p, "Erreur 3.", cf("198.51.100.50")), http.StatusTooManyRequests, "Trop de tentatives")
	wantRedirect(t, "other client", f.fileReport(other, p, "Erreur 4.", cf("198.51.100.51")), path+"/reported")
}
