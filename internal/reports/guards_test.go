package reports_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/hadden-me/learnoo/internal/reports"
	"github.com/jackc/pgx/v5"
)

// Database guards of migrations/0009, exercised with direct SQL that
// bypasses the Store.

func reportSQL(l lesson, revision, reporter int64, message string) string {
	return fmt.Sprintf(`INSERT INTO error_reports (lesson_id, revision_id, reporter_id, message) VALUES (%d, %d, %d, '%s')`,
		l.id, revision, reporter, message)
}

func resolutionSQL(report, actor int64, action, note string) string {
	return fmt.Sprintf(`INSERT INTO error_report_resolutions (report_id, actor_id, action, note) VALUES (%d, %d, '%s', '%s')`,
		report, actor, action, note)
}

func reportStatusSQL(report int64, status string) string {
	return fmt.Sprintf(`UPDATE error_reports SET status = '%s' WHERE id = %d`, status, report)
}

func TestReportInsertGuard(t *testing.T) {
	e := newEnv(t)
	first := e.published(lesson{}, e.subject, "TEST Garde")
	second := e.published(lesson{id: first.id}, e.subject, "TEST Garde 2")
	before := e.reportsState()

	wantGuard(t, "earlier revision", e.inTx(reportSQL(first, first.revision, e.student, "Erreur.")), "current published revision")
	wantGuard(t, "created resolved", e.inTx(fmt.Sprintf(
		`INSERT INTO error_reports (lesson_id, revision_id, reporter_id, message, status) VALUES (%d, %d, %d, 'Erreur.', 'resolved')`,
		first.id, second.revision, e.student)), "created open")
	wantGuard(t, "backdated", e.inTx(fmt.Sprintf(
		`INSERT INTO error_reports (lesson_id, revision_id, reporter_id, message, created_at) VALUES (%d, %d, %d, 'Erreur.', now() - interval '1 day')`,
		first.id, second.revision, e.student)), "transaction time")
	wantSQLState(t, "revision of another lesson", e.inTx(reportSQL(lesson{id: first.id}, e.published(lesson{}, e.subject, "TEST Autre").revision,
		e.student, "Erreur.")), "LRN01")
	for name, msg := range map[string]string{
		"blank":    " \n ",
		"too long": strings.Repeat("x", reports.MaxMessage+1),
		"tab":      "a\tb",
		"escape":   "a\x1bb",
		"rlo":      "a‮b",
		"isolate":  "a⁦b",
		"alm":      "a؜b",
		"line sep": "a b",
	} {
		wantSQLState(t, "message "+name, e.inTx(reportSQL(second, second.revision, e.student, msg)), "23514")
	}
	if e.reportsState() != before {
		t.Fatal("refused inserts stored reports")
	}
	if err := e.inTx(reportSQL(second, second.revision, e.student, "Ligne 1\nLigne 2 en العربية.")); err != nil {
		t.Fatalf("valid direct report: %v", err)
	}

	archived := e.published(lesson{}, e.subject, "TEST Archivée")
	if err := e.exec(`UPDATE lessons SET archived_at = now() WHERE id = $1`, archived.id); err != nil {
		t.Fatal(err)
	}
	wantGuard(t, "archived lesson", e.inTx(reportSQL(archived, archived.revision, e.student, "Erreur.")), "current published revision")

	if _, err := e.review.Unpublish(e.ctx, e.admin, first.id, "TEST unpublication", second.revision); err != nil {
		t.Fatal(err)
	}
	wantGuard(t, "unpublished lesson", e.inTx(reportSQL(second, second.revision, e.student, "Erreur.")), "current published revision")
}

// At READ COMMITTED a report waits for an in-flight change of the lesson's
// published revision (the guard share-locks the lesson row) and is then
// checked against the committed result.
func TestReportWaitsForPointerChange(t *testing.T) {
	e := newEnv(t)
	l := e.published(lesson{}, e.subject, "TEST Attente")
	tx, err := e.db.Begin(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(e.ctx) }()
	var pid uint32
	if err := tx.QueryRow(e.ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{
		fmt.Sprintf(`INSERT INTO publication_audit (lesson_id, revision_id, previous_revision_id, actor_id, action, reason)
			VALUES (%d, NULL, %d, %d, 'unpublish', 'TEST unpublication')`, l.id, l.revision, e.admin),
		fmt.Sprintf(`UPDATE lessons SET published_revision_id = NULL WHERE id = %d`, l.id),
	} {
		if _, err := tx.Exec(e.ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	done := make(chan error, 1)
	go func() { done <- e.inTx(reportSQL(l, l.revision, e.student, "Erreur.")) }()
	e.waitBlockedBy(pid)
	if err := tx.Commit(e.ctx); err != nil {
		t.Fatal(err)
	}
	wantGuard(t, "report after the unpublication committed", <-done, "current published revision")
	if n := e.count(`SELECT count(*) FROM error_reports`); n != 0 {
		t.Fatalf("%d reports", n)
	}
}

// waitBlockedBy waits until some backend waits on a lock held by pid.
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

// Report content never changes; the status changes once, with a matching
// resolution of the same transaction; nothing is deleted; resolutions are
// append-only.
func TestReportImmutabilityAndResolutionGuards(t *testing.T) {
	e := newEnv(t)
	l := e.published(lesson{}, e.subject, "TEST Immuable")
	other := e.published(lesson{}, e.subject, "TEST Autre leçon")
	id := e.report(e.student, l, "Erreur.")
	stranger := e.user("stranger")
	otherReviewer := e.user("other-reviewer")
	e.assign(otherReviewer, e.other)
	before := e.reportsState()

	for name, stmt := range map[string]string{
		"message":   fmt.Sprintf(`UPDATE error_reports SET message = 'Modifié.' WHERE id = %d`, id),
		"lesson":    fmt.Sprintf(`UPDATE error_reports SET lesson_id = %d, revision_id = %d WHERE id = %d`, other.id, other.revision, id),
		"reporter":  fmt.Sprintf(`UPDATE error_reports SET reporter_id = %d WHERE id = %d`, e.author, id),
		"time":      fmt.Sprintf(`UPDATE error_reports SET created_at = created_at - interval '1 day' WHERE id = %d`, id),
		"with note": fmt.Sprintf(`UPDATE error_reports SET message = 'Modifié.', status = 'resolved' WHERE id = %d`, id),
	} {
		wantGuard(t, "change "+name, e.inTx(stmt), "immutable")
	}
	wantGuard(t, "delete", e.inTx(fmt.Sprintf(`DELETE FROM error_reports WHERE id = %d`, id)), "never deleted")
	wantGuard(t, "truncate", e.inTx(`TRUNCATE error_reports CASCADE`), "never deleted")
	wantGuard(t, "status without resolution", e.inTx(reportStatusSQL(id, "resolved")), "matching resolution")
	wantGuard(t, "resolution without status", e.inTx(resolutionSQL(id, e.reviewer, "resolve", "Note.")), "was not applied")
	wantGuard(t, "mismatched status", e.inTx(resolutionSQL(id, e.reviewer, "resolve", "Note."), reportStatusSQL(id, "dismissed")),
		"matching resolution")
	wantGuard(t, "unassigned actor", e.inTx(resolutionSQL(id, stranger, "resolve", "Note."), reportStatusSQL(id, "resolved")),
		"neither an active administrator")
	wantGuard(t, "reviewer of another subject", e.inTx(resolutionSQL(id, otherReviewer, "dismiss", "Note."), reportStatusSQL(id, "dismissed")),
		"neither an active administrator")
	wantGuard(t, "backdated resolution", e.inTx(fmt.Sprintf(
		`INSERT INTO error_report_resolutions (report_id, actor_id, action, note, occurred_at) VALUES (%d, %d, 'resolve', 'Note.', now() - interval '1 day')`,
		id, e.reviewer), reportStatusSQL(id, "resolved")), "own transaction")
	wantGuard(t, "forged transaction", e.inTx(fmt.Sprintf(
		`INSERT INTO error_report_resolutions (report_id, actor_id, action, note, xact_id) VALUES (%d, %d, 'resolve', 'Note.', '1')`,
		id, e.reviewer), reportStatusSQL(id, "resolved")), "own transaction")
	wantSQLState(t, "blank note", e.inTx(resolutionSQL(id, e.reviewer, "resolve", " "), reportStatusSQL(id, "resolved")), "23514")
	wantSQLState(t, "bidi note", e.inTx(resolutionSQL(id, e.reviewer, "resolve", "a‮b"), reportStatusSQL(id, "resolved")), "23514")
	wantSQLState(t, "unknown action", e.inTx(resolutionSQL(id, e.reviewer, "delete", "Note."), reportStatusSQL(id, "resolved")), "23514")
	wantSQLState(t, "two resolutions in one transaction", e.inTx(resolutionSQL(id, e.reviewer, "resolve", "Note."),
		resolutionSQL(id, e.admin, "dismiss", "Note."), reportStatusSQL(id, "resolved")), "23505")
	wantSQLState(t, "forced failure", e.inTx(resolutionSQL(id, e.reviewer, "resolve", "Note."), reportStatusSQL(id, "resolved"), `SELECT 1/0`), "22012")
	if e.reportsState() != before {
		t.Fatal("refused changes altered reports")
	}

	if err := e.inTx(resolutionSQL(id, e.reviewer, "resolve", "Corrigé."), reportStatusSQL(id, "resolved")); err != nil {
		t.Fatalf("valid direct resolution: %v", err)
	}
	resolved := e.reportsState()
	wantGuard(t, "reopen", e.inTx(reportStatusSQL(id, "open")), "already closed")
	wantGuard(t, "second resolution", e.inTx(resolutionSQL(id, e.admin, "dismiss", "Note."), reportStatusSQL(id, "dismissed")), "not open")
	for name, stmt := range map[string]string{
		"note changed":   `UPDATE error_report_resolutions SET note = 'Modifiée.'`,
		"actor changed":  fmt.Sprintf(`UPDATE error_report_resolutions SET actor_id = %d`, e.admin),
		"time changed":   `UPDATE error_report_resolutions SET occurred_at = occurred_at - interval '1 day'`,
		"action changed": `UPDATE error_report_resolutions SET action = 'dismiss'`,
		"deleted":        `DELETE FROM error_report_resolutions`,
		"truncated":      `TRUNCATE error_report_resolutions`,
	} {
		wantGuard(t, "resolution "+name, e.inTx(stmt), "append-only")
	}
	if err := e.inTx(fmt.Sprintf(`DELETE FROM lessons WHERE id = %d`, l.id)); err == nil {
		t.Fatal("a reported lesson was deleted")
	}
	if after := e.reportsState(); after != resolved {
		t.Fatalf("audit changed:\n%s\n%s", resolved, after)
	}
}

// A writer at REPEATABLE READ or SERIALIZABLE whose snapshot predates an
// unpublication or a resolution fails instead of acting on the old state.
func TestGuardsRefuseStaleSnapshots(t *testing.T) {
	for _, level := range []string{"REPEATABLE READ", "SERIALIZABLE"} {
		t.Run(level, func(t *testing.T) {
			e := newEnv(t)
			l := e.published(lesson{}, e.subject, "TEST Instantané")
			id := e.report(e.student, l, "Erreur.")

			stale := func(change func(), stmts ...string) error {
				t.Helper()
				tx, err := e.db.BeginTx(e.ctx, pgx.TxOptions{IsoLevel: pgx.TxIsoLevel(strings.ToLower(level))})
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = tx.Rollback(e.ctx) }()
				// Take the snapshot before the concurrent change commits.
				if _, err := tx.Exec(e.ctx, `SELECT count(*) FROM error_reports`); err != nil {
					t.Fatal(err)
				}
				change()
				for _, s := range stmts {
					if _, err := tx.Exec(e.ctx, s); err != nil {
						return err
					}
				}
				return tx.Commit(e.ctx)
			}

			err := stale(func() {
				if _, err := e.reports.Resolve(e.ctx, e.admin, id, reports.ActionDismiss, "Pas une erreur."); err != nil {
					t.Fatal(err)
				}
			}, resolutionSQL(id, e.reviewer, "resolve", "Corrigé."), reportStatusSQL(id, "resolved"))
			wantSQLState(t, "resolution after a concurrent dismissal", err, "40001")

			err = stale(func() {
				if _, err := e.review.Unpublish(e.ctx, e.admin, l.id, "TEST unpublication", l.revision); err != nil {
					t.Fatal(err)
				}
			}, reportSQL(l, l.revision, e.student, "Erreur."))
			wantSQLState(t, "report after a concurrent unpublication", err, "40001")
			if n := e.count(`SELECT count(*) FROM error_report_resolutions`); n != 1 {
				t.Fatalf("%d resolutions", n)
			}
			if n := e.count(`SELECT count(*) FROM error_reports`); n != 1 {
				t.Fatalf("%d reports", n)
			}
		})
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

// The guards read lessons, revision_withdrawals, users, error_reports,
// error_report_resolutions, user_permissions and reviewer_assignments. A
// temporary table of the calling role with any of those names must not fool
// them (pg_temp is otherwise searched first for relations).
func TestReportGuardsIgnoreTemporaryShadowTables(t *testing.T) {
	e := newEnv(t)
	first := e.published(lesson{}, e.subject, "TEST Ombre")
	l := e.published(lesson{id: first.id}, e.subject, "TEST Ombre 2")
	disabled := e.user("disabled-student")
	if err := e.exec(`UPDATE users SET disabled_at = now() WHERE id = $1`, disabled); err != nil {
		t.Fatal(err)
	}
	stranger := e.user("stranger")
	otherReviewer := e.user("other-reviewer")
	e.assign(otherReviewer, e.other)
	asRole, qs := e.leastPrivilegeRole()
	q := func(sql string, args ...any) string {
		return strings.ReplaceAll(fmt.Sprintf(sql, args...), "S.", qs+".")
	}
	report := func(rev, reporter int64) string {
		return q(`INSERT INTO S.error_reports (lesson_id, revision_id, reporter_id, message) VALUES (%d, %d, %d, 'Erreur.')`, l.id, rev, reporter)
	}
	open := e.report(e.student, l, "Erreur ouverte.")
	closed := e.report(e.student, l, "Erreur close.")
	if _, err := e.reports.Resolve(e.ctx, e.reviewer, closed, reports.ActionResolve, "Corrigé."); err != nil {
		t.Fatal(err)
	}
	resolve := func(report, actor int64) []string {
		return []string{
			q(`INSERT INTO S.error_report_resolutions (report_id, actor_id, action, note) VALUES (%d, %d, 'resolve', 'Note.')`, report, actor),
			q(`UPDATE S.error_reports SET status = 'resolved' WHERE id = %d`, report),
		}
	}
	before := e.reportsState()

	wantGuard(t, "earlier revision via shadow lessons", asRole(
		`CREATE TEMP TABLE lessons (id bigint, published_revision_id bigint, archived_at timestamptz, subject_id bigint)`,
		fmt.Sprintf(`INSERT INTO pg_temp.lessons VALUES (%d, %d, NULL, %d)`, l.id, first.revision, e.subject),
		report(first.revision, e.student),
	), "current published revision")
	wantGuard(t, "disabled reporter via shadow users", asRole(
		`CREATE TEMP TABLE users (id bigint, disabled_at timestamptz)`,
		fmt.Sprintf(`INSERT INTO pg_temp.users VALUES (%d, NULL)`, disabled),
		report(l.revision, disabled),
	), "not an active account")
	wantGuard(t, "status via shadow resolutions", asRole(
		`CREATE TEMP TABLE error_report_resolutions (report_id bigint, action text, xact_id xid8)`,
		fmt.Sprintf(`INSERT INTO pg_temp.error_report_resolutions VALUES (%d, 'resolve', pg_current_xact_id())`, open),
		q(`UPDATE S.error_reports SET status = 'resolved' WHERE id = %d`, open),
	), "matching resolution")
	wantGuard(t, "closed report via shadow reports", asRole(append([]string{
		`CREATE TEMP TABLE error_reports (id bigint, lesson_id bigint, status text)`,
		fmt.Sprintf(`INSERT INTO pg_temp.error_reports VALUES (%d, %d, 'open')`, closed, l.id),
	}, resolve(closed, e.admin)[0])...), "not open")
	wantGuard(t, "unassigned actor via shadow assignments", asRole(append([]string{
		`CREATE TEMP TABLE reviewer_assignments (user_id bigint, subject_id bigint)`,
		`CREATE TEMP TABLE users (id bigint, disabled_at timestamptz)`,
		fmt.Sprintf(`INSERT INTO pg_temp.reviewer_assignments VALUES (%d, %d)`, stranger, e.subject),
		fmt.Sprintf(`INSERT INTO pg_temp.users VALUES (%d, NULL)`, stranger),
	}, resolve(open, stranger)...)...), "neither an active administrator")
	wantGuard(t, "unassigned actor via shadow permissions", asRole(append([]string{
		`CREATE TEMP TABLE user_permissions (user_id bigint, permission text)`,
		`CREATE TEMP TABLE users (id bigint, disabled_at timestamptz)`,
		fmt.Sprintf(`INSERT INTO pg_temp.user_permissions VALUES (%d, 'administrator')`, stranger),
		fmt.Sprintf(`INSERT INTO pg_temp.users VALUES (%d, NULL)`, stranger),
	}, resolve(open, stranger)...)...), "neither an active administrator")
	wantGuard(t, "other subject via shadow lessons", asRole(append([]string{
		`CREATE TEMP TABLE lessons (id bigint, subject_id bigint)`,
		fmt.Sprintf(`INSERT INTO pg_temp.lessons VALUES (%d, %d)`, l.id, e.other),
	}, resolve(open, otherReviewer)...)...), "neither an active administrator")
	wantGuard(t, "unapplied resolution via shadow reports", asRole(
		resolve(open, e.reviewer)[0],
		`CREATE TEMP TABLE error_reports (id bigint, status text)`,
		fmt.Sprintf(`INSERT INTO pg_temp.error_reports VALUES (%d, 'resolved')`, open),
	), "was not applied")
	if after := e.reportsState(); after != before {
		t.Fatalf("shadowed statements changed reports:\n%s\n%s", before, after)
	}

	// The same role reports and resolves legitimately, also with a shadow
	// revision_withdrawals claiming the current revision is withdrawn.
	if err := asRole(
		`CREATE TEMP TABLE revision_withdrawals (revision_id bigint)`,
		fmt.Sprintf(`INSERT INTO pg_temp.revision_withdrawals VALUES (%d)`, l.revision),
		report(l.revision, e.student),
	); err != nil {
		t.Fatalf("legitimate report as the role: %v", err)
	}
	if err := asRole(resolve(open, e.reviewer)...); err != nil {
		t.Fatalf("legitimate resolution as the role: %v", err)
	}
	if n := e.count(`SELECT count(*) FROM error_reports WHERE status = 'open'`); n != 1 {
		t.Fatalf("%d open reports", n)
	}
}
