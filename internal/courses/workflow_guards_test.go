package courses_test

import (
	"fmt"
	"testing"

	"github.com/hadden-me/learnoo/internal/courses"
	"github.com/hadden-me/learnoo/internal/permissions"
	"github.com/jackc/pgx/v5"
)

// Every guard of migrations/0015 by direct SQL under the least-privilege
// runtime role, with and without temporary shadow tables, and the
// legitimate direct-SQL workflow under the same role.
func TestWorkflowGuardsBindTheRuntimeRole(t *testing.T) {
	e := newEnv(t)
	if _, err := e.perms.GrantReviewer(e.ctx, testOperator, e.owner, e.math); err != nil {
		t.Fatal(err)
	}
	lessonAuthor := e.user("lesson-author", permissions.Author)
	if _, err := e.perms.GrantReviewer(e.ctx, testOperator, lessonAuthor, e.math); err != nil {
		t.Fatal(err)
	}
	theirs := e.publishedLesson(lessonAuthor, e.math, "fr", "Theirs")
	asRole := e.leastPrivilegeRole()

	// A course in review.
	r := e.readyCourse(theirs)
	C, R := itoa(r.course.ID), itoa(r.rev.ID)
	O, A, P, RV, LA := itoa(e.owner), itoa(e.adminID()), itoa(e.publisher), itoa(e.reviewer), itoa(lessonAuthor)
	submitSQL := []string{
		`INSERT INTO S.course_submission_audit (course_id, revision_id, actor_id) VALUES (` + C + `, ` + R + `, ` + O + `)`,
		`UPDATE S.course_revisions SET status = 'in_review', submitted_at = now(), updated_at = now(), version = version + 1 WHERE id = ` + R,
	}
	decide := func(reviewer, action string) []string {
		return []string{
			`INSERT INTO S.course_review_decisions (course_id, revision_id, reviewer_id, action, comment) VALUES (` + C + `, ` + R + `, ` + reviewer + `, '` + action + `', 'c')`,
			`UPDATE S.course_revisions SET status = '` + map[string]string{"approve": "approved", "request_changes": "changes_requested"}[action] + `', updated_at = now() WHERE id = ` + R,
		}
	}
	// Submission.
	wantGuard(t, "submit without audit", asRole(submitSQL[1]), "submission audit row")
	wantGuard(t, "audit without status change", asRole(submitSQL[0]), "not applied")
	wantGuard(t, "submit by another user", asRole(`INSERT INTO S.course_submission_audit (course_id, revision_id, actor_id) VALUES (`+C+`, `+R+`, `+itoa(e.other)+`)`),
		"only the author")
	wantGuard(t, "backdated submission", asRole(`INSERT INTO S.course_submission_audit (course_id, revision_id, actor_id, submitted_at) VALUES (`+C+`, `+R+`, `+O+`, now() - interval '1 hour')`),
		"its own transaction")
	wantGuard(t, "submit changing content", asRole(submitSQL[0],
		`UPDATE S.course_revisions SET status = 'in_review', submitted_at = now(), updated_at = now(), version = version + 1, title = 'x' WHERE id = `+R),
		"changes only its status")
	if err := asRole(submitSQL...); err != nil {
		t.Fatalf("legitimate submission: %v", err)
	}
	// Review: self-approval, author of an added lesson revision, wrong
	// status, missing decision, a shadow assignment table.
	wantGuard(t, "self approval", asRole(decide(O, "approve")...), "not an eligible reviewer")
	wantGuard(t, "lesson author approval", asRole(decide(LA, "approve")...), "not an eligible reviewer")
	wantGuard(t, "decision without status", asRole(decide(RV, "approve")[0]), "not applied")
	wantGuard(t, "status without decision", asRole(decide(RV, "approve")[1]), "recorded matching review decision")
	wantGuard(t, "mismatched decision", asRole(decide(RV, "request_changes")[0], decide(RV, "approve")[1]), "recorded matching review decision")
	wantGuard(t, "self approval via shadow revisions", asRole(append([]string{
		`CREATE TEMP TABLE course_revisions (id bigint, course_id bigint, author_id bigint, subject_id bigint, status text, withdrawn_at timestamptz, compare_revision_id bigint) ON COMMIT DROP`,
		`INSERT INTO pg_temp.course_revisions VALUES (` + R + `, ` + C + `, ` + RV + `, ` + itoa(e.math) + `, 'in_review', NULL, NULL)`,
	}, decide(O, "approve")...)...), "not an eligible reviewer")
	nobody := e.user("nobody")
	wantGuard(t, "unassigned via shadow assignments", asRole(append([]string{
		`CREATE TEMP TABLE reviewer_assignments (user_id bigint, subject_id bigint) ON COMMIT DROP`,
		`INSERT INTO pg_temp.reviewer_assignments VALUES (` + itoa(nobody) + `, ` + itoa(e.math) + `)`,
	}, decide(itoa(nobody), "approve")...)...), "not an eligible reviewer")
	if err := asRole(decide(RV, "approve")...); err != nil {
		t.Fatalf("legitimate approval: %v", err)
	}
	// Publication audit and pointer.
	pub := func(actor, action, rev, prev string) string {
		return `INSERT INTO S.course_publication_audit (course_id, revision_id, previous_revision_id, actor_id, action) VALUES (` +
			C + `, ` + rev + `, ` + prev + `, ` + actor + `, '` + action + `')`
	}
	setPtr := `UPDATE S.courses SET published_revision_id = ` + R + ` WHERE id = ` + C
	wantGuard(t, "pointer without audit", asRole(setPtr), "course publication audit row")
	wantGuard(t, "audit without pointer", asRole(pub(P, "publish", R, "NULL")), "not applied")
	wantGuard(t, "publish by an author", asRole(pub(O, "publish", R, "NULL"), setPtr), "may not publish")
	wantGuard(t, "restore never published", asRole(pub(P, "restore", R, "NULL"), setPtr), "never published")
	wantGuard(t, "wrong previous", asRole(pub(P, "publish", R, R)), "current published revision")
	wantGuard(t, "stale previous via shadow courses", asRole(
		`CREATE TEMP TABLE courses (id bigint, published_revision_id bigint, archived_at timestamptz) ON COMMIT DROP`,
		`INSERT INTO pg_temp.courses VALUES (`+C+`, `+R+`, NULL)`,
		pub(P, "publish", R, R+" - 0 + 0")), "")
	// The legitimate publication as the runtime role; a shadow courses
	// table claiming the course is archived is ignored.
	if err := asRole(
		`CREATE TEMP TABLE courses (id bigint, published_revision_id bigint, archived_at timestamptz) ON COMMIT DROP`,
		`INSERT INTO pg_temp.courses VALUES (`+C+`, NULL, now())`,
		pub(P, "publish", R, "NULL"), setPtr); err != nil {
		t.Fatalf("legitimate publication: %v", err)
	}
	if *e.pointer(r.course.ID) != r.rev.ID {
		t.Fatal("legitimate publication via direct SQL did not apply")
	}
	wantGuard(t, "suspend by a publisher", asRole(
		`INSERT INTO S.course_publication_audit (course_id, revision_id, previous_revision_id, actor_id, action, reason) VALUES (`+C+`, NULL, `+R+`, `+P+`, 'suspend', 'x')`,
		`UPDATE S.courses SET published_revision_id = NULL WHERE id = `+C), "may not suspend")
	wantGuard(t, "withdraw the published revision", asRole(
		`INSERT INTO S.course_revision_withdrawals (course_id, revision_id, actor_id, reason) VALUES (`+C+`, `+R+`, `+A+`, 'x')`), "suspend the course instead")
	wantGuard(t, "withdrawn_at directly", asRole(`UPDATE S.course_revisions SET withdrawn_at = now() WHERE id = `+R), "audited workflow")
	wantGuard(t, "archive without row", asRole(`UPDATE S.courses SET archived_at = now(), published_revision_id = NULL WHERE id = `+C), "audited archive")
	wantGuard(t, "lifecycle archive of a published course", asRole(
		`INSERT INTO S.course_lifecycle_audit (course_id, actor_id, action, reason) VALUES (`+C+`, `+A+`, 'archive', 'x')`), "archived or published")
	wantGuard(t, "transfer without row", asRole(`UPDATE S.courses SET owner_id = `+itoa(e.other)+` WHERE id = `+C), "audited transfer")
	wantGuard(t, "transfer to a reviewer", asRole(
		`INSERT INTO S.course_lifecycle_audit (course_id, actor_id, action, reason, from_owner_id, to_owner_id) VALUES (`+C+`, `+A+`, 'transfer', 'x', `+O+`, `+RV+`)`), "not an active author")
	wantGuard(t, "archive request by another author", asRole(
		`INSERT INTO S.course_lifecycle_audit (course_id, actor_id, action, reason) VALUES (`+C+`, `+itoa(e.other)+`, 'archive_request', 'x')`), "only the owner")
	wantGuard(t, "dependency_count", asRole(`UPDATE S.courses SET dependency_count = dependency_count + 1 WHERE id = `+C), "recorded dependency events")
	wantGuard(t, "dependency event", asRole(fmt.Sprintf(
		`INSERT INTO S.course_dependency_events (course_id, course_revision_id, lesson_id, lesson_revision_id, cause) VALUES (%s, %s, %d, %d, 'lesson_archived')`,
		C, R, theirs.LessonID, theirs.ID)), "only by their triggers")
	// The owner-run functions cannot be attached by the runtime role.
	for _, fn := range []string{"course_dependency_effects", "course_revision_withdrawals_apply", "courses_close_published_candidate"} {
		err := asRole(`CREATE TEMP TABLE lessons (id bigint) ON COMMIT DROP`,
			`CREATE TRIGGER t AFTER INSERT ON pg_temp.lessons FOR EACH ROW EXECUTE FUNCTION S.`+fn+`()`)
		wantSQLState(t, "attach "+fn, err, "42501")
	}
	// Legitimate suspension, withdrawal of an older revision, and a
	// lesson withdrawal recording its dependency event, as the runtime role.
	if err := asRole(
		`INSERT INTO S.course_publication_audit (course_id, revision_id, previous_revision_id, actor_id, action, reason) VALUES (`+C+`, NULL, `+R+`, `+A+`, 'suspend', 'x')`,
		`UPDATE S.courses SET published_revision_id = NULL WHERE id = `+C); err != nil {
		t.Fatalf("legitimate suspension: %v", err)
	}
	if err := asRole(`INSERT INTO S.course_revision_withdrawals (course_id, revision_id, actor_id, reason) VALUES (` + C + `, ` + R + `, ` + A + `, 'x')`); err != nil {
		t.Fatalf("legitimate withdrawal: %v", err)
	}
	var withdrawn bool
	if err := e.db.QueryRow(e.ctx, `SELECT withdrawn_at = (SELECT occurred_at FROM course_revision_withdrawals WHERE revision_id = $1)
		FROM course_revisions WHERE id = $1`, r.rev.ID).Scan(&withdrawn); err != nil || !withdrawn {
		t.Fatalf("withdrawn_at: %v %v", withdrawn, err)
	}
	wantGuard(t, "restore withdrawn", asRole(pub(A, "restore", R, "NULL"), setPtr), "withdrawn")

	other := e.publishedLesson(e.other, e.math, "fr", "Other")
	r2 := e.publishedCourse(other)
	if err := asRole(fmt.Sprintf(`INSERT INTO S.revision_withdrawals (lesson_id, revision_id, actor_id, reason) VALUES (%d, %d, %s, 'x')`, other.LessonID, other.ID, A),
		fmt.Sprintf(`INSERT INTO S.publication_audit (lesson_id, revision_id, previous_revision_id, actor_id, action, reason) VALUES (%d, NULL, %d, %s, 'unpublish', 'x')`, other.LessonID, other.ID, A),
		fmt.Sprintf(`UPDATE S.lessons SET published_revision_id = NULL WHERE id = %d`, other.LessonID)); err != nil {
		t.Fatalf("lesson withdrawal as the runtime role: %v", err)
	}
	if n := e.count(`SELECT dependency_count FROM courses WHERE id = $1`, r2.course.ID); n != 2 {
		t.Fatalf("dependency_count %d", n)
	}
	if _, err := e.store.RequestArchive(e.ctx, e.owner, r.course.ID, "Simulated request."); err != nil {
		t.Fatal(err)
	}
	// Every audit table now has rows: none can be changed or removed.
	for _, table := range []string{"course_submission_audit", "course_review_decisions", "course_publication_audit", "course_lifecycle_audit",
		"course_revision_withdrawals", "course_dependency_events"} {
		wantGuard(t, "delete "+table, asRole(`DELETE FROM S.`+table), "append-only")
		wantGuard(t, "update "+table, asRole(`UPDATE S.`+table+` SET course_id = course_id`), "append-only")
		wantGuard(t, "truncate "+table, asRole(`TRUNCATE S.`+table+` CASCADE`), "")
	}
}

// Guards that evaluate eligibility refuse outside READ COMMITTED,
// including B03's step guard for another author's lesson.
func TestEligibilityGuardsRequireReadCommitted(t *testing.T) {
	e := newEnv(t)
	l := e.publishedLesson(e.other, e.math, "fr", "L")
	r := e.readyCourse(l)
	key := r.rev.Modules[0].Key
	foreign := e.publishedLesson(e.other, e.math, "fr", "Foreign")
	for _, tc := range []struct {
		name string
		sql  []string
	}{
		{"step", []string{fmt.Sprintf(`INSERT INTO course_steps (course_revision_id, module_key, position, title, lesson_id, subject_id, language, lesson_revision_id)
			VALUES (%d, '%s', 9, 's', %d, %d, 'fr', %d)`, r.rev.ID, key, foreign.LessonID, e.math, foreign.ID)}},
		{"submit", []string{fmt.Sprintf(`INSERT INTO course_submission_audit (course_id, revision_id, actor_id) VALUES (%d, %d, %d)`, r.course.ID, r.rev.ID, e.owner)}},
	} {
		for _, iso := range []pgx.TxIsoLevel{pgx.RepeatableRead, pgx.Serializable} {
			tx, err := e.db.BeginTx(e.ctx, pgx.TxOptions{IsoLevel: iso})
			if err != nil {
				t.Fatal(err)
			}
			for _, s := range tc.sql {
				if _, err = tx.Exec(e.ctx, s); err != nil {
					break
				}
			}
			_ = tx.Rollback(e.ctx)
			wantGuard(t, tc.name+" "+string(iso), err, "READ COMMITTED")
		}
	}
	sub := e.submit(r.rev)
	e.approveCourse(sub.ID)
	tx, err := e.db.BeginTx(e.ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	if err != nil {
		t.Fatal(err)
	}
	_, err = tx.Exec(e.ctx, `INSERT INTO course_publication_audit (course_id, revision_id, actor_id, action) VALUES ($1, $2, $3, 'publish')`,
		r.course.ID, sub.ID, e.publisher)
	_ = tx.Rollback(e.ctx)
	wantGuard(t, "publish at REPEATABLE READ", err, "READ COMMITTED")
	// And the store, which uses READ COMMITTED, publishes.
	if _, err := e.store.Publish(e.ctx, e.publisher, sub.ID, nil); err != nil {
		t.Fatal(err)
	}
	_ = courses.ErrNotFound
}
