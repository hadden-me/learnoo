package lessons_test

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/hadden-me/learnoo/internal/database"
	"github.com/hadden-me/learnoo/internal/database/dbtest"
	"github.com/hadden-me/learnoo/internal/lessons"
	"github.com/hadden-me/learnoo/internal/permissions"
	"github.com/hadden-me/learnoo/migrations"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Submission audit (migrations/0011): one append-only row per submission,
// written with the status change or not at all.

// submissionRows counts the audit rows of a revision.
func (f fixture) submissionRows(rev int64) int {
	f.t.Helper()
	return f.count(`SELECT count(*) FROM submission_audit WHERE revision_id = $1`, rev)
}

// lessonBy creates a draft of a new lesson owned by author.
func (f fixture) lessonBy(author int64, title string) lessons.Revision {
	f.t.Helper()
	rev, err := f.store.CreateLesson(f.ctx, author, lessons.NewLesson{
		SubjectID: f.subject, Language: "fr", Fields: lessons.Fields{Content: doc(title, "fr")},
	})
	if err != nil {
		f.t.Fatal(err)
	}
	return rev
}

// inTx runs statements in one transaction and commits it.
func (f fixture) inTx(stmts ...string) error {
	return pgx.BeginFunc(f.ctx, f.db, func(tx pgx.Tx) error {
		for _, s := range stmts {
			if _, err := tx.Exec(f.ctx, s); err != nil {
				return err
			}
		}
		return nil
	})
}

func submissionAuditSQL(lesson, rev, actor int64, extra ...string) string {
	cols, vals := "", ""
	for i := 0; i+1 < len(extra); i += 2 {
		cols += ", " + extra[i]
		vals += ", " + extra[i+1]
	}
	return fmt.Sprintf(`INSERT INTO submission_audit (lesson_id, revision_id, actor_id%s) VALUES (%d, %d, %d%s)`,
		cols, lesson, rev, actor, vals)
}

func submitStatusSQL(rev int64, submittedAt string) string {
	return fmt.Sprintf(`UPDATE lesson_revisions SET status = 'in_review', submitted_at = %s WHERE id = %d`, submittedAt, rev)
}

func TestSubmitRecordsOneAuditRow(t *testing.T) {
	f := newFixture(t)
	rev := f.lesson("Audited submission")
	if n := f.submissionRows(rev.ID); n != 0 {
		t.Fatalf("draft has %d submission rows", n)
	}
	start := time.Now()
	sub, err := f.store.Submit(f.ctx, f.author, rev.ID, rev.Version)
	if err != nil {
		t.Fatal(err)
	}
	var lesson, actor int64
	var at time.Time
	var backfilled, sameXact bool
	err = f.db.QueryRow(f.ctx, `
		SELECT a.lesson_id, a.actor_id, a.submitted_at, a.backfilled, xid(a.xact_id) = r.xmin
		  FROM submission_audit a JOIN lesson_revisions r ON r.id = a.revision_id
		 WHERE a.revision_id = $1`, rev.ID).Scan(&lesson, &actor, &at, &backfilled, &sameXact)
	if err != nil {
		t.Fatal(err)
	}
	if n := f.count(`SELECT count(*) FROM submission_audit`); n != 1 {
		t.Fatalf("%d submission rows, want 1", n)
	}
	if lesson != rev.LessonID || actor != f.author || backfilled || !sameXact {
		t.Fatalf("audit row: lesson %d actor %d backfilled %v same transaction %v", lesson, actor, backfilled, sameXact)
	}
	if sub.SubmittedAt == nil || !at.Equal(*sub.SubmittedAt) {
		t.Fatalf("audit time %v, revision submitted_at %v", at, sub.SubmittedAt)
	}
	// The transaction time, read from the same machine's clock.
	if at.Before(start.Add(-time.Minute)) || at.After(time.Now().Add(time.Minute)) {
		t.Fatalf("audit time %v outside the submission window starting %v", at, start)
	}
}

func TestDeniedSubmissionsWriteNoAudit(t *testing.T) {
	f := newFixture(t)
	other := f.newAuthor("other-author")
	revoked := f.newAuthor("revoked-author")
	disabled := f.newAuthor("disabled-author")

	own := f.lesson("Own draft")
	revokedDraft := f.lessonBy(revoked, "Revoked draft")
	disabledDraft := f.lessonBy(disabled, "Disabled draft")
	archived := f.lesson("Archived draft")
	twice := f.submitted("Submitted once")

	if _, err := f.perms.Revoke(f.ctx, operator, revoked, permissions.Author); err != nil {
		t.Fatal(err)
	}
	f.mustExec(`UPDATE users SET disabled_at = now() WHERE id = $1`, disabled)
	f.mustExec(`UPDATE lessons SET archived_at = now() WHERE id = $1`, archived.LessonID)

	cases := []struct {
		name  string
		actor int64
		rev   lessons.Revision
		ver   int
		want  error
		rows  int
	}{
		{"non-owner", other, own, own.Version, lessons.ErrDenied, 0},
		{"no author permission", revoked, revokedDraft, revokedDraft.Version, lessons.ErrDenied, 0},
		{"disabled account", disabled, disabledDraft, disabledDraft.Version, lessons.ErrDenied, 0},
		{"stale version", f.author, own, own.Version + 1, lessons.ErrVersionConflict, 0},
		{"already submitted", f.author, twice, twice.Version, lessons.ErrNotDraft, 1},
		{"archived lesson", f.author, archived, archived.Version, lessons.ErrArchived, 0},
	}
	for _, c := range cases {
		before := f.snapshot(c.rev.ID)
		_, err := f.store.Submit(f.ctx, c.actor, c.rev.ID, c.ver)
		if !errors.Is(err, c.want) {
			t.Errorf("%s: want %v, got %v", c.name, c.want, err)
		}
		if n := f.submissionRows(c.rev.ID); n != c.rows {
			t.Errorf("%s: %d submission rows, want %d", c.name, n, c.rows)
		}
		if f.snapshot(c.rev.ID) != before {
			t.Errorf("%s: revision changed", c.name)
		}
	}
	if n := f.count(`SELECT count(*) FROM submission_audit`); n != 1 {
		t.Fatalf("%d submission rows in all, want 1", n)
	}
}

func TestConcurrentSubmissionsWriteOneAudit(t *testing.T) {
	f := newFixture(t)
	rev := f.lesson("Raced submission")
	const n = 6
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = f.store.Submit(context.Background(), f.author, rev.ID, rev.Version)
		}(i)
	}
	wg.Wait()
	ok := 0
	for _, err := range errs {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, lessons.ErrNotDraft), errors.Is(err, lessons.ErrVersionConflict):
		default:
			t.Errorf("unexpected error: %v", err)
		}
	}
	if ok != 1 {
		t.Fatalf("%d submissions succeeded, want 1", ok)
	}
	if rows := f.submissionRows(rev.ID); rows != 1 {
		t.Fatalf("%d submission rows, want 1", rows)
	}
}

// The database keeps the audit row and the status change together, at the
// transaction time, for every writer.
func TestSubmissionAuditAndStatusAreAtomic(t *testing.T) {
	f := newFixture(t)
	rev := f.lesson("Atomic submission")
	otherLesson := f.lesson("Other lesson")
	before := f.snapshot(rev.ID)
	audit := func(extra ...string) string { return submissionAuditSQL(rev.LessonID, rev.ID, f.author, extra...) }
	now := submitStatusSQL(rev.ID, "now()")

	wantGuard(t, "status without audit", f.inTx(now), "leaves draft only with a submission audit row")
	wantGuard(t, "audit without status", f.inTx(audit()), "audited but not applied")
	wantSQLState(t, "forced failure after the status change", f.inTx(audit(), now, `SELECT 1/0`), "22012")
	wantGuard(t, "backdated audit", f.inTx(audit("submitted_at", "now() - interval '1 day'"), now), "its own transaction and its time")
	wantGuard(t, "future audit", f.inTx(audit("submitted_at", "now() + interval '1 day'"), now), "its own transaction and its time")
	wantGuard(t, "forged transaction", f.inTx(audit("xact_id", "'1'::xid8"), now), "its own transaction and its time")
	wantGuard(t, "claimed backfill", f.inTx(audit("backfilled", "true"), now), "backfilled only by their migration")
	wantGuard(t, "backdated revision", f.inTx(audit(), submitStatusSQL(rev.ID, "now() - interval '1 day'")),
		"submitted_at must be the transaction time")
	wantGuard(t, "actor is not the author", f.inTx(submissionAuditSQL(rev.LessonID, rev.ID, f.reviewer), now), "only the author")
	wantGuard(t, "revision of another lesson", f.inTx(submissionAuditSQL(otherLesson.LessonID, rev.ID, f.author), now),
		"is not a revision of lesson")
	wantGuard(t, "audit rolled back to a savepoint", f.inTx(`SAVEPOINT s`, audit(), `ROLLBACK TO SAVEPOINT s`, now),
		"leaves draft only with a submission audit row")
	wantGuard(t, "status rolled back to a savepoint", f.inTx(audit(), `SAVEPOINT s`, now, `ROLLBACK TO SAVEPOINT s`),
		"audited but not applied")
	if f.snapshot(rev.ID) != before || f.count(`SELECT count(*) FROM submission_audit`) != 0 {
		t.Fatal("refused submissions changed the revision or wrote audit rows")
	}

	// The rules on the submitter and the revision bind direct SQL too.
	revoked := f.newAuthor("revoked-author")
	disabled := f.newAuthor("disabled-author")
	revokedDraft := f.lessonBy(revoked, "Revoked draft")
	disabledDraft := f.lessonBy(disabled, "Disabled draft")
	archived := f.lesson("Archived draft")
	submitted := f.submitted("Already submitted")
	if _, err := f.perms.Revoke(f.ctx, operator, revoked, permissions.Author); err != nil {
		t.Fatal(err)
	}
	f.mustExec(`UPDATE users SET disabled_at = now() WHERE id = $1`, disabled)
	f.mustExec(`UPDATE lessons SET archived_at = now() WHERE id = $1`, archived.LessonID)
	for name, c := range map[string]struct {
		rev   lessons.Revision
		actor int64
		msg   string
	}{
		"author without permission": {revokedDraft, revoked, "is not an active author"},
		"disabled author":           {disabledDraft, disabled, "is not an active author"},
		"archived lesson":           {archived, f.author, "is archived"},
		"already submitted":         {submitted, f.author, "is not a draft"},
	} {
		wantGuard(t, name, f.inTx(submissionAuditSQL(c.rev.LessonID, c.rev.ID, c.actor), submitStatusSQL(c.rev.ID, "now()")), c.msg)
	}
	if n := f.count(`SELECT count(*) FROM submission_audit`); n != 1 {
		t.Fatalf("%d submission rows, want only the store's", n)
	}

	// The same statements in order, with nothing forged, are accepted.
	dbtest.Submit(t, f.db, rev.ID)
	if n := f.submissionRows(rev.ID); n != 1 {
		t.Fatalf("%d submission rows after a direct submission, want 1", n)
	}
	// A revision is submitted once: even with its status left alone, a
	// second row is refused.
	wantGuard(t, "second audit row", f.inTx(audit()), "is not a draft")
}

func TestSubmissionAuditIsAppendOnly(t *testing.T) {
	f := newFixture(t)
	rev := f.submitted("Append-only submission")
	var before string
	snapshot := func() string {
		var s string
		if err := f.db.QueryRow(f.ctx, `SELECT coalesce(string_agg(to_jsonb(a)::text, ',' ORDER BY id), '') FROM submission_audit a`).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	before = snapshot()
	for name, stmt := range map[string]string{
		"update actor": `UPDATE submission_audit SET actor_id = actor_id`,
		"update time":  `UPDATE submission_audit SET submitted_at = submitted_at - interval '1 day'`,
		"delete":       `DELETE FROM submission_audit`,
		"truncate":     `TRUNCATE submission_audit`,
	} {
		wantGuard(t, name, f.exec(stmt), "append-only")
	}
	wantSQLState(t, "truncate revisions", f.exec(`TRUNCATE lesson_revisions CASCADE`), "LRN01")
	wantSQLState(t, "delete revision", f.exec(`DELETE FROM lesson_revisions WHERE id = $1`, rev.ID), "LRN01")
	if after := snapshot(); after != before || before == "" {
		t.Fatalf("audit changed:\n%s\n%s", before, after)
	}
}

// A writer whose snapshot predates a revocation, a disablement or an
// archival cannot submit with it (the guard's locks conflict).
func TestSubmissionRefusedForStaleSnapshots(t *testing.T) {
	f := newFixture(t)
	for _, level := range []pgx.TxIsoLevel{pgx.RepeatableRead, pgx.Serializable} {
		for _, change := range []string{"revoke", "disable", "archive"} {
			author := f.newAuthor(fmt.Sprintf("stale-%s-%s", strings.ReplaceAll(string(level), " ", "-"), change))
			rev := f.lessonBy(author, "Stale "+change)
			tx, err := f.db.BeginTx(f.ctx, pgx.TxOptions{IsoLevel: level})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := tx.Exec(f.ctx, `SELECT count(*) FROM lesson_revisions`); err != nil { // takes the snapshot
				t.Fatal(err)
			}
			switch change {
			case "revoke":
				if _, err := f.perms.Revoke(f.ctx, operator, author, permissions.Author); err != nil {
					t.Fatal(err)
				}
			case "disable":
				f.mustExec(`UPDATE users SET disabled_at = now() WHERE id = $1`, author)
			case "archive":
				f.mustExec(`UPDATE lessons SET archived_at = now() WHERE id = $1`, rev.LessonID)
			}
			err = dbtest.SubmitTx(f.ctx, tx, rev.ID)
			if err == nil {
				err = tx.Commit(f.ctx)
			}
			_ = tx.Rollback(f.ctx)
			var pgErr *pgconn.PgError
			if !errors.As(err, &pgErr) || (pgErr.Code != "40001" && pgErr.Code != "LRN01") {
				t.Errorf("%s at %s: want a serialization failure or a guard refusal, got %v", change, level, err)
			}
			if n := f.submissionRows(rev.ID); n != 0 {
				t.Errorf("%s at %s: %d submission rows", change, level, n)
			}
		}
	}
}

// The new guards read lesson_revisions, lessons, submission_audit,
// user_permissions and users. A temporary table of a non-owner role with any
// of those names must not fool them (pg_temp is otherwise searched first).
// Each attempt runs on a fresh connection (PL/pgSQL caches plans per
// session).
func TestSubmissionGuardsIgnoreTemporaryShadowTables(t *testing.T) {
	f := newFixture(t)
	other := f.newAuthor("other-author")
	revoked := f.newAuthor("revoked-author")
	disabled := f.newAuthor("disabled-author")
	draft := f.lesson("Shadowed draft")
	inReview := f.submitted("Shadowed submitted")
	revokedDraft := f.lessonBy(revoked, "Shadowed revoked")
	disabledDraft := f.lessonBy(disabled, "Shadowed disabled")
	archived := f.lesson("Shadowed archived")
	if _, err := f.perms.Revoke(f.ctx, operator, revoked, permissions.Author); err != nil {
		t.Fatal(err)
	}
	f.mustExec(`UPDATE users SET disabled_at = now() WHERE id = $1`, disabled)
	f.mustExec(`UPDATE lessons SET archived_at = now() WHERE id = $1`, archived.LessonID)

	asRole, qs := f.leastPrivilegeRole()
	audit := func(rev lessons.Revision, actor int64) string {
		return fmt.Sprintf(`INSERT INTO %s.submission_audit (lesson_id, revision_id, actor_id) VALUES (%d, %d, %d)`,
			qs, rev.LessonID, rev.ID, actor)
	}
	status := func(rev lessons.Revision) string {
		return fmt.Sprintf(`UPDATE %s.lesson_revisions SET status = 'in_review', submitted_at = now() WHERE id = %d`, qs, rev.ID)
	}
	shadowRevision := func(rev lessons.Revision, author int64, st string) []string {
		return []string{
			`CREATE TEMP TABLE lesson_revisions (id bigint, lesson_id bigint, author_id bigint, status text, submitted_at timestamptz) ON COMMIT DROP`,
			fmt.Sprintf(`INSERT INTO pg_temp.lesson_revisions VALUES (%d, %d, %d, '%s', now())`, rev.ID, rev.LessonID, author, st),
		}
	}
	cases := []struct {
		name  string
		stmts []string
		msg   string
	}{
		{"status with a shadow audit row", []string{
			`CREATE TEMP TABLE submission_audit (revision_id bigint, lesson_id bigint, actor_id bigint, submitted_at timestamptz,
				xact_id xid8, backfilled boolean) ON COMMIT DROP`,
			fmt.Sprintf(`INSERT INTO pg_temp.submission_audit VALUES (%d, %d, %d, now(), pg_current_xact_id(), false)`,
				draft.ID, draft.LessonID, f.author),
			status(draft),
		}, "leaves draft only with a submission audit row"},
		{"submitted revision shown as a draft", append(shadowRevision(inReview, f.author, "draft"), audit(inReview, f.author)),
			"is not a draft"},
		{"another author shown as the author", append(shadowRevision(draft, other, "draft"), audit(draft, other), status(draft)),
			"only the author"},
		{"revoked author shown as granted", []string{
			`CREATE TEMP TABLE user_permissions (user_id bigint, permission text) ON COMMIT DROP`,
			fmt.Sprintf(`INSERT INTO pg_temp.user_permissions VALUES (%d, 'author')`, revoked),
			audit(revokedDraft, revoked), status(revokedDraft),
		}, "is not an active author"},
		{"disabled author shown as active", []string{
			`CREATE TEMP TABLE users (id bigint, disabled_at timestamptz) ON COMMIT DROP`,
			fmt.Sprintf(`INSERT INTO pg_temp.users VALUES (%d, NULL)`, disabled),
			audit(disabledDraft, disabled), status(disabledDraft),
		}, "is not an active author"},
		{"archived lesson shown as open", []string{
			`CREATE TEMP TABLE lessons (id bigint, archived_at timestamptz) ON COMMIT DROP`,
			fmt.Sprintf(`INSERT INTO pg_temp.lessons VALUES (%d, NULL)`, archived.LessonID),
			audit(archived, f.author), status(archived),
		}, "is archived"},
	}
	for _, c := range cases {
		wantGuard(t, c.name, asRole(c.stmts...), c.msg)
	}
	if n := f.count(`SELECT count(*) FROM submission_audit`); n != 1 {
		t.Fatalf("shadowed attempts wrote audit rows: %d rows, want 1", n)
	}
	if n := f.count(`SELECT count(*) FROM lesson_revisions WHERE status <> 'draft'`); n != 1 {
		t.Fatalf("shadowed attempts submitted revisions: %d submitted, want 1", n)
	}
	// Without a shadow the same role submits the draft.
	if err := asRole(audit(draft, f.author), status(draft)); err != nil {
		t.Fatalf("legitimate submission as the role: %v", err)
	}
	if n := f.submissionRows(draft.ID); n != 1 {
		t.Fatalf("%d submission rows, want 1", n)
	}
}

// migrationsUpTo returns the embedded migrations up to and including version.
func migrationsUpTo(t *testing.T, version string) fstest.MapFS {
	t.Helper()
	out := fstest.MapFS{}
	entries, err := fs.ReadDir(migrations.FS, ".")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".sql") || e.Name()[:4] > version {
			continue
		}
		b, err := fs.ReadFile(migrations.FS, e.Name())
		if err != nil {
			t.Fatal(err)
		}
		out[e.Name()] = &fstest.MapFile{Data: b}
	}
	return out
}

// A database migrated to 0010 with submitted revisions gets one backfilled
// row per submitted revision when 0011 is applied; drafts get none, and the
// guards apply from then on. A fresh database starts with none.
func TestUpgradeBackfillsSubmissionAudit(t *testing.T) {
	ctx := context.Background()
	if n := newFixture(t).count(`SELECT count(*) FROM submission_audit`); n != 0 {
		t.Fatalf("fresh database has %d submission rows", n)
	}

	db := dbtest.EmptySchema(t)
	if _, err := database.Migrate(ctx, db, migrationsUpTo(t, "0010")); err != nil {
		t.Fatal(err)
	}
	f := newFixtureOn(newEnvOn(t, db))
	// Before 0011 a submission is a bare status change (no audit table).
	submitBare := func(title string) lessons.Revision {
		rev := f.lesson(title)
		f.mustExec(`UPDATE lesson_revisions SET status = 'in_review', submitted_at = now() - interval '1 hour' WHERE id = $1`, rev.ID)
		return rev
	}
	published := submitBare("Upgrade published")
	f.mustApprove(published.ID)
	if err := f.publish(published.LessonID, published.ID); err != nil {
		t.Fatal(err)
	}
	sentBack := submitBare("Upgrade changes requested")
	f.mustRequestChanges(sentBack.ID)
	inReview := submitBare("Upgrade in review")
	draft := f.lesson("Upgrade draft")

	applied, err := database.Migrate(ctx, db, migrations.FS)
	if err != nil {
		t.Fatal(err)
	}
	if len(applied) != 1 || applied[0] != "0011_submission_audit.sql" {
		t.Fatalf("applied %v", applied)
	}
	if err := database.Check(ctx, db, migrations.FS); err != nil {
		t.Fatal(err)
	}
	for _, rev := range []lessons.Revision{published, sentBack, inReview} {
		var actorOK, timeOK, backfilled, migrationXact bool
		err := db.QueryRow(ctx, `
			SELECT a.actor_id = r.author_id, a.submitted_at = r.submitted_at, a.backfilled,
			       xid(a.xact_id) = (SELECT xmin FROM schema_migrations WHERE version = '0011')
			  FROM submission_audit a JOIN lesson_revisions r ON r.id = a.revision_id AND r.lesson_id = a.lesson_id
			 WHERE a.revision_id = $1`, rev.ID).Scan(&actorOK, &timeOK, &backfilled, &migrationXact)
		if err != nil {
			t.Fatalf("revision %d: %v", rev.ID, err)
		}
		if !actorOK || !timeOK || !backfilled || !migrationXact {
			t.Errorf("revision %d backfill: actor %v time %v backfilled %v migration transaction %v",
				rev.ID, actorOK, timeOK, backfilled, migrationXact)
		}
	}
	if n := f.count(`SELECT count(*) FROM submission_audit`); n != 3 {
		t.Fatalf("%d backfilled rows, want 3", n)
	}
	if n := f.submissionRows(draft.ID); n != 0 {
		t.Fatalf("draft has %d submission rows", n)
	}
	if n := f.count(`SELECT count(*) FROM lesson_revisions r WHERE status <> 'draft'
	                  AND NOT EXISTS (SELECT 1 FROM submission_audit a WHERE a.revision_id = r.id)`); n != 0 {
		t.Fatalf("%d submitted revisions without a submission row", n)
	}

	// From now on submissions are audited and the backfill is append-only.
	wantGuard(t, "bare submission after upgrade", f.exec(submitStatusSQL(draft.ID, "now()")), "leaves draft only with a submission audit row")
	wantGuard(t, "delete a backfilled row", f.exec(`DELETE FROM submission_audit WHERE revision_id = $1`, published.ID), "append-only")
	sub, err := f.store.Submit(ctx, f.author, draft.ID, draft.Version)
	if err != nil {
		t.Fatal(err)
	}
	var backfilled bool
	if err := db.QueryRow(ctx, `SELECT backfilled FROM submission_audit WHERE revision_id = $1 AND actor_id = $2`, sub.ID, f.author).Scan(&backfilled); err != nil || backfilled {
		t.Fatalf("post-upgrade submission row: backfilled %v err %v", backfilled, err)
	}
	// The upgraded workflow continues: the in-review revision can be decided.
	f.mustApprove(inReview.ID)
}
