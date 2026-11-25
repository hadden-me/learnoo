package lessons_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/hadden-me/learnoo/internal/auth"
	"github.com/hadden-me/learnoo/internal/database/dbtest"
	"github.com/hadden-me/learnoo/internal/lessons"
	"github.com/hadden-me/learnoo/internal/permissions"
	"github.com/hadden-me/learnoo/internal/rendering"
	"github.com/hadden-me/learnoo/internal/users"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Simulated users, subjects, chapters and unreviewed test documents only.
// Nothing here is curriculum data or reviewed educational content.

var testArgon2 = auth.Argon2Params{MemoryKiB: 1024, Iterations: 1, Lanes: 1, SaltLen: 16, KeyLen: 32}

// operator is the local operator actor the tests grant and revoke with.
var operator = func() permissions.Actor {
	a, err := permissions.OperatorAs("test")
	if err != nil {
		panic(err)
	}
	return a
}()

type env struct {
	t     *testing.T
	ctx   context.Context
	db    *pgxpool.Pool
	perms *permissions.Store
	store *lessons.Store
}

func newEnv(t *testing.T) *env {
	db := dbtest.Migrated(t)
	return &env{t: t, ctx: context.Background(), db: db, perms: permissions.NewStore(db), store: lessons.NewStore(db, nil)}
}

func (e *env) user(name string) int64 {
	e.t.Helper()
	hash, err := auth.HashPassword("simulated-password-1", testArgon2)
	if err != nil {
		e.t.Fatal(err)
	}
	u, err := users.Create(e.ctx, e.db, name+"@example.test", name, hash)
	if err != nil {
		e.t.Fatal(err)
	}
	return u.ID
}

func (e *env) newAuthor(name string) int64 {
	e.t.Helper()
	id := e.user(name)
	if _, err := e.perms.Grant(e.ctx, operator, id, permissions.Author); err != nil {
		e.t.Fatal(err)
	}
	return id
}

func (e *env) newSubject(code string) int64 {
	e.t.Helper()
	var id int64
	if err := e.db.QueryRow(e.ctx, `INSERT INTO subjects (code) VALUES ($1) RETURNING id`, code).Scan(&id); err != nil {
		e.t.Fatal(err)
	}
	return id
}

func (e *env) chapter(subject int64, code string) int64 {
	e.t.Helper()
	var id int64
	if err := e.db.QueryRow(e.ctx, `INSERT INTO chapters (subject_id, code) VALUES ($1, $2) RETURNING id`, subject, code).Scan(&id); err != nil {
		e.t.Fatal(err)
	}
	return id
}

func (e *env) media(owner int64) int64 {
	e.t.Helper()
	var sum [32]byte
	if _, err := rand.Read(sum[:]); err != nil {
		e.t.Fatal(err)
	}
	var id int64
	err := e.db.QueryRow(e.ctx, `
		INSERT INTO media_assets (sha256, content_type, byte_size, width, height, storage_key, created_at, created_by)
		VALUES ($1, 'image/png', 10, 2, 2, $2, now(), $3) RETURNING id`,
		sum[:], "simulated/"+hex.EncodeToString(sum[:])+".png", owner).Scan(&id)
	if err != nil {
		e.t.Fatal(err)
	}
	return id
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

// snapshot returns the stored revision row and its media references as text,
// for byte-for-byte comparison.
func (e *env) snapshot(id int64) string {
	e.t.Helper()
	var row, media string
	err := e.db.QueryRow(e.ctx, `
		SELECT to_jsonb(r)::text,
		       coalesce((SELECT string_agg(media_asset_id::text, ',' ORDER BY media_asset_id)
		                   FROM revision_media WHERE revision_id = r.id), '')
		  FROM lesson_revisions r WHERE id = $1`, id).Scan(&row, &media)
	if err != nil {
		e.t.Fatal(err)
	}
	return row + "|" + media
}

// doc builds a minimal schema v1 test document with an embedded exercise.
func doc(title, lang string) []byte {
	d := map[string]any{
		"schemaVersion": 1,
		"title":         title,
		"language":      lang,
		"status":        "unreviewed",
		"objective":     "Simulated objective for persistence tests.",
		"blocks": []any{
			map[string]any{"type": "paragraph", "children": []any{map[string]any{"type": "text", "text": "Simulated text: " + title}}},
			map[string]any{"type": "exercise", "title": "Simulated exercise", "children": []any{
				map[string]any{"type": "paragraph", "children": []any{map[string]any{"type": "math", "text": "1+1"}}},
				map[string]any{"type": "solution", "children": []any{
					map[string]any{"type": "paragraph", "children": []any{map[string]any{"type": "text", "text": "Simulated solution"}}},
				}},
			}},
		},
	}
	b, err := json.Marshal(d)
	if err != nil {
		panic(err)
	}
	return b
}

func wantErr(t *testing.T, err, want error) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("want %v, got %v", want, err)
	}
}

// wantSQLState asserts a database error with the given SQLSTATE.
func wantSQLState(t *testing.T, what string, err error, code string) {
	t.Helper()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != code {
		t.Errorf("%s: want SQLSTATE %s, got %v", what, code, err)
	}
}

type fixture struct {
	*env
	author    int64
	subject   int64
	reviewer  int64 // assigned to subject
	publisher int64
}

func newFixture(t *testing.T) fixture {
	e := newEnv(t)
	subject := e.newSubject("simulated-math")
	reviewer := e.user("fixture-reviewer")
	if _, err := e.perms.GrantReviewer(e.ctx, operator, reviewer, subject); err != nil {
		t.Fatal(err)
	}
	publisher := e.user("fixture-publisher")
	if _, err := e.perms.Grant(e.ctx, operator, publisher, permissions.Publisher); err != nil {
		t.Fatal(err)
	}
	return fixture{env: e, author: e.newAuthor("author"), subject: subject, reviewer: reviewer, publisher: publisher}
}

// decide simulates a review decision with direct SQL, as the database
// requires since LRN-007 (migrations/0006_review_publication.sql): the
// fixture's reviewer records the decision and set applies the status change
// in the same transaction. The review workflow itself is internal/review.
func (f fixture) decide(rev int64, action, set string) error {
	tx, err := f.db.Begin(f.ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(f.ctx) }()
	var comment *string
	if action == "request_changes" {
		c := "Simulated reviewer comment."
		comment = &c
	}
	if _, err := tx.Exec(f.ctx, `INSERT INTO review_decisions (revision_id, reviewer_id, action, comment) VALUES ($1, $2, $3, $4)`,
		rev, f.reviewer, action, comment); err != nil {
		return err
	}
	if _, err := tx.Exec(f.ctx, `UPDATE lesson_revisions SET `+set+` WHERE id = $1`, rev); err != nil {
		return err
	}
	return tx.Commit(f.ctx)
}

func (f fixture) mustApprove(rev int64) {
	f.t.Helper()
	if err := f.decide(rev, "approve", `status = 'approved'`); err != nil {
		f.t.Fatalf("approve %d: %v", rev, err)
	}
}

func (f fixture) mustRequestChanges(rev int64) {
	f.t.Helper()
	if err := f.decide(rev, "request_changes", `status = 'changes_requested'`); err != nil {
		f.t.Fatalf("request changes on %d: %v", rev, err)
	}
}

// publishIn simulates publication inside tx with direct SQL: an audit row by
// the fixture's publisher naming the current pointer, then the pointer.
func (f fixture) publishIn(tx pgx.Tx, lesson, rev int64) error {
	if _, err := tx.Exec(f.ctx, `
		INSERT INTO publication_audit (lesson_id, revision_id, previous_revision_id, actor_id, action)
		SELECT $1, $2, published_revision_id, $3, 'publish' FROM lessons WHERE id = $1`, lesson, rev, f.publisher); err != nil {
		return err
	}
	_, err := tx.Exec(f.ctx, `UPDATE lessons SET published_revision_id = $1 WHERE id = $2`, rev, lesson)
	return err
}

// publish runs publishIn in its own transaction.
func (f fixture) publish(lesson, rev int64) error {
	tx, err := f.db.Begin(f.ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(f.ctx) }()
	if err := f.publishIn(tx, lesson, rev); err != nil {
		return err
	}
	return tx.Commit(f.ctx)
}

func (f fixture) lesson(title string) lessons.Revision {
	f.t.Helper()
	rev, err := f.store.CreateLesson(f.ctx, f.author, lessons.NewLesson{
		SubjectID: f.subject, Language: "fr", Fields: lessons.Fields{Content: doc(title, "fr")},
	})
	if err != nil {
		f.t.Fatal(err)
	}
	return rev
}

func (f fixture) submitted(title string) lessons.Revision {
	f.t.Helper()
	rev := f.lesson(title)
	rev, err := f.store.Submit(f.ctx, f.author, rev.ID, rev.Version)
	if err != nil {
		f.t.Fatal(err)
	}
	return rev
}

func TestDraftLifecycle(t *testing.T) {
	f := newFixture(t)
	chapter := f.chapter(f.subject, "simulated-chapter")
	img := f.media(f.author)

	fixtureJSON, err := rendering.Files.ReadFile("fixtures/complex-numbers.json")
	if err != nil {
		t.Fatal(err)
	}
	rev, err := f.store.CreateLesson(f.ctx, f.author, lessons.NewLesson{
		SubjectID: f.subject, Language: "fr",
		Fields: lessons.Fields{Content: fixtureJSON, ChapterID: &chapter, Prerequisites: "Simulated prerequisite", MediaIDs: []int64{img, img}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if rev.Number != 1 || rev.Status != lessons.StatusDraft || rev.Version != 1 || rev.BaseRevisionID != nil ||
		rev.SubmittedAt != nil || rev.AuthorID != f.author || rev.SubjectID != f.subject || *rev.ChapterID != chapter {
		t.Fatalf("unexpected first draft: %+v", rev)
	}
	if !strings.HasPrefix(rev.Title, "Nombres complexes") || rev.Objectives == "" || rev.Language != "fr" || rev.SchemaVersion != 1 {
		t.Fatalf("metadata not taken from document: %+v", rev)
	}
	if len(rev.MediaIDs) != 1 || rev.MediaIDs[0] != img {
		t.Fatalf("media: %v", rev.MediaIDs)
	}
	lesson, err := f.store.Lesson(f.ctx, rev.LessonID)
	if err != nil {
		t.Fatal(err)
	}
	if lesson.PublicID == "" || lesson.PublicID == lesson.TranslationGroupID || lesson.PublishedRevisionID != nil || lesson.CreatedBy != f.author {
		t.Fatalf("unexpected lesson: %+v", lesson)
	}

	saved, err := f.store.SaveDraft(f.ctx, f.author, rev.ID, 1, lessons.Fields{Content: doc("Second title", "fr")})
	if err != nil {
		t.Fatal(err)
	}
	if saved.Version != 2 || saved.Title != "Second title" || saved.ChapterID != nil || len(saved.MediaIDs) != 0 || saved.Prerequisites != "" {
		t.Fatalf("save did not replace fields: %+v", saved)
	}
	sub, err := f.store.Submit(f.ctx, f.author, rev.ID, 2)
	if err != nil {
		t.Fatal(err)
	}
	if sub.Status != lessons.StatusInReview || sub.SubmittedAt == nil || sub.Title != "Second title" {
		t.Fatalf("unexpected submission: %+v", sub)
	}
	var stored rendering.Document
	if err := json.Unmarshal(sub.Content, &stored); err != nil || rendering.Validate(stored) != nil {
		t.Fatalf("stored snapshot does not validate: %v", err)
	}
}

func TestPermissionDenials(t *testing.T) {
	f := newFixture(t)
	student := f.user("student")
	reviewer := f.user("reviewer")
	if _, err := f.perms.GrantReviewer(f.ctx, operator, reviewer, f.subject); err != nil {
		t.Fatal(err)
	}
	publisher := f.user("publisher")
	if _, err := f.perms.Grant(f.ctx, operator, publisher, permissions.Publisher); err != nil {
		t.Fatal(err)
	}
	nl := lessons.NewLesson{SubjectID: f.subject, Language: "fr", Fields: lessons.Fields{Content: doc("Denied", "fr")}}
	for name, actor := range map[string]int64{"anonymous": 0, "student": student, "reviewer": reviewer, "publisher": publisher, "unknown": 999999} {
		_, err := f.store.CreateLesson(f.ctx, actor, nl)
		if !errors.Is(err, lessons.ErrDenied) {
			t.Errorf("%s CreateLesson: %v", name, err)
		}
	}
	if n := f.count(`SELECT count(*) FROM lessons`); n != 0 {
		t.Fatalf("denied creation stored %d lessons", n)
	}

	rev := f.lesson("Owned")
	other := f.newAuthor("other")
	before := f.snapshot(rev.ID)
	if _, err := f.store.SaveDraft(f.ctx, other, rev.ID, rev.Version, lessons.Fields{Content: doc("Hijack", "fr")}); !errors.Is(err, lessons.ErrDenied) {
		t.Errorf("other author SaveDraft: %v", err)
	}
	if _, err := f.store.Submit(f.ctx, other, rev.ID, rev.Version); !errors.Is(err, lessons.ErrDenied) {
		t.Errorf("other author Submit: %v", err)
	}
	for name, actor := range map[string]int64{"anonymous": 0, "student": student} {
		if _, err := f.store.SaveDraft(f.ctx, actor, rev.ID, rev.Version, lessons.Fields{Content: doc("Hijack", "fr")}); !errors.Is(err, lessons.ErrDenied) {
			t.Errorf("%s SaveDraft: %v", name, err)
		}
		if _, err := f.store.Submit(f.ctx, actor, rev.ID, rev.Version); !errors.Is(err, lessons.ErrDenied) {
			t.Errorf("%s Submit: %v", name, err)
		}
		if _, err := f.store.CreateDraft(f.ctx, actor, rev.LessonID, lessons.Fields{Content: doc("x", "fr")}); !errors.Is(err, lessons.ErrDenied) {
			t.Errorf("%s CreateDraft: %v", name, err)
		}
	}
	if after := f.snapshot(rev.ID); after != before {
		t.Fatal("denied calls changed the draft")
	}

	// Revoking or disabling takes effect on the next call, even for one's own draft.
	if _, err := f.perms.Revoke(f.ctx, operator, f.author, permissions.Author); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.SaveDraft(f.ctx, f.author, rev.ID, rev.Version, lessons.Fields{Content: doc("Revoked", "fr")}); !errors.Is(err, lessons.ErrDenied) {
		t.Errorf("revoked author SaveDraft: %v", err)
	}
	disabled := f.newAuthor("disabled")
	f.mustExec(`UPDATE users SET disabled_at = now() WHERE id = $1`, disabled)
	if _, err := f.store.CreateLesson(f.ctx, disabled, nl); !errors.Is(err, lessons.ErrDenied) {
		t.Errorf("disabled author CreateLesson: %v", err)
	}
}

func TestInvalidContentStoresNothing(t *testing.T) {
	f := newFixture(t)
	valid := string(doc("Valid", "fr"))
	cases := map[string][]byte{
		"not json":         []byte("{"),
		"unknown field":    []byte(strings.Replace(valid, `"blocks"`, `"html":"<script>","blocks"`, 1)),
		"reviewed status":  []byte(strings.Replace(valid, `"unreviewed"`, `"approved"`, 1)),
		"schema version 2": []byte(strings.Replace(valid, `"schemaVersion":1`, `"schemaVersion":2`, 1)),
		"raw html node":    []byte(strings.Replace(valid, `"type":"paragraph"`, `"type":"html"`, 1)),
		"remote image":     []byte(strings.Replace(valid, `{"children":[{"text":"Simulated text: Valid","type":"text"}],"type":"paragraph"}`, `{"type":"image","src":"https://example.test/x.png","alt":"x"}`, 1)),
		"two documents":    []byte(valid + valid),
		"empty":            nil,
	}
	for name, content := range cases {
		if string(content) == valid {
			t.Fatalf("%s: case did not modify the document", name)
		}
		_, err := f.store.CreateLesson(f.ctx, f.author, lessons.NewLesson{SubjectID: f.subject, Language: "fr", Fields: lessons.Fields{Content: content}})
		if !errors.Is(err, lessons.ErrInvalidContent) {
			t.Errorf("%s: want ErrInvalidContent, got %v", name, err)
		}
	}
	_, err := f.store.CreateLesson(f.ctx, f.author, lessons.NewLesson{SubjectID: f.subject, Language: "ar", Fields: lessons.Fields{Content: doc("Wrong language", "fr")}})
	wantErr(t, err, lessons.ErrLanguageMismatch)
	_, err = f.store.CreateLesson(f.ctx, f.author, lessons.NewLesson{SubjectID: f.subject, Language: "fr",
		Fields: lessons.Fields{Content: doc("Long prerequisites", "fr"), Prerequisites: strings.Repeat("x", lessons.MaxPrerequisites+1)}})
	wantErr(t, err, lessons.ErrInvalidContent)
	if n := f.count(`SELECT count(*) FROM lessons`) + f.count(`SELECT count(*) FROM lesson_revisions`); n != 0 {
		t.Fatalf("invalid content stored %d rows", n)
	}

	rev := f.lesson("Kept")
	before := f.snapshot(rev.ID)
	for name, content := range cases {
		if _, err := f.store.SaveDraft(f.ctx, f.author, rev.ID, rev.Version, lessons.Fields{Content: content}); !errors.Is(err, lessons.ErrInvalidContent) {
			t.Errorf("save %s: want ErrInvalidContent, got %v", name, err)
		}
	}
	if _, err := f.store.SaveDraft(f.ctx, f.author, rev.ID, rev.Version, lessons.Fields{Content: doc("Arabic", "ar")}); !errors.Is(err, lessons.ErrLanguageMismatch) {
		t.Errorf("save wrong language: %v", err)
	}
	if f.snapshot(rev.ID) != before {
		t.Fatal("rejected saves changed the draft")
	}
}

func TestForeignKeysAndUniqueness(t *testing.T) {
	f := newFixture(t)
	physics := f.newSubject("simulated-physics")
	foreignChapter := f.chapter(physics, "simulated-chapter")
	ownChapter := f.chapter(f.subject, "simulated-chapter") // same code, other subject: allowed

	// Store paths map violations to typed errors.
	_, err := f.store.CreateLesson(f.ctx, f.author, lessons.NewLesson{SubjectID: 999999, Language: "fr", Fields: lessons.Fields{Content: doc("x", "fr")}})
	wantErr(t, err, lessons.ErrUnknownSubject)
	_, err = f.store.CreateLesson(f.ctx, f.author, lessons.NewLesson{SubjectID: f.subject, Language: "fr", Fields: lessons.Fields{Content: doc("x", "fr"), ChapterID: &foreignChapter}})
	wantErr(t, err, lessons.ErrUnknownChapter)
	missing := int64(999999)
	_, err = f.store.CreateLesson(f.ctx, f.author, lessons.NewLesson{SubjectID: f.subject, Language: "fr", Fields: lessons.Fields{Content: doc("x", "fr"), ChapterID: &missing}})
	wantErr(t, err, lessons.ErrUnknownChapter)
	_, err = f.store.CreateLesson(f.ctx, f.author, lessons.NewLesson{SubjectID: f.subject, Language: "fr", Fields: lessons.Fields{Content: doc("x", "fr"), MediaIDs: []int64{999999}}})
	wantErr(t, err, lessons.ErrUnknownMedia)
	if n := f.count(`SELECT count(*) FROM lessons`); n != 0 {
		t.Fatalf("rejected creations stored %d lessons", n)
	}

	rev := f.lesson("Draft")
	if _, err := f.store.SaveDraft(f.ctx, f.author, rev.ID, rev.Version, lessons.Fields{Content: doc("x", "fr"), ChapterID: &foreignChapter}); !errors.Is(err, lessons.ErrUnknownChapter) {
		t.Errorf("save foreign chapter: %v", err)
	}
	if _, err := f.store.SaveDraft(f.ctx, f.author, rev.ID, rev.Version, lessons.Fields{Content: doc("x", "fr"), MediaIDs: []int64{999999}}); !errors.Is(err, lessons.ErrUnknownMedia) {
		t.Errorf("save unknown media: %v", err)
	}
	if _, err := f.store.CreateDraft(f.ctx, f.author, 999999, lessons.Fields{Content: doc("x", "fr")}); !errors.Is(err, lessons.ErrNotFound) {
		t.Errorf("draft of unknown lesson: %v", err)
	}
	if _, err := f.store.SaveDraft(f.ctx, f.author, 999999, 1, lessons.Fields{Content: doc("x", "fr")}); !errors.Is(err, lessons.ErrNotFound) {
		t.Errorf("save unknown revision: %v", err)
	}

	// Direct SQL is rejected by the database itself.
	content := string(doc("Direct", "fr"))
	insert := func(lesson int64, number int, language string, subject int64, chapter, base *int64, status string) error {
		return f.exec(`
			INSERT INTO lesson_revisions (lesson_id, revision_number, status, author_id, base_revision_id, title, objectives,
				content, schema_version, language, subject_id, chapter_id, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6::jsonb->>'title', $6::jsonb->>'objective', $6, 1, $7, $8, $9, now(), now())`,
			lesson, number, status, f.author, base, strings.Replace(content, `"language":"fr"`, `"language":"`+language+`"`, 1),
			language, subject, chapter)
	}
	other := f.lesson("Other lesson")
	f.mustExec(`UPDATE lesson_revisions SET status = 'in_review', submitted_at = now() WHERE id = $1`, rev.ID)
	f.mustRequestChanges(rev.ID)
	wantSQLState(t, "duplicate revision number", insert(rev.LessonID, 1, "fr", f.subject, nil, nil, "draft"), "23505")
	wantSQLState(t, "chapter of another subject", insert(rev.LessonID, 2, "fr", f.subject, &foreignChapter, nil, "draft"), "23503")
	wantSQLState(t, "subject differs from lesson", insert(rev.LessonID, 2, "fr", physics, nil, nil, "draft"), "23503")
	wantSQLState(t, "language differs from lesson", insert(rev.LessonID, 2, "ar", f.subject, nil, nil, "draft"), "23503")
	// The ownership guard refuses a revision of an unknown lesson (it has no
	// owner) before the foreign key would.
	wantGuard(t, "unknown lesson", insert(999999, 1, "fr", f.subject, nil, nil, "draft"), "authored by the lesson's owner")
	// A base from another lesson is also covered by a composite foreign key;
	// the insert guard (base must equal the current published pointer) fires first.
	wantSQLState(t, "base from another lesson", insert(rev.LessonID, 2, "fr", f.subject, nil, &other.ID, "draft"), "LRN01")
	wantSQLState(t, "base is not the published revision", insert(rev.LessonID, 2, "fr", f.subject, nil, &rev.ID, "draft"), "LRN01")
	wantSQLState(t, "created as approved", insert(rev.LessonID, 2, "fr", f.subject, nil, nil, "approved"), "LRN01")
	if err := insert(rev.LessonID, 2, "fr", f.subject, &ownChapter, nil, "draft"); err != nil {
		t.Fatalf("valid direct insert rejected: %v", err)
	}
	wantSQLState(t, "title differs from content", f.exec(`UPDATE lesson_revisions SET title = 'Other' WHERE lesson_id = $1 AND revision_number = 2`, rev.LessonID), "23514")
	wantSQLState(t, "duplicate chapter code", f.exec(`INSERT INTO chapters (subject_id, code) VALUES ($1, 'simulated-chapter')`, f.subject), "23505")
	wantSQLState(t, "chapter of unknown subject", f.exec(`INSERT INTO chapters (subject_id, code) VALUES (999999, 'x')`), "23503")
	wantSQLState(t, "bad chapter code", f.exec(`INSERT INTO chapters (subject_id, code) VALUES ($1, 'Bad Code')`, f.subject), "23514")
	wantSQLState(t, "chapter repurposed", f.exec(`UPDATE chapters SET subject_id = $1 WHERE id = $2`, physics, ownChapter), "LRN01")
	wantSQLState(t, "chapter renamed", f.exec(`UPDATE chapters SET code = 'renamed' WHERE id = $1`, ownChapter), "LRN01")
	wantSQLState(t, "lesson subject changed", f.exec(`UPDATE lessons SET subject_id = $1 WHERE id = $2`, physics, rev.LessonID), "LRN01")
	wantSQLState(t, "lesson language changed", f.exec(`UPDATE lessons SET language = 'ar' WHERE id = $1`, rev.LessonID), "LRN01")
	wantSQLState(t, "lesson public id changed", f.exec(`UPDATE lessons SET public_id = gen_random_uuid() WHERE id = $1`, rev.LessonID), "LRN01")
	wantSQLState(t, "bad lesson language", f.exec(`
		INSERT INTO lessons (translation_group_id, language, subject_id, created_at, created_by)
		SELECT translation_group_id, 'en', subject_id, now(), created_by FROM lessons WHERE id = $1`, rev.LessonID), "23514")
	// The published pointer must name a revision of the same lesson: the
	// approved-revision guard rejects it first, the deferred composite
	// foreign key would at commit.
	wantSQLState(t, "pointer to another lesson's revision", f.exec(`UPDATE lessons SET published_revision_id = $1 WHERE id = $2`, other.ID, rev.LessonID), "LRN01")
	wantSQLState(t, "media from unknown asset", f.exec(`INSERT INTO revision_media VALUES ($1, 999999)`, other.ID), "23503")

	// Translation groups hold at most one lesson per language.
	ar, err := f.store.CreateLesson(f.ctx, f.author, lessons.NewLesson{SubjectID: f.subject, Language: "ar", TranslationOf: rev.LessonID, Fields: lessons.Fields{Content: doc("Translation", "ar")}})
	if err != nil {
		t.Fatal(err)
	}
	a, _ := f.store.Lesson(f.ctx, ar.LessonID)
	b, _ := f.store.Lesson(f.ctx, rev.LessonID)
	if a.TranslationGroupID != b.TranslationGroupID || a.PublicID == b.PublicID {
		t.Fatalf("translation not grouped: %+v %+v", a, b)
	}
	_, err = f.store.CreateLesson(f.ctx, f.author, lessons.NewLesson{SubjectID: f.subject, Language: "fr", TranslationOf: rev.LessonID, Fields: lessons.Fields{Content: doc("Duplicate", "fr")}})
	wantErr(t, err, lessons.ErrTranslationExists)
}

func TestConcurrentDraftCreationExactlyOne(t *testing.T) {
	f := newFixture(t)
	const workers = 8
	for i := 0; i < 15; i++ {
		rev := f.submitted(fmt.Sprintf("Lesson %d", i))
		f.mustRequestChanges(rev.ID)
		var start sync.WaitGroup
		start.Add(1)
		errs := make(chan error, workers)
		for w := 0; w < workers; w++ {
			go func() {
				start.Wait()
				_, err := f.store.CreateDraft(f.ctx, f.author, rev.LessonID, lessons.Fields{Content: doc("Successor", "fr")})
				errs <- err
			}()
		}
		start.Done()
		ok := 0
		for w := 0; w < workers; w++ {
			err := <-errs
			switch {
			case err == nil:
				ok++
			case !errors.Is(err, lessons.ErrActiveCandidateExists):
				t.Fatalf("iteration %d: unexpected error %v", i, err)
			}
		}
		if ok != 1 {
			t.Fatalf("iteration %d: %d drafts created, want exactly 1", i, ok)
		}
		if n := f.count(`SELECT count(*) FROM lesson_revisions WHERE lesson_id = $1 AND status IN ('draft', 'in_review')`, rev.LessonID); n != 1 {
			t.Fatalf("iteration %d: %d active candidates", i, n)
		}
	}
}

// The database constraint holds even for writers that bypass the Store's lock.
func TestActiveCandidateConstraintUnderDirectInserts(t *testing.T) {
	f := newFixture(t)
	const workers = 8
	for i := 0; i < 15; i++ {
		rev := f.submitted(fmt.Sprintf("Lesson %d", i))
		f.mustRequestChanges(rev.ID)
		var start sync.WaitGroup
		start.Add(1)
		errs := make(chan error, workers)
		for w := 0; w < workers; w++ {
			go func() {
				start.Wait()
				// Distinct revision numbers, so only the active-candidate index can reject.
				errs <- f.exec(`
					INSERT INTO lesson_revisions (lesson_id, revision_number, author_id, title, objectives, content,
						schema_version, language, subject_id, created_at, updated_at)
					SELECT lesson_id, $2, author_id, title, objectives, content, schema_version, language, subject_id, now(), now()
					  FROM lesson_revisions WHERE id = $1`, rev.ID, w+2)
			}()
		}
		start.Done()
		ok := 0
		for w := 0; w < workers; w++ {
			err := <-errs
			var pgErr *pgconn.PgError
			switch {
			case err == nil:
				ok++
			case !errors.As(err, &pgErr) || pgErr.ConstraintName != "lesson_revisions_one_active_candidate":
				t.Fatalf("iteration %d: unexpected error %v", i, err)
			}
		}
		if ok != 1 {
			t.Fatalf("iteration %d: %d inserts succeeded, want exactly 1", i, ok)
		}
	}
}

func TestConcurrentSavesOfSameVersion(t *testing.T) {
	f := newFixture(t)
	for i := 0; i < 15; i++ {
		rev := f.lesson(fmt.Sprintf("Lesson %d", i))
		titles := []string{"Tab A", "Tab B"}
		var start sync.WaitGroup
		start.Add(1)
		type result struct {
			title string
			rev   lessons.Revision
			err   error
		}
		results := make(chan result, len(titles))
		for _, title := range titles {
			go func() {
				start.Wait()
				r, err := f.store.SaveDraft(f.ctx, f.author, rev.ID, rev.Version, lessons.Fields{Content: doc(title, "fr")})
				results <- result{title, r, err}
			}()
		}
		start.Done()
		var winner string
		conflicts := 0
		for range titles {
			r := <-results
			switch {
			case r.err == nil:
				winner = r.title
			case errors.Is(r.err, lessons.ErrVersionConflict):
				conflicts++
			default:
				t.Fatalf("iteration %d: %v", i, r.err)
			}
		}
		if winner == "" || conflicts != 1 {
			t.Fatalf("iteration %d: winner %q, %d conflicts", i, winner, conflicts)
		}
		stored, err := f.store.Revision(f.ctx, rev.ID)
		if err != nil {
			t.Fatal(err)
		}
		if stored.Title != winner || stored.Version != rev.Version+1 || !strings.Contains(string(stored.Content), "Simulated text: "+winner) {
			t.Fatalf("iteration %d: winner %q overwritten: %+v", i, winner, stored)
		}
		// A later stale save is rejected too and leaves the winner intact.
		before := f.snapshot(rev.ID)
		if _, err := f.store.SaveDraft(f.ctx, f.author, rev.ID, rev.Version, lessons.Fields{Content: doc("Stale", "fr")}); !errors.Is(err, lessons.ErrVersionConflict) {
			t.Fatalf("stale save: %v", err)
		}
		if _, err := f.store.Submit(f.ctx, f.author, rev.ID, rev.Version); !errors.Is(err, lessons.ErrVersionConflict) {
			t.Fatalf("stale submit: %v", err)
		}
		if f.snapshot(rev.ID) != before {
			t.Fatal("stale writes changed the draft")
		}
	}
}

func TestSubmittedSnapshotIsImmutable(t *testing.T) {
	f := newFixture(t)
	chapter := f.chapter(f.subject, "simulated-chapter")
	other := f.chapter(f.subject, "simulated-other")
	img, img2 := f.media(f.author), f.media(f.author)
	rev, err := f.store.CreateLesson(f.ctx, f.author, lessons.NewLesson{SubjectID: f.subject, Language: "fr",
		Fields: lessons.Fields{Content: doc("Frozen", "fr"), ChapterID: &chapter, Prerequisites: "Simulated", MediaIDs: []int64{img}}})
	if err != nil {
		t.Fatal(err)
	}
	rev, err = f.store.Submit(f.ctx, f.author, rev.ID, rev.Version)
	if err != nil {
		t.Fatal(err)
	}
	before := f.snapshot(rev.ID)

	// Through the Store.
	if _, err := f.store.SaveDraft(f.ctx, f.author, rev.ID, rev.Version, lessons.Fields{Content: doc("Changed", "fr")}); !errors.Is(err, lessons.ErrNotDraft) {
		t.Errorf("SaveDraft after submit: %v", err)
	}
	if _, err := f.store.Submit(f.ctx, f.author, rev.ID, rev.Version); !errors.Is(err, lessons.ErrNotDraft) {
		t.Errorf("second Submit: %v", err)
	}

	// Through direct SQL, column by column; the guard runs before any other check.
	updates := map[string]string{
		"content":          `content = jsonb_set(content, '{blocks,1,children,1,children,0,children,0,text}', '"Edited solution"')`,
		"exercise":         `content = jsonb_set(content, '{blocks,1,title}', '"Edited exercise"')`,
		"title":            `title = 'Edited'`,
		"objectives":       `objectives = 'Edited'`,
		"prerequisites":    `prerequisites = 'Edited'`,
		"schema_version":   `schema_version = 2`,
		"chapter_id":       fmt.Sprintf(`chapter_id = %d`, other),
		"chapter_id null":  `chapter_id = NULL`,
		"version":          `version = version + 1`,
		"submitted_at":     `submitted_at = submitted_at + interval '1 second'`,
		"created_at":       `created_at = created_at - interval '1 day'`,
		"author_id":        `author_id = author_id`, // unchanged value is allowed; see below
		"base_revision_id": `base_revision_id = id`,
		"lesson_id":        `lesson_id = lesson_id + 1`,
		"revision_number":  `revision_number = 99`,
		"language":         `language = 'ar'`,
		"subject_id":       `subject_id = subject_id + 1`,
		"back to draft":    `status = 'draft', submitted_at = NULL`,
		"status only back": `status = 'draft'`,
		"decide and edit":  `status = 'changes_requested', title = 'Edited'`,
	}
	for name, set := range updates {
		err := f.exec(`UPDATE lesson_revisions SET `+set+` WHERE id = $1`, rev.ID)
		if name == "author_id" {
			if err != nil {
				t.Errorf("no-op update rejected: %v", err)
			}
			continue
		}
		wantSQLState(t, name, err, "LRN01")
	}
	reviewer := f.user("someone")
	wantSQLState(t, "author_id changed", f.exec(`UPDATE lesson_revisions SET author_id = $2 WHERE id = $1`, rev.ID, reviewer), "LRN01")
	wantSQLState(t, "media added", f.exec(`INSERT INTO revision_media VALUES ($1, $2)`, rev.ID, img2), "LRN01")
	wantSQLState(t, "media removed", f.exec(`DELETE FROM revision_media WHERE revision_id = $1`, rev.ID), "LRN01")
	wantSQLState(t, "media swapped", f.exec(`UPDATE revision_media SET media_asset_id = $2 WHERE revision_id = $1`, rev.ID, img2), "LRN01")
	wantSQLState(t, "revision deleted", f.exec(`DELETE FROM lesson_revisions WHERE id = $1`, rev.ID), "LRN01")
	wantSQLState(t, "revisions truncated", f.exec(`TRUNCATE lesson_revisions CASCADE`), "LRN01")
	wantSQLState(t, "revision media truncated", f.exec(`TRUNCATE revision_media`), "LRN01")

	if after := f.snapshot(rev.ID); after != before {
		t.Fatalf("submitted snapshot changed:\nbefore %s\nafter  %s", before, after)
	}

	// Status itself may still move forward (review decisions are LRN-007),
	// and the frozen snapshot stays byte-identical apart from status.
	f.mustApprove(rev.ID)
	if got := strings.Replace(f.snapshot(rev.ID), `"status": "approved"`, `"status": "in_review"`, 1); got != before {
		t.Fatalf("status change altered the snapshot:\n%s\n%s", before, got)
	}
	wantSQLState(t, "approved back to review", f.exec(`UPDATE lesson_revisions SET status = 'in_review' WHERE id = $1`, rev.ID), "LRN01")
}

func TestDraftIdentityColumnsAreFixed(t *testing.T) {
	f := newFixture(t)
	rev := f.lesson("Draft")
	other := f.newAuthor("other")
	wantSQLState(t, "draft author changed", f.exec(`UPDATE lesson_revisions SET author_id = $2 WHERE id = $1`, rev.ID, other), "LRN01")
	wantSQLState(t, "draft renumbered", f.exec(`UPDATE lesson_revisions SET revision_number = 5 WHERE id = $1`, rev.ID), "LRN01")
	// Drafts are not frozen otherwise, and may be discarded.
	img := f.media(f.author)
	f.mustExec(`INSERT INTO revision_media VALUES ($1, $2)`, rev.ID, img)
	f.mustExec(`DELETE FROM lesson_revisions WHERE id = $1`, rev.ID)
}

func TestChangesRequestedDoesNotBlockSuccessor(t *testing.T) {
	f := newFixture(t)
	rev := f.submitted("First")
	// An in-review revision is the active candidate.
	if _, err := f.store.CreateDraft(f.ctx, f.author, rev.LessonID, lessons.Fields{Content: doc("Too early", "fr")}); !errors.Is(err, lessons.ErrActiveCandidateExists) {
		t.Fatalf("draft while in review: %v", err)
	}
	f.mustRequestChanges(rev.ID)
	before := f.snapshot(rev.ID)

	next, err := f.store.CreateDraft(f.ctx, f.author, rev.LessonID, lessons.Fields{Content: doc("Corrected", "fr")})
	if err != nil {
		t.Fatalf("successor blocked: %v", err)
	}
	if next.Number != 2 || next.Status != lessons.StatusDraft || next.BaseRevisionID != nil {
		t.Fatalf("unexpected successor: %+v", next)
	}
	if _, err := f.store.CreateDraft(f.ctx, f.author, rev.LessonID, lessons.Fields{Content: doc("Third", "fr")}); !errors.Is(err, lessons.ErrActiveCandidateExists) {
		t.Fatalf("second successor: %v", err)
	}
	if _, err := f.store.Submit(f.ctx, f.author, next.ID, next.Version); err != nil {
		t.Fatal(err)
	}
	if f.snapshot(rev.ID) != before {
		t.Fatal("changes-requested revision was altered")
	}
	if n := f.count(`SELECT count(*) FROM lesson_revisions WHERE lesson_id = $1`, rev.LessonID); n != 2 {
		t.Fatalf("history has %d revisions, want 2", n)
	}
}

func TestPublishedPointerUnaffectedByDrafting(t *testing.T) {
	f := newFixture(t)
	rev := f.submitted("Published")
	// Simulate approval and publication with direct SQL.
	f.mustApprove(rev.ID)
	if err := f.publish(rev.LessonID, rev.ID); err != nil {
		t.Fatal(err)
	}
	published := f.snapshot(rev.ID)

	// An approved, published revision is not an active candidate.
	draft, err := f.store.CreateDraft(f.ctx, f.author, rev.LessonID, lessons.Fields{Content: doc("Next edition", "fr")})
	if err != nil {
		t.Fatal(err)
	}
	if draft.BaseRevisionID == nil || *draft.BaseRevisionID != rev.ID {
		t.Fatalf("base %v, want %d", draft.BaseRevisionID, rev.ID)
	}
	draft, err = f.store.SaveDraft(f.ctx, f.author, draft.ID, draft.Version, lessons.Fields{Content: doc("Next edition, saved", "fr")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.Submit(f.ctx, f.author, draft.ID, draft.Version); err != nil {
		t.Fatal(err)
	}
	lesson, err := f.store.Lesson(f.ctx, rev.LessonID)
	if err != nil {
		t.Fatal(err)
	}
	if lesson.PublishedRevisionID == nil || *lesson.PublishedRevisionID != rev.ID {
		t.Fatalf("published pointer moved: %v", lesson.PublishedRevisionID)
	}
	if f.snapshot(rev.ID) != published {
		t.Fatal("published snapshot changed")
	}
	// Archived lessons accept no new drafts.
	f.mustRequestChanges(draft.ID)
	f.mustExec(`UPDATE lessons SET archived_at = now() WHERE id = $1`, rev.LessonID)
	if _, err := f.store.CreateDraft(f.ctx, f.author, rev.LessonID, lessons.Fields{Content: doc("Archived", "fr")}); !errors.Is(err, lessons.ErrArchived) {
		t.Fatalf("draft of archived lesson: %v", err)
	}
}

func TestMediaAssetsAreImmutable(t *testing.T) {
	f := newFixture(t)
	img := f.media(f.author)
	wantSQLState(t, "update", f.exec(`UPDATE media_assets SET width = 3 WHERE id = $1`, img), "LRN01")
	wantSQLState(t, "update key", f.exec(`UPDATE media_assets SET storage_key = 'simulated/x.png' WHERE id = $1`, img), "LRN01")
	wantSQLState(t, "delete", f.exec(`DELETE FROM media_assets WHERE id = $1`, img), "LRN01")
	wantSQLState(t, "truncate", f.exec(`TRUNCATE media_assets CASCADE`), "LRN01")

	var sum []byte
	if err := f.db.QueryRow(f.ctx, `SELECT sha256 FROM media_assets WHERE id = $1`, img).Scan(&sum); err != nil {
		t.Fatal(err)
	}
	ins := func(sha []byte, ctype, key string) error {
		return f.exec(`INSERT INTO media_assets (sha256, content_type, byte_size, width, height, storage_key, created_at, created_by)
			VALUES ($1, $2, 10, 2, 2, $3, now(), $4)`, sha, ctype, key, f.author)
	}
	fresh := make([]byte, 32)
	if _, err := rand.Read(fresh); err != nil {
		t.Fatal(err)
	}
	wantSQLState(t, "duplicate sha256", ins(sum, "image/png", "simulated/dup.png"), "23505")
	wantSQLState(t, "svg", ins(fresh, "image/svg+xml", "simulated/a.png"), "23514")
	wantSQLState(t, "traversal key", ins(fresh, "image/png", "../etc/passwd.png"), "23514")
	wantSQLState(t, "absolute key", ins(fresh, "image/png", "/tmp/a.png"), "23514")
	wantSQLState(t, "short hash", ins(fresh[:16], "image/png", "simulated/b.png"), "23514")
	if err := ins(fresh, "image/webp", "simulated/c.webp"); err != nil {
		t.Fatalf("valid asset rejected: %v", err)
	}
}
