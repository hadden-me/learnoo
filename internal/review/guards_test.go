package review_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"testing"

	"github.com/hadden-me/learnoo/internal/lessons"
	"github.com/hadden-me/learnoo/internal/permissions"
	"github.com/jackc/pgx/v5"
)

// Database guards of migrations/0006, exercised with direct SQL that
// bypasses the Store.

func decisionSQL(rev, reviewer int64, action string, comment string) string {
	c := "NULL"
	if comment != "" {
		c = "'" + comment + "'"
	}
	return fmt.Sprintf(`INSERT INTO review_decisions (revision_id, reviewer_id, action, comment) VALUES (%d, %d, '%s', %s)`,
		rev, reviewer, action, c)
}

func statusSQL(rev int64, status string) string {
	return fmt.Sprintf(`UPDATE lesson_revisions SET status = '%s' WHERE id = %d`, status, rev)
}

func auditSQL(lesson, rev int64, previous string, actor int64) string {
	return fmt.Sprintf(`INSERT INTO publication_audit (lesson_id, revision_id, previous_revision_id, actor_id, action)
		VALUES (%d, %d, %s, %d, 'publish')`, lesson, rev, previous, actor)
}

func pointerSQL(lesson int64, rev string) string {
	return fmt.Sprintf(`UPDATE lessons SET published_revision_id = %s WHERE id = %d`, rev, lesson)
}

// A decision and its status change happen together or not at all.
func TestDecisionAndStatusAreAtomic(t *testing.T) {
	e := newEnv(t)
	reviewer := e.reviewer("reviewer", []int64{e.subject})
	rev := e.submitted(e.author, e.subject, "Atomic")
	before := e.state(rev)

	wantGuard(t, "decision without status change", e.inTx(decisionSQL(rev.ID, reviewer, "approve", "")), "was not applied")
	wantGuard(t, "approval without decision", e.inTx(statusSQL(rev.ID, "approved")), "recorded matching review decision")
	wantGuard(t, "changes requested without decision", e.inTx(statusSQL(rev.ID, "changes_requested")), "recorded matching review decision")
	wantGuard(t, "decision applied as the other status", e.inTx(decisionSQL(rev.ID, reviewer, "approve", ""), statusSQL(rev.ID, "changes_requested")),
		"recorded matching review decision")
	// Forced failure after the decision insert and status change: rollback leaves nothing.
	wantSQLState(t, "forced failure", e.inTx(decisionSQL(rev.ID, reviewer, "approve", ""), statusSQL(rev.ID, "approved"), `SELECT 1/0`), "22012")
	if after := e.state(rev); after != before {
		t.Fatalf("failed transactions changed state: %s -> %s", before, after)
	}

	wantSQLState(t, "change request without comment", e.inTx(decisionSQL(rev.ID, reviewer, "request_changes", ""), statusSQL(rev.ID, "changes_requested")), "23514")
	wantSQLState(t, "blank comment", e.inTx(decisionSQL(rev.ID, reviewer, "request_changes", " \t"), statusSQL(rev.ID, "changes_requested")), "23514")
	wantSQLState(t, "unknown action", e.inTx(decisionSQL(rev.ID, reviewer, "publish", ""), statusSQL(rev.ID, "approved")), "23514")
	wantGuard(t, "backdated decision", e.inTx(
		fmt.Sprintf(`INSERT INTO review_decisions (revision_id, reviewer_id, action, decided_at) VALUES (%d, %d, 'approve', now() - interval '1 day')`, rev.ID, reviewer),
		statusSQL(rev.ID, "approved")), "transaction time")
	wantGuard(t, "unassigned reviewer", e.inTx(decisionSQL(rev.ID, e.user("stranger"), "approve", ""), statusSQL(rev.ID, "approved")), "not an active reviewer")
	if after := e.state(rev); after != before {
		t.Fatalf("rejected decisions changed state: %s -> %s", before, after)
	}

	if err := e.inTx(decisionSQL(rev.ID, reviewer, "request_changes", "Simulated comment."), statusSQL(rev.ID, "changes_requested")); err != nil {
		t.Fatalf("valid direct decision: %v", err)
	}
	second := e.reviewer("second", []int64{e.subject})
	wantGuard(t, "second decision", e.inTx(decisionSQL(rev.ID, second, "approve", ""), statusSQL(rev.ID, "approved")), "not in review")
	draft, err := e.lessons.CreateDraft(e.ctx, e.author, rev.LessonID, lessons.Fields{Content: doc("Successor")})
	if err != nil {
		t.Fatal(err)
	}
	wantGuard(t, "decision on a draft", e.inTx(decisionSQL(draft.ID, reviewer, "approve", "")), "not in review")
}

// A publication audit row and the pointer change happen together; the
// audit names the replaced pointer, an approved revision and an active
// publisher; an audit row of another transaction cannot be reused.
func TestPublicationAndAuditAreAtomic(t *testing.T) {
	e := newEnv(t)
	reviewer := e.reviewer("reviewer", []int64{e.subject})
	publisher := e.user("publisher", permissions.Publisher)
	rev := e.submitted(e.author, e.subject, "Published")
	before := e.state(rev)
	wantGuard(t, "audit of an unapproved revision", e.inTx(auditSQL(rev.LessonID, rev.ID, "NULL", publisher), pointerSQL(rev.LessonID, fmt.Sprint(rev.ID))),
		"is not an approved revision of lesson")
	if _, err := e.review.Approve(e.ctx, reviewer, rev.ID, ""); err != nil {
		t.Fatal(err)
	}
	before = e.state(rev)
	id := fmt.Sprint(rev.ID)

	wantGuard(t, "pointer without audit", e.inTx(pointerSQL(rev.LessonID, id)), "requires a publication audit row")
	wantGuard(t, "audit without pointer", e.inTx(auditSQL(rev.LessonID, rev.ID, "NULL", publisher)), "does not point at it")
	wantGuard(t, "audit by a non-publisher", e.inTx(auditSQL(rev.LessonID, rev.ID, "NULL", reviewer), pointerSQL(rev.LessonID, id)), "not an active publisher")
	wantGuard(t, "audit with a wrong previous revision", e.inTx(auditSQL(rev.LessonID, rev.ID, id, publisher), pointerSQL(rev.LessonID, id)),
		"previous_revision_id must be")
	wantGuard(t, "backdated audit", e.inTx(
		fmt.Sprintf(`INSERT INTO publication_audit (lesson_id, revision_id, actor_id, action, occurred_at) VALUES (%d, %d, %d, 'publish', now() - interval '1 day')`,
			rev.LessonID, rev.ID, publisher), pointerSQL(rev.LessonID, id)), "own transaction")
	wantGuard(t, "forged transaction id", e.inTx(
		fmt.Sprintf(`INSERT INTO publication_audit (lesson_id, revision_id, actor_id, action, xact_id) VALUES (%d, %d, %d, 'publish', '1')`,
			rev.LessonID, rev.ID, publisher), pointerSQL(rev.LessonID, id)), "own transaction")
	wantSQLState(t, "forced failure", e.inTx(auditSQL(rev.LessonID, rev.ID, "NULL", publisher), pointerSQL(rev.LessonID, id), `SELECT 1/0`), "22012")
	if after := e.state(rev); after != before {
		t.Fatalf("rejected publications changed state: %s -> %s", before, after)
	}

	if err := e.inTx(auditSQL(rev.LessonID, rev.ID, "NULL", publisher), pointerSQL(rev.LessonID, id)); err != nil {
		t.Fatalf("valid direct publication: %v", err)
	}
	// Clearing the pointer (LRN-009 unpublication) is not guarded here; an
	// audit row of the earlier transaction does not allow re-pointing.
	if err := e.exec(pointerSQL(rev.LessonID, "NULL")); err != nil {
		t.Fatal(err)
	}
	wantGuard(t, "reused audit row", e.inTx(pointerSQL(rev.LessonID, id)), "requires a publication audit row")
}

func TestReviewAuditIsAppendOnly(t *testing.T) {
	e := newEnv(t)
	reviewer := e.reviewer("reviewer", []int64{e.subject})
	publisher := e.user("publisher", permissions.Publisher)
	rev := e.submitted(e.author, e.subject, "Audited")
	if _, err := e.review.Approve(e.ctx, reviewer, rev.ID, "Simulated comment."); err != nil {
		t.Fatal(err)
	}
	if _, err := e.review.Publish(e.ctx, publisher, rev.ID); err != nil {
		t.Fatal(err)
	}
	snapshot := func() string {
		var s string
		if err := e.db.QueryRow(e.ctx, `SELECT (SELECT jsonb_agg(d)::text FROM review_decisions d) || (SELECT jsonb_agg(p)::text FROM publication_audit p)`).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	before := snapshot()
	for name, stmt := range map[string]string{
		"decision comment changed":  `UPDATE review_decisions SET comment = 'Edited'`,
		"decision reviewer changed": fmt.Sprintf(`UPDATE review_decisions SET reviewer_id = %d`, e.author),
		"decision deleted":          `DELETE FROM review_decisions`,
		"decisions truncated":       `TRUNCATE review_decisions`,
		"audit actor changed":       fmt.Sprintf(`UPDATE publication_audit SET actor_id = %d`, e.author),
		"audit time changed":        `UPDATE publication_audit SET occurred_at = occurred_at - interval '1 day'`,
		"audit deleted":             `DELETE FROM publication_audit`,
		"audit truncated":           `TRUNCATE publication_audit`,
	} {
		wantGuard(t, name, e.exec(stmt), "append-only")
	}
	wantSQLState(t, "decided revision deleted", e.exec(`DELETE FROM lesson_revisions WHERE id = $1`, rev.ID), "LRN01")
	wantSQLState(t, "truncate cascade", e.exec(`TRUNCATE lesson_revisions CASCADE`), "LRN01")
	if after := snapshot(); after != before {
		t.Fatalf("audit changed:\n%s\n%s", before, after)
	}
}

// leastPrivilegeRole creates a non-owner role like the deployed runtime
// role (the pattern of internal/lessons/candidate_test.go): USAGE on the
// test schema, DML on its tables and sequences, and TEMP. asRole runs
// statements in one committed transaction as that role on a new connection,
// so no cached plan hides a shadowing regression.
func (e *env) leastPrivilegeRole() (asRole func(stmts ...string) error, qs string) {
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
	must := func(sql string) {
		t.Helper()
		if err := e.exec(sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	must(`CREATE ROLE ` + role + ` NOLOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT`)
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = e.db.Exec(ctx, `DROP OWNED BY `+role)
		if _, err := e.db.Exec(ctx, `DROP ROLE `+role); err != nil {
			t.Errorf("drop test role: %v", err)
		}
	})
	qs = pgx.Identifier{schema}.Sanitize()
	must(`GRANT USAGE ON SCHEMA ` + qs + ` TO ` + role)
	must(`GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA ` + qs + ` TO ` + role)
	must(`GRANT USAGE ON ALL SEQUENCES IN SCHEMA ` + qs + ` TO ` + role)
	must(`GRANT TEMP ON DATABASE ` + pgx.Identifier{dbName}.Sanitize() + ` TO ` + role)
	asRole = func(stmts ...string) error {
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
			if _, err := tx.Exec(e.ctx, s); err != nil {
				return err
			}
		}
		return tx.Commit(e.ctx)
	}
	return asRole, qs
}

// The new guards read lesson_revisions, lessons, review_decisions,
// publication_audit, reviewer_assignments, user_permissions and users. A
// temporary table of the calling role with any of those names must not fool
// them (pg_temp is otherwise searched first for relations).
func TestReviewGuardsIgnoreTemporaryShadowTables(t *testing.T) {
	e := newEnv(t)
	reviewer := e.reviewer("reviewer", []int64{e.subject})
	authorReviewer := e.reviewer("author-reviewer", []int64{e.subject}, permissions.Author)
	publisher := e.user("publisher", permissions.Publisher)
	stranger := e.user("stranger")
	asRole, qs := e.leastPrivilegeRole()
	q := func(sql string) string { return fmt.Sprintf(sql, qs) }

	own := e.submitted(authorReviewer, e.subject, "Own revision")
	rev := e.submitted(e.author, e.subject, "Shadowed review")
	beforeOwn, beforeRev := e.state(own), e.state(rev)

	wantGuard(t, "self-approval via shadow lesson_revisions", asRole(
		`CREATE TEMP TABLE lesson_revisions (id bigint, lesson_id bigint, author_id bigint, subject_id bigint, status text)`,
		fmt.Sprintf(`INSERT INTO pg_temp.lesson_revisions VALUES (%d, %d, %d, %d, 'in_review')`, own.ID, own.LessonID, e.author, e.subject),
		q(fmt.Sprintf(`INSERT INTO %%s.review_decisions (revision_id, reviewer_id, action) VALUES (%d, %d, 'approve')`, own.ID, authorReviewer)),
		q(fmt.Sprintf(`UPDATE %%s.lesson_revisions SET status = 'approved' WHERE id = %d`, own.ID)),
	), "own revision")
	wantGuard(t, "unassigned reviewer via shadow assignments", asRole(
		`CREATE TEMP TABLE reviewer_assignments (user_id bigint, subject_id bigint)`,
		`CREATE TEMP TABLE users (id bigint, disabled_at timestamptz)`,
		fmt.Sprintf(`INSERT INTO pg_temp.reviewer_assignments VALUES (%d, %d)`, stranger, e.subject),
		fmt.Sprintf(`INSERT INTO pg_temp.users VALUES (%d, NULL)`, stranger),
		q(fmt.Sprintf(`INSERT INTO %%s.review_decisions (revision_id, reviewer_id, action) VALUES (%d, %d, 'approve')`, rev.ID, stranger)),
		q(fmt.Sprintf(`UPDATE %%s.lesson_revisions SET status = 'approved' WHERE id = %d`, rev.ID)),
	), "not an active reviewer")
	wantGuard(t, "approval via shadow review_decisions", asRole(
		`CREATE TEMP TABLE review_decisions (revision_id bigint, action text)`,
		fmt.Sprintf(`INSERT INTO pg_temp.review_decisions VALUES (%d, 'approve')`, rev.ID),
		q(fmt.Sprintf(`UPDATE %%s.lesson_revisions SET status = 'approved' WHERE id = %d`, rev.ID)),
	), "recorded matching review decision")
	wantGuard(t, "unapplied decision via shadow lesson_revisions", asRole(
		`CREATE TEMP TABLE lesson_revisions (id bigint, status text)`,
		fmt.Sprintf(`INSERT INTO pg_temp.lesson_revisions VALUES (%d, 'approved')`, rev.ID),
		q(fmt.Sprintf(`INSERT INTO %%s.review_decisions (revision_id, reviewer_id, action) VALUES (%d, %d, 'approve')`, rev.ID, reviewer)),
	), "was not applied")
	if e.state(own) != beforeOwn || e.state(rev) != beforeRev {
		t.Fatal("shadowed decisions changed state")
	}

	// The same role decides legitimately without a shadow.
	if err := asRole(
		q(fmt.Sprintf(`INSERT INTO %%s.review_decisions (revision_id, reviewer_id, action) VALUES (%d, %d, 'approve')`, rev.ID, reviewer)),
		q(fmt.Sprintf(`UPDATE %%s.lesson_revisions SET status = 'approved' WHERE id = %d`, rev.ID)),
	); err != nil {
		t.Fatalf("legitimate decision as the role: %v", err)
	}
	before := e.state(rev)
	audit := func(actor int64, previous string) string {
		return q(fmt.Sprintf(`INSERT INTO %%s.publication_audit (lesson_id, revision_id, previous_revision_id, actor_id, action) VALUES (%d, %d, %s, %d, 'publish')`,
			rev.LessonID, rev.ID, previous, actor))
	}
	point := q(fmt.Sprintf(`UPDATE %%s.lessons SET published_revision_id = %d WHERE id = %d`, rev.ID, rev.LessonID))

	wantGuard(t, "non-publisher via shadow user_permissions", asRole(
		`CREATE TEMP TABLE user_permissions (user_id bigint, permission text)`,
		`CREATE TEMP TABLE users (id bigint, disabled_at timestamptz)`,
		fmt.Sprintf(`INSERT INTO pg_temp.user_permissions VALUES (%d, 'publisher')`, stranger),
		fmt.Sprintf(`INSERT INTO pg_temp.users VALUES (%d, NULL)`, stranger),
		audit(stranger, "NULL"), point,
	), "not an active publisher")
	wantGuard(t, "forged previous pointer via shadow lessons", asRole(
		`CREATE TEMP TABLE lessons (id bigint, published_revision_id bigint)`,
		fmt.Sprintf(`INSERT INTO pg_temp.lessons VALUES (%d, %d)`, rev.LessonID, own.ID),
		audit(publisher, fmt.Sprint(own.ID)), point,
	), "previous_revision_id must be")
	wantGuard(t, "pointer via shadow publication_audit", asRole(
		`CREATE TEMP TABLE publication_audit (lesson_id bigint, revision_id bigint, previous_revision_id bigint, xact_id xid8)`,
		fmt.Sprintf(`INSERT INTO pg_temp.publication_audit VALUES (%d, %d, NULL, pg_current_xact_id())`, rev.LessonID, rev.ID),
		point,
	), "requires a publication audit row")
	wantGuard(t, "unapplied audit via shadow lessons", asRole(
		`CREATE TEMP TABLE lessons (id bigint, published_revision_id bigint)`,
		audit(publisher, "NULL"),
		fmt.Sprintf(`INSERT INTO pg_temp.lessons VALUES (%d, %d)`, rev.LessonID, rev.ID),
	), "does not point at it")
	if after := e.state(rev); after != before {
		t.Fatalf("shadowed publications changed state: %s -> %s", before, after)
	}
	if err := asRole(audit(publisher, "NULL"), point); err != nil {
		t.Fatalf("legitimate publication as the role: %v", err)
	}
	if p := e.pointer(rev.LessonID); p == nil || *p != rev.ID {
		t.Fatalf("pointer %v", p)
	}
}
