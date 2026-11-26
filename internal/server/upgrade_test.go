package server_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/hadden-me/learnoo/internal/accounts"
	"github.com/hadden-me/learnoo/internal/database"
	"github.com/hadden-me/learnoo/internal/database/dbtest"
	"github.com/hadden-me/learnoo/internal/lessons"
	"github.com/hadden-me/learnoo/internal/permissions"
	"github.com/hadden-me/learnoo/internal/review"
	"github.com/hadden-me/learnoo/migrations"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// LMS-A11 (docs/LMS_ACCEPTANCE.md): a database populated at the schema of
// develop before the LMS (0012) is upgraded through every LMS migration
// (0013 to the latest). Every row of every table that existed before keeps
// its values (IDs, public IDs, so URLs, bookmarks, sessions, audit and
// history rows), nothing is placed into a curriculum edition, and the
// upgraded application serves the same lessons, bookmarks and history to the
// sessions opened before the upgrade. Run in the test database's locale and
// in a C-locale database.
//
// The 0012 data is written by the current stores where their tables did not
// change after 0012 (accounts, permissions, lessons, review, publication,
// withdrawal, bookmarks, media uploads embedded in a lesson revision), and by SQL that follows the 0012 guards where the
// current code writes later columns (error reports, invitations, catalogue
// names): it is not produced by the develop binary itself.
func TestUpgradeFrom0012KeepsLessonsBookmarksAndHistory(t *testing.T) {
	for name, open := range map[string]func(*testing.T) *pgxpool.Pool{
		"default locale": func(t *testing.T) *pgxpool.Pool { return dbtest.EmptySchema(t) },
		"C locale":       cLocalePool,
	} {
		t.Run(name, func(t *testing.T) { upgradeFrom0012(t, open(t)) })
	}
}

func upgradeFrom0012(t *testing.T, db *pgxpool.Pool) {
	ctx := context.Background()
	if _, err := database.Migrate(ctx, db, migrationsBefore(t, "0013")); err != nil {
		t.Fatal(err)
	}
	e := newEnvOn(t, db, nil)
	subject := e.newSubject("test-math")
	ch := e.chapter(subject, "test-chapter")
	f := readerFixture{env: e, subject: subject,
		author:    e.newUser("test-author", subject, permissions.Author),
		reviewer:  e.newUser("test-reviewer", subject, permissions.Reviewer),
		publisher: e.newUser("test-publisher", subject, permissions.Publisher),
		admin:     e.newUser("test-admin", subject, permissions.Administrator),
		ls:        lessons.NewStore(db),
		rs:        review.NewStore(db),
	}
	student := e.newUser("test-student", 0)

	// Lessons: one in a chapter, one in Arabic without a chapter, one whose
	// second edition was withdrawn and its first restored, one unpublished,
	// and one with a draft awaiting review.
	placed := f.publishNew("TEST Leçon placée", "fr", &ch)
	arabic := f.publishNew("TEST درس بلا فصل", "ar", nil)
	restored := f.publishNew("TEST Leçon restaurée, première édition", "fr", &ch)
	second := f.publish(f.approved(restored.lessonID, readerDoc("TEST Leçon restaurée, deuxième édition", "fr"), "fr", &ch))
	if _, err := f.rs.Withdraw(ctx, f.admin.id, second.revisionID, "TEST retrait.", &second.revisionID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.rs.Restore(ctx, f.publisher.id, restored.revisionID, nil); err != nil {
		t.Fatal(err)
	}
	unpublished := f.publishNew("TEST Leçon dépubliée", "fr", nil)
	f.unpublish(unpublished.lessonID, unpublished.revisionID)
	pending, err := f.ls.CreateDraft(ctx, f.author.id, placed.lessonID, lessons.Fields{Content: readerDoc("TEST Leçon placée, correction", "fr"), ChapterID: &ch})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.ls.Submit(ctx, f.author.id, pending.ID, pending.Version); err != nil {
		t.Fatal(err)
	}

	// A lesson with an uploaded image placed in its text, through the Studio
	// API (upload, link, submit, approve, publish), and a failed sign-in.
	rf := withdrawFixture{reviewFixture: reviewFixture{studioFixture: studioFixture{env: e, subject: subject, author: f.author}, reviewer: f.reviewer, publisher: f.publisher}, admin: f.admin}
	image := pngBytes(t, 42)
	w := rf.uploadBytes(f.author, "image/png", image)
	wantStatus(t, "upload", w, http.StatusCreated, "")
	var up struct{ ID int64 }
	if err := json.Unmarshal(w.Body.Bytes(), &up); err != nil || up.ID == 0 {
		t.Fatalf("upload: %v %s", err, w.Body)
	}
	w = rf.createWithMedia(f.author, subject, mediaDoc("TEST Leçon illustrée", false, up.ID), []int64{up.ID})
	wantStatus(t, "create with image", w, http.StatusCreated, "")
	illustrated := rf.approvedRev(rf.submitRev(f.author, decodeRevision(t, w)))
	rf.publishedRev(illustrated)
	illustratedID := f.publicID(illustrated.LessonID)
	if w := e.login("test-student@example.test", "wrong-password-123", withAddr("198.51.100.250")); w.Code != http.StatusUnauthorized {
		t.Fatalf("failed sign-in: %d", w.Code)
	}

	// Bookmarks through the reader, an error report and its resolution, an
	// invitation, catalogue names and an account disabled and enabled again.
	for _, p := range []published{placed, arabic} {
		wantRedirect(t, "bookmark", f.post(student, lessonPath(p.publicID)+"/bookmark", nil), lessonPath(p.publicID))
	}
	for _, sql := range [][]string{
		{`INSERT INTO error_reports (lesson_id, revision_id, reporter_id, message) VALUES ($1, $2, $3, 'TEST erreur résolue.')`},
		{`INSERT INTO error_reports (lesson_id, revision_id, reporter_id, message) VALUES ($1, $2, $3, 'TEST erreur ouverte.')`},
	} {
		if _, err := db.Exec(ctx, sql[0], placed.lessonID, placed.revisionID, student.id); err != nil {
			t.Fatal(err)
		}
	}
	inTx(t, db,
		[]any{`INSERT INTO error_report_resolutions (report_id, actor_id, action, note) SELECT min(id), $1, 'resolve', 'TEST corrigé.' FROM error_reports`, f.reviewer.id},
		[]any{`UPDATE error_reports SET status = 'resolved' WHERE id = (SELECT min(id) FROM error_reports)`})
	if _, err := db.Exec(ctx, `INSERT INTO student_invitations (token_hash, email, operator_label, created_at, expires_at)
		VALUES (sha256('t'), 'test-invited@example.test', 'test-operator@test-host', now(), now() + interval '1 hour')`); err != nil {
		t.Fatal(err)
	}
	inTx(t, db,
		[]any{`INSERT INTO catalogue_audit (operator_label, action, subject_id, old_name_fr, new_name_fr, old_name_ar, new_name_ar)
			VALUES ('test-operator@test-host', 'rename_subject', $1, NULL, 'TEST Mathématiques', NULL, 'TEST رياضيات')`, subject},
		[]any{`UPDATE subjects SET name_fr = 'TEST Mathématiques', name_ar = 'TEST رياضيات' WHERE id = $1`, subject})
	other := e.newUser("test-other", subject, permissions.Author)
	as := accounts.NewStore(db)
	if _, err := as.Disable(ctx, testOperator, other.id); err != nil {
		t.Fatal(err)
	}
	if _, err := as.Enable(ctx, testOperator, other.id); err != nil {
		t.Fatal(err)
	}

	shape := tableColumns(t, db)
	before := snapshot(t, db, shape)
	for _, want := range []string{"users", "sessions", "user_permissions", "reviewer_assignments", "permission_audit", "lessons", "lesson_revisions", "publication_audit",
		"revision_withdrawals", "review_decisions", "submission_audit", "bookmarks", "error_reports", "error_report_resolutions",
		"student_invitations", "student_invitation_audit", "catalogue_audit", "account_status_audit", "media_assets", "revision_media",
		"media_uploads", "login_attempts", "translation_groups"} {
		if before[want] == "" {
			t.Fatalf("table %s is empty or missing before the upgrade", want)
		}
	}

	applied, err := database.Migrate(ctx, db, migrations.FS)
	if err != nil {
		t.Fatal(err)
	}
	if len(applied) == 0 || !strings.HasPrefix(applied[0], "0013_") {
		t.Fatalf("applied %v", applied)
	}
	after := snapshot(t, db, shape)
	for table := range shape {
		if table == "schema_migrations" {
			continue
		}
		if before[table] != after[table] {
			t.Errorf("table %s changed by the upgrade:\nbefore %s\nafter  %s", table, before[table], after[table])
		}
	}
	var editions, placements, courses int
	if err := db.QueryRow(ctx, `SELECT (SELECT count(*) FROM curriculum_editions), (SELECT count(*) FROM curriculum_assignment_chapters),
		(SELECT count(*) FROM courses)`).Scan(&editions, &placements, &courses); err != nil {
		t.Fatal(err)
	}
	if editions+placements+courses != 0 {
		t.Fatalf("upgrade created %d editions, %d placements, %d courses", editions, placements, courses)
	}

	// The upgraded application, with the sessions opened before the upgrade.
	for _, p := range []struct {
		pub  published
		want string
	}{{placed, "TEST Leçon placée"}, {arabic, "TEST درس بلا فصل"}, {restored, "TEST Leçon restaurée, première édition"}} {
		wantPage(t, "read "+p.want, f.get(student, lessonPath(p.pub.publicID)), http.StatusOK, p.want)
	}
	wantPage(t, "read illustrated lesson", f.get(student, lessonPath(illustratedID)), http.StatusOK, "TEST Leçon illustrée")
	if w := f.get(student, mediaPath(illustratedID, up.ID)); w.Code != http.StatusOK || !bytes.Equal(w.Body.Bytes(), image) {
		t.Fatalf("image after the upgrade: %d", w.Code)
	}
	wantAbsent(t, "withdrawn edition", f.get(student, lessonPath(restored.publicID)), "deuxième édition")
	if w := f.get(student, lessonPath(unpublished.publicID)); w.Code != http.StatusNotFound {
		t.Fatalf("unpublished lesson: %d", w.Code)
	}
	wantPage(t, "dashboard", f.get(student, "/learn"), http.StatusOK, "Mes signets (2)", "TEST Leçon placée", "TEST درس بلا فصل")
	// No edition yet: the public catalogue lists the published lessons as not yet placed, never under an invented level.
	wantPage(t, "catalogue", f.page(apiUser{}, http.MethodGet, "/catalogue", nil), http.StatusOK, "TEST Leçon placée")
	w = f.api(f.reviewer, http.MethodGet, "/api/review/error-reports?status=open", nil)
	wantStatus(t, "error reports", w, http.StatusOK, "")
	if !strings.Contains(w.Body.String(), "TEST erreur ouverte.") {
		t.Fatalf("error reports after the upgrade: %s", w.Body)
	}
	w = f.api(f.admin, http.MethodGet, fmt.Sprintf("/api/lessons/%d/publication", restored.lessonID), nil)
	wantStatus(t, "publication history", w, http.StatusOK, "")
	if !strings.Contains(w.Body.String(), "TEST retrait.") {
		t.Fatalf("publication history after the upgrade: %s", w.Body)
	}
	w = f.api(f.reviewer, http.MethodGet, "/api/review/queue", nil)
	wantStatus(t, "review queue", w, http.StatusOK, "")
	if !strings.Contains(w.Body.String(), "TEST Leçon placée, correction") {
		t.Fatalf("pending submission after the upgrade: %s", w.Body)
	}
}

// migrationsBefore returns the embedded migrations whose version is below v.
func migrationsBefore(t *testing.T, v string) fs.FS {
	t.Helper()
	out := fstest.MapFS{}
	entries, err := fs.ReadDir(migrations.FS, ".")
	if err != nil {
		t.Fatal(err)
	}
	for _, en := range entries {
		if name := en.Name(); strings.HasSuffix(name, ".sql") && name < v {
			data, err := fs.ReadFile(migrations.FS, name)
			if err != nil {
				t.Fatal(err)
			}
			out[name] = &fstest.MapFile{Data: data}
		}
	}
	return out
}

// tableColumns lists the tables of the schema and their columns.
func tableColumns(t *testing.T, db *pgxpool.Pool) map[string][]string {
	t.Helper()
	ctx := context.Background()
	rows, err := db.Query(ctx, `
		SELECT c.relname::text, a.attname::text
		  FROM pg_catalog.pg_class c
		  JOIN pg_catalog.pg_attribute a ON a.attrelid = c.oid AND a.attnum > 0 AND NOT a.attisdropped
		 WHERE c.relnamespace = current_schema()::regnamespace AND c.relkind = 'r'
		 ORDER BY c.relname, a.attnum`)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string][]string{}
	var table, column string
	if _, err := pgx.ForEachRow(rows, []any{&table, &column}, func() error {
		out[table] = append(out[table], column)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

// snapshot renders every row of each table in shape, limited to the columns
// shape lists, sorted: one string per non-empty table.
func snapshot(t *testing.T, db *pgxpool.Pool, shape map[string][]string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for table, columns := range shape {
		cols := make([]string, len(columns))
		for i, c := range columns {
			cols[i] = pgx.Identifier{c}.Sanitize()
		}
		var s *string
		if err := db.QueryRow(context.Background(), `SELECT string_agg(r, E'\n' ORDER BY r) FROM (SELECT ROW(`+strings.Join(cols, ", ")+`)::text AS r FROM `+
			pgx.Identifier{table}.Sanitize()+`) x`).Scan(&s); err != nil {
			t.Fatalf("snapshot %s: %v", table, err)
		}
		if s != nil {
			out[table] = *s
		}
	}
	return out
}

func inTx(t *testing.T, db *pgxpool.Pool, statements ...[]any) {
	t.Helper()
	if err := pgx.BeginFunc(context.Background(), db, func(tx pgx.Tx) error {
		for _, st := range statements {
			if _, err := tx.Exec(context.Background(), st[0].(string), st[1:]...); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// cLocalePool is a fresh UTF-8 database with the C locale, dropped when the
// test ends (the pattern of internal/curriculum/upgrade_test.go).
func cLocalePool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()
	url := dbtest.URL(t)
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatal(err)
	}
	name := "learnoo_upgrade_c_" + hex.EncodeToString(b[:])
	admin, err := database.Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()+
		" TEMPLATE template0 ENCODING 'UTF8' LOCALE_PROVIDER libc LC_COLLATE 'C' LC_CTYPE 'C'"); err != nil {
		t.Fatalf("create C database: %v", err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), "DROP DATABASE IF EXISTS "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)")
	})
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.Database = name
	cfg.MaxConns = 8
	pool, err := database.OpenConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}
