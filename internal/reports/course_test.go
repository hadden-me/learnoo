package reports_test

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"sync"
	"testing"
	"testing/fstest"

	"github.com/hadden-me/learnoo/internal/coursetest"
	"github.com/hadden-me/learnoo/internal/database"
	"github.com/hadden-me/learnoo/internal/database/dbtest"
	"github.com/hadden-me/learnoo/internal/reports"
	"github.com/hadden-me/learnoo/migrations"
	"github.com/jackc/pgx/v5"
)

// Course-context error reports (LMS-B08, migrations/0017): the Store and,
// with direct SQL that bypasses it, the replaced insert guard.

// course publishes a TEST course pinning the given lessons, one step each.
func (e *env) course(title string, ls ...lesson) coursetest.Course {
	e.t.Helper()
	return coursetest.Publish(e.t, e.db, e.courseSpec(title, ls...))
}

func (e *env) courseSpec(title string, ls ...lesson) coursetest.Spec {
	var steps []coursetest.Step
	for i, l := range ls {
		steps = append(steps, coursetest.Step{LessonID: l.id, RevisionID: l.revision, Title: fmt.Sprintf("TEST étape %d", i+1)})
	}
	return coursetest.Spec{Admin: e.admin, Owner: e.author, Reviewer: e.reviewer, Publisher: e.publisher, SubjectID: e.subject,
		Title: title, Modules: []coursetest.Module{{Title: "TEST module", Steps: steps}}}
}

func courseReportSQL(l lesson, revision, courseRevision, reporter int64) string {
	return fmt.Sprintf(`INSERT INTO error_reports (lesson_id, revision_id, reporter_id, message, course_revision_id) VALUES (%d, %d, %d, 'Erreur.', %d)`,
		l.id, revision, reporter, courseRevision)
}

const courseGuardMsg = "pinned revision of a readable step"

func TestCreateInCourseNamesThePinnedRevision(t *testing.T) {
	e := newEnv(t)
	first := e.published(lesson{}, e.subject, "TEST Cours rapport")
	other := e.published(lesson{}, e.subject, "TEST Autre étape")
	spec := e.courseSpec("TEST Cours", first, other)
	c := coursetest.Publish(t, e.db, spec)
	// The standalone lesson moves to revision 2; the course keeps 1.
	second := e.published(lesson{id: first.id}, e.subject, "TEST Cours rapport v2")

	rc, err := e.reports.CreateInCourse(e.ctx, e.student, c.PublicID, c.RevisionNumber, first.publicID, 1, "Erreur dans le cours.")
	if err != nil {
		t.Fatal(err)
	}
	if rc.RevisionID != first.revision {
		t.Fatalf("stored revision %d, pinned %d", rc.RevisionID, first.revision)
	}
	rep, err := e.reports.Get(e.ctx, e.reviewer, rc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rep.CourseRevisionID == nil || *rep.CourseRevisionID != c.RevisionID || rep.CoursePublicID != c.PublicID ||
		rep.CourseRevisionNumber != c.RevisionNumber || !rep.Current || rep.RevisionNumber != 1 {
		t.Fatalf("report: %+v", rep)
	}
	// A standalone report names the current revision, as before.
	e.report(e.student, second, "Erreur seule.")

	before := e.reportsState()
	wantErr(t, "lesson revision of the standalone page", func() error {
		_, err := e.reports.CreateInCourse(e.ctx, e.student, c.PublicID, c.RevisionNumber, first.publicID, 2, "Erreur.")
		return err
	}(), reports.ErrRevisionChanged)
	wantErr(t, "unknown course revision", func() error {
		_, err := e.reports.CreateInCourse(e.ctx, e.student, c.PublicID, c.RevisionNumber+1, first.publicID, 1, "Erreur.")
		return err
	}(), reports.ErrRevisionChanged)
	notStep := e.published(lesson{}, e.subject, "TEST Hors cours")
	wantErr(t, "not a step", func() error {
		_, err := e.reports.CreateInCourse(e.ctx, e.student, c.PublicID, c.RevisionNumber, notStep.publicID, 1, "Erreur.")
		return err
	}(), reports.ErrNotFound)
	wantErr(t, "malformed", func() error {
		_, err := e.reports.CreateInCourse(e.ctx, e.student, "x", c.RevisionNumber, first.publicID, 1, "Erreur.")
		return err
	}(), reports.ErrNotFound)
	wantErr(t, "blank", func() error {
		_, err := e.reports.CreateInCourse(e.ctx, e.student, c.PublicID, c.RevisionNumber, first.publicID, 1, " ")
		return err
	}(), reports.ErrTextRequired)

	// A successor course revision: the old number is stale.
	spec.Title = "TEST Cours v2"
	c2 := coursetest.Republish(t, e.db, c, spec)
	wantErr(t, "stale course revision", func() error {
		_, err := e.reports.CreateInCourse(e.ctx, e.student, c.PublicID, c.RevisionNumber, first.publicID, 1, "Erreur.")
		return err
	}(), reports.ErrRevisionChanged)
	// The earlier report is no longer current where it was filed.
	if rep, err = e.reports.Get(e.ctx, e.reviewer, rc.ID); err != nil || rep.Current {
		t.Fatalf("old course report current: %+v %v", rep, err)
	}
	// Unreadable step, suspended course.
	if _, err := e.review.Withdraw(e.ctx, e.admin, other.revision, "TEST retrait", &other.revision); err != nil {
		t.Fatal(err)
	}
	wantErr(t, "withdrawn step", func() error {
		_, err := e.reports.CreateInCourse(e.ctx, e.student, c.PublicID, c2.RevisionNumber, other.publicID, 1, "Erreur.")
		return err
	}(), reports.ErrNotFound)
	coursetest.Suspend(t, e.db, e.admin, c2)
	wantErr(t, "suspended", func() error {
		_, err := e.reports.CreateInCourse(e.ctx, e.student, c.PublicID, c2.RevisionNumber, first.publicID, 1, "Erreur.")
		return err
	}(), reports.ErrNotFound)
	if after := e.reportsState(); after != before {
		t.Fatalf("refused reports changed the table:\n%s\n%s", before, after)
	}
}

func TestCourseReportInsertGuard(t *testing.T) {
	e := newEnv(t)
	first := e.published(lesson{}, e.subject, "TEST Garde cours")
	other := e.published(lesson{}, e.subject, "TEST Garde autre")
	spec := e.courseSpec("TEST Cours gardé", first, other)
	c := coursetest.Publish(t, e.db, spec)
	second := e.published(lesson{id: first.id}, e.subject, "TEST Garde cours v2")
	outside := e.published(lesson{}, e.subject, "TEST Hors cours")
	before := e.reportsState()

	// Not a pinned revision of a step of that course revision: the guard
	// (the composite foreign key would refuse it too).
	wantGuard(t, "standalone current revision", e.inTx(courseReportSQL(first, second.revision, c.RevisionID, e.student)), courseGuardMsg)
	wantGuard(t, "lesson outside the course", e.inTx(courseReportSQL(outside, outside.revision, c.RevisionID, e.student)), courseGuardMsg)
	wantGuard(t, "unknown course revision", e.inTx(courseReportSQL(first, first.revision, 999999, e.student)), courseGuardMsg)
	// Snapshot isolation other than READ COMMITTED is refused.
	for _, level := range []pgx.TxIsoLevel{pgx.RepeatableRead, pgx.Serializable} {
		err := func() error {
			tx, err := e.db.BeginTx(e.ctx, pgx.TxOptions{IsoLevel: level})
			if err != nil {
				return err
			}
			defer func() { _ = tx.Rollback(e.ctx) }()
			if _, err := tx.Exec(e.ctx, courseReportSQL(first, first.revision, c.RevisionID, e.student)); err != nil {
				return err
			}
			return tx.Commit(e.ctx)
		}()
		wantGuard(t, "isolation "+string(level), err, "READ COMMITTED")
	}
	disabled := e.user("disabled-course-student")
	if err := e.exec(`UPDATE users SET disabled_at = now() WHERE id = $1`, disabled); err != nil {
		t.Fatal(err)
	}
	wantGuard(t, "disabled reporter", e.inTx(courseReportSQL(first, first.revision, c.RevisionID, disabled)), "not an active account")
	if e.reportsState() != before {
		t.Fatal("refused inserts stored reports")
	}
	// The pinned revision of a readable step: accepted, although it is not
	// the lesson's current revision.
	if err := e.inTx(courseReportSQL(first, first.revision, c.RevisionID, e.student)); err != nil {
		t.Fatalf("valid course report: %v", err)
	}
	// The lesson path is unchanged: the pinned revision is refused there.
	wantGuard(t, "pinned revision without course", e.inTx(reportSQL(first, first.revision, e.student, "Erreur.")), "current published revision")

	// A superseded course revision.
	spec.Title = "TEST Cours gardé v2"
	c2 := coursetest.Republish(t, e.db, c, spec)
	wantGuard(t, "superseded course revision", e.inTx(courseReportSQL(first, first.revision, c.RevisionID, e.student)), courseGuardMsg)
	if err := e.inTx(courseReportSQL(first, first.revision, c2.RevisionID, e.student)); err != nil {
		t.Fatalf("current course revision: %v", err)
	}
	// Withdrawn pinned revision, unpublished lesson, archived lesson,
	// suspended course: refused.
	if _, err := e.review.Withdraw(e.ctx, e.admin, first.revision, "TEST retrait", &second.revision); err != nil {
		t.Fatal(err)
	}
	wantGuard(t, "withdrawn pinned revision", e.inTx(courseReportSQL(first, first.revision, c2.RevisionID, e.student)), courseGuardMsg)
	if _, err := e.review.Unpublish(e.ctx, e.admin, other.id, "TEST dépublication", other.revision); err != nil {
		t.Fatal(err)
	}
	wantGuard(t, "unpublished lesson", e.inTx(courseReportSQL(other, other.revision, c2.RevisionID, e.student)), courseGuardMsg)
	third := e.published(lesson{}, e.subject, "TEST Archivée")
	spec3 := e.courseSpec("TEST Cours archivé", third)
	c3 := coursetest.Publish(t, e.db, spec3)
	if err := e.exec(`UPDATE lessons SET archived_at = now() WHERE id = $1`, third.id); err != nil {
		t.Fatal(err)
	}
	wantGuard(t, "archived lesson", e.inTx(courseReportSQL(third, third.revision, c3.RevisionID, e.student)), courseGuardMsg)
	fourth := e.published(lesson{}, e.subject, "TEST Suspendu")
	c4 := coursetest.Publish(t, e.db, e.courseSpec("TEST Cours suspendu", fourth))
	coursetest.Suspend(t, e.db, e.admin, c4)
	wantGuard(t, "suspended course", e.inTx(courseReportSQL(fourth, fourth.revision, c4.RevisionID, e.student)), courseGuardMsg)

	// Reports stay immutable, including their course revision.
	var id int64
	if err := e.db.QueryRow(e.ctx, `SELECT id FROM error_reports WHERE course_revision_id = $1`, c2.RevisionID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	wantGuard(t, "change course revision", e.exec(`UPDATE error_reports SET course_revision_id = NULL WHERE id = $1`, id), "immutable")
}

// Under the least-privilege role, temporary tables named like the tables
// the guard reads cannot make it accept a report.
func TestCourseReportGuardIgnoresShadowTables(t *testing.T) {
	e := newEnv(t)
	first := e.published(lesson{}, e.subject, "TEST Ombre cours")
	spec := e.courseSpec("TEST Cours ombre", first)
	c := coursetest.Publish(t, e.db, spec)
	spec.Title = "TEST Cours ombre v2"
	c2 := coursetest.Republish(t, e.db, c, spec)
	asRole, qs := e.leastPrivilegeRole()
	q := func(sql string, args ...any) string {
		return strings.ReplaceAll(fmt.Sprintf(sql, args...), "S.", qs+".")
	}
	report := func(courseRev int64) string {
		return q(`INSERT INTO S.error_reports (lesson_id, revision_id, reporter_id, message, course_revision_id) VALUES (%d, %d, %d, 'Erreur.', %d)`,
			first.id, first.revision, e.student, courseRev)
	}
	before := e.reportsState()
	wantGuard(t, "old course revision via shadow courses", asRole(
		`CREATE TEMP TABLE courses (id bigint, published_revision_id bigint, archived_at timestamptz)`,
		fmt.Sprintf(`INSERT INTO pg_temp.courses VALUES (%d, %d, NULL)`, c.ID, c.RevisionID),
		report(c.RevisionID),
	), courseGuardMsg)
	wantGuard(t, "old course revision via shadow course_revisions", asRole(
		`CREATE TEMP TABLE course_revisions (id bigint, course_id bigint, status text, withdrawn_at timestamptz)`,
		fmt.Sprintf(`INSERT INTO pg_temp.course_revisions VALUES (%d, %d, 'approved', NULL)`, c2.RevisionID, c.ID),
		report(c.RevisionID),
	), courseGuardMsg)
	coursetest.Suspend(t, e.db, e.admin, c2)
	wantGuard(t, "suspended via shadow courses", asRole(
		`CREATE TEMP TABLE courses (id bigint, published_revision_id bigint, archived_at timestamptz)`,
		fmt.Sprintf(`INSERT INTO pg_temp.courses VALUES (%d, %d, NULL)`, c.ID, c2.RevisionID),
		report(c2.RevisionID),
	), courseGuardMsg)
	if after := e.reportsState(); after != before {
		t.Fatalf("shadowed statements changed reports:\n%s\n%s", before, after)
	}
	// The same role files a legitimate course report.
	fresh := e.published(lesson{}, e.subject, "TEST Ombre légitime")
	c3 := coursetest.Publish(t, e.db, e.courseSpec("TEST Cours légitime", fresh))
	if err := asRole(q(`INSERT INTO S.error_reports (lesson_id, revision_id, reporter_id, message, course_revision_id) VALUES (%d, %d, %d, 'Erreur.', %d)`,
		fresh.id, fresh.revision, e.student, c3.RevisionID)); err != nil {
		t.Fatalf("legitimate course report as the role: %v", err)
	}
}

// A course-context report waits for an in-flight lesson unpublication or
// course suspension (the guard share-locks the lesson, then the course) and
// is then refused.
func TestCourseReportWaitsForWithdrawalAndSuspension(t *testing.T) {
	e := newEnv(t)
	l := e.published(lesson{}, e.subject, "TEST Attente cours")
	c := e.course("TEST Cours attente", l)
	hold := func(stmts ...string) (pgx.Tx, uint32) {
		t.Helper()
		tx, err := e.db.Begin(e.ctx)
		if err != nil {
			t.Fatal(err)
		}
		var pid uint32
		if err := tx.QueryRow(e.ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
			t.Fatal(err)
		}
		for _, s := range stmts {
			if _, err := tx.Exec(e.ctx, s); err != nil {
				t.Fatal(err)
			}
		}
		return tx, pid
	}
	// Suspension in flight.
	tx, pid := hold(
		fmt.Sprintf(`INSERT INTO course_publication_audit (course_id, revision_id, previous_revision_id, actor_id, action, reason)
			VALUES (%d, NULL, %d, %d, 'suspend', 'TEST suspension')`, c.ID, c.RevisionID, e.admin),
		fmt.Sprintf(`UPDATE courses SET published_revision_id = NULL WHERE id = %d`, c.ID))
	done := make(chan error, 1)
	go func() { done <- e.inTx(courseReportSQL(l, l.revision, c.RevisionID, e.student)) }()
	e.waitBlockedBy(pid)
	if err := tx.Commit(e.ctx); err != nil {
		t.Fatal(err)
	}
	wantGuard(t, "report after the suspension committed", <-done, courseGuardMsg)

	// Lesson unpublication in flight (another course, same lesson pattern).
	l2 := e.published(lesson{}, e.subject, "TEST Attente leçon")
	c2 := e.course("TEST Cours attente 2", l2)
	tx, pid = hold(
		fmt.Sprintf(`INSERT INTO publication_audit (lesson_id, revision_id, previous_revision_id, actor_id, action, reason)
			VALUES (%d, NULL, %d, %d, 'unpublish', 'TEST unpublication')`, l2.id, l2.revision, e.admin),
		fmt.Sprintf(`UPDATE lessons SET published_revision_id = NULL WHERE id = %d`, l2.id))
	go func() { done <- e.inTx(courseReportSQL(l2, l2.revision, c2.RevisionID, e.student)) }()
	e.waitBlockedBy(pid)
	if err := tx.Commit(e.ctx); err != nil {
		t.Fatal(err)
	}
	wantGuard(t, "report after the unpublication committed", <-done, courseGuardMsg)
	if n := e.count(`SELECT count(*) FROM error_reports`); n != 0 {
		t.Fatalf("%d reports", n)
	}
}

// Concurrent withdrawals of the pinned revision and course-context reports:
// each report either commits before the withdrawal (the guard's lesson lock
// orders them) or is refused; nothing else fails, and once the withdrawal
// committed no report is accepted.
func TestCourseReportRacingWithdrawal(t *testing.T) {
	e := newEnv(t)
	dbtest.Warm(t, e.db, 4)
	for i := range 6 {
		l := e.published(lesson{}, e.subject, fmt.Sprintf("TEST Course %d", i))
		c := e.course(fmt.Sprintf("TEST Cours course %d", i), l)
		var wg sync.WaitGroup
		var repErr, wErr error
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, repErr = e.reports.CreateInCourse(e.ctx, e.student, c.PublicID, c.RevisionNumber, l.publicID, l.number, "Erreur.")
		}()
		go func() {
			defer wg.Done()
			_, wErr = e.review.Withdraw(e.ctx, e.admin, l.revision, "TEST retrait", &l.revision)
		}()
		wg.Wait()
		if wErr != nil {
			t.Fatalf("withdrawal %d: %v", i, wErr)
		}
		if repErr != nil && !errors.Is(repErr, reports.ErrNotFound) {
			t.Fatalf("report %d: %v", i, repErr)
		}
		// Once the withdrawal committed, no report is accepted.
		if _, err := e.reports.CreateInCourse(e.ctx, e.student, c.PublicID, c.RevisionNumber, l.publicID, l.number, "Erreur."); !errors.Is(err, reports.ErrNotFound) {
			t.Fatalf("report after the withdrawal %d: %v", i, err)
		}
		wantGuard(t, "direct report after the withdrawal", e.inTx(courseReportSQL(l, l.revision, c.RevisionID, e.student)), courseGuardMsg)
		stored := e.count(`SELECT count(*) FROM error_reports WHERE revision_id = $1`, l.revision)
		if (repErr == nil) != (stored == 1) {
			t.Fatalf("round %d: error %v, %d stored", i, repErr, stored)
		}
	}
}

func migrationsUpTo(t *testing.T, v string) fs.FS {
	t.Helper()
	out := fstest.MapFS{}
	entries, err := fs.ReadDir(migrations.FS, ".")
	if err != nil {
		t.Fatal(err)
	}
	for _, en := range entries {
		name := en.Name()
		if !strings.HasSuffix(name, ".sql") || name[:4] > v {
			continue
		}
		data, err := fs.ReadFile(migrations.FS, name)
		if err != nil {
			t.Fatal(err)
		}
		out[name] = &fstest.MapFile{Data: data}
	}
	return out
}

// Upgrading a populated B04 (0015) database keeps every existing ID and
// row: lessons, revisions, bookmarks, reports, courses, steps and audit.
// Existing reports get no course revision; course reports work afterwards.
func TestUpgradeFromB04KeepsExistingRows(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.EmptySchema(t)
	if _, err := database.Migrate(ctx, pool, migrationsUpTo(t, "0015")); err != nil {
		t.Fatal(err)
	}
	e := newEnvOn(t, pool)
	l := e.published(lesson{}, e.subject, "TEST Avant la mise à jour")
	c := e.course("TEST Cours avant", l)
	if err := e.exec(`INSERT INTO bookmarks (user_id, lesson_id) VALUES ($1, $2)`, e.student, l.id); err != nil {
		t.Fatal(err)
	}
	if err := e.inTx(reportSQL(l, l.revision, e.student, "Erreur avant.")); err != nil {
		t.Fatal(err)
	}
	snapshot := func() string {
		var s string
		if err := pool.QueryRow(ctx, `
			SELECT concat_ws('|',
			  (SELECT string_agg(id || ':' || email, ',' ORDER BY id) FROM users),
			  (SELECT string_agg(id || ':' || public_id || ':' || coalesce(published_revision_id::text, '-'), ',' ORDER BY id) FROM lessons),
			  (SELECT string_agg(id || ':' || lesson_id || ':' || status, ',' ORDER BY id) FROM lesson_revisions),
			  (SELECT string_agg(user_id || ':' || lesson_id || ':' || created_at, ',' ORDER BY user_id, lesson_id) FROM bookmarks),
			  (SELECT string_agg(id || ':' || lesson_id || ':' || revision_id || ':' || status || ':' || created_at, ',' ORDER BY id) FROM error_reports),
			  (SELECT string_agg(id || ':' || public_id || ':' || coalesce(published_revision_id::text, '-'), ',' ORDER BY id) FROM courses),
			  (SELECT string_agg(id || ':' || course_id || ':' || status, ',' ORDER BY id) FROM course_revisions),
			  (SELECT string_agg(course_revision_id || ':' || lesson_id || ':' || lesson_revision_id, ',' ORDER BY course_revision_id, lesson_id) FROM course_steps),
			  (SELECT string_agg(id || ':' || action, ',' ORDER BY id) FROM course_publication_audit),
			  (SELECT string_agg(id || ':' || action, ',' ORDER BY id) FROM publication_audit),
			  (SELECT string_agg(id || ':' || action, ',' ORDER BY id) FROM curriculum_audit))`).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	before := snapshot()
	applied, err := database.Migrate(ctx, pool, migrations.FS)
	if err != nil {
		t.Fatal(err)
	}
	if len(applied) == 0 || applied[len(applied)-1] != "0017_course_reading.sql" {
		t.Fatalf("applied %v", applied)
	}
	if after := snapshot(); after != before {
		t.Fatalf("upgrade changed existing rows:\nbefore %s\nafter  %s", before, after)
	}
	if n := e.count(`SELECT count(*) FROM error_reports WHERE course_revision_id IS NOT NULL`); n != 0 {
		t.Fatalf("%d reports gained a course revision", n)
	}
	if _, err := e.reports.CreateInCourse(ctx, e.student, c.PublicID, c.RevisionNumber, l.publicID, l.number, "Erreur après."); err != nil {
		t.Fatalf("course report after the upgrade: %v", err)
	}
	if _, err := e.reports.List(ctx, e.admin, reports.Filter{}); err != nil {
		t.Fatal(err)
	}
}
