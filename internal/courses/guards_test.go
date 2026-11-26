package courses_test

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hadden-me/learnoo/internal/courses"
	"github.com/hadden-me/learnoo/internal/lessons"
	"github.com/jackc/pgx/v5"
)

// Every guard of migrations/0014_courses.sql, by direct SQL under the
// least-privilege runtime role.
func TestGuardsBindTheRuntimeRole(t *testing.T) {
	e := newEnv(t)
	ch := e.chapter(e.math, "ch-01")
	p := e.edition(e.math, []int64{ch}, true)
	draftEd := e.edition(e.math, []int64{ch}, false)
	own := e.lessonDraft(e.owner, e.math, "fr", "Own")
	physics := e.lessonDraft(e.owner, e.physics, "fr", "Physics")
	mine, theirs := e.media(e.owner), e.media(e.other)
	asRole := e.leastPrivilegeRole()

	c, rev := e.course(e.math, "fr")
	saved := e.mustSave(rev, courses.Draft{AssignmentIDs: []int64{p.assignment},
		Modules: []courses.ModuleInput{{Title: "m", ChapterID: &ch, Steps: []courses.StepInput{step(own, "s")}}}})
	C, R, O := itoa(c.ID), itoa(rev.ID), itoa(e.owner)
	key := saved.Modules[0].Key
	// A second course whose first revision is submitted (simulated: B03 has
	// no submission path).
	sub, subRev := e.course(e.math, "fr")
	subSaved := e.mustSave(subRev, courses.Draft{AssignmentIDs: []int64{p.assignment},
		Modules: []courses.ModuleInput{{Title: "m", ChapterID: &ch, Steps: []courses.StepInput{step(own, "s")}}}})
	e.forceStatus(subRev.ID, `status = 'in_review', submitted_at = now()`)
	SC, SR, SK := itoa(sub.ID), itoa(subRev.ID), subSaved.Modules[0].Key

	insertCourse := func(owner, creator int64, extra string) string {
		return fmt.Sprintf(`INSERT INTO S.courses (owner_id, subject_id, language, created_by%s) VALUES (%d, %d, 'fr', %d%s)`,
			strings.Split(extra+"|", "|")[0], owner, e.math, creator, strings.Split(extra+"|", "|")[1])
	}
	insertRevision := func(cols, vals string) string {
		return `INSERT INTO S.course_revisions (course_id, subject_id, language, revision_number, author_id` + cols + `)
			VALUES (` + C + `, ` + itoa(e.math) + `, 'fr', 99, ` + O + vals + `)`
	}
	for _, tc := range []struct{ sql, msg string }{
		// courses
		{insertCourse(e.owner, e.other, ""), "created by its owner"},
		{insertCourse(e.owner, e.owner, ", archived_at|, now()"), "created by its owner"},
		{insertCourse(e.owner, e.owner, ", last_revision_number|, 3"), "created by its owner"},
		{insertCourse(e.owner, e.owner, ", dependency_count|, 1"), "created by its owner"},
		{insertCourse(e.owner, e.owner, ", created_at|, now() - interval '1 day'"), "created by its owner"},
		{`UPDATE S.courses SET public_id = gen_random_uuid() WHERE id = ` + C, "identity is immutable"},
		{`UPDATE S.courses SET created_by = ` + itoa(e.other) + ` WHERE id = ` + C, "identity is immutable"},
		{`UPDATE S.courses SET owner_id = ` + itoa(e.other) + ` WHERE id = ` + C, "owner, publication, archive"},
		{`UPDATE S.courses SET archived_at = now() WHERE id = ` + C, "owner, publication, archive"},
		{`UPDATE S.courses SET dependency_count = 1 WHERE id = ` + C, "owner, publication, archive"},
		{`UPDATE S.courses SET published_revision_id = ` + R + ` WHERE id = ` + C, "owner, publication, archive"},
		{`UPDATE S.courses SET last_revision_number = 0 WHERE id = ` + C, "never reused"},
		{`DELETE FROM S.courses WHERE id = ` + SC, "submitted history"},
		{`TRUNCATE S.courses CASCADE`, "cannot be truncated"},
		{`TRUNCATE S.course_revisions CASCADE`, "cannot be truncated"},
		{`TRUNCATE S.course_revision_assignments`, "cannot be truncated"},
		{`TRUNCATE S.course_modules CASCADE`, "cannot be truncated"},
		{`TRUNCATE S.course_steps`, "cannot be truncated"},
		// course revisions: insert
		{insertRevision(`, status, submitted_at`, `, 'in_review', now()`), "open draft"},
		{insertRevision(`, version`, `, 2`), "open draft"},
		{insertRevision(`, created_at`, `, now() - interval '1 day'`), "open draft"},
		{insertRevision(`, kind, compare_revision_id`, `, 'amendment', `+SR), "amendment"},
		{insertRevision(`, base_revision_id`, `, `+R), "base_revision_id"},
		{insertRevision(`, compare_revision_id`, `, `+R), "compare_revision_id"},
		// course revisions: update and delete
		{`UPDATE S.course_revisions SET author_id = ` + itoa(e.other) + `, version = version + 1 WHERE id = ` + R, "authored by the course's owner"},
		{`UPDATE S.course_revisions SET revision_number = 5, version = version + 1 WHERE id = ` + R, "identity is immutable"},
		{`UPDATE S.course_revisions SET base_revision_id = ` + R + `, version = version + 1 WHERE id = ` + R, "identity is immutable"},
		{`UPDATE S.course_revisions SET title = 'x' WHERE id = ` + R, "increments its version"},
		{`UPDATE S.course_revisions SET status = 'in_review', submitted_at = now(), version = version + 1 WHERE id = ` + R, "cannot be submitted or decided yet"},
		{`UPDATE S.course_revisions SET status = 'approved' WHERE id = ` + SR, "cannot be submitted or decided yet"},
		{`UPDATE S.course_revisions SET title = 'x' WHERE id = ` + SR, "submitted and immutable"},
		{`UPDATE S.course_revisions SET withdrawn_at = now() WHERE id = ` + SR, "set only by its audited workflow"},
		{`UPDATE S.course_revisions SET candidate_closed_at = now() WHERE id = ` + R, "set only by its audited workflow"},
		{`DELETE FROM S.course_revisions WHERE id = ` + SR, "cannot be deleted"},
		{`UPDATE S.course_revisions SET cover_media_id = ` + itoa(theirs) + `, cover_alt = 'a', version = version + 1 WHERE id = ` + R, "cover must be an image"},
		// children of a submitted revision
		{`INSERT INTO S.course_modules (course_revision_id, subject_id, language, module_key, position, title)
		  VALUES (` + SR + `, ` + itoa(e.math) + `, 'fr', gen_random_uuid(), 9, 'x')`, "are immutable"},
		{`UPDATE S.course_modules SET title = 'x' WHERE course_revision_id = ` + SR, "are immutable"},
		{`DELETE FROM S.course_steps WHERE course_revision_id = ` + SR, "are immutable"},
		{`DELETE FROM S.course_revision_assignments WHERE course_revision_id = ` + SR, "are immutable"},
		{`UPDATE S.course_steps SET title = 'x' WHERE course_revision_id = ` + SR + ` AND module_key = '` + SK + `'`, "are immutable"},
		// placements of a draft edition
		{`INSERT INTO S.course_revision_assignments (course_revision_id, subject_id, assignment_id) VALUES (` + R + `, ` + itoa(e.math) + `, ` + itoa(draftEd.assignment) + `)`, "left draft"},
	} {
		wantGuard(t, tc.sql, asRole(tc.sql), tc.msg)
	}

	// The author-equals-owner guard: a revision for another user's course.
	otherCourse, otherRev := e.courseOf(e.other)
	if err := e.store.DiscardDraft(e.ctx, e.other, otherRev.ID, otherRev.Version); err != nil {
		t.Fatal(err)
	}
	wantGuard(t, "author is not the owner", asRole(`INSERT INTO S.course_revisions (course_id, subject_id, language, revision_number, author_id)
		VALUES (`+itoa(otherCourse.ID)+`, `+itoa(e.math)+`, 'fr', 5, `+O+`)`), "authored by the course's owner")
	// A discarded number is never reused.
	wantGuard(t, "reused number", asRole(`INSERT INTO S.course_revisions (course_id, subject_id, language, revision_number, author_id)
		VALUES (`+itoa(otherCourse.ID)+`, `+itoa(e.math)+`, 'fr', 1, `+itoa(e.other)+`)`), "already used")

	// Attachability: another author's draft (or any non-eligible revision)
	// cannot be pinned by direct SQL, so it can never block that author's
	// discard; the owner's own draft can.
	foreignDraft := e.lessonDraft(e.other, e.math, "fr", "Foreign draft")
	pin := func(l int64, lr int64, pos int) string {
		return fmt.Sprintf(`INSERT INTO S.course_steps (course_revision_id, module_key, position, title, lesson_id, subject_id, language, lesson_revision_id)
			VALUES (%s, '%s', %d, 's', %d, %d, 'fr', %d)`, R, key, pos, l, e.math, lr)
	}
	wantGuard(t, "pin another author's draft", asRole(pin(foreignDraft.LessonID, foreignDraft.ID, 5)), "is not eligible")
	wantGuard(t, "move a step to another author's draft", asRole(fmt.Sprintf(
		`UPDATE S.course_steps SET lesson_id = %d, lesson_revision_id = %d WHERE course_revision_id = %s`, foreignDraft.LessonID, foreignDraft.ID, R)), "is not eligible")
	if err := e.lessons.DiscardDraft(e.ctx, e.other, foreignDraft.ID, foreignDraft.Version); err != nil {
		t.Fatalf("the other author's discard: %v", err)
	}
	// last_revision_number only ever becomes the highest number used.
	wantGuard(t, "jump last_revision_number", asRole(`UPDATE S.courses SET last_revision_number = last_revision_number + 5 WHERE id = `+C),
		"highest revision number")

	// Constraints.
	for _, tc := range []struct{ sql, code string }{
		{insertRevision(``, ``), "23505"}, // second active candidate
		{`UPDATE S.course_revisions SET title = E'a\nb', version = version + 1 WHERE id = ` + R, "23514"},
		{`UPDATE S.course_revisions SET summary = E'a‮b', version = version + 1 WHERE id = ` + R, "23514"},
		{`UPDATE S.course_revisions SET title = ' padded', version = version + 1 WHERE id = ` + R, "23514"},
		{`UPDATE S.course_revisions SET title = repeat('a', 201), version = version + 1 WHERE id = ` + R, "23514"},
		{`UPDATE S.course_revisions SET cover_media_id = ` + itoa(mine) + `, version = version + 1 WHERE id = ` + R, "23514"},
		{`UPDATE S.course_revisions SET cover_alt = 'a', version = version + 1 WHERE id = ` + R, "23514"},
		{`UPDATE S.course_modules SET position = 51 WHERE course_revision_id = ` + R, "23514"},
		{`UPDATE S.course_modules SET title = '' WHERE course_revision_id = ` + R, "23514"},
		{`INSERT INTO S.course_steps (course_revision_id, module_key, position, title, lesson_id, subject_id, language, lesson_revision_id)
		  VALUES (` + R + `, '` + key + `', 2, 's', ` + itoa(physics.LessonID) + `, ` + itoa(e.math) + `, 'fr', ` + itoa(physics.ID) + `)`, "23503"},
		{`INSERT INTO S.course_steps (course_revision_id, module_key, position, title, lesson_id, subject_id, language, lesson_revision_id)
		  VALUES (` + R + `, '` + key + `', 2, 's', ` + itoa(own.LessonID) + `, ` + itoa(e.math) + `, 'fr', ` + itoa(own.ID) + `)`, "23505"},
		{`INSERT INTO S.course_modules (course_revision_id, subject_id, language, module_key, position, title, chapter_id)
		  VALUES (` + R + `, ` + itoa(e.math) + `, 'fr', gen_random_uuid(), 2, 'x', ` + itoa(e.chapter(e.physics, "ch-01")) + `)`, "23503"},
		{`INSERT INTO S.course_modules (course_revision_id, subject_id, language, module_key, position, title)
		  VALUES (` + R + `, ` + itoa(e.math) + `, 'ar', gen_random_uuid(), 2, 'x')`, "23503"},
	} {
		wantSQLState(t, tc.sql, asRole(tc.sql), tc.code)
	}

	// Accepted direct writes: an owner's cover with alt, a module edit of a
	// draft, a course created by its owner.
	if err := asRole(`UPDATE S.course_revisions SET cover_media_id = `+itoa(mine)+`, cover_alt = 'TEST alt', version = version + 1 WHERE id = `+R,
		`UPDATE S.course_modules SET title = 'renamed' WHERE course_revision_id = `+R,
		insertCourse(e.owner, e.owner, "")); err != nil {
		t.Fatalf("legitimate direct writes: %v", err)
	}
	// Deleting a never-submitted course cascades to its draft and children.
	if err := asRole(`DELETE FROM S.courses WHERE id = ` + C); err != nil {
		t.Fatalf("delete never-submitted course: %v", err)
	}
	if n := e.count(`SELECT count(*) FROM course_revisions WHERE course_id = $1`, c.ID); n != 0 {
		t.Fatalf("%d revisions left", n)
	}
	// The functions are callable by the role.
	if err := asRole(`SELECT S.course_step_revision_eligible(1, 1), S.course_revision_placement_valid(` + SR + `, 'submit')`); err != nil {
		t.Fatal(err)
	}
}

func (e *env) courseOf(owner int64) (courses.Course, courses.Revision) {
	e.t.Helper()
	c, rev, err := e.store.CreateCourse(e.ctx, owner, e.math, "fr")
	if err != nil {
		e.t.Fatal(err)
	}
	return c, rev
}

// At most 200 steps per revision, counted by the step guard.
func TestStepLimitGuard(t *testing.T) {
	e := newEnv(t)
	own := e.lessonDraft(e.owner, e.math, "fr", "Own")
	_, rev := e.course(e.math, "fr")
	saved := e.mustSave(rev, courses.Draft{Modules: []courses.ModuleInput{{Title: "m"}, {Title: "n"}}})
	// 200 placeholder steps, inserted with triggers and foreign keys off
	// (setup only).
	tx, err := e.db.Begin(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(e.ctx) }()
	for _, sql := range []string{
		`SET LOCAL session_replication_role = replica`,
		fmt.Sprintf(`INSERT INTO course_steps (course_revision_id, module_key, position, title, lesson_id, subject_id, language, lesson_revision_id)
			SELECT %d, '%s', g, 's', -g, %d, 'fr', -g FROM generate_series(1, 200) g`, rev.ID, saved.Modules[0].Key, e.math),
	} {
		if _, err := tx.Exec(e.ctx, sql); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(e.ctx); err != nil {
		t.Fatal(err)
	}
	asRole := e.leastPrivilegeRole()
	wantGuard(t, "201st step", asRole(fmt.Sprintf(`INSERT INTO S.course_steps
		(course_revision_id, module_key, position, title, lesson_id, subject_id, language, lesson_revision_id)
		VALUES (%d, '%s', 1, 's', %d, %d, 'fr', %d)`, rev.ID, saved.Modules[1].Key, own.LessonID, e.math, own.ID)), "at most 200 steps")
	// Moving a step of another draft into the full revision is counted too.
	_, other := e.course(e.math, "fr")
	e.mustSave(other, courses.Draft{Modules: []courses.ModuleInput{{Title: "m", Steps: []courses.StepInput{step(own, "s")}}}})
	wantGuard(t, "move a step into a full revision", asRole(fmt.Sprintf(`UPDATE S.course_steps SET course_revision_id = %d, module_key = '%s'
		WHERE course_revision_id = %d`, rev.ID, saved.Modules[1].Key, other.ID)), "at most 200 steps")
}

// Temporary tables of the calling role cannot shadow what the guards and
// the shared functions read.
func TestGuardsIgnoreTemporaryShadowTables(t *testing.T) {
	e := newEnv(t)
	ch := e.chapter(e.math, "ch-01")
	p := e.edition(e.math, []int64{ch}, true)
	draftEd := e.edition(e.math, []int64{ch}, false)
	approvedOnly := e.lessonDraft(e.other, e.math, "fr", "Approved, never published")
	e.approve(approvedOnly)
	theirs := e.media(e.other)
	asRole := e.leastPrivilegeRole()

	_, rev := e.course(e.math, "fr")
	_, subRev := e.course(e.math, "fr")
	e.forceStatus(subRev.ID, `status = 'in_review', submitted_at = now()`)
	R, SR := itoa(rev.ID), itoa(subRev.ID)

	wantGuard(t, "module on a submitted revision via shadow revisions", asRole(
		`CREATE TEMP TABLE course_revisions (id bigint, status text) ON COMMIT DROP`,
		`INSERT INTO pg_temp.course_revisions VALUES (`+SR+`, 'draft')`,
		`INSERT INTO S.course_modules (course_revision_id, subject_id, language, module_key, position, title)
		 VALUES (`+SR+`, `+itoa(e.math)+`, 'fr', gen_random_uuid(), 1, 'x')`), "are immutable")
	otherCourse, otherRev := e.courseOf(e.other)
	if err := e.store.DiscardDraft(e.ctx, e.other, otherRev.ID, otherRev.Version); err != nil {
		t.Fatal(err)
	}
	wantGuard(t, "foreign author via shadow courses", asRole(
		`CREATE TEMP TABLE courses (id bigint, owner_id bigint, published_revision_id bigint) ON COMMIT DROP`,
		`INSERT INTO pg_temp.courses VALUES (`+itoa(otherCourse.ID)+`, `+itoa(e.owner)+`, NULL)`,
		`INSERT INTO S.course_revisions (course_id, subject_id, language, revision_number, author_id)
		 VALUES (`+itoa(otherCourse.ID)+`, `+itoa(e.math)+`, 'fr', 7, `+itoa(e.owner)+`)`), "authored by the course's owner")
	wantGuard(t, "forged base via shadow courses", asRole(
		`CREATE TEMP TABLE courses (id bigint, owner_id bigint, published_revision_id bigint) ON COMMIT DROP`,
		`INSERT INTO pg_temp.courses VALUES (`+itoa(otherCourse.ID)+`, `+itoa(e.other)+`, `+SR+`)`,
		`INSERT INTO S.course_revisions (course_id, subject_id, language, revision_number, author_id, base_revision_id)
		 VALUES (`+itoa(otherCourse.ID)+`, `+itoa(e.math)+`, 'fr', 7, `+itoa(e.other)+`, `+SR+`)`), "base_revision_id")
	foreignDraft := e.lessonDraft(e.other, e.math, "fr", "Foreign draft")
	keyed := e.mustSave(rev, courses.Draft{Modules: []courses.ModuleInput{{Title: "m"}}})
	wantGuard(t, "foreign pin via shadow lessons", asRole(
		`CREATE TEMP TABLE lessons (id bigint, created_by bigint) ON COMMIT DROP`,
		`INSERT INTO pg_temp.lessons VALUES (`+itoa(foreignDraft.LessonID)+`, `+itoa(e.owner)+`)`,
		fmt.Sprintf(`INSERT INTO S.course_steps (course_revision_id, module_key, position, title, lesson_id, subject_id, language, lesson_revision_id)
			VALUES (%s, '%s', 1, 's', %d, %d, 'fr', %d)`, R, keyed.Modules[0].Key, foreignDraft.LessonID, e.math, foreignDraft.ID)), "is not eligible")
	wantGuard(t, "cover via shadow uploads", asRole(
		`CREATE TEMP TABLE media_uploads (user_id bigint, media_asset_id bigint) ON COMMIT DROP`,
		`INSERT INTO pg_temp.media_uploads VALUES (`+itoa(e.owner)+`, `+itoa(theirs)+`)`,
		`UPDATE S.course_revisions SET cover_media_id = `+itoa(theirs)+`, cover_alt = 'a', version = version + 1 WHERE id = `+R), "cover must be an image")
	wantGuard(t, "draft edition via shadow editions", asRole(
		`CREATE TEMP TABLE curriculum_editions (id bigint, state text) ON COMMIT DROP`,
		`INSERT INTO pg_temp.curriculum_editions VALUES (`+itoa(draftEd.edition.ID)+`, 'current')`,
		`INSERT INTO S.course_revision_assignments (course_revision_id, subject_id, assignment_id) VALUES (`+R+`, `+itoa(e.math)+`, `+itoa(draftEd.assignment)+`)`), "left draft")
	wantGuard(t, "delete with history via shadow revisions", asRole(
		`CREATE TEMP TABLE course_revisions (id bigint, course_id bigint, status text) ON COMMIT DROP`,
		`DELETE FROM S.courses WHERE id = (SELECT course_id FROM S.course_revisions WHERE id = `+SR+`)`), "submitted history")

	// The shared functions are bound at creation: a shadow publication_audit
	// does not make a never-published revision eligible, nor a shadow edition
	// make a draft assignment selectable.
	var eligible, valid bool
	err := e.asRoleQuery(asRole, []string{
		`CREATE TEMP TABLE publication_audit (revision_id bigint, action text) ON COMMIT DROP`,
		`INSERT INTO pg_temp.publication_audit VALUES (` + itoa(approvedOnly.ID) + `, 'publish')`,
		`CREATE TEMP TABLE lessons (id bigint, published_revision_id bigint, archived_at timestamptz) ON COMMIT DROP`,
		`INSERT INTO pg_temp.lessons VALUES (` + itoa(approvedOnly.LessonID) + `, ` + itoa(approvedOnly.ID) + `, NULL)`,
	}, `SELECT S.course_step_revision_eligible(`+itoa(approvedOnly.LessonID)+`, `+itoa(approvedOnly.ID)+`)`, &eligible)
	if err != nil || eligible {
		t.Fatalf("eligibility via shadow: %v %v", eligible, err)
	}
	// A draft whose module chapter is outside its assignments stays invalid
	// with shadow assignment chapters.
	outside := e.chapter(e.math, "ch-02")
	e.forceChildren(rev.ID, p.assignment, nil, &outside)
	if e.placementValid(rev.ID, "save") {
		t.Fatal("setup: placement should be invalid")
	}
	err = e.asRoleQuery(asRole, []string{
		`CREATE TEMP TABLE curriculum_assignment_chapters (assignment_id bigint, chapter_id bigint) ON COMMIT DROP`,
		`INSERT INTO pg_temp.curriculum_assignment_chapters VALUES (` + itoa(p.assignment) + `, ` + itoa(outside) + `)`,
	}, `SELECT S.course_revision_placement_valid(`+R+`, 'save')`, &valid)
	if err != nil || valid {
		t.Fatalf("placement via shadow: %v %v", valid, err)
	}
}

// asRoleQuery evaluates a boolean SQL expression ("SELECT expr") as the
// role after the setup statements, in the same transaction.
func (e *env) asRoleQuery(asRole func(...string) error, setup []string, query string, dst *bool) error {
	expr := strings.TrimPrefix(query, "SELECT ")
	err := asRole(append(slices.Clone(setup), `DO $$ BEGIN IF NOT (`+expr+`) THEN RAISE EXCEPTION 'expression is false'; END IF; END $$`)...)
	*dst = err == nil
	if err != nil && !strings.Contains(err.Error(), "expression is false") {
		return err
	}
	return nil
}

// forceChildren replaces a revision's placements and modules with triggers
// off (setup only): one assignment (or none) and one module with chapter.
func (e *env) forceChildren(revisionID int64, assignment int64, key *string, chapter *int64) string {
	e.t.Helper()
	k := "00000000-0000-4000-8000-00000000000a"
	if key != nil {
		k = *key
	}
	tx, err := e.db.Begin(e.ctx)
	if err != nil {
		e.t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(e.ctx) }()
	stmts := []string{
		`SET LOCAL session_replication_role = replica`,
		fmt.Sprintf(`DELETE FROM course_steps WHERE course_revision_id = %d`, revisionID),
		fmt.Sprintf(`DELETE FROM course_modules WHERE course_revision_id = %d`, revisionID),
		fmt.Sprintf(`DELETE FROM course_revision_assignments WHERE course_revision_id = %d`, revisionID),
	}
	if assignment != 0 {
		stmts = append(stmts, fmt.Sprintf(`INSERT INTO course_revision_assignments SELECT %d, subject_id, %d FROM course_revisions WHERE id = %d`,
			revisionID, assignment, revisionID))
	}
	if chapter != nil {
		stmts = append(stmts, fmt.Sprintf(`INSERT INTO course_modules (course_revision_id, subject_id, language, module_key, position, title, chapter_id)
			SELECT id, subject_id, language, '%s', 1, 'm', %d FROM course_revisions WHERE id = %d`, k, *chapter, revisionID))
	}
	for _, s := range stmts {
		if _, err := tx.Exec(e.ctx, s); err != nil {
			e.t.Fatal(err)
		}
	}
	if err := tx.Commit(e.ctx); err != nil {
		e.t.Fatal(err)
	}
	return k
}

func (e *env) placementProblems(revisionID int64, mode string) []string {
	e.t.Helper()
	rows, err := e.db.Query(e.ctx, `SELECT problem FROM course_revision_placement_problems($1, $2) ORDER BY 1`, revisionID, mode)
	if err != nil {
		e.t.Fatal(err)
	}
	out, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		e.t.Fatal(err)
	}
	return out
}

func (e *env) placementValid(revisionID int64, mode string) bool {
	e.t.Helper()
	var ok bool
	if err := e.db.QueryRow(e.ctx, `SELECT course_revision_placement_valid($1, $2)`, revisionID, mode).Scan(&ok); err != nil {
		e.t.Fatal(err)
	}
	return ok
}

// course_revision_placement_problems by mode, with a comparison revision
// (set with triggers off: B03 never publishes, so every draft's comparison
// is NULL in production).
func TestPlacementModes(t *testing.T) {
	e := newEnv(t)
	ch1, ch2 := e.chapter(e.math, "ch-01"), e.chapter(e.math, "ch-02")
	carried := e.edition(e.math, []int64{ch1, ch2}, true)
	fresh := e.edition(e.math, []int64{ch1, ch2}, true)
	c, cmp := e.course(e.math, "fr")
	key := e.forceChildren(cmp.ID, carried.assignment, nil, &ch1)
	// cmp becomes history (changes requested), so a draft can be created;
	// the draft is then pointed at cmp as its comparison (setup only).
	e.forceStatus(cmp.ID, `status = 'changes_requested', submitted_at = now()`)
	rev, err := e.store.CreateDraft(e.ctx, e.owner, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	e.forceStatus(rev.ID, `compare_revision_id = `+itoa(cmp.ID))
	k := &key

	check := func(what string, mode string, want ...string) {
		t.Helper()
		got := e.placementProblems(rev.ID, mode)
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("%s (%s): problems %v, want %v", what, mode, got, want)
		}
		if e.placementValid(rev.ID, mode) != (len(want) == 0) {
			t.Errorf("%s (%s): valid disagrees with problems", what, mode)
		}
	}
	check("carried current", "save")
	check("carried current", "submit")
	check("carried current", "publish")
	check("unknown mode", "review", "unknown_mode")

	// The carried edition is superseded: still fine to save, submit and publish.
	e.editionIn(carried.level, carried.section, e.math, []int64{ch1}, true)
	check("carried superseded", "save")
	check("carried superseded", "submit")
	check("carried superseded", "publish")
	// A superseded assignment that was not on the comparison is refused at
	// save and submit, accepted at publish.
	e.forceChildren(rev.ID, carried.assignment, k, &ch1)
	other := e.edition(e.math, []int64{ch1}, true)
	e.editionIn(other.level, other.section, e.math, []int64{ch1}, true)
	e.forceChildren(rev.ID, other.assignment, k, &ch1)
	check("added superseded", "save", "assignment_not_selectable")
	check("added superseded", "submit", "assignment_not_selectable")
	check("added superseded", "publish")
	// A newly selected current assignment.
	e.forceChildren(rev.ID, fresh.assignment, k, &ch1)
	check("added current", "save")
	// Module chapter outside the assignments; no assignment at all.
	e.forceChildren(rev.ID, fresh.assignment, k, &ch2)
	check("chapter inside", "save")
	e.forceChildren(rev.ID, 0, k, &ch1)
	check("no assignment", "save", "chapter_outside_assignments")
	check("no assignment", "submit", "chapter_outside_assignments", "no_assignment")

	// Retired carried edition: flagged only on save, refused at submit and
	// publish.
	e.forceChildren(rev.ID, carried.assignment, k, &ch1)
	if _, err := e.cur.RetireEdition(e.ctx, e.admin, carried.edition.ID, e.versionOf(carried.edition.ID)); err != nil {
		t.Fatal(err)
	}
	check("carried retired", "save")
	check("carried retired", "submit", "assignment_retired")
	check("carried retired", "publish", "assignment_retired")
	// An amendment keeps the comparison's assignments whatever their state.
	e.forceStatus(rev.ID, `kind = 'amendment'`)
	check("amendment, retired edition", "submit")
	check("amendment, retired edition", "publish")
	e.forceChildren(rev.ID, fresh.assignment, k, &ch1)
	check("amendment, changed assignments", "save", "amendment_assignments_changed", "amendment_assignments_changed")
	e.forceStatus(rev.ID, `kind = 'ordinary'`)

	// A retired chapter is accepted only when inherited unchanged (same
	// module key and chapter on the comparison).
	if err := e.cur.RetireChapter(e.ctx, e.admin, ch1); err != nil {
		t.Fatal(err)
	}
	e.forceChildren(rev.ID, fresh.assignment, k, &ch1)
	check("retired chapter inherited", "save")
	other2 := "00000000-0000-4000-8000-0000000000bb"
	e.forceChildren(rev.ID, fresh.assignment, &other2, &ch1)
	check("retired chapter in a new module", "save", "chapter_retired")
}

// Lock order (contract §2): a save takes the editions, then the lessons and
// pinned lesson revisions, then the course, then the revision.
func TestSaveLockOrder(t *testing.T) {
	e := newEnv(t)
	ch := e.chapter(e.math, "ch-01")
	p := e.edition(e.math, []int64{ch}, true)
	own := e.lessonDraft(e.owner, e.math, "fr", "Own")
	c, rev := e.course(e.math, "fr")
	d := courses.Draft{AssignmentIDs: []int64{p.assignment}, Modules: []courses.ModuleInput{{Title: "m", ChapterID: &ch, Steps: []courses.StepInput{step(own, "s")}}}}

	// blockOn holds row locked FOR UPDATE, starts a save, waits until the
	// save waits on that table, runs probe on another connection, then
	// releases the lock and returns the save's result.
	blockOn := func(table string, id int64, probe func()) error {
		t.Helper()
		holder, err := e.db.Begin(e.ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = holder.Rollback(e.ctx) }()
		if _, err := holder.Exec(e.ctx, `SELECT 1 FROM `+table+` WHERE id = $1 FOR UPDATE`, id); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		cur, _ := e.store.Revision(e.ctx, e.owner, rev.ID)
		go func() {
			_, err := e.store.SaveDraft(context.Background(), e.owner, rev.ID, cur.Version, d)
			done <- err
		}()
		deadline := time.Now().Add(10 * time.Second)
		for {
			var waiting bool
			if err := e.db.QueryRow(e.ctx, `SELECT EXISTS (SELECT 1 FROM pg_locks WHERE locktype = 'tuple' AND relation = $1::regclass)`,
				table).Scan(&waiting); err != nil {
				t.Fatal(err)
			}
			if waiting {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("save never waited on %s", table)
			}
			time.Sleep(10 * time.Millisecond)
		}
		probe()
		if err := holder.Rollback(e.ctx); err != nil {
			t.Fatal(err)
		}
		return <-done
	}
	// nowait reports whether row can be locked FOR UPDATE without waiting.
	nowait := func(table string, id int64) bool {
		t.Helper()
		tx, err := e.db.Begin(e.ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(e.ctx) }()
		_, err = tx.Exec(e.ctx, `SELECT 1 FROM `+table+` WHERE id = $1 FOR UPDATE NOWAIT`, id)
		if err != nil {
			wantSQLState(t, "nowait "+table, err, "55P03")
		}
		return err == nil
	}

	// Waiting on the edition: nothing later is locked yet.
	if err := blockOn("curriculum_editions", p.edition.ID, func() {
		if !nowait("lessons", own.LessonID) || !nowait("courses", c.ID) || !nowait("course_revisions", rev.ID) {
			t.Error("save locked a lesson, the course or the revision before the edition")
		}
	}); err != nil {
		t.Fatal(err)
	}
	// Waiting on the lesson: the edition is held, the course and revision not.
	if err := blockOn("lessons", own.LessonID, func() {
		if nowait("curriculum_editions", p.edition.ID) {
			t.Error("edition not locked before the lesson")
		}
		if !nowait("lesson_revisions", own.ID) || !nowait("courses", c.ID) || !nowait("course_revisions", rev.ID) {
			t.Error("save locked the lesson revision, course or revision before the lesson")
		}
	}); err != nil {
		t.Fatal(err)
	}
	// Waiting on the course: the lesson and its revision are held, the
	// course revision not.
	if err := blockOn("courses", c.ID, func() {
		if nowait("lessons", own.LessonID) || nowait("lesson_revisions", own.ID) {
			t.Error("lesson or lesson revision not locked before the course")
		}
		if !nowait("course_revisions", rev.ID) {
			t.Error("course revision locked before the course")
		}
	}); err != nil {
		t.Fatal(err)
	}
	// Waiting on the revision: the course is held.
	if err := blockOn("course_revisions", rev.ID, func() {
		if nowait("courses", c.ID) {
			t.Error("course not locked before the revision")
		}
	}); err != nil {
		t.Fatal(err)
	}
}

// A lesson draft discard racing a course save that pins it: either the save
// wins and the discard is refused with ErrReferencedByCourse, or the discard
// wins and the save reports the step; never a raw foreign-key error.
func TestDiscardRacesCourseSave(t *testing.T) {
	e := newEnv(t)
	for i := 0; i < 10; i++ {
		own := e.lessonDraft(e.owner, e.math, "fr", fmt.Sprintf("Racing %d", i))
		_, rev := e.course(e.math, "fr")
		d := courses.Draft{Modules: []courses.ModuleInput{{Title: "m", Steps: []courses.StepInput{step(own, "s")}}}}
		saveErr, discardErr := make(chan error, 1), make(chan error, 1)
		go func() { _, err := e.save(rev, d); saveErr <- err }()
		go func() { discardErr <- e.lessons.DiscardDraft(e.ctx, e.owner, own.ID, own.Version) }()
		se, de := <-saveErr, <-discardErr
		switch {
		case se == nil && de != nil:
			wantErr(t, "discard after save", de, lessons.ErrReferencedByCourse)
		case de == nil && se != nil:
			wantErr(t, "save after discard", se, courses.ErrInvalidStep)
		default:
			t.Fatalf("save %v, discard %v", se, de)
		}
	}
}
