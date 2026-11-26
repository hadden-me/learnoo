package courses_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/hadden-me/learnoo/internal/auth"
	"github.com/hadden-me/learnoo/internal/courses"
	"github.com/hadden-me/learnoo/internal/curriculum"
	"github.com/hadden-me/learnoo/internal/database/dbtest"
	"github.com/hadden-me/learnoo/internal/lessons"
	"github.com/hadden-me/learnoo/internal/permissions"
	"github.com/hadden-me/learnoo/internal/users"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Simulated users, TEST subjects, chapters, curriculum entries and
// unreviewed test documents only.

var testArgon2 = auth.Argon2Params{MemoryKiB: 1024, Iterations: 1, Lanes: 1, SaltLen: 16, KeyLen: 32}

var testOperator = func() permissions.Actor {
	a, err := permissions.OperatorAs("test-operator@test-host")
	if err != nil {
		panic(err)
	}
	return a
}()

type env struct {
	t         *testing.T
	ctx       context.Context
	db        *pgxpool.Pool
	store     *courses.Store
	lessons   *lessons.Store
	cur       *curriculum.Store
	perms     *permissions.Store
	admin     permissions.Actor
	owner     int64 // author, owns the courses
	other     int64 // another author
	reviewer  int64 // reviewer of math
	publisher int64
	math      int64
	physics   int64
}

func newEnv(t *testing.T) *env {
	t.Helper()
	return newEnvOn(t, dbtest.Migrated(t))
}

func newEnvOn(t *testing.T, db *pgxpool.Pool) *env {
	t.Helper()
	e := &env{t: t, ctx: context.Background(), db: db, store: courses.NewStore(db), lessons: lessons.NewStore(db),
		cur: curriculum.NewStore(db), perms: permissions.NewStore(db)}
	adminID := e.user("admin", permissions.Administrator)
	e.admin = permissions.UserActor(adminID)
	e.owner = e.user("owner", permissions.Author)
	e.other = e.user("other", permissions.Author)
	e.publisher = e.user("publisher", permissions.Publisher)
	e.math = dbtest.AddSubject(t, db, "test-math")
	e.physics = dbtest.AddSubject(t, db, "test-physics")
	e.reviewer = e.user("reviewer")
	if _, err := e.perms.GrantReviewer(e.ctx, testOperator, e.reviewer, e.math); err != nil {
		t.Fatal(err)
	}
	if _, err := e.perms.GrantReviewer(e.ctx, testOperator, e.reviewer, e.physics); err != nil {
		t.Fatal(err)
	}
	return e
}

func (e *env) user(name string, perms ...permissions.Permission) int64 {
	e.t.Helper()
	hash, err := auth.HashPassword("simulated-password-1", testArgon2)
	if err != nil {
		e.t.Fatal(err)
	}
	u, err := users.Create(e.ctx, e.db, name+"@example.test", "TEST "+name, hash)
	if err != nil {
		e.t.Fatal(err)
	}
	for _, p := range perms {
		if _, err := e.perms.Grant(e.ctx, testOperator, u.ID, p); err != nil {
			e.t.Fatal(err)
		}
	}
	return u.ID
}

func (e *env) exec(sql string, args ...any) error {
	_, err := e.db.Exec(e.ctx, sql, args...)
	return err
}

func (e *env) mustExec(sql string, args ...any) {
	e.t.Helper()
	if err := e.exec(sql, args...); err != nil {
		e.t.Fatalf("%s: %v", sql, err)
	}
}

func (e *env) count(sql string, args ...any) int {
	e.t.Helper()
	var n int
	if err := e.db.QueryRow(e.ctx, sql, args...).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

func (e *env) chapter(subject int64, code string) int64 {
	e.t.Helper()
	return dbtest.AddChapter(e.t, e.db, subject, code)
}

// media inserts a simulated image asset and an upload link for uploader.
func (e *env) media(uploader int64) int64 {
	e.t.Helper()
	var sum [32]byte
	if _, err := rand.Read(sum[:]); err != nil {
		e.t.Fatal(err)
	}
	var id int64
	err := e.db.QueryRow(e.ctx, `
		WITH a AS (INSERT INTO media_assets (sha256, content_type, byte_size, width, height, storage_key, created_at, created_by)
		           VALUES ($1, 'image/png', 10, 2, 2, $2, now(), $3) RETURNING id)
		INSERT INTO media_uploads (user_id, media_asset_id, created_at) SELECT $3, id, now() FROM a RETURNING media_asset_id`,
		sum[:], "simulated/"+hex.EncodeToString(sum[:])+".png", uploader).Scan(&id)
	if err != nil {
		e.t.Fatal(err)
	}
	return id
}

// ---- curriculum ----

type placement struct {
	level, section int64
	edition        curriculum.Edition
	assignment     int64
}

var codeSeq int

// edition creates a level, section and edition assigning subject with
// chapters; current makes it current.
func (e *env) edition(subject int64, chapters []int64, current bool) placement {
	e.t.Helper()
	codeSeq++
	code := fmt.Sprintf("test-%d", codeSeq)
	l, err := e.cur.CreateLevel(e.ctx, e.admin, curriculum.Entry{Code: code, NameFR: "TEST " + code, NameAR: "اختبار"})
	if err != nil {
		e.t.Fatal(err)
	}
	s, err := e.cur.CreateSection(e.ctx, e.admin, l.ID, curriculum.Entry{Code: code, NameFR: "TEST " + code, NameAR: "شعبة"})
	if err != nil {
		e.t.Fatal(err)
	}
	return e.editionIn(l.ID, s.ID, subject, chapters, current)
}

func (e *env) editionIn(level, section, subject int64, chapters []int64, current bool) placement {
	e.t.Helper()
	codeSeq++
	code := fmt.Sprintf("test-ed-%d", codeSeq)
	ed, err := e.cur.CreateEdition(e.ctx, e.admin, section, curriculum.NewEdition{Code: code, LabelFR: "TEST " + code, LabelAR: "طبعة"})
	if err != nil {
		e.t.Fatal(err)
	}
	ed, a, err := e.cur.AddAssignment(e.ctx, e.admin, ed.ID, ed.Version, subject)
	if err != nil {
		e.t.Fatal(err)
	}
	if ed, err = e.cur.SetChapters(e.ctx, e.admin, ed.ID, a.ID, ed.Version, chapters); err != nil {
		e.t.Fatal(err)
	}
	if current {
		var prev *int64
		var id int64
		if err := e.db.QueryRow(e.ctx, `SELECT id FROM curriculum_editions WHERE section_id = $1 AND state = 'current'`, section).Scan(&id); err == nil {
			prev = &id
		}
		if ed, _, err = e.cur.MakeCurrent(e.ctx, e.admin, ed.ID, ed.Version, prev); err != nil {
			e.t.Fatal(err)
		}
	}
	return placement{level: level, section: section, edition: ed, assignment: a.ID}
}

// ---- lessons ----

func doc(title, lang string) []byte {
	b, err := json.Marshal(map[string]any{
		"schemaVersion": 1, "title": title, "language": lang, "status": "unreviewed",
		"objective": "Simulated objective for course tests.",
		"blocks":    []any{map[string]any{"type": "paragraph", "children": []any{map[string]any{"type": "text", "text": "Simulated text: " + title}}}},
	})
	if err != nil {
		panic(err)
	}
	return b
}

// lessonDraft creates a lesson of author with its first draft.
func (e *env) lessonDraft(author, subject int64, lang, title string) lessons.Revision {
	e.t.Helper()
	rev, err := e.lessons.CreateLesson(e.ctx, author, lessons.NewLesson{SubjectID: subject, Language: lang, Fields: lessons.Fields{Content: doc(title, lang)}})
	if err != nil {
		e.t.Fatal(err)
	}
	return rev
}

// approve submits rev (by its author) and approves it with direct SQL by
// the math/physics reviewer.
func (e *env) approve(rev lessons.Revision) {
	e.t.Helper()
	if _, err := e.lessons.Submit(e.ctx, rev.AuthorID, rev.ID, rev.Version); err != nil {
		e.t.Fatal(err)
	}
	tx, err := e.db.Begin(e.ctx)
	if err != nil {
		e.t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(e.ctx) }()
	if _, err := tx.Exec(e.ctx, `INSERT INTO review_decisions (revision_id, reviewer_id, action) VALUES ($1, $2, 'approve')`, rev.ID, e.reviewer); err != nil {
		e.t.Fatal(err)
	}
	if _, err := tx.Exec(e.ctx, `UPDATE lesson_revisions SET status = 'approved' WHERE id = $1`, rev.ID); err != nil {
		e.t.Fatal(err)
	}
	if err := tx.Commit(e.ctx); err != nil {
		e.t.Fatal(err)
	}
}

// publish points the lesson at rev with its audit row.
func (e *env) publish(rev lessons.Revision) {
	e.t.Helper()
	tx, err := e.db.Begin(e.ctx)
	if err != nil {
		e.t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(e.ctx) }()
	if _, err := tx.Exec(e.ctx, `
		INSERT INTO publication_audit (lesson_id, revision_id, previous_revision_id, actor_id, action)
		SELECT id, $2, published_revision_id, $3, 'publish' FROM lessons WHERE id = $1`, rev.LessonID, rev.ID, e.publisher); err != nil {
		e.t.Fatal(err)
	}
	if _, err := tx.Exec(e.ctx, `UPDATE lessons SET published_revision_id = $1 WHERE id = $2`, rev.ID, rev.LessonID); err != nil {
		e.t.Fatal(err)
	}
	if err := tx.Commit(e.ctx); err != nil {
		e.t.Fatal(err)
	}
}

// publishedLesson creates, approves and publishes a lesson of author.
func (e *env) publishedLesson(author, subject int64, lang, title string) lessons.Revision {
	e.t.Helper()
	rev := e.lessonDraft(author, subject, lang, title)
	e.approve(rev)
	e.publish(rev)
	return rev
}

// unpublish clears the lesson's pointer with its audit row.
func (e *env) unpublish(lesson int64) {
	e.t.Helper()
	adminID := *e.admin.AuditID()
	tx, err := e.db.Begin(e.ctx)
	if err != nil {
		e.t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(e.ctx) }()
	if _, err := tx.Exec(e.ctx, `
		INSERT INTO publication_audit (lesson_id, revision_id, previous_revision_id, actor_id, action, reason)
		SELECT id, NULL, published_revision_id, $2, 'unpublish', 'Simulated unpublication.' FROM lessons WHERE id = $1`, lesson, adminID); err != nil {
		e.t.Fatal(err)
	}
	if _, err := tx.Exec(e.ctx, `UPDATE lessons SET published_revision_id = NULL WHERE id = $1`, lesson); err != nil {
		e.t.Fatal(err)
	}
	if err := tx.Commit(e.ctx); err != nil {
		e.t.Fatal(err)
	}
}

// ---- courses ----

func (e *env) course(subject int64, lang string) (courses.Course, courses.Revision) {
	e.t.Helper()
	c, rev, err := e.store.CreateCourse(e.ctx, e.owner, subject, lang)
	if err != nil {
		e.t.Fatal(err)
	}
	return c, rev
}

func step(rev lessons.Revision, title string) courses.StepInput {
	return courses.StepInput{LessonID: rev.LessonID, RevisionID: rev.ID, Title: title}
}

func (e *env) save(rev courses.Revision, d courses.Draft) (courses.Revision, error) {
	return e.store.SaveDraft(e.ctx, e.owner, rev.ID, rev.Version, d)
}

func (e *env) mustSave(rev courses.Revision, d courses.Draft) courses.Revision {
	e.t.Helper()
	out, err := e.save(rev, d)
	if err != nil {
		e.t.Fatalf("save: %v", err)
	}
	return out
}

// forceStatus moves a course revision out of draft with every trigger
// disabled (session_replication_role = replica, superuser test connection),
// for B03 tests that need a submitted revision without its audit rows. Test setup only.
func (e *env) forceStatus(revisionID int64, set string) {
	e.t.Helper()
	tx, err := e.db.Begin(e.ctx)
	if err != nil {
		e.t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(e.ctx) }()
	if _, err := tx.Exec(e.ctx, `SET LOCAL session_replication_role = replica`); err != nil {
		e.t.Fatal(err)
	}
	if _, err := tx.Exec(e.ctx, `UPDATE course_revisions SET `+set+` WHERE id = $1`, revisionID); err != nil {
		e.t.Fatal(err)
	}
	if err := tx.Commit(e.ctx); err != nil {
		e.t.Fatal(err)
	}
}

func wantErr(t *testing.T, what string, err, want error) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("%s: err %v, want %v", what, err, want)
	}
}

func wantGuard(t *testing.T, what string, err error, msg string) {
	t.Helper()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "LRN01" || !strings.Contains(pgErr.Message, msg) {
		t.Errorf("%s: want LRN01 %q, got %v", what, msg, err)
	}
}

func wantSQLState(t *testing.T, what string, err error, code string) {
	t.Helper()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != code {
		t.Errorf("%s: want SQLSTATE %s, got %v", what, code, err)
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// leastPrivilegeRole creates a non-owner role like the deployed runtime
// role (the pattern of internal/curriculum and internal/reports): USAGE on
// the test schema, DML on its tables and sequences, EXECUTE on its functions
// (the default for PUBLIC) and TEMP. asRole runs statements in one committed
// transaction as that role on a new connection; "S." is replaced by the
// quoted test schema.
func (e *env) leastPrivilegeRole() (asRole func(stmts ...string) error) {
	t := e.t
	t.Helper()
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatal(err)
	}
	role := pgx.Identifier{"learnoo_test_app_" + hex.EncodeToString(b[:])}.Sanitize()
	var schema, dbName string
	if err := e.db.QueryRow(e.ctx, `SELECT current_schema(), current_database()`).Scan(&schema, &dbName); err != nil {
		t.Fatal(err)
	}
	qs := pgx.Identifier{schema}.Sanitize()
	for _, sql := range []string{
		`CREATE ROLE ` + role + ` NOLOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT`,
		`GRANT USAGE ON SCHEMA ` + qs + ` TO ` + role,
		`GRANT SELECT, INSERT, UPDATE, DELETE, TRUNCATE ON ALL TABLES IN SCHEMA ` + qs + ` TO ` + role,
		`GRANT USAGE ON ALL SEQUENCES IN SCHEMA ` + qs + ` TO ` + role,
		`GRANT TEMP ON DATABASE ` + pgx.Identifier{dbName}.Sanitize() + ` TO ` + role,
	} {
		e.mustExec(sql)
		if strings.HasPrefix(sql, "CREATE ROLE") {
			t.Cleanup(func() {
				ctx := context.Background()
				_, _ = e.db.Exec(ctx, `DROP OWNED BY `+role)
				if _, err := e.db.Exec(ctx, `DROP ROLE `+role); err != nil {
					t.Errorf("drop test role: %v", err)
				}
			})
		}
	}
	return func(stmts ...string) error {
		conn, err := pgx.ConnectConfig(e.ctx, e.db.Config().ConnConfig.Copy())
		if err != nil {
			return err
		}
		defer func() { _ = conn.Close(context.Background()) }()
		tx, err := conn.Begin(e.ctx)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback(e.ctx) }()
		if _, err := tx.Exec(e.ctx, `SET LOCAL ROLE `+role); err != nil {
			return err
		}
		for _, s := range stmts {
			if _, err := tx.Exec(e.ctx, strings.ReplaceAll(s, "S.", qs+".")); err != nil {
				return err
			}
		}
		return tx.Commit(e.ctx)
	}
}
