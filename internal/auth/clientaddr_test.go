package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hadden-me/learnoo/internal/clientip"
)

// Without the middleware the fallback normalises like it (issue 46).
func TestClientAddrFallbackNormalizes(t *testing.T) {
	for remote, want := range map[string]string{
		"192.0.2.1:4000":          "192.0.2.1",
		"[::ffff:192.0.2.1]:4000": "192.0.2.1",
		"[fe80::1%eth0]:4000":     "fe80::1",
		"[2001:DB8::1]:4000":      "2001:db8::1",
	} {
		r := httptest.NewRequest(http.MethodGet, "http://learnoo.test/", nil)
		r.RemoteAddr = remote
		r.Header.Set("X-Forwarded-For", "198.51.100.9")
		if got := ClientAddr(r); got != want {
			t.Errorf("%q: got %q, want %q", remote, got, want)
		}
	}
}

func TestClientAddrUsesMiddleware(t *testing.T) {
	trusted, err := clientip.ParseTrusted("10.0.0.0/8")
	if err != nil {
		t.Fatal(err)
	}
	res, err := clientip.New(trusted, clientip.XForwardedFor)
	if err != nil {
		t.Fatal(err)
	}
	var got string
	h := clientip.Middleware(res, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { got = ClientAddr(r) }))
	r := httptest.NewRequest(http.MethodGet, "http://learnoo.test/", nil)
	r.RemoteAddr = "10.0.0.2:4000"
	r.Header.Set("X-Forwarded-For", "198.51.100.9")
	h.ServeHTTP(httptest.NewRecorder(), r)
	if got != "198.51.100.9" {
		t.Fatalf("got %q", got)
	}
}
