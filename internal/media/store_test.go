package media

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func newStore(t *testing.T) (*Store, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "media")
	s, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, dir
}

func processed(t *testing.T, data []byte) *Asset {
	t.Helper()
	a, err := process(data, "")
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// files lists every entry below dir (relative paths), including temporaries.
func files(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			rel, _ := filepath.Rel(dir, p)
			out = append(out, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestStorePutAndOpen(t *testing.T) {
	s, dir := newStore(t)
	for _, a := range []*Asset{processed(t, pngBytes(t, 3, 3)), processed(t, jpegBytes(t, 9, 9))} {
		key, err := s.Put(a)
		if err != nil {
			t.Fatal(err)
		}
		if key != a.Key() || !strings.HasPrefix(key, a.SHA256()) {
			t.Fatalf("key %q for sha %s", key, a.SHA256())
		}
		want := filepath.Join(dir, key[0:2], key[2:4], key)
		fi, err := os.Stat(want)
		if err != nil {
			t.Fatalf("not stored at %s: %v", want, err)
		}
		if fi.Mode().Perm() != 0o444 {
			t.Errorf("file mode %v, want 0444", fi.Mode().Perm())
		}
		di, err := os.Stat(filepath.Dir(want))
		// 0750 before umask: never group-writable or world-accessible.
		if err != nil || di.Mode().Perm()&^0o750 != 0 {
			t.Errorf("shard dir mode %v err %v, want at most 0750", di.Mode().Perm(), err)
		}
		if ct, err := ContentTypeForKey(key); err != nil || ct != a.ContentType() {
			t.Errorf("ContentTypeForKey = %q, %v", ct, err)
		}
		f, err := s.Open(key)
		if err != nil {
			t.Fatal(err)
		}
		got, _ := io.ReadAll(f)
		f.Close()
		if !bytes.Equal(got, a.Data()) {
			t.Fatal("stored bytes differ")
		}
	}
	if n := len(files(t, dir)); n != 2 {
		t.Fatalf("%d files stored, want 2 (no temporaries left)", n)
	}
}

func TestStoreIdempotent(t *testing.T) {
	s, dir := newStore(t)
	// Different uploads with identical pixels canonicalise to the same asset.
	in := pngBytes(t, 4, 4)
	a := processed(t, in)
	b := processed(t, insertBeforeIEND(in, pngChunk("tEXt", []byte("Comment\x00other"))))
	k1, err := s.Put(a)
	if err != nil {
		t.Fatal(err)
	}
	k2, err := s.Put(b)
	if err != nil {
		t.Fatal(err)
	}
	if k1 != k2 {
		t.Fatalf("keys differ: %s %s", k1, k2)
	}
	if n := len(files(t, dir)); n != 1 {
		t.Fatalf("%d files, want 1", n)
	}
}

func TestStoreConcurrentSameContent(t *testing.T) {
	s, dir := newStore(t)
	a := processed(t, pngBytes(t, 8, 8))
	const n = 32
	var wg sync.WaitGroup
	keys := make([]string, n)
	errs := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			keys[i], errs[i] = s.Put(a)
		}()
	}
	wg.Wait()
	for i := range n {
		if errs[i] != nil || keys[i] != a.Key() {
			t.Fatalf("writer %d: key %q err %v", i, keys[i], errs[i])
		}
	}
	if got := files(t, dir); len(got) != 1 {
		t.Fatalf("files %v, want exactly one", got)
	}
}

func TestStoreNeverReplaces(t *testing.T) {
	s, dir := newStore(t)
	a := processed(t, pngBytes(t, 2, 2))
	key := a.Key()
	p := filepath.Join(dir, key[0:2], key[2:4], key)
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("other content"), 0o444); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Put(a); !errors.Is(err, ErrKeyConflict) {
		t.Fatalf("got %v, want ErrKeyConflict", err)
	}
	if got, _ := os.ReadFile(p); string(got) != "other content" {
		t.Fatal("existing file was replaced")
	}
}

func TestStoreRejectsUnprocessedAssets(t *testing.T) {
	s, dir := newStore(t)
	a := processed(t, pngBytes(t, 2, 2))
	flipped := a.Data()
	flipped[len(flipped)-1] ^= 1
	svg := []byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`)
	svgSum := sha256.Sum256(svg)

	tests := []struct {
		name  string
		asset *Asset
		want  error
	}{
		{"nil", nil, ErrNotProcessed},
		{"zero value", &Asset{}, ErrNotProcessed},
		{"hand-built copy of a real asset", &Asset{data: a.data, sha256: a.sha256, contentType: a.contentType, width: 2, height: 2}, ErrNotProcessed},
		{"hand-built svg", &Asset{data: svg, sha256: hex.EncodeToString(svgSum[:]), contentType: ContentTypePNG}, ErrNotProcessed},
		{"processed, bytes changed", &Asset{processed: true, data: flipped, sha256: a.sha256, contentType: a.contentType}, ErrHashMismatch},
		{"processed, traversal hash", &Asset{processed: true, data: a.data, sha256: "../../../etc/passwd", contentType: a.contentType}, ErrHashMismatch},
		{"processed, png bytes as jpeg", &Asset{processed: true, data: a.data, sha256: a.sha256, contentType: ContentTypeJPEG}, ErrNotProcessed},
		{"processed, png bytes as webp", &Asset{processed: true, data: a.data, sha256: a.sha256, contentType: ContentTypeWebP}, ErrNotProcessed},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := s.Put(tc.asset); !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
		})
	}
	if n := len(files(t, dir)); n != 0 {
		t.Fatalf("%d files written", n)
	}
}

// Data returns a copy, so callers cannot change an asset after Process.
func TestAssetDataIsACopy(t *testing.T) {
	a := processed(t, pngBytes(t, 2, 2))
	d := a.Data()
	d[0] ^= 0xFF
	s, _ := newStore(t)
	if _, err := s.Put(a); err != nil {
		t.Fatal(err)
	}
}

func TestStoreConcurrentLostRace(t *testing.T) {
	s, dir := newStore(t)
	a := processed(t, pngBytes(t, 8, 8))
	const n = 8
	// Hold every writer before Link until all have written their temporary
	// file, so all but one must take the lost-race (EEXIST) branch.
	var arrived sync.WaitGroup
	arrived.Add(n)
	release := make(chan struct{})
	go func() { arrived.Wait(); close(release) }()
	var lost atomic.Int64
	testHookBeforeLink = func(string) error { arrived.Done(); <-release; return nil }
	testHookLostRace = func() { lost.Add(1) }
	defer func() { testHookBeforeLink, testHookLostRace = nil, nil }()

	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = s.Put(a)
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("writer %d: %v", i, err)
		}
	}
	if got := lost.Load(); got != n-1 {
		t.Fatalf("lost-race branch taken %d times, want %d", got, n-1)
	}
	if got := files(t, dir); len(got) != 1 {
		t.Fatalf("files %v, want exactly one", got)
	}
}

func TestStoreLostRaceToDifferentContent(t *testing.T) {
	s, dir := newStore(t)
	a := processed(t, pngBytes(t, 2, 2))
	p := filepath.Join(dir, filepath.FromSlash(relPath(a.Key())))
	// Another file appears under the key between the existence check and Link.
	testHookBeforeLink = func(string) error { return os.WriteFile(p, []byte("other content"), 0o444) }
	defer func() { testHookBeforeLink = nil }()
	if _, err := s.Put(a); !errors.Is(err, ErrKeyConflict) {
		t.Fatalf("got %v, want ErrKeyConflict", err)
	}
	if got, _ := os.ReadFile(p); string(got) != "other content" {
		t.Fatal("existing file was replaced")
	}
	if got := files(t, dir); len(got) != 1 {
		t.Fatalf("files %v: temporary file left behind", got)
	}
}

func TestStoreRemovesTempOnFailure(t *testing.T) {
	injected := errors.New("injected failure")
	for name, set := range map[string]func(){
		"write": func() { testHookAfterWrite = func() error { return injected } },
		"link":  func() { testHookBeforeLink = func(string) error { return injected } },
	} {
		t.Run(name, func(t *testing.T) {
			s, dir := newStore(t)
			set()
			defer func() { testHookAfterWrite, testHookBeforeLink = nil, nil }()
			if _, err := s.Put(processed(t, pngBytes(t, 2, 2))); !errors.Is(err, injected) {
				t.Fatalf("got %v", err)
			}
			if got := files(t, dir); len(got) != 0 {
				t.Fatalf("files left behind: %v", got)
			}
		})
	}
}

func TestKeyValidation(t *testing.T) {
	sha := strings.Repeat("ab", 32)
	valid := []string{sha + ".png", sha + ".jpg"}
	invalid := []string{
		"",
		sha,
		sha + ".jpeg",
		sha + ".webp",
		sha + ".svg",
		sha + ".PNG",
		strings.ToUpper(sha) + ".png",
		sha[:63] + ".png",
		sha + "0.png",
		sha + ".png/",
		sha + ".png\x00",
		sha + ".png\n",
		"../" + sha + ".png",
		"ab/ab/" + sha + ".png",
		"/" + sha + ".png",
		"/etc/passwd",
		"..",
		strings.Repeat("g", 64) + ".png",
		sha[:62] + ".." + ".png",
	}
	for _, k := range valid {
		if !ValidKey(k) {
			t.Errorf("valid key %q rejected", k)
		}
	}
	s, _ := newStore(t)
	for _, k := range invalid {
		if ValidKey(k) {
			t.Errorf("invalid key %q accepted", k)
		}
		if _, err := s.Open(k); !errors.Is(err, ErrInvalidKey) {
			t.Errorf("Open(%q) = %v, want ErrInvalidKey", k, err)
		}
		if _, err := ContentTypeForKey(k); !errors.Is(err, ErrInvalidKey) {
			t.Errorf("ContentTypeForKey(%q) = %v", k, err)
		}
	}
	if _, err := s.Open(valid[0]); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("missing key: got %v, want fs.ErrNotExist", err)
	}
}

func TestStoreOpenDoesNotFollowSymlinkOutOfRoot(t *testing.T) {
	s, dir := newStore(t)
	outside := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(outside, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	key := strings.Repeat("cd", 32) + ".png"
	shard := filepath.Join(dir, key[0:2], key[2:4])
	if err := os.MkdirAll(shard, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(shard, key)); err != nil {
		t.Fatal(err)
	}
	if f, err := s.Open(key); err == nil {
		f.Close()
		t.Fatal("opened a symlink escaping the store root")
	}
}
