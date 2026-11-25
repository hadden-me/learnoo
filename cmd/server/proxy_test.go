package main

import (
	"bytes"
	"io"
	"log/slog"
	"strings"
	"testing"
)

// Trusted proxies and the public base URL (issue 46).

func TestPublicBaseURL(t *testing.T) {
	for raw, want := range map[string]string{
		"https://learnoo.example":       "https://learnoo.example",
		"https://learnoo.example/":      "https://learnoo.example",
		"https://learnoo.example:8443":  "https://learnoo.example:8443",
		"https://[2001:db8::1]":         "https://[2001:db8::1]",
		"HTTPS://learnoo.example":       "https://learnoo.example",
		"https://Learnoo.Example":       "https://learnoo.example",
		"https://xn--lcole-6ra.example": "https://xn--lcole-6ra.example",
		"https://192.0.2.10:443":        "https://192.0.2.10:443",
		"https://learnoo.example:65535": "https://learnoo.example:65535",
	} {
		got, err := publicBaseURL(raw)
		if err != nil || got != want {
			t.Errorf("%q: got %q, %v; want %q", raw, got, err, want)
		}
	}
	for _, raw := range []string{
		"http://learnoo.example",
		"learnoo.example",
		"https://",
		"https:///path",
		"https://user:secret@learnoo.example",
		"https://learnoo.example/learn",
		"https://learnoo.example/?x=1",
		"https://learnoo.example/?",
		"https://learnoo.example/#top",
		"https://learnoo.example\\@evil.example",
		"https://learnoo.example /",
		"javascript:alert(1)",
		"//learnoo.example",
		"https://lécole.example",
		"https://learnoo.example:",
		"https://learnoo.example:0",
		"https://learnoo.example:65536",
		"https://learnoo.example:080",
		"https://learnoo.example:http",
		"https://[fe80::1%25eth0]",
		"https://-bad.example",
		"https://bad..example",
		"https://learnoo.example.",
		"https://under_score.example",
		"https://" + strings.Repeat("a", 64) + ".example",
	} {
		if got, err := publicBaseURL(raw); err == nil {
			t.Errorf("%q accepted as %q", raw, got)
		} else if strings.Contains(err.Error(), "secret") {
			t.Errorf("%q: error echoes the value: %v", raw, err)
		}
	}
}

func TestInviteStudentUsesPublicBaseURL(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	// Validated before the database is touched, whatever LEARNOO_ADDR is.
	t.Setenv("LEARNOO_DATABASE_URL", "")
	t.Setenv("LEARNOO_ADDR", "0.0.0.0:8090")
	t.Setenv("LEARNOO_PUBLIC_BASE_URL", "http://learnoo.example")
	if err := run([]string{"invite-student", "-email", "a@example.test"}, nil, io.Discard, log); err == nil || !strings.Contains(err.Error(), "https") {
		t.Fatalf("http base URL: %v", err)
	}
	t.Setenv("LEARNOO_PUBLIC_BASE_URL", "")
	if err := run([]string{"invite-student", "-email", "a@example.test"}, nil, io.Discard, log); err == nil || !strings.Contains(err.Error(), "LEARNOO_PUBLIC_BASE_URL") {
		t.Fatalf("container address without a base URL: %v", err)
	}

	t.Setenv("LEARNOO_DATABASE_URL", schemaURL(t, true))
	t.Setenv("LEARNOO_DATABASE_PASSWORD_FILE", "")
	t.Setenv("LEARNOO_PUBLIC_BASE_URL", "https://learnoo.example/")
	var out bytes.Buffer
	if err := run([]string{"invite-student", "-email", "test-invited@example.test"}, nil, &out, log); err != nil {
		t.Fatal(err)
	}
	var token string
	for _, line := range strings.Split(out.String(), "\n") {
		if rest, ok := strings.CutPrefix(line, "https://learnoo.example/invite/"); ok {
			token = rest
		}
	}
	if len(token) != 43 || strings.Contains(out.String(), "0.0.0.0") {
		t.Fatalf("unexpected output: %s", out.String())
	}
}

func TestServeValidatesProxySettingsFirst(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	t.Setenv("LEARNOO_DATABASE_URL", "")
	t.Setenv("LEARNOO_MEDIA_DIR", "")
	t.Setenv("LEARNOO_ADDR", "")
	for spec, header := range map[string]string{
		"0.0.0.0/0":  "",
		"10.0.0.0/7": "",
		"::/0":       "",
		"not-a-cidr": "",
		"192.0.2.1":  "X-Real-IP",
		"":           "CF-Connecting-IP",
	} {
		t.Setenv("LEARNOO_TRUSTED_PROXIES", spec)
		t.Setenv("LEARNOO_CLIENT_ADDRESS_HEADER", header)
		err := run([]string{"serve"}, nil, io.Discard, log)
		if err == nil || strings.Contains(err.Error(), "LEARNOO_MEDIA_DIR") {
			t.Errorf("%q %q: got %v", spec, header, err)
		}
	}
	// A bad public base URL also stops serve, before the database.
	t.Setenv("LEARNOO_TRUSTED_PROXIES", "")
	t.Setenv("LEARNOO_CLIENT_ADDRESS_HEADER", "")
	t.Setenv("LEARNOO_PUBLIC_BASE_URL", "http://learnoo.example")
	if err := run([]string{"serve"}, nil, io.Discard, log); err == nil || !strings.Contains(err.Error(), "LEARNOO_PUBLIC_BASE_URL") {
		t.Errorf("bad base URL: %v", err)
	}
	t.Setenv("LEARNOO_PUBLIC_BASE_URL", "https://learnoo.example")
	// Valid settings pass on to the next check.
	t.Setenv("LEARNOO_TRUSTED_PROXIES", "192.0.2.0/24, 2001:db8::/32")
	t.Setenv("LEARNOO_CLIENT_ADDRESS_HEADER", "")
	if err := run([]string{"serve"}, nil, io.Discard, log); err == nil || !strings.Contains(err.Error(), "LEARNOO_MEDIA_DIR") {
		t.Errorf("valid settings: %v", err)
	}
}
