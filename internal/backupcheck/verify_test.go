package backupcheck_test

import (
	"bytes"
	"context"
	"encoding/hex"
	"image"
	"image/color"
	"image/png"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/hadden-me/learnoo/internal/auth"
	"github.com/hadden-me/learnoo/internal/backupcheck"
	"github.com/hadden-me/learnoo/internal/database/dbtest"
	"github.com/hadden-me/learnoo/internal/media"
	"github.com/hadden-me/learnoo/internal/users"
	"github.com/hadden-me/learnoo/migrations"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TEST fixtures only: a simulated user and generated images.

type fixture struct {
	t    *testing.T
	db   *pgxpool.Pool
	dir  string
	user int64
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	db := dbtest.Migrated(t)
	hash, err := auth.HashPassword("simulated-password-1", auth.Argon2Params{MemoryKiB: 1024, Iterations: 1, Lanes: 1, SaltLen: 16, KeyLen: 32})
	if err != nil {
		t.Fatal(err)
	}
	u, err := users.Create(context.Background(), db, "test-uploader@example.test", "TEST Uploader", hash)
	if err != nil {
		t.Fatal(err)
	}
	return fixture{t: t, db: db, dir: t.TempDir(), user: u.ID}
}

func testPNG(t *testing.T, seed uint8) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, 4, 3))
	for x := 0; x < 4; x++ {
		for y := 0; y < 3; y++ {
			img.Set(x, y, color.NRGBA{R: seed, G: uint8(x * 50), B: uint8(y * 70), A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// addAsset stores a generated image and records its row, as the upload
// endpoint does. It returns the file's absolute path.
func (f fixture) addAsset(seed uint8) string {
	f.t.Helper()
	a, err := media.Process(context.Background(), bytes.NewReader(testPNG(f.t, seed)), media.ContentTypePNG)
	if err != nil {
		f.t.Fatal(err)
	}
	s, err := media.OpenStore(f.dir)
	if err != nil {
		f.t.Fatal(err)
	}
	defer s.Close()
	key, err := s.Put(a)
	if err != nil {
		f.t.Fatal(err)
	}
	sum, _ := hex.DecodeString(a.SHA256())
	if _, err := f.db.Exec(context.Background(), `
		INSERT INTO media_assets (sha256, content_type, byte_size, width, height, storage_key, created_at, created_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		sum, a.ContentType(), a.ByteSize(), a.Width(), a.Height(), key, time.Now().UTC(), f.user); err != nil {
		f.t.Fatal(err)
	}
	rel, err := media.RelPath(key)
	if err != nil {
		f.t.Fatal(err)
	}
	return filepath.Join(f.dir, filepath.FromSlash(rel))
}

func (f fixture) verify() *backupcheck.Report {
	f.t.Helper()
	r, err := backupcheck.Verify(context.Background(), f.db, f.dir, migrations.FS)
	if err != nil {
		f.t.Fatal(err)
	}
	return r
}

func wantProblem(t *testing.T, r *backupcheck.Report, substr string) {
	t.Helper()
	if !r.Failed(false) {
		t.Fatalf("verification passed; want a problem containing %q", substr)
	}
	for _, p := range r.Problems {
		if strings.Contains(p, substr) {
			return
		}
	}
	t.Fatalf("no problem contains %q: %q", substr, r.Problems)
}

func TestVerifyPassesForConsistentState(t *testing.T) {
	f := newFixture(t)
	f.addAsset(1)
	f.addAsset(2)
	r := f.verify()
	if r.Failed(true) {
		t.Fatalf("unexpected failure: %+v", r)
	}
	if r.Assets != 2 {
		t.Fatalf("checked %d assets", r.Assets)
	}
	var users int64 = -1
	for _, c := range r.Counts {
		if c.Table == "users" {
			users = c.Rows
		}
	}
	if users != 1 {
		t.Fatalf("users count %d in %+v", users, r.Counts)
	}
	var out strings.Builder
	r.WriteText(&out, true)
	if !strings.Contains(out.String(), "verification passed") || strings.Contains(out.String(), "TEST Uploader") {
		t.Fatalf("report:\n%s", out.String())
	}
}

func TestVerifyDetectsMissingFile(t *testing.T) {
	f := newFixture(t)
	f.addAsset(1)
	p := f.addAsset(2)
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	wantProblem(t, f.verify(), "is missing")
}

func TestVerifyDetectsCorruptedFile(t *testing.T) {
	f := newFixture(t)
	p := f.addAsset(1)
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	data[len(data)/2] ^= 0xff // same size, different content
	if err := os.Chmod(p, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, data, 0o444); err != nil {
		t.Fatal(err)
	}
	wantProblem(t, f.verify(), "does not match the recorded SHA-256")
}

func TestVerifyDetectsTruncatedFile(t *testing.T) {
	f := newFixture(t)
	p := f.addAsset(1)
	if err := os.Chmod(p, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(p, 10); err != nil {
		t.Fatal(err)
	}
	wantProblem(t, f.verify(), "has 10 bytes")
}

func TestVerifyRejectsSymlinkInPlaceOfAsset(t *testing.T) {
	f := newFixture(t)
	p := f.addAsset(1)
	outside := filepath.Join(t.TempDir(), "copy")
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(outside, data, 0o444); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, p); err != nil {
		t.Fatal(err)
	}
	r := f.verify()
	wantProblem(t, r, "not a regular file")
	if len(r.Unexpected) != 1 {
		t.Fatalf("symlink not reported as unexpected: %q", r.Unexpected)
	}
}

func TestVerifyReportsUnexpectedAndTemporaryFiles(t *testing.T) {
	f := newFixture(t)
	p := f.addAsset(1)
	shard := filepath.Dir(p)
	for _, name := range []string{filepath.Join(shard, ".tmp-0123"), filepath.Join(shard, "stray.png"), filepath.Join(f.dir, "notes.txt")} {
		if err := os.WriteFile(name, []byte("x"), 0o444); err != nil {
			t.Fatal(err)
		}
	}
	r := f.verify()
	if r.Failed(false) {
		t.Fatalf("unexpected files must be reported, not fail the default mode: %q", r.Problems)
	}
	if !r.Failed(true) {
		t.Fatal("strict mode passed with unexpected files")
	}
	if len(r.Unexpected) != 2 || len(r.Temporary) != 1 {
		t.Fatalf("unexpected %q temporary %q", r.Unexpected, r.Temporary)
	}
	// Reporting never deletes.
	for _, rel := range append(r.Unexpected, r.Temporary...) {
		if _, err := os.Stat(filepath.Join(f.dir, filepath.FromSlash(rel))); err != nil {
			t.Fatalf("%s: %v", rel, err)
		}
	}
}

func TestVerifyDetectsPendingMigrations(t *testing.T) {
	f := newFixture(t)
	extra := fstest.MapFS{}
	entries, err := fs.ReadDir(migrations.FS, ".")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		b, err := fs.ReadFile(migrations.FS, e.Name())
		if err != nil {
			t.Fatal(err)
		}
		extra[e.Name()] = &fstest.MapFile{Data: b}
	}
	extra["9999_future.sql"] = &fstest.MapFile{Data: []byte("SELECT 1;")}
	r, err := backupcheck.Verify(context.Background(), f.db, f.dir, extra)
	if err != nil {
		t.Fatal(err)
	}
	wantProblem(t, r, "pending migrations")
}

// A pointer that bypassed the guards (here with triggers disabled, which
// only a superuser or owner can do) is reported.
func TestVerifyDetectsInconsistentPointer(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	tx, err := f.db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	steps := []string{
		`SET LOCAL session_replication_role = replica`,
		`INSERT INTO subjects (code) VALUES ('test-math')`,
		`INSERT INTO translation_groups (id, subject_id) SELECT gen_random_uuid(), id FROM subjects`,
		`INSERT INTO lessons (translation_group_id, language, subject_id, created_at, created_by, published_revision_id)
		 SELECT g.id, 'fr', g.subject_id, now(), ` + strconv.FormatInt(f.user, 10) + `, 987654 FROM translation_groups g`,
	}
	for _, s := range steps {
		if _, err := tx.Exec(ctx, s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	wantProblem(t, f.verify(), "names a missing revision")
}

// A submission that bypassed the guards (triggers disabled, which only a
// superuser or owner can do) is reported: a submitted revision without its
// submission audit row (migrations/0011).
func TestVerifyDetectsUnauditedSubmission(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	tx, err := f.db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	user := strconv.FormatInt(f.user, 10)
	steps := []string{
		`SET LOCAL session_replication_role = replica`,
		`INSERT INTO subjects (code) VALUES ('test-math')`,
		`INSERT INTO translation_groups (id, subject_id) SELECT gen_random_uuid(), id FROM subjects`,
		`INSERT INTO lessons (translation_group_id, language, subject_id, created_at, created_by)
		 SELECT g.id, 'fr', g.subject_id, now(), ` + user + ` FROM translation_groups g`,
		`INSERT INTO lesson_revisions (lesson_id, revision_number, status, author_id, title, objectives, content,
		     schema_version, language, subject_id, created_at, updated_at, submitted_at)
		 SELECT id, 1, 'in_review', ` + user + `, 'TEST', '', '{"title": "TEST", "language": "fr", "schemaVersion": 1}',
		     1, 'fr', subject_id, now(), now(), now() FROM lessons`,
	}
	for _, s := range steps {
		if _, err := tx.Exec(ctx, s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	wantProblem(t, f.verify(), "has no submission audit row")
}

// Course rows that bypassed the guards (triggers disabled, which only a
// superuser or owner can do) are reported: a published pointer to a missing
// revision without a publication audit row, and a submitted course revision
// without its submission audit row (migrations/0014, 0015).
func TestVerifyDetectsInconsistentCourses(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	tx, err := f.db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	user := strconv.FormatInt(f.user, 10)
	steps := []string{
		`SET LOCAL session_replication_role = replica`,
		`INSERT INTO subjects (code) VALUES ('test-math')`,
		`INSERT INTO courses (owner_id, subject_id, language, created_by, published_revision_id, last_revision_number)
		 SELECT ` + user + `, id, 'fr', ` + user + `, 987654, 2 FROM subjects`,
		`INSERT INTO course_revisions (course_id, subject_id, language, revision_number, status, author_id, submitted_at)
		 SELECT id, subject_id, language, 1, 'in_review', ` + user + `, now() FROM courses`,
	}
	for _, s := range steps {
		if _, err := tx.Exec(ctx, s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	r := f.verify()
	wantProblem(t, r, "names a missing revision or one of another course")
	wantProblem(t, r, "has no course publication audit row")
	wantProblem(t, r, "has no submission audit row")
	if r.Courses != 1 {
		t.Fatalf("published courses: %d", r.Courses)
	}
}
