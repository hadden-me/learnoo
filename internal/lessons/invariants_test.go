package lessons_test

import (
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hadden-me/learnoo/internal/lessons"
	"github.com/hadden-me/learnoo/internal/permissions"
	"github.com/hadden-me/learnoo/internal/rendering"
	"github.com/jackc/pgx/v5"
)

// waitBlockedBy waits until some backend is waiting on a lock held by pid.
func (e *env) waitBlockedBy(pid uint32) {
	e.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var blocked bool
		err := e.db.QueryRow(e.ctx, `SELECT EXISTS (SELECT 1 FROM pg_stat_activity WHERE $1::int = ANY (pg_blocking_pids(pid)))`, int32(pid)).Scan(&blocked)
		if err != nil {
			e.t.Fatal(err)
		}
		if blocked {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	e.t.Fatal("no backend waited on the held lock")
}

// largeDoc builds a valid test document whose compact encoding is just under
// the 128 KiB limit, with enough nodes that PostgreSQL's spaced jsonb output
// is over it.
func largeDoc(t *testing.T) []byte {
	t.Helper()
	build := func(n int) []byte {
		d := rendering.Document{SchemaVersion: 1, Title: "Simulated large document", Language: "fr", Status: "unreviewed", Objective: "Simulated."}
		for i := 0; i < 240; i++ {
			d.Blocks = append(d.Blocks, rendering.Node{Type: "paragraph", Children: []rendering.Node{{Type: "text", Text: strings.Repeat("a", n)}}})
		}
		b, err := json.Marshal(d)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	base := len(build(1)) // text is omitted when empty, so measure from one byte
	b := build(1 + (rendering.MaxDocumentBytes-64-base)/240)
	if len(b) > rendering.MaxDocumentBytes || len(b) < rendering.MaxDocumentBytes-400 {
		t.Fatalf("large document is %d bytes", len(b))
	}
	return b
}

func TestNearLimitDocumentSavesAndSubmits(t *testing.T) {
	f := newFixture(t)
	big := largeDoc(t)
	rev, err := f.store.CreateLesson(f.ctx, f.author, lessons.NewLesson{SubjectID: f.subject, Language: "fr", Fields: lessons.Fields{Content: big}})
	if err != nil {
		t.Fatal(err)
	}
	var stored int
	if err := f.db.QueryRow(f.ctx, `SELECT octet_length(content::text) FROM lesson_revisions WHERE id = $1`, rev.ID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored <= rendering.MaxDocumentBytes {
		t.Fatalf("test is not meaningful: jsonb text is only %d bytes", stored)
	}
	// Indented input over the limit is accepted: validation sees the compact form.
	var v any
	if err := json.Unmarshal(big, &v); err != nil {
		t.Fatal(err)
	}
	indented, err := json.MarshalIndent(v, "", "    ")
	if err != nil {
		t.Fatal(err)
	}
	if len(indented) <= rendering.MaxDocumentBytes {
		t.Fatal("indented document is not over the limit")
	}
	rev, err = f.store.SaveDraft(f.ctx, f.author, rev.ID, rev.Version, lessons.Fields{Content: indented})
	if err != nil {
		t.Fatalf("indented save: %v", err)
	}
	if _, err := f.store.Submit(f.ctx, f.author, rev.ID, rev.Version); err != nil {
		t.Fatalf("submit of near-limit document: %v", err)
	}
	// Over the limit even when compact: rejected.
	over := strings.Replace(string(big), `"Simulated."`, `"Simulated.`+strings.Repeat("b", 500)+`"`, 1)
	_, err = f.store.CreateLesson(f.ctx, f.author, lessons.NewLesson{SubjectID: f.subject, Language: "fr", Fields: lessons.Fields{Content: []byte(over)}})
	wantErr(t, err, lessons.ErrInvalidContent)
}

func TestSubmitRevalidatesStoredContent(t *testing.T) {
	f := newFixture(t)
	rev := f.lesson("Corrupted")
	// A writer bypassing the Store stores content the schema rejects.
	f.mustExec(`UPDATE lesson_revisions SET content = jsonb_set(content, '{blocks,0,type}', '"html"') WHERE id = $1`, rev.ID)
	before := f.snapshot(rev.ID)
	if _, err := f.store.Submit(f.ctx, f.author, rev.ID, rev.Version); !errors.Is(err, lessons.ErrInvalidContent) {
		t.Fatalf("submit of invalid stored content: %v", err)
	}
	if f.snapshot(rev.ID) != before {
		t.Fatal("rejected submission changed the draft")
	}
}

func TestInvalidTextIsTypedError(t *testing.T) {
	f := newFixture(t)
	_, err := f.store.CreateLesson(f.ctx, f.author, lessons.NewLesson{SubjectID: f.subject, Language: "fr",
		Fields: lessons.Fields{Content: doc("NUL prerequisites", "fr"), Prerequisites: "a\x00b"}})
	wantErr(t, err, lessons.ErrInvalidContent)
	_, err = f.store.CreateLesson(f.ctx, f.author, lessons.NewLesson{SubjectID: f.subject, Language: "fr",
		Fields: lessons.Fields{Content: doc("NUL \x00 title", "fr")}})
	wantErr(t, err, lessons.ErrInvalidContent)
	if n := f.count(`SELECT count(*) FROM lessons`); n != 0 {
		t.Fatalf("stored %d lessons", n)
	}
}

func TestDiscardDraft(t *testing.T) {
	f := newFixture(t)
	other := f.newAuthor("other")
	img := f.media(f.author)
	rev, err := f.store.CreateLesson(f.ctx, f.author, lessons.NewLesson{SubjectID: f.subject, Language: "fr",
		Fields: lessons.Fields{Content: doc("Discarded", "fr"), MediaIDs: []int64{img}}})
	if err != nil {
		t.Fatal(err)
	}
	wantErr(t, f.store.DiscardDraft(f.ctx, other, rev.ID, rev.Version), lessons.ErrDenied)
	wantErr(t, f.store.DiscardDraft(f.ctx, 0, rev.ID, rev.Version), lessons.ErrDenied)
	wantErr(t, f.store.DiscardDraft(f.ctx, f.author, rev.ID, rev.Version+1), lessons.ErrVersionConflict)
	if err := f.store.DiscardDraft(f.ctx, f.author, rev.ID, rev.Version); err != nil {
		t.Fatal(err)
	}
	if n := f.count(`SELECT count(*) FROM lesson_revisions WHERE id = $1`, rev.ID) + f.count(`SELECT count(*) FROM revision_media WHERE revision_id = $1`, rev.ID); n != 0 {
		t.Fatalf("discarded draft left %d rows", n)
	}
	if _, err := f.store.Revision(f.ctx, rev.ID); !errors.Is(err, lessons.ErrNotFound) {
		t.Fatalf("discarded draft still readable: %v", err)
	}
	// The lesson accepts a new draft; the media asset itself is untouched.
	next, err := f.store.CreateDraft(f.ctx, f.author, rev.LessonID, lessons.Fields{Content: doc("Again", "fr")})
	if err != nil {
		t.Fatal(err)
	}
	if n := f.count(`SELECT count(*) FROM media_assets WHERE id = $1`, img); n != 1 {
		t.Fatal("media asset removed")
	}
	next, err = f.store.Submit(f.ctx, f.author, next.ID, next.Version)
	if err != nil {
		t.Fatal(err)
	}
	wantErr(t, f.store.DiscardDraft(f.ctx, f.author, next.ID, next.Version), lessons.ErrNotDraft)
	// A revoked author cannot discard either.
	f.mustExec(`UPDATE lesson_revisions SET status = 'changes_requested' WHERE id = $1`, next.ID)
	third, err := f.store.CreateDraft(f.ctx, f.author, rev.LessonID, lessons.Fields{Content: doc("Third", "fr")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.perms.Revoke(f.ctx, operator, f.author, permissions.Author); err != nil {
		t.Fatal(err)
	}
	wantErr(t, f.store.DiscardDraft(f.ctx, f.author, third.ID, third.Version), lessons.ErrDenied)
}

func TestTranslations(t *testing.T) {
	f := newFixture(t)
	physics := f.newSubject("simulated-physics")
	src := f.lesson("Source")
	_, err := f.store.CreateLesson(f.ctx, f.author, lessons.NewLesson{SubjectID: physics, Language: "ar", TranslationOf: src.LessonID, Fields: lessons.Fields{Content: doc("Other subject", "ar")}})
	wantErr(t, err, lessons.ErrTranslationSubject)
	_, err = f.store.CreateLesson(f.ctx, f.author, lessons.NewLesson{SubjectID: f.subject, Language: "ar", TranslationOf: 999999, Fields: lessons.Fields{Content: doc("Missing source", "ar")}})
	wantErr(t, err, lessons.ErrNotFound)
	// The database keeps every lesson of a group in the group's subject.
	wantSQLState(t, "cross-subject translation", f.exec(`
		INSERT INTO lessons (translation_group_id, language, subject_id, created_at, created_by)
		SELECT translation_group_id, 'ar', $2, now(), created_by FROM lessons WHERE id = $1`, src.LessonID, physics), "23503")
	wantSQLState(t, "group subject changed", f.exec(`UPDATE translation_groups SET subject_id = $1`, physics), "LRN01")

	f.mustExec(`UPDATE lessons SET archived_at = now() WHERE id = $1`, src.LessonID)
	_, err = f.store.CreateLesson(f.ctx, f.author, lessons.NewLesson{SubjectID: f.subject, Language: "ar", TranslationOf: src.LessonID, Fields: lessons.Fields{Content: doc("Archived source", "ar")}})
	wantErr(t, err, lessons.ErrArchived)
	if n := f.count(`SELECT count(*) FROM lessons`); n != 1 {
		t.Fatalf("rejected translations stored lessons: %d", n)
	}
}

func TestPublishedPointerRequiresApprovedRevision(t *testing.T) {
	f := newFixture(t)
	publish := func(lesson, rev int64) error {
		return f.exec(`UPDATE lessons SET published_revision_id = $1 WHERE id = $2`, rev, lesson)
	}
	rev := f.lesson("Pointer")
	wantSQLState(t, "draft", publish(rev.LessonID, rev.ID), "LRN01")
	f.mustExec(`UPDATE lesson_revisions SET status = 'in_review', submitted_at = now() WHERE id = $1`, rev.ID)
	wantSQLState(t, "in review", publish(rev.LessonID, rev.ID), "LRN01")
	f.mustExec(`UPDATE lesson_revisions SET status = 'changes_requested' WHERE id = $1`, rev.ID)
	wantSQLState(t, "changes requested", publish(rev.LessonID, rev.ID), "LRN01")

	other := f.submitted("Other")
	f.mustExec(`UPDATE lesson_revisions SET status = 'approved' WHERE id = $1`, other.ID)
	wantSQLState(t, "approved revision of another lesson", publish(rev.LessonID, other.ID), "LRN01")
	wantSQLState(t, "unknown revision", publish(rev.LessonID, 999999), "LRN01")
	if err := publish(other.LessonID, other.ID); err != nil {
		t.Fatalf("approved revision rejected: %v", err)
	}
	// Unpublishing (NULL) stays possible for LRN-009.
	f.mustExec(`UPDATE lessons SET published_revision_id = NULL WHERE id = $1`, other.LessonID)
}

func TestStatusAndTimestampRules(t *testing.T) {
	f := newFixture(t)
	rev := f.lesson("Draft")
	wantSQLState(t, "draft to approved", f.exec(`UPDATE lesson_revisions SET status = 'approved', submitted_at = now() WHERE id = $1`, rev.ID), "LRN01")
	wantSQLState(t, "draft to changes requested", f.exec(`UPDATE lesson_revisions SET status = 'changes_requested', submitted_at = now() WHERE id = $1`, rev.ID), "LRN01")
	sub, err := f.store.Submit(f.ctx, f.author, rev.ID, rev.Version)
	if err != nil {
		t.Fatal(err)
	}
	wantSQLState(t, "updated_at only", f.exec(`UPDATE lesson_revisions SET updated_at = updated_at + interval '1 second' WHERE id = $1`, sub.ID), "LRN01")
	f.mustExec(`UPDATE lesson_revisions SET status = 'approved', updated_at = updated_at + interval '1 second' WHERE id = $1`, sub.ID)
}

func TestReferencedRowsCannotBeDeletedOrTruncated(t *testing.T) {
	f := newFixture(t)
	chapter := f.chapter(f.subject, "simulated-chapter")
	rev := f.submitted("Kept")

	// A user without grants (so no permission audit rows) who authored rows
	// directly, in a translation group of their own (created in the same
	// transaction, as the ownership guard requires).
	plain := f.user("plain")
	var lesson int64
	err := pgx.BeginFunc(f.ctx, f.db, func(tx pgx.Tx) error {
		var group string
		if err := tx.QueryRow(f.ctx, `INSERT INTO translation_groups (subject_id) VALUES ($1) RETURNING id::text`, f.subject).Scan(&group); err != nil {
			return err
		}
		return tx.QueryRow(f.ctx, `
			INSERT INTO lessons (translation_group_id, language, subject_id, created_at, created_by)
			VALUES ($1::uuid, 'ar', $2, now(), $3) RETURNING id`, group, f.subject, plain).Scan(&lesson)
	})
	if err != nil {
		t.Fatal(err)
	}
	wantSQLState(t, "delete lesson with revisions", f.exec(`DELETE FROM lessons WHERE id = $1`, rev.LessonID), "23503")
	wantSQLState(t, "delete lesson creator", f.exec(`DELETE FROM users WHERE id = $1`, plain), "23503")
	wantSQLState(t, "delete revision author", f.exec(`DELETE FROM users WHERE id = $1`, f.author), "23503")
	wantSQLState(t, "delete subject", f.exec(`DELETE FROM subjects WHERE id = $1`, f.subject), "23503")
	wantSQLState(t, "delete unused chapter", f.exec(`DELETE FROM chapters WHERE id = $1`, chapter), "LRN01")
	wantSQLState(t, "truncate chapters", f.exec(`TRUNCATE chapters CASCADE`), "LRN01")
	for _, table := range []string{"lessons", "users", "subjects"} {
		if err := f.exec(`TRUNCATE ` + table + ` CASCADE`); err == nil {
			t.Errorf("TRUNCATE %s CASCADE succeeded", table)
		}
	}
	if n := f.count(`SELECT count(*) FROM lesson_revisions`); n != 1 {
		t.Fatalf("revisions lost: %d", n)
	}
	if n := f.count(`SELECT count(*) FROM lessons`) + f.count(`SELECT count(*) FROM chapters`); n != 3 {
		t.Fatalf("lessons or chapters lost: %d", n)
	}
}

func TestSubmissionAndMediaChangeAreSerialized(t *testing.T) {
	f := newFixture(t)
	img := f.media(f.author)

	// Submission first: a media insert waits, then sees the frozen revision.
	rev := f.lesson("Submitted first")
	tx, err := f.db.Begin(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(f.ctx) }()
	if _, err := tx.Exec(f.ctx, `UPDATE lesson_revisions SET status = 'in_review', submitted_at = now(), version = version + 1 WHERE id = $1`, rev.ID); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- f.exec(`INSERT INTO revision_media VALUES ($1, $2)`, rev.ID, img) }()
	f.waitBlockedBy(tx.Conn().PgConn().PID())
	if err := tx.Commit(f.ctx); err != nil {
		t.Fatal(err)
	}
	wantSQLState(t, "media insert racing a submission", <-done, "LRN01")
	if n := f.count(`SELECT count(*) FROM revision_media WHERE revision_id = $1`, rev.ID); n != 0 {
		t.Fatal("media reference added to a submitted revision")
	}

	// Media change first: the Store's submission waits and freezes the result.
	rev = f.lesson("Media first")
	tx2, err := f.db.Begin(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx2.Rollback(f.ctx) }()
	if _, err := tx2.Exec(f.ctx, `INSERT INTO revision_media VALUES ($1, $2)`, rev.ID, img); err != nil {
		t.Fatal(err)
	}
	subDone := make(chan error, 1)
	go func() {
		_, err := f.store.Submit(f.ctx, f.author, rev.ID, rev.Version)
		subDone <- err
	}()
	f.waitBlockedBy(tx2.Conn().PgConn().PID())
	if err := tx2.Commit(f.ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-subDone; err != nil {
		t.Fatal(err)
	}
	got, err := f.store.Revision(f.ctx, rev.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != lessons.StatusInReview || len(got.MediaIDs) != 1 {
		t.Fatalf("unexpected submitted revision: %+v", got)
	}
	wantSQLState(t, "remove after submission", f.exec(`DELETE FROM revision_media WHERE revision_id = $1`, rev.ID), "LRN01")
}

func TestRevokedAuthorCannotCreate(t *testing.T) {
	f := newFixture(t)
	rev := f.submitted("Before revocation")
	f.mustExec(`UPDATE lesson_revisions SET status = 'changes_requested' WHERE id = $1`, rev.ID)
	if _, err := f.perms.Revoke(f.ctx, operator, f.author, permissions.Author); err != nil {
		t.Fatal(err)
	}
	_, err := f.store.CreateLesson(f.ctx, f.author, lessons.NewLesson{SubjectID: f.subject, Language: "fr", Fields: lessons.Fields{Content: doc("After", "fr")}})
	wantErr(t, err, lessons.ErrDenied)
	_, err = f.store.CreateDraft(f.ctx, f.author, rev.LessonID, lessons.Fields{Content: doc("After", "fr")})
	wantErr(t, err, lessons.ErrDenied)
}

func TestArchivedLessonRejectsSaveAndSubmit(t *testing.T) {
	f := newFixture(t)
	rev := f.lesson("Archived")
	f.mustExec(`UPDATE lessons SET archived_at = now() WHERE id = $1`, rev.LessonID)
	before := f.snapshot(rev.ID)
	if _, err := f.store.SaveDraft(f.ctx, f.author, rev.ID, rev.Version, lessons.Fields{Content: doc("Edited", "fr")}); !errors.Is(err, lessons.ErrArchived) {
		t.Fatalf("save on archived lesson: %v", err)
	}
	if _, err := f.store.Submit(f.ctx, f.author, rev.ID, rev.Version); !errors.Is(err, lessons.ErrArchived) {
		t.Fatalf("submit on archived lesson: %v", err)
	}
	if f.snapshot(rev.ID) != before {
		t.Fatal("archived lesson's draft changed")
	}
}

func TestSubmitRejectsNonCanonicalStoredContent(t *testing.T) {
	f := newFixture(t)
	for name, node := range map[string]string{
		// jsonb orders keys by length then bytes: "ſrc" (4 bytes) follows
		// "src", and encoding/json folds it onto Src, last one winning.
		"long s": `{"type":"image","alt":"a","src":"javascript:alert(1)","ſrc":"/static/complex-plane.svg"}`,
		"upper":  `{"type":"image","alt":"a","SRC":"/static/complex-plane.svg","src":"javascript:alert(1)"}`,
		// Valid for the decoder but not canonical: a case-folded duplicate
		// of an allowed value.
		"duplicate": `{"type":"image","alt":"a","SRC":"/static/complex-plane.svg","src":"/static/complex-plane.svg"}`,
	} {
		rev := f.lesson("Non-canonical " + name)
		f.mustExec(`UPDATE lesson_revisions SET content = jsonb_set(content, '{blocks,0}', $2::jsonb) WHERE id = $1`, rev.ID, node)
		before := f.snapshot(rev.ID)
		if _, err := f.store.Submit(f.ctx, f.author, rev.ID, rev.Version); !errors.Is(err, lessons.ErrInvalidContent) {
			t.Errorf("%s: submit of non-canonical content: %v", name, err)
		}
		if f.snapshot(rev.ID) != before {
			t.Errorf("%s: rejected submission changed the draft", name)
		}
	}
}

func TestStoredContentIsCanonical(t *testing.T) {
	f := newFixture(t)
	// A case-folded duplicate key is refused outright by the exact-key
	// decoder (issue 41) and stores nothing.
	folded := `{
	  "schemaVersion": 1, "title": "Canonical <&>", "language": "fr", "status": "unreviewed", "objective": "x",
	  "blocks": [{"type": "image", "alt": "a", "SRC": "/static/complex-plane.svg", "src": "/static/complex-plane.svg"}]
	}`
	_, err := f.store.CreateLesson(f.ctx, f.author, lessons.NewLesson{SubjectID: f.subject, Language: "fr", Fields: lessons.Fields{Content: []byte(folded)}})
	wantErr(t, err, lessons.ErrInvalidContent)
	if n := f.count(`SELECT count(*) FROM lesson_revisions`); n != 0 {
		t.Fatalf("%d revisions stored after a refused create", n)
	}
	// Spacing and key order are normalised to the canonical encoding.
	input := `{
	  "blocks": [{"src": "/static/complex-plane.svg", "type": "image",   "alt": "a"}],
	  "schemaVersion": 1, "title": "Canonical <&>", "language": "fr", "status": "unreviewed", "objective": "x"
	}`
	rev, err := f.store.CreateLesson(f.ctx, f.author, lessons.NewLesson{SubjectID: f.subject, Language: "fr", Fields: lessons.Fields{Content: []byte(input)}})
	if err != nil {
		t.Fatal(err)
	}
	d, err := rendering.Decode(strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	var buf strings.Builder
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(d); err != nil {
		t.Fatal(err)
	}
	if n := f.count(`SELECT count(*) FROM lesson_revisions WHERE id = $1 AND content = $2::jsonb`, rev.ID, strings.TrimSpace(buf.String())); n != 1 {
		t.Fatal("stored content is not the canonical encoding")
	}
	if _, err := f.store.Submit(f.ctx, f.author, rev.ID, rev.Version); err != nil {
		t.Fatalf("canonical content rejected at submission: %v", err)
	}
}

func TestInvalidUTF8Rejected(t *testing.T) {
	f := newFixture(t)
	bad := strings.Replace(string(doc("Invalid UTF-8", "fr")), "Invalid UTF-8", "Invalid \xff UTF-8", 2)
	_, err := f.store.CreateLesson(f.ctx, f.author, lessons.NewLesson{SubjectID: f.subject, Language: "fr", Fields: lessons.Fields{Content: []byte(bad)}})
	wantErr(t, err, lessons.ErrInvalidContent)
	_, err = f.store.CreateLesson(f.ctx, f.author, lessons.NewLesson{SubjectID: f.subject, Language: "fr",
		Fields: lessons.Fields{Content: doc("Valid", "fr"), Prerequisites: "bad \xff"}})
	wantErr(t, err, lessons.ErrInvalidContent)
	if n := f.count(`SELECT count(*) FROM lessons`); n != 0 {
		t.Fatalf("stored %d lessons", n)
	}
}

func TestRevisionNumbersAreNeverReused(t *testing.T) {
	f := newFixture(t)
	first := f.submitted("Numbered")
	f.mustExec(`UPDATE lesson_revisions SET status = 'changes_requested' WHERE id = $1`, first.ID)
	second, err := f.store.CreateDraft(f.ctx, f.author, first.LessonID, lessons.Fields{Content: doc("Second", "fr")})
	if err != nil {
		t.Fatal(err)
	}
	if second.Number != 2 {
		t.Fatalf("second draft is #%d", second.Number)
	}
	if err := f.store.DiscardDraft(f.ctx, f.author, second.ID, second.Version); err != nil {
		t.Fatal(err)
	}
	third, err := f.store.CreateDraft(f.ctx, f.author, first.LessonID, lessons.Fields{Content: doc("Third", "fr")})
	if err != nil {
		t.Fatal(err)
	}
	if third.Number != 3 {
		t.Fatalf("draft after discarding #2 is #%d, want #3", third.Number)
	}
	if err := f.store.DiscardDraft(f.ctx, f.author, third.ID, third.Version); err != nil {
		t.Fatal(err)
	}
	// Direct SQL cannot reuse a number or lower the counter either.
	wantSQLState(t, "reuse discarded number", f.exec(`
		INSERT INTO lesson_revisions (lesson_id, revision_number, author_id, title, objectives, content,
			schema_version, language, subject_id, created_at, updated_at)
		SELECT lesson_id, 2, author_id, title, objectives, content, schema_version, language, subject_id, now(), now()
		  FROM lesson_revisions WHERE id = $1`, first.ID), "LRN01")
	wantSQLState(t, "lower counter", f.exec(`UPDATE lessons SET last_revision_number = 1 WHERE id = $1`, first.LessonID), "LRN01")
}

func TestConcurrentDiscardAndSubmit(t *testing.T) {
	f := newFixture(t)
	img := f.media(f.author)
	for i := 0; i < 15; i++ {
		rev, err := f.store.CreateLesson(f.ctx, f.author, lessons.NewLesson{SubjectID: f.subject, Language: "fr",
			Fields: lessons.Fields{Content: doc("Race", "fr"), MediaIDs: []int64{img}}})
		if err != nil {
			t.Fatal(err)
		}
		var start sync.WaitGroup
		start.Add(1)
		discarded, submitted := make(chan error, 1), make(chan error, 1)
		go func() {
			start.Wait()
			discarded <- f.store.DiscardDraft(f.ctx, f.author, rev.ID, rev.Version)
		}()
		go func() {
			start.Wait()
			_, err := f.store.Submit(f.ctx, f.author, rev.ID, rev.Version)
			submitted <- err
		}()
		start.Done()
		dErr, sErr := <-discarded, <-submitted
		rows := f.count(`SELECT count(*) FROM lesson_revisions WHERE id = $1`, rev.ID)
		media := f.count(`SELECT count(*) FROM revision_media WHERE revision_id = $1`, rev.ID)
		switch {
		case dErr == nil && errors.Is(sErr, lessons.ErrNotFound):
			if rows != 0 || media != 0 {
				t.Fatalf("iteration %d: discard won but %d rows, %d media remain", i, rows, media)
			}
		case sErr == nil && errors.Is(dErr, lessons.ErrNotDraft):
			got, err := f.store.Revision(f.ctx, rev.ID)
			if err != nil || got.Status != lessons.StatusInReview || len(got.MediaIDs) != 1 {
				t.Fatalf("iteration %d: submit won but revision is %+v (%v)", i, got, err)
			}
		default:
			t.Fatalf("iteration %d: discard %v, submit %v", i, dErr, sErr)
		}
	}
}
