package media

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"regexp"
)

// Storage errors.
var (
	ErrInvalidKey = errors.New("media: invalid storage key")
	// ErrKeyConflict means a file already exists under the key but its content
	// does not hash to the key. Stored files are never replaced; this
	// indicates storage corruption and needs operator attention.
	ErrKeyConflict  = errors.New("media: existing file does not match its key")
	ErrHashMismatch = errors.New("media: asset data does not match its SHA-256")
	// ErrNotProcessed means the Asset was not produced by Process or its
	// bytes do not match its content type.
	ErrNotProcessed = errors.New("media: asset was not produced by Process")
)

// Test hooks; nil in production.
var (
	testHookAfterWrite func() error
	testHookBeforeLink func(rel string) error
	testHookLostRace   func()
)

const (
	dirPerm  fs.FileMode = 0o750
	filePerm fs.FileMode = 0o444
)

// keyPattern is the only accepted key form: lowercase hex SHA-256 of the stored
// bytes and the stored type's extension. It admits no separators, dots other
// than the extension, or uppercase, so a key can never name a path outside its
// shard directory.
var keyPattern = regexp.MustCompile(`^[0-9a-f]{64}\.(png|jpg)$`)

// ValidKey reports whether key is a well-formed storage key.
func ValidKey(key string) bool {
	return keyPattern.MatchString(key)
}

// ContentTypeForKey returns the stored content type for a valid key.
func ContentTypeForKey(key string) (string, error) {
	if !ValidKey(key) {
		return "", ErrInvalidKey
	}
	if path.Ext(key) == ".jpg" {
		return ContentTypeJPEG, nil
	}
	return ContentTypePNG, nil
}

func keyFor(sha, contentType string) string {
	if contentType == ContentTypeJPEG {
		return sha + ".jpg"
	}
	return sha + ".png"
}

// relPath maps a valid key to "ab/cd/<key>" inside the store root.
func relPath(key string) string {
	return path.Join(key[0:2], key[2:4], key)
}

// Store is immutable, content-addressed image storage rooted at one directory
// (the persistent media volume; later configured by LEARNOO_MEDIA_DIR). Every
// access goes through os.Root, so paths cannot escape the directory even via
// symlinks.
type Store struct {
	root *os.Root
}

// OpenStore creates dir (0750) if needed and opens it as a Store.
func OpenStore(dir string) (*Store, error) {
	if dir == "" {
		return nil, errors.New("media: empty storage directory")
	}
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		return nil, fmt.Errorf("media: create storage directory: %w", err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("media: open storage directory: %w", err)
	}
	return &Store{root: root}, nil
}

// Close releases the store's directory handle.
func (s *Store) Close() error {
	return s.root.Close()
}

// Put stores an asset returned by Process and returns its key. Any other
// Asset value is rejected, as is one whose bytes no longer match its hash or
// content type.
//
// Writing is atomic and never overwrites: data goes to a temporary file in the
// shard directory, is fsynced and made read-only, then hard-linked to its final
// name, and the shard directories are fsynced before Put returns. The media
// volume must therefore support hard links. If the key already exists with the
// same content (an identical or concurrent upload) Put succeeds without
// writing; if it exists with different content Put returns ErrKeyConflict and
// leaves the file untouched.
//
// A crash can leave ".tmp-*" files in shard directories. They are harmless:
// they never match a key, so Open cannot serve them, and they may be deleted.
func (s *Store) Put(a *Asset) (string, error) {
	if a == nil || !a.processed {
		return "", ErrNotProcessed
	}
	sum := sha256.Sum256(a.data)
	if hex.EncodeToString(sum[:]) != a.sha256 {
		return "", ErrHashMismatch
	}
	if f, ok := sniff(a.data); !ok || f.contentType != a.contentType ||
		(a.contentType != ContentTypePNG && a.contentType != ContentTypeJPEG) {
		return "", ErrNotProcessed
	}
	key := a.Key()
	if !ValidKey(key) {
		return "", ErrInvalidKey
	}
	rel := relPath(key)
	dir := path.Dir(rel)
	if err := s.root.MkdirAll(dir, dirPerm); err != nil {
		return "", fmt.Errorf("media: create shard directory: %w", err)
	}

	if ok, err := s.matches(rel, a.data); err == nil {
		if !ok {
			return "", ErrKeyConflict
		}
		// The file may have just been linked by a writer that has not yet
		// synced; make it durable before reporting success.
		return key, s.syncDirs(dir)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}

	tmp, err := s.writeTemp(dir, a.data)
	if err != nil {
		return "", err
	}
	defer s.root.Remove(tmp)

	if testHookBeforeLink != nil {
		if err := testHookBeforeLink(rel); err != nil {
			return "", err
		}
	}
	if err := s.root.Link(tmp, rel); err != nil {
		if !errors.Is(err, fs.ErrExist) {
			return "", fmt.Errorf("media: publish file: %w", err)
		}
		// Lost a race with another writer: accept only identical content.
		if testHookLostRace != nil {
			testHookLostRace()
		}
		ok, err := s.matches(rel, a.data)
		if err != nil {
			return "", err
		}
		if !ok {
			return "", ErrKeyConflict
		}
	}
	return key, s.syncDirs(dir)
}

func (s *Store) writeTemp(dir string, data []byte) (string, error) {
	var rnd [16]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		return "", err
	}
	name := path.Join(dir, ".tmp-"+hex.EncodeToString(rnd[:]))
	f, err := s.root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, filePerm)
	if err != nil {
		return "", fmt.Errorf("media: create temporary file: %w", err)
	}
	_, werr := f.Write(data)
	if werr == nil && testHookAfterWrite != nil {
		werr = testHookAfterWrite()
	}
	if werr == nil {
		werr = f.Chmod(filePerm) // independent of umask
	}
	if werr == nil {
		werr = f.Sync()
	}
	cerr := f.Close()
	if werr == nil {
		werr = cerr
	}
	if werr != nil {
		_ = s.root.Remove(name)
		return "", fmt.Errorf("media: write temporary file: %w", werr)
	}
	return name, nil
}

// syncDirs fsyncs the shard directory and its parents up to the root, so newly
// created directories and the new link survive a crash.
func (s *Store) syncDirs(dir string) error {
	for {
		if err := s.syncDir(dir); err != nil {
			return err
		}
		if dir == "." {
			return nil
		}
		dir = path.Dir(dir)
	}
}

func (s *Store) syncDir(dir string) error {
	d, err := s.root.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("media: sync directory: %w", err)
	}
	return nil
}

// matches reports whether the file at rel exists with exactly data.
func (s *Store) matches(rel string, data []byte) (bool, error) {
	existing, err := s.root.ReadFile(rel)
	if err != nil {
		return false, err
	}
	return bytes.Equal(existing, data), nil
}

// Open opens a stored asset for reading after strictly validating the key.
// A missing asset returns an error matching fs.ErrNotExist.
func (s *Store) Open(key string) (*os.File, error) {
	if !ValidKey(key) {
		return nil, ErrInvalidKey
	}
	f, err := s.root.Open(relPath(key))
	if err != nil {
		return nil, err
	}
	if fi, err := f.Stat(); err != nil || !fi.Mode().IsRegular() {
		f.Close()
		return nil, fmt.Errorf("%w: not a regular file", ErrInvalidKey)
	}
	return f, nil
}
