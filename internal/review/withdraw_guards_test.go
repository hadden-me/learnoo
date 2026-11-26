package review_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/hadden-me/learnoo/internal/permissions"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Database guards of migrations/0007, exercised with direct SQL that
// bypasses the Store.

func withdrawalSQL(lesson, rev, actor int64) string {
	return fmt.Sprintf(`INSERT INTO revision_withdrawals (lesson_id, revision_id, actor_id, reason) VALUES (%d, %d, %d, 'Simulated reason.')`,
		lesson, rev, actor)
}

func restoreSQL(lesson, rev int64, previous string, actor int64) string {
	return fmt.Sprintf(`INSERT INTO publication_audit (lesson_id, revision_id, previous_revision_id, actor_id, action)
		VALUES (%d, %d, %s, %d, 'restore')`, lesson, rev, previous, actor)
}

// Every pointer change needs an audit row of its transaction recording
// exactly that change, including unpublication to NULL.
func TestEveryPointerChangeIsAudited(t *testing.T) {
	w := newWithdrawEnv(t)
	first, second := w.firstAndSecond()
	l, f, s := first.LessonID, fmt.Sprint(first.ID), fmt.Sprint(second.ID)
	before := w.world()

	wantGuard(t, "unpublish without audit", w.inTx(pointerSQL(l, "NULL")), "requires a publication audit row")
	wantGuard(t, "restore without audit", w.inTx(pointerSQL(l, f)), "requires a publication audit row")
	wantGuard(t, "unpublish audited as restore", w.inTx(restoreSQL(l, first.ID, s, w.publisher), pointerSQL(l, "NULL")),
		"requires a publication audit row")
	wantGuard(t, "unpublish audit without pointer change", w.inTx(unpublishSQL(l, second.ID, w.admin)), "does not point at it")
	wantGuard(t, "restore audit without pointer change", w.inTx(restoreSQL(l, first.ID, s, w.publisher)), "does not point at it")
	wantGuard(t, "unpublish by a publisher", w.inTx(unpublishSQL(l, second.ID, w.publisher), pointerSQL(l, "NULL")),
		"not an active administrator")
	wantGuard(t, "unpublish naming a stale previous", w.inTx(unpublishSQL(l, first.ID, w.admin), pointerSQL(l, "NULL")),
		"previous_revision_id must be")
	wantSQLState(t, "unpublish without reason", w.inTx(
		fmt.Sprintf(`INSERT INTO publication_audit (lesson_id, previous_revision_id, actor_id, action) VALUES (%d, %d, %d, 'unpublish')`, l, second.ID, w.admin),
		pointerSQL(l, "NULL")), "23514")
	wantSQLState(t, "unpublish with a blank reason", w.inTx(
		fmt.Sprintf(`INSERT INTO publication_audit (lesson_id, previous_revision_id, actor_id, action, reason) VALUES (%d, %d, %d, 'unpublish', ' ')`, l, second.ID, w.admin),
		pointerSQL(l, "NULL")), "23514")
	wantSQLState(t, "unpublish naming a target", w.inTx(
		fmt.Sprintf(`INSERT INTO publication_audit (lesson_id, revision_id, previous_revision_id, actor_id, action, reason) VALUES (%d, %d, %d, %d, 'unpublish', 'x')`,
			l, first.ID, second.ID, w.admin), pointerSQL(l, f)), "23514")
	wantSQLState(t, "restore with a reason", w.inTx(
		fmt.Sprintf(`INSERT INTO publication_audit (lesson_id, revision_id, previous_revision_id, actor_id, action, reason) VALUES (%d, %d, %d, %d, 'restore', 'x')`,
			l, first.ID, second.ID, w.publisher), pointerSQL(l, f)), "23514")
	wantGuard(t, "restore by an author", w.inTx(restoreSQL(l, first.ID, s, w.author), pointerSQL(l, f)), "not an active publisher or administrator")
	// Publication is for the never-published candidate whose base is current.
	wantGuard(t, "republish as publish", w.inTx(auditSQL(l, first.ID, s, w.publisher), pointerSQL(l, f)), "re-pointing to it is restoration")
	third := w.approved(w.successor(w.author, l, "Third"))
	wantGuard(t, "publish with a stale base", w.inTx(
		unpublishSQL(l, second.ID, w.admin), pointerSQL(l, "NULL"), auditSQL(l, third.ID, "NULL", w.publisher), pointerSQL(l, fmt.Sprint(third.ID))),
		"stale base")
	wantGuard(t, "restore a never-published candidate", w.inTx(restoreSQL(l, third.ID, s, w.publisher), pointerSQL(l, fmt.Sprint(third.ID))),
		"never published")
	// Two pointer changes in one transaction: the first is no longer applied at commit.
	wantGuard(t, "unpublish then restore in one transaction", w.inTx(
		unpublishSQL(l, second.ID, w.admin), pointerSQL(l, "NULL"), restoreSQL(l, first.ID, "NULL", w.publisher), pointerSQL(l, f)),
		"does not point at it")
	before = w.world()

	if err := w.inTx(unpublishSQL(l, second.ID, w.admin), pointerSQL(l, "NULL")); err != nil {
		t.Fatalf("valid direct unpublication: %v", err)
	}
	if err := w.inTx(restoreSQL(l, first.ID, "NULL", w.admin), pointerSQL(l, f)); err != nil {
		t.Fatalf("valid direct restoration: %v", err)
	}
	wantGuard(t, "reused unpublish audit", w.inTx(pointerSQL(l, "NULL")), "requires a publication audit row")
	if w.world() == before {
		t.Fatal("valid changes were not applied")
	}
}

// A withdrawal needs an approved revision and an active administrator; the
// withdrawn revision cannot stay or become the published one.
func TestWithdrawalGuards(t *testing.T) {
	w := newWithdrawEnv(t)
	first, second := w.firstAndSecond()
	l, f, s := first.LessonID, fmt.Sprint(first.ID), fmt.Sprint(second.ID)
	inReview := w.successor(w.author, l, "In review")
	other := w.approved(w.submitted(w.author, w.subject, "Other"))
	before := w.world()

	wantGuard(t, "withdrawal by a publisher", w.inTx(withdrawalSQL(l, first.ID, w.publisher)), "not an active administrator")
	wantGuard(t, "withdrawal of a revision in review", w.inTx(withdrawalSQL(l, inReview.ID, w.admin)), "not an approved revision")
	wantGuard(t, "withdrawal of an approved revision under another lesson", w.inTx(withdrawalSQL(l, other.ID, w.admin)),
		"not an approved revision")
	wantGuard(t, "withdrawal of the published revision without unpublication", w.inTx(withdrawalSQL(l, second.ID, w.admin)),
		"still published")
	wantGuard(t, "backdated withdrawal", w.inTx(fmt.Sprintf(
		`INSERT INTO revision_withdrawals (lesson_id, revision_id, actor_id, reason, occurred_at) VALUES (%d, %d, %d, 'x', now() - interval '1 day')`,
		l, first.ID, w.admin)), "own transaction")
	wantGuard(t, "forged transaction id", w.inTx(fmt.Sprintf(
		`INSERT INTO revision_withdrawals (lesson_id, revision_id, actor_id, reason, xact_id) VALUES (%d, %d, %d, 'x', '1')`,
		l, first.ID, w.admin)), "own transaction")
	wantSQLState(t, "blank reason", w.inTx(fmt.Sprintf(
		`INSERT INTO revision_withdrawals (lesson_id, revision_id, actor_id, reason) VALUES (%d, %d, %d, ' ')`, l, first.ID, w.admin)), "23514")
	// A withdrawal written before the pointer moves to the revision, in the
	// same transaction: the lessons guard refuses the pointer.
	wantGuard(t, "restore to a revision withdrawn in the same transaction", w.inTx(
		restoreSQL(l, first.ID, s, w.publisher), withdrawalSQL(l, first.ID, w.admin), pointerSQL(l, f)), "cannot name withdrawn revision")
	w.unchanged(t, "rejected withdrawals", before)

	if err := w.inTx(withdrawalSQL(l, second.ID, w.admin), unpublishSQL(l, second.ID, w.admin), pointerSQL(l, "NULL")); err != nil {
		t.Fatalf("valid direct withdrawal of the published revision: %v", err)
	}
	wantSQLState(t, "second withdrawal", w.inTx(withdrawalSQL(l, second.ID, w.admin)), "23505")
	wantGuard(t, "restore a withdrawn revision", w.inTx(restoreSQL(l, second.ID, "NULL", w.publisher), pointerSQL(l, s)), "is withdrawn")
	wantGuard(t, "restore audit of a withdrawn revision alone", w.inTx(restoreSQL(l, second.ID, "NULL", w.publisher)), "is withdrawn")
	// Withdrawal does not undo history: the publish audit rows remain.
	if n := w.count(`SELECT count(*) FROM publication_audit WHERE revision_id = $1 AND action = 'publish'`, second.ID); n != 1 {
		t.Fatalf("publish rows %d", n)
	}
}

func TestWithdrawalAuditIsAppendOnly(t *testing.T) {
	w := newWithdrawEnv(t)
	first, second := w.firstAndSecond()
	if _, err := w.review.Withdraw(w.ctx, w.admin, second.ID, "Simulated reason.", &second.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := w.review.Restore(w.ctx, w.publisher, first.ID, nil); err != nil {
		t.Fatal(err)
	}
	before := w.world()
	for name, stmt := range map[string]string{
		"withdrawal reason changed":   `UPDATE revision_withdrawals SET reason = 'Edited'`,
		"withdrawal actor changed":    fmt.Sprintf(`UPDATE revision_withdrawals SET actor_id = %d`, w.author),
		"withdrawal deleted":          `DELETE FROM revision_withdrawals`,
		"withdrawals truncated":       `TRUNCATE revision_withdrawals`,
		"unpublish reason changed":    `UPDATE publication_audit SET reason = 'Edited' WHERE action = 'unpublish'`,
		"restore turned into publish": `UPDATE publication_audit SET action = 'publish' WHERE action = 'restore'`,
		"unpublish deleted":           `DELETE FROM publication_audit WHERE action = 'unpublish'`,
		"audit truncated":             `TRUNCATE publication_audit CASCADE`,
	} {
		wantGuard(t, name, w.exec(stmt), "append-only")
	}
	wantSQLState(t, "withdrawn revision deleted", w.exec(`DELETE FROM lesson_revisions WHERE id = $1`, second.ID), "LRN01")
	w.unchanged(t, "append-only audit", before)
}

// The guards of 0007 read lessons, lesson_revisions, publication_audit,
// revision_withdrawals, user_permissions and users. A temporary table of
// the calling (non-owner) role with any of those names must not fool them.
func TestWithdrawalGuardsIgnoreTemporaryShadowTables(t *testing.T) {
	w := newWithdrawEnv(t)
	first, second := w.firstAndSecond()
	pending := w.approved(w.successor(w.author, first.LessonID, "Pending"))
	stranger := w.user("stranger", permissions.Author)
	asRole, qs := w.leastPrivilegeRole()
	q := func(sql string, args ...any) string { return fmt.Sprintf(sql, append([]any{qs}, args...)...) }
	l := first.LessonID
	withdraw := func(rev, actor int64) string {
		return q(`INSERT INTO %s.revision_withdrawals (lesson_id, revision_id, actor_id, reason) VALUES (%d, %d, %d, 'Simulated reason.')`, l, rev, actor)
	}
	audit := func(action string, rev, previous string, actor int64) string {
		reason := "NULL"
		if action == "unpublish" {
			reason = "'Simulated reason.'"
		}
		return q(`INSERT INTO %s.publication_audit (lesson_id, revision_id, previous_revision_id, actor_id, action, reason) VALUES (%d, %s, %s, %d, '%s', %s)`,
			l, rev, previous, actor, action, reason)
	}
	point := func(rev string) string {
		return q(`UPDATE %s.lessons SET published_revision_id = %s WHERE id = %d`, rev, l)
	}
	f, s, p := fmt.Sprint(first.ID), fmt.Sprint(second.ID), fmt.Sprint(pending.ID)
	before := w.world()

	wantGuard(t, "withdrawal by a non-administrator via shadow user_permissions", asRole(
		`CREATE TEMP TABLE user_permissions (user_id bigint, permission text)`,
		`CREATE TEMP TABLE users (id bigint, disabled_at timestamptz)`,
		fmt.Sprintf(`INSERT INTO pg_temp.user_permissions VALUES (%d, 'administrator')`, stranger),
		fmt.Sprintf(`INSERT INTO pg_temp.users VALUES (%d, NULL)`, stranger),
		withdraw(first.ID, stranger),
	), "not an active administrator")
	inReview := w.submitted(w.author, w.subject, "In review elsewhere")
	wantGuard(t, "withdrawal of an unapproved revision via shadow lesson_revisions", asRole(
		`CREATE TEMP TABLE lesson_revisions (id bigint, lesson_id bigint, status text)`,
		fmt.Sprintf(`INSERT INTO pg_temp.lesson_revisions VALUES (%d, %d, 'approved')`, inReview.ID, inReview.LessonID),
		q(`INSERT INTO %s.revision_withdrawals (lesson_id, revision_id, actor_id, reason) VALUES (%d, %d, %d, 'x')`, inReview.LessonID, inReview.ID, w.admin),
	), "not an approved revision")
	wantGuard(t, "published withdrawn revision via shadow lessons", asRole(
		`CREATE TEMP TABLE lessons (id bigint, published_revision_id bigint)`,
		fmt.Sprintf(`INSERT INTO pg_temp.lessons VALUES (%d, NULL)`, l),
		withdraw(second.ID, w.admin),
	), "still published")
	wantGuard(t, "unpublish via shadow publication_audit", asRole(
		`CREATE TEMP TABLE publication_audit (lesson_id bigint, revision_id bigint, previous_revision_id bigint, xact_id xid8)`,
		fmt.Sprintf(`INSERT INTO pg_temp.publication_audit VALUES (%d, NULL, %d, pg_current_xact_id())`, l, second.ID),
		point("NULL"),
	), "requires a publication audit row")
	wantGuard(t, "unpublish by a non-administrator via shadow user_permissions", asRole(
		`CREATE TEMP TABLE user_permissions (user_id bigint, permission text)`,
		`CREATE TEMP TABLE users (id bigint, disabled_at timestamptz)`,
		fmt.Sprintf(`INSERT INTO pg_temp.user_permissions VALUES (%d, 'administrator')`, stranger),
		fmt.Sprintf(`INSERT INTO pg_temp.users VALUES (%d, NULL)`, stranger),
		audit("unpublish", "NULL", s, stranger), point("NULL"),
	), "not an active administrator")
	wantGuard(t, "restore of a never-published candidate via shadow publication_audit", asRole(
		`CREATE TEMP TABLE publication_audit (lesson_id bigint, revision_id bigint, action text)`,
		fmt.Sprintf(`INSERT INTO pg_temp.publication_audit VALUES (%d, %d, 'publish')`, l, pending.ID),
		audit("restore", p, s, w.publisher), point(p),
	), "never published")
	wantGuard(t, "restore by a non-publisher via shadow user_permissions", asRole(
		`CREATE TEMP TABLE user_permissions (user_id bigint, permission text)`,
		`CREATE TEMP TABLE users (id bigint, disabled_at timestamptz)`,
		fmt.Sprintf(`INSERT INTO pg_temp.user_permissions VALUES (%d, 'publisher')`, stranger),
		fmt.Sprintf(`INSERT INTO pg_temp.users VALUES (%d, NULL)`, stranger),
		audit("restore", f, s, stranger), point(f),
	), "not an active publisher or administrator")
	wantGuard(t, "republish via shadow lesson_revisions", asRole(
		`CREATE TEMP TABLE lesson_revisions (id bigint, lesson_id bigint, status text, base_revision_id bigint, candidate_closed_at timestamptz)`,
		fmt.Sprintf(`INSERT INTO pg_temp.lesson_revisions VALUES (%d, %d, 'approved', %d, NULL)`, first.ID, l, second.ID),
		audit("publish", f, s, w.publisher), point(f),
	), "re-pointing to it is restoration")
	wantGuard(t, "stale base via shadow lessons", asRole(
		`CREATE TEMP TABLE lessons (id bigint, published_revision_id bigint)`,
		fmt.Sprintf(`INSERT INTO pg_temp.lessons VALUES (%d, %d)`, l, second.ID),
		audit("unpublish", "NULL", s, w.admin), point("NULL"),
		audit("publish", p, "NULL", w.publisher), point(p),
	), "stale base")
	// Withdrawn revisions, hidden by an empty shadow revision_withdrawals.
	// The pointer guard on lessons: the withdrawal is written after the
	// audit row, in the same transaction (the commit-time check would
	// report "still published" if the guard were fooled).
	wantGuard(t, "pointer to a withdrawn revision via shadow revision_withdrawals", asRole(
		`CREATE TEMP TABLE revision_withdrawals (revision_id bigint)`,
		audit("restore", f, s, w.publisher), withdraw(first.ID, w.admin), point(f),
	), "cannot name withdrawn revision")
	if err := w.inTx(withdrawalSQL(l, first.ID, w.admin)); err != nil {
		t.Fatal(err)
	}
	before = w.world()
	// The audit insert guard.
	wantGuard(t, "restore of a withdrawn revision via shadow revision_withdrawals", asRole(
		`CREATE TEMP TABLE revision_withdrawals (revision_id bigint)`,
		audit("restore", f, s, w.publisher),
	), "is withdrawn")
	w.unchanged(t, "shadowed changes", before)

	// The same role withdraws the pending candidate legitimately; the
	// owner-run trigger closes it.
	if err := asRole(withdraw(pending.ID, w.admin)); err != nil {
		t.Fatalf("legitimate withdrawal as the role: %v", err)
	}
	if n := w.count(`SELECT count(*) FROM lesson_revisions WHERE id = $1 AND candidate_closed_at = (SELECT occurred_at FROM revision_withdrawals WHERE revision_id = $1)`, pending.ID); n != 1 {
		t.Fatal("withdrawal as the role did not close the candidate at the transaction time")
	}
	if err := asRole(audit("unpublish", "NULL", s, w.admin), point("NULL")); err != nil {
		t.Fatalf("legitimate unpublication as the role: %v", err)
	}
	if err := asRole(audit("restore", s, "NULL", w.publisher), point(s)); err != nil {
		t.Fatalf("legitimate restoration as the role: %v", err)
	}
}

// The withdrawal close trigger is SECURITY DEFINER: no other role may
// attach it to a table of its own, it refuses to run on any relation other
// than the owner's permanent revision_withdrawals, and a non-owner's own
// trigger cannot close a candidate.
func TestWithdrawalCloseTriggerIsHardened(t *testing.T) {
	w := newWithdrawEnv(t)
	first := w.approved(w.submitted(w.author, w.subject, "First edition"))
	w.published(first)
	pending := w.approved(w.successor(w.author, first.LessonID, "Pending"))
	asRole, qs := w.leastPrivilegeRole()
	before := w.world()

	// EXECUTE is revoked from PUBLIC: the role cannot attach the function.
	wantSQLState(t, "attach the definer function", asRole(
		`CREATE TEMP TABLE revision_withdrawals (lesson_id bigint, revision_id bigint)`,
		fmt.Sprintf(`CREATE TRIGGER t AFTER INSERT ON pg_temp.revision_withdrawals FOR EACH ROW EXECUTE FUNCTION %s.revision_withdrawals_close_candidate()`, qs),
	), "42501")
	// The role's own trigger function runs as the role: the immutability
	// guard refuses the close.
	wantGuard(t, "close through the role's own trigger", asRole(
		`CREATE TEMP TABLE revision_withdrawals (lesson_id bigint, revision_id bigint)`,
		fmt.Sprintf(`CREATE FUNCTION pg_temp.close_it() RETURNS trigger LANGUAGE plpgsql AS $$
			BEGIN UPDATE %s.lesson_revisions SET candidate_closed_at = now() WHERE id = NEW.revision_id; RETURN NULL; END $$`, qs),
		`CREATE TRIGGER t AFTER INSERT ON pg_temp.revision_withdrawals FOR EACH ROW EXECUTE FUNCTION pg_temp.close_it()`,
		fmt.Sprintf(`INSERT INTO pg_temp.revision_withdrawals VALUES (%d, %d)`, pending.LessonID, pending.ID),
	), "set only by publication or audited withdrawal")
	// Even the owner cannot fire it from a temporary table.
	tx, err := w.db.Begin(w.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(w.ctx) }()
	var execErr error
	for _, s := range []string{
		`CREATE TEMP TABLE revision_withdrawals (lesson_id bigint, revision_id bigint)`,
		fmt.Sprintf(`CREATE TRIGGER t AFTER INSERT ON pg_temp.revision_withdrawals FOR EACH ROW EXECUTE FUNCTION %s.revision_withdrawals_close_candidate()`, qs),
		fmt.Sprintf(`INSERT INTO pg_temp.revision_withdrawals VALUES (%d, %d)`, pending.LessonID, pending.ID),
	} {
		if _, execErr = tx.Exec(w.ctx, s); execErr != nil {
			break
		}
	}
	_ = tx.Rollback(w.ctx)
	wantGuard(t, "owner fires it from a temporary table", execErr, "may only fire on the revision_withdrawals table")
	w.unchanged(t, "hardened close trigger", before)
}

// staleSnapshotWriter starts a transaction at isolation iso on a new
// connection (as role, if set), takes its snapshot, runs between (a
// concurrent change that commits), then runs stmts and commits.
func (e *env) staleSnapshotWriter(iso, role string, between func(), stmts ...string) error {
	e.t.Helper()
	conn, err := pgx.ConnectConfig(e.ctx, e.db.Config().ConnConfig.Copy())
	if err != nil {
		e.t.Fatal(err)
	}
	defer func() { _ = conn.Close(context.Background()) }()
	tx, err := conn.Begin(e.ctx)
	if err != nil {
		e.t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(e.ctx) }()
	setup := []string{`SET TRANSACTION ISOLATION LEVEL ` + iso}
	if role != "" {
		setup = append(setup, `SET LOCAL ROLE `+role)
	}
	setup = append(setup, `SELECT count(*) FROM lessons`) // takes the snapshot
	for _, s := range setup {
		if _, err := tx.Exec(e.ctx, s); err != nil {
			e.t.Fatalf("%s: %v", s, err)
		}
	}
	between()
	for _, s := range stmts {
		if _, err := tx.Exec(e.ctx, s); err != nil {
			return err
		}
	}
	return tx.Commit(e.ctx)
}

// wantRefused accepts a serialization failure (40001) or a guard (LRN01).
func wantRefused(t *testing.T, what string, err error) {
	t.Helper()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || (pgErr.Code != "40001" && pgErr.Code != "LRN01") {
		t.Errorf("%s: want 40001 or LRN01, got %v", what, err)
	}
}

// A writer whose REPEATABLE READ or SERIALIZABLE snapshot predates a
// committed withdrawal must not publish or restore the withdrawn revision:
// the withdrawal updates the lesson row, so the stale writer's lock or
// update of it fails. Run as the store's role and as a non-owner role.
func TestStaleSnapshotCannotPublishWithdrawnRevision(t *testing.T) {
	w := newWithdrawEnv(t)
	role, qs := w.newLeastPrivilegeRole()
	q := func(sql string) string { return strings.ReplaceAll(sql, "lessons", qs+".lessons") }
	for _, iso := range []string{"REPEATABLE READ", "SERIALIZABLE"} {
		for who, r := range map[string]string{"store role": "", "non-owner role": role} {
			name := iso + " as " + who
			qual := func(sql string) string { return sql }
			if r != "" {
				qual = func(sql string) string {
					sql = strings.ReplaceAll(sql, "INTO publication_audit", "INTO "+qs+".publication_audit")
					return q(sql)
				}
			}
			// Restoration of a previously published, non-current revision.
			first, second := w.firstAndSecond()
			err := w.staleSnapshotWriter(iso, r, func() {
				if _, err := w.review.Withdraw(w.ctx, w.admin, first.ID, "Simulated reason.", &second.ID); err != nil {
					t.Fatal(err)
				}
			}, qual(restoreSQL(first.LessonID, first.ID, fmt.Sprint(second.ID), w.publisher)), qual(pointerSQL(first.LessonID, fmt.Sprint(first.ID))))
			wantRefused(t, name+": restore withdrawn", err)
			if p := w.pointer(first.LessonID); p == nil || *p != second.ID {
				t.Fatalf("%s: pointer %v, want %d", name, p, second.ID)
			}
			// Publication of a pending candidate.
			pending := w.approved(w.successor(w.author, first.LessonID, "Pending"))
			err = w.staleSnapshotWriter(iso, r, func() {
				if _, err := w.review.Withdraw(w.ctx, w.admin, pending.ID, "Simulated reason.", &second.ID); err != nil {
					t.Fatal(err)
				}
			}, qual(auditSQL(first.LessonID, pending.ID, fmt.Sprint(second.ID), w.publisher)), qual(pointerSQL(first.LessonID, fmt.Sprint(pending.ID))))
			wantRefused(t, name+": publish withdrawn", err)
			if p := w.pointer(first.LessonID); p == nil || *p != second.ID {
				t.Fatalf("%s: pointer %v, want %d", name, p, second.ID)
			}
		}
	}
}

// lessons.withdrawal_count changes only through a withdrawal, by one.
func TestWithdrawalCountGuard(t *testing.T) {
	w := newWithdrawEnv(t)
	first, second := w.firstAndSecond()
	asRole, qs := w.leastPrivilegeRole()
	before := w.world()
	l := first.LessonID
	wantGuard(t, "direct increment", w.exec(`UPDATE lessons SET withdrawal_count = withdrawal_count + 1 WHERE id = $1`, l),
		"changed only by an audited withdrawal")
	wantGuard(t, "increment through the role's own trigger", asRole(
		`CREATE TEMP TABLE bump (lesson_id bigint)`,
		fmt.Sprintf(`CREATE FUNCTION pg_temp.bump_it() RETURNS trigger LANGUAGE plpgsql AS $$
			BEGIN UPDATE %s.lessons SET withdrawal_count = withdrawal_count + 1 WHERE id = NEW.lesson_id; RETURN NULL; END $$`, qs),
		`CREATE TRIGGER t AFTER INSERT ON pg_temp.bump FOR EACH ROW EXECUTE FUNCTION pg_temp.bump_it()`,
		fmt.Sprintf(`INSERT INTO pg_temp.bump VALUES (%d)`, l),
	), "changed only by an audited withdrawal")
	wantGuard(t, "lesson created with withdrawals", w.exec(`
		INSERT INTO lessons (translation_group_id, language, subject_id, created_at, created_by, withdrawal_count)
		SELECT translation_group_id, 'ar', subject_id, now(), created_by, 3 FROM lessons WHERE id = $1`, l),
		"starts with no withdrawals")
	w.unchanged(t, "rejected counter changes", before)
	count := func() int { return w.count(`SELECT withdrawal_count FROM lessons WHERE id = $1`, l) }
	if count() != 0 {
		t.Fatal("counter not zero")
	}
	if _, err := w.review.Withdraw(w.ctx, w.admin, first.ID, "Simulated reason.", &second.ID); err != nil {
		t.Fatal(err)
	}
	if err := asRole(fmt.Sprintf(`INSERT INTO %s.revision_withdrawals (lesson_id, revision_id, actor_id, reason) VALUES (%d, %d, %d, 'x')`, qs, l, second.ID, w.admin),
		fmt.Sprintf(`INSERT INTO %s.publication_audit (lesson_id, previous_revision_id, actor_id, action, reason) VALUES (%d, %d, %d, 'unpublish', 'x')`, qs, l, second.ID, w.admin),
		fmt.Sprintf(`UPDATE %s.lessons SET published_revision_id = NULL WHERE id = %d`, qs, l)); err != nil {
		t.Fatalf("withdrawal as the non-owner role: %v", err)
	}
	if count() != 2 {
		t.Fatalf("withdrawal_count %d, want 2", count())
	}
}
