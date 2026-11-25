package reader

import (
	"strings"
	"testing"
)

// Pure helpers; database-backed behaviour is tested through the server
// handler in internal/server/reader_test.go.

func TestSafeNext(t *testing.T) {
	for in, want := range map[string]string{
		"/learn":              "/learn",
		"/learn/lessons/0a-b": "/learn/lessons/0a-b",
		"":                    "/learn",
		"/":                   "/learn",
		"//evil.test/learn":   "/learn",
		"https://evil.test":   "/learn",
		"/learnx":             "/learn",
		"/learn/":             "/learn",
		"/learn//x":           "/learn",
		"/learn/../api":       "/learn",
		"/learn/./x":          "/learn",
		"/learn?q=1":          "/learn",
		`/learn\evil`:         "/learn",
		"/api/auth/session":   "/learn",
		"/learn/%2e%2e":       "/learn",
		"/studio":             "/studio",
		"/studio/lessons/12":  "/studio/lessons/12",
		"/studio/":            "/learn",
		"/studiox":            "/learn",
		"/studio/../api":      "/learn",
		"/studio//evil.test":  "/learn",
	} {
		if got := safeNext(in); got != want {
			t.Errorf("safeNext(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNormalizeQuery(t *testing.T) {
	for in, want := range map[string]struct {
		q  string
		ok bool
	}{
		"  Équations  du\tsecond ": {"Équations du second", true},
		"a":                        {"a", false},
		" a ":                      {"a", false},
		"ab":                       {"ab", true},
		"a\x00b":                   {"ab", true},
		strings.Repeat("é", 100):   {strings.Repeat("é", 100), true},
		strings.Repeat("é", 101):   {strings.Repeat("é", 101), false},
		"\xff\xfe":                 {"", false},
		"% _":                      {"% _", true},
	} {
		q, ok := NormalizeQuery(in)
		if q != want.q || ok != want.ok {
			t.Errorf("NormalizeQuery(%q) = %q, %v; want %q, %v", in, q, ok, want.q, want.ok)
		}
	}
}

func TestValidPublicIDAndDir(t *testing.T) {
	for id, ok := range map[string]bool{
		"0b6b1f8e-3c1a-4c55-9a51-2f1a0c9e7d10": true,
		"0B6B1F8E-3C1A-4C55-9A51-2F1A0C9E7D10": false,
		"0b6b1f8e3c1a4c559a512f1a0c9e7d10":     false,
		"0b6b1f8e-3c1a-4c55-9a51-2f1a0c9e7d1":  false,
		"' OR 1=1 --":                          false,
	} {
		if ValidPublicID(id) != ok {
			t.Errorf("ValidPublicID(%q) != %v", id, ok)
		}
	}
	if Dir("ar") != "rtl" || Dir("fr") != "ltr" || Dir("") != "ltr" {
		t.Error("Dir")
	}
}

func TestTemplatesParse(t *testing.T) {
	h := New(Config{})
	for _, name := range []string{"login", "invite", "dashboard", "subject", "lesson", "search", "error"} {
		if h.pages[name] == nil || h.pages[name].Lookup("layout") == nil || h.pages[name].Lookup("content") == nil {
			t.Errorf("page %s incomplete", name)
		}
	}
}
