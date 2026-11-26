package main

import (
	"bytes"
	"errors"
	"io/fs"
	"log/slog"
	"math"
	"runtime/debug"
	"strings"
	"testing"
)

func cgroup(files map[string]string) func(string) ([]byte, error) {
	return func(name string) ([]byte, error) {
		if v, ok := files[name]; ok {
			return []byte(v), nil
		}
		return nil, fs.ErrNotExist
	}
}

func TestContainerMemoryLimit(t *testing.T) {
	v2, v1 := cgroupFiles[0], cgroupFiles[1]
	for name, c := range map[string]struct {
		files map[string]string
		want  int64
		ok    bool
	}{
		"none":         {nil, 0, false},
		"v2":           {map[string]string{v2: "536870912\n"}, 512 << 20, true},
		"v2 unlimited": {map[string]string{v2: "max\n"}, 0, false},
		"v1":           {map[string]string{v1: "1073741824\n"}, 1 << 30, true},
		"v1 unlimited": {map[string]string{v1: "9223372036854771712\n"}, 0, false},
		"v2 first":     {map[string]string{v2: "536870912", v1: "1073741824"}, 512 << 20, true},
		"garbage":      {map[string]string{v2: "lots"}, 0, false},
	} {
		got, ok := containerMemoryLimit(cgroup(c.files))
		if got != c.want || ok != c.ok {
			t.Errorf("%s: %d %v", name, got, ok)
		}
	}
	unreadable := func(string) ([]byte, error) { return nil, errors.New("permission denied") }
	if _, ok := containerMemoryLimit(unreadable); ok {
		t.Error("unreadable file gave a limit")
	}
}

func TestApplyMemoryLimit(t *testing.T) {
	defer debug.SetMemoryLimit(debug.SetMemoryLimit(-1))
	var out bytes.Buffer
	log := slog.New(slog.NewTextHandler(&out, nil))

	t.Setenv("GOMEMLIMIT", "")
	debug.SetMemoryLimit(math.MaxInt64)
	applyMemoryLimit(log, cgroup(map[string]string{cgroupFiles[0]: "536870912"}))
	if got := debug.SetMemoryLimit(-1); got != 384<<20 {
		t.Fatalf("limit %d, want 384 MiB", got)
	}
	if strings.Contains(out.String(), "below the documented minimum") {
		t.Fatal("512 MiB reported as too small")
	}

	debug.SetMemoryLimit(math.MaxInt64)
	applyMemoryLimit(log, cgroup(map[string]string{cgroupFiles[0]: "268435456"}))
	if !strings.Contains(out.String(), "below the documented minimum") {
		t.Fatal("256 MiB not reported")
	}

	// An explicit GOMEMLIMIT (already applied by the runtime) is kept.
	t.Setenv("GOMEMLIMIT", "300MiB")
	debug.SetMemoryLimit(300 << 20)
	applyMemoryLimit(log, cgroup(map[string]string{cgroupFiles[0]: "536870912"}))
	if got := debug.SetMemoryLimit(-1); got != 300<<20 {
		t.Fatalf("explicit GOMEMLIMIT replaced: %d", got)
	}
}
