package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOpenStudio(t *testing.T) {
	t.Setenv("LEARNOO_STUDIO_DIR", "")
	if root, err := openStudio(); root != nil || err != nil {
		t.Fatalf("unset: %v %v", root, err)
	}
	empty := t.TempDir()
	for value, want := range map[string]string{
		"web/studio/dist":               "absolute path",
		filepath.Join(empty, "missing"): "LEARNOO_STUDIO_DIR",
		empty:                           "no index.html",
	} {
		t.Setenv("LEARNOO_STUDIO_DIR", value)
		root, err := openStudio()
		if root != nil || err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: %v %v, want error with %q", value, root, err, want)
		}
	}
	if err := os.WriteFile(filepath.Join(empty, "index.html"), []byte("<!doctype html>"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LEARNOO_STUDIO_DIR", empty)
	root, err := openStudio()
	if err != nil || root == nil {
		t.Fatalf("valid: %v", err)
	}
	root.Close()
}
