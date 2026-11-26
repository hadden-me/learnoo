package main

import (
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"runtime/debug"
	"strconv"
	"strings"
)

// Go memory limit (docs/measurements/upload-memory.md). Without a soft
// limit the garbage collector lets the heap grow to about twice the live
// heap, so image uploads that fit the container's memory limit could still
// get the process killed. serve therefore sets GOMEMLIMIT from the
// container's cgroup memory limit unless GOMEMLIMIT is set explicitly.

const (
	// minAppMemory is the smallest container limit the measured worst case
	// fits with collector headroom.
	minAppMemory = 512 << 20
)

// cgroupFiles are the container memory limits for cgroup v2 and v1.
var cgroupFiles = []string{"/sys/fs/cgroup/memory.max", "/sys/fs/cgroup/memory/memory.limit_in_bytes"}

// containerMemoryLimit returns the cgroup memory limit in bytes, or false if
// there is none (not in a container, or unlimited).
func containerMemoryLimit(read func(string) ([]byte, error)) (int64, bool) {
	for _, name := range cgroupFiles {
		b, err := read(name)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return 0, false
		}
		v, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
		// "max" (v2) or a huge value (v1) means unlimited.
		if err != nil || v <= 0 || v >= 1<<60 {
			return 0, false
		}
		return v, true
	}
	return 0, false
}

// applyMemoryLimit sets the Go soft memory limit to three quarters of the
// container limit (the rest covers the binary, page cache and memory outside
// the Go runtime's accounting), unless GOMEMLIMIT is set (the runtime has then
// applied it already).
func applyMemoryLimit(log *slog.Logger, read func(string) ([]byte, error)) {
	if os.Getenv("GOMEMLIMIT") != "" {
		log.Info("Go memory limit from GOMEMLIMIT", "bytes", debug.SetMemoryLimit(-1))
		return
	}
	limit, ok := containerMemoryLimit(read)
	if !ok {
		log.Info("no container memory limit found: Go memory limit not set")
		return
	}
	soft := limit - limit/4
	debug.SetMemoryLimit(soft)
	log.Info("Go memory limit set from the container limit", "container_bytes", limit, "gomemlimit_bytes", soft)
	if limit < minAppMemory {
		log.Warn("container memory limit is below the documented minimum for image uploads",
			"container_bytes", limit, "minimum_bytes", minAppMemory)
	}
}
