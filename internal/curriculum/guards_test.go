package curriculum_test

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/hadden-me/learnoo/internal/curriculum"
	"github.com/jackc/pgx/v5"
)

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// Every guard, by direct SQL under the least-privilege runtime role.
func TestGuardsBindTheRuntimeRole(t *testing.T) {
	e := newEnv(t)
	l, s, ed := e.hierarchy()
	c := e.chapter(e.math, "ch-01")
	ed, a, err := e.store.AddAssignment(e.ctx, e.admin, ed.ID, ed.Version, e.math)
	if err != nil {
		t.Fatal(err)
	}
	ed, err = e.store.SetChapters(e.ctx, e.admin, ed.ID, a.ID, ed.Version, []int64{c})
	if err != nil {
		t.Fatal(err)
	}
	asRole := e.leastPrivilegeRole()
	L, S, E, A := itoa(l.ID), itoa(s.ID), itoa(ed.ID), itoa(a.ID)

	// Identities persist.
	for _, tc := range []struct{ sql, msg string }{
		{`DELETE FROM S.education_levels WHERE id = ` + L, "never deleted"},
		{`DELETE FROM S.academic_sections WHERE id = ` + S, "never deleted"},
		{`DELETE FROM S.curriculum_editions WHERE id = ` + E, "never deleted"},
		{`TRUNCATE S.education_levels CASCADE`, "never deleted"},
		{`TRUNCATE S.academic_sections CASCADE`, "never deleted"},
		{`TRUNCATE S.curriculum_editions CASCADE`, "never deleted"},
		{`TRUNCATE S.curriculum_assignments CASCADE`, "never deleted"},
		{`TRUNCATE S.curriculum_assignment_chapters`, "never deleted"},
		{`UPDATE S.education_levels SET code = 'test-new', version = version + 1 WHERE id = ` + L, "identity is immutable"},
		{`UPDATE S.academic_sections SET level_id = level_id, code = 'test-new', version = version + 1 WHERE id = ` + S, "identity is immutable"},
		{`UPDATE S.curriculum_editions SET code = 'test-new', version = version + 1 WHERE id = ` + E, "identity is immutable"},
		{`UPDATE S.education_levels SET display_order = 5 WHERE id = ` + L, "increments its version"},
		{`UPDATE S.curriculum_editions SET version = version + 2 WHERE id = ` + E, "increments its version"},
		{`UPDATE S.curriculum_assignments SET subject_id = subject_id WHERE id = ` + A, "never updated"},
		{`UPDATE S.curriculum_assignment_chapters SET position = 2 WHERE assignment_id = ` + A, "never updated"},
		{`INSERT INTO S.education_levels (code, name_fr, name_ar, retired_at) VALUES ('test-z', 'z', 'z', now())`, "not retired"},
		{`INSERT INTO S.curriculum_editions (section_id, code, label_fr, label_ar, state, current_since) VALUES (` + S + `, 'test-z', 'z', 'z', 'current', now())`, "created as a draft"},
		{`UPDATE S.curriculum_editions SET state = 'retired', retired_at = now(), version = version + 1 WHERE id = ` + E, "cannot change from draft to retired"},
		{`UPDATE S.curriculum_editions SET state = 'current', current_since = now() - interval '1 hour', version = version + 1 WHERE id = ` + E, "set once"},
	} {
		wantGuard(t, tc.sql, asRole(tc.sql), tc.msg)
	}

	// Composite keys keep chapters in the assignment's subject.
	other := e.chapter(e.physics, "ch-01")
	other2 := e.chapter(e.math, "ch-09")
	wantSQLState(t, "chapter of another subject", asRole(
		auditSQL("assignment", a.ID, &ed.ID, "set_chapters", chaptersJSON(c), chaptersJSON(c, other)),
		`INSERT INTO S.curriculum_assignment_chapters (assignment_id, subject_id, chapter_id, position) VALUES (`+A+`, `+itoa(e.math)+`, `+itoa(other)+`, 2)`),
		"23503")
	wantSQLState(t, "assignment chapter with a wrong subject", asRole(
		auditSQL("assignment", a.ID, &ed.ID, "set_chapters", chaptersJSON(c), chaptersJSON(c, other)),
		`INSERT INTO S.curriculum_assignment_chapters (assignment_id, subject_id, chapter_id, position) VALUES (`+A+`, `+itoa(e.physics)+`, `+itoa(other)+`, 2)`),
		"23503")
	wantSQLState(t, "two current editions", asRole(
		auditSQL("edition", ed.ID, &ed.ID, "make_current", stateJSON("draft"), stateJSON("current")),
		`UPDATE S.curriculum_editions SET state = 'current', current_since = now(), version = version + 1 WHERE id = `+E,
		`INSERT INTO S.curriculum_editions (section_id, code, label_fr, label_ar) VALUES (`+S+`, 'test-z', 'z', 'z')`,
		`INSERT INTO S.curriculum_audit (operator_label, target, target_id, edition_id, action, before, after)
		 SELECT 'test-operator@test-host', 'edition', id, id, 'make_current', `+stateJSON("draft")+`, `+stateJSON("current")+`
		   FROM S.curriculum_editions WHERE code = 'test-z'`,
		`UPDATE S.curriculum_editions SET state = 'current', current_since = now(), version = version + 1 WHERE code = 'test-z'`,
	), "23505")

	// Every change needs its audit row of the same transaction (at commit).
	for _, tc := range []struct{ what string }{{"level"}, {"section"}, {"edition"}, {"assignment"}, {"chapters"}, {"subject"}, {"chapter"}} {
		var sql string
		switch tc.what {
		case "level":
			sql = `UPDATE S.education_levels SET name_fr = 'TEST autre', version = version + 1 WHERE id = ` + L
		case "section":
			sql = `UPDATE S.academic_sections SET display_order = 7, version = version + 1 WHERE id = ` + S
		case "edition":
			sql = `UPDATE S.curriculum_editions SET label_ar = 'آخر', version = version + 1 WHERE id = ` + E
		case "assignment":
			sql = `INSERT INTO S.curriculum_assignments (edition_id, subject_id) VALUES (` + E + `, ` + itoa(e.physics) + `)`
		case "chapters":
			sql = `DELETE FROM S.curriculum_assignment_chapters WHERE assignment_id = ` + A
		case "subject":
			sql = `UPDATE S.subjects SET retired_at = now() WHERE id = ` + itoa(e.physics)
		case "chapter":
			sql = `UPDATE S.chapters SET retired_at = now() WHERE id = ` + itoa(other)
		}
		wantGuard(t, tc.what+" without audit", asRole(sql), "curriculum_audit row of the same transaction")
	}
	// An audit row of the wrong action does not count: a rename audited as a
	// reorder, a level created without its row.
	wantGuard(t, "rename audited as reorder", asRole(
		auditSQL("level", l.ID, nil, "reorder", `'{"displayOrder": 0}'`, `'{"displayOrder": 5}'`),
		`UPDATE S.education_levels SET name_fr = 'TEST autre', version = version + 1 WHERE id = `+L), "(rename)")
	wantGuard(t, "level without audit", asRole(
		`INSERT INTO S.education_levels (code, name_fr, name_ar) VALUES ('test-q', 'q', 'q')`), "curriculum_audit row of the same transaction")
	// A superseded edition needs a successor made current in the same transaction.
	cur, _, err := e.store.MakeCurrent(e.ctx, e.admin, ed.ID, ed.Version, nil)
	if err != nil {
		t.Fatal(err)
	}
	wantGuard(t, "supersede alone", asRole(
		auditSQL("edition", cur.ID, &cur.ID, "supersede", stateJSON("current"), stateJSON("superseded")),
		`UPDATE S.curriculum_editions SET state = 'superseded', superseded_at = now(), version = version + 1 WHERE id = `+itoa(cur.ID)),
		"can only be superseded")
	// A superseded_at stamped on retirement is not a supersession.
	wantGuard(t, "superseded_at on retirement", asRole(
		auditSQL("edition", cur.ID, &cur.ID, "retire", stateJSON("current"), stateJSON("retired")),
		`UPDATE S.curriculum_editions SET state = 'retired', retired_at = now(), superseded_at = now(), version = version + 1 WHERE id = `+itoa(cur.ID)),
		"by their own transition")
	// A successor never retires its predecessor.
	wantGuard(t, "predecessor retired by its successor", asRole(
		`INSERT INTO S.curriculum_editions (section_id, code, label_fr, label_ar) VALUES (`+S+`, 'test-next', 'n', 'n')`,
		`INSERT INTO S.curriculum_audit (operator_label, target, target_id, edition_id, action)
		 SELECT 'test-operator@test-host', 'edition', id, id, 'create' FROM S.curriculum_editions WHERE code = 'test-next'`,
		auditSQL("edition", cur.ID, &cur.ID, "retire", stateJSON("current"), stateJSON("retired")),
		`UPDATE S.curriculum_editions SET state = 'retired', retired_at = now(), version = version + 1 WHERE id = `+itoa(cur.ID),
		`INSERT INTO S.curriculum_audit (operator_label, target, target_id, edition_id, action, before, after)
		 SELECT 'test-operator@test-host', 'edition', id, id, 'make_current', `+stateJSON("draft")+`, `+stateJSON("current")+`
		   FROM S.curriculum_editions WHERE code = 'test-next'`,
		`UPDATE S.curriculum_editions SET state = 'current', current_since = now(), version = version + 1 WHERE code = 'test-next'`),
		"must be superseded, not retired")

	// A direct change with its audit row is accepted.
	renameRow := func(to string) string {
		return `INSERT INTO S.curriculum_audit (operator_label, target, target_id, action, before, after)
			SELECT 'test-operator@test-host', 'level', id, 'rename', jsonb_build_object('nameFr', name_fr, 'nameAr', name_ar),
			       jsonb_build_object('nameFr', '` + to + `', 'nameAr', name_ar)
			  FROM S.education_levels WHERE id = ` + L
	}
	if err := asRole(renameRow("TEST direct"),
		`UPDATE S.education_levels SET name_fr = 'TEST direct', version = version + 1 WHERE id = `+L); err != nil {
		t.Fatalf("audited direct rename: %v", err)
	}

	// Audit exactness (rule 1 to 3).
	wantGuard(t, "stale before values", asRole(
		auditSQL("level", l.ID, nil, "rename", `'{"nameFr": "TEST faux", "nameAr": "x"}'`, `'{"nameFr": "TEST y", "nameAr": "x"}'`)),
		"current values as its before values")
	wantGuard(t, "forged retirement", asRole(auditSQL("level", l.ID, nil, "retire", notRetired, isRetired)),
		"records a change that was not applied")
	wantGuard(t, "forged chapter retirement", asRole(auditSQL("chapter", other, nil, "retire", notRetired, isRetired)),
		"records a change that was not applied")
	wantGuard(t, "rename recorded, not applied", asRole(renameRow("TEST jamais")), "records a change that was not applied")
	wantGuard(t, "two renames, one row", asRole(renameRow("TEST un"),
		`UPDATE S.education_levels SET name_fr = 'TEST un', version = version + 1 WHERE id = `+L,
		`UPDATE S.education_levels SET name_fr = 'TEST deux', version = version + 1 WHERE id = `+L),
		"(rename) without its curriculum_audit row")
	if err := asRole(renameRow("TEST un"),
		`UPDATE S.education_levels SET name_fr = 'TEST un', version = version + 1 WHERE id = `+L,
		renameRow("TEST deux"),
		`UPDATE S.education_levels SET name_fr = 'TEST deux', version = version + 1 WHERE id = `+L); err != nil {
		t.Fatalf("two renames, two rows: %v", err)
	}
	wantGuard(t, "rename row then a different rename", asRole(renameRow("TEST trois"),
		`UPDATE S.education_levels SET name_fr = 'TEST quatre', version = version + 1 WHERE id = `+L),
		"(rename) without its curriculum_audit row")
	draft := e.edition(s.ID, "test-other-draft")
	wantGuard(t, "assignment row with a wrong edition", asRole(
		auditSQL("assignment", a.ID, &draft.ID, "set_chapters", chaptersJSON(c), chaptersJSON())),
		"with another edition")
	wantGuard(t, "add_assignment with a wrong subject", asRole(
		`INSERT INTO S.curriculum_assignments (edition_id, subject_id) VALUES (`+itoa(draft.ID)+`, `+itoa(e.physics)+`)`,
		`INSERT INTO S.curriculum_audit (operator_label, target, target_id, edition_id, action, after)
		 SELECT 'test-operator@test-host', 'assignment', id, edition_id, 'add_assignment', jsonb_build_object('subjectId', `+itoa(e.math)+`)
		   FROM S.curriculum_assignments WHERE edition_id = `+itoa(draft.ID)),
		"records the assignment's subject")
	wantGuard(t, "chapter list differing from its audit", asRole(
		`INSERT INTO S.curriculum_assignments (edition_id, subject_id) VALUES (`+itoa(draft.ID)+`, `+itoa(e.math)+`)`,
		`INSERT INTO S.curriculum_audit (operator_label, target, target_id, edition_id, action, after)
		 SELECT 'test-operator@test-host', 'assignment', id, edition_id, 'add_assignment', jsonb_build_object('subjectId', subject_id)
		   FROM S.curriculum_assignments WHERE edition_id = `+itoa(draft.ID),
		`INSERT INTO S.curriculum_audit (operator_label, target, target_id, edition_id, action, before, after)
		 SELECT 'test-operator@test-host', 'assignment', id, edition_id, 'set_chapters', '{"chapterIds": []}', `+chaptersJSON(c)+`
		   FROM S.curriculum_assignments WHERE edition_id = `+itoa(draft.ID),
		`INSERT INTO S.curriculum_assignment_chapters (assignment_id, subject_id, chapter_id, position)
		 SELECT id, subject_id, `+itoa(c)+`, 2 FROM S.curriculum_assignments WHERE edition_id = `+itoa(draft.ID),
		`INSERT INTO S.curriculum_assignment_chapters (assignment_id, subject_id, chapter_id, position)
		 SELECT id, subject_id, `+itoa(other2)+`, 1 FROM S.curriculum_assignments WHERE edition_id = `+itoa(draft.ID)),
		"records a change that was not applied")

	// The audit is append-only and records its own transaction and time.
	for _, sql := range []string{
		`UPDATE S.curriculum_audit SET action = action`,
		`DELETE FROM S.curriculum_audit`,
		`TRUNCATE S.curriculum_audit`,
	} {
		wantGuard(t, sql, asRole(sql), "append-only")
	}
	wantGuard(t, "backdated audit", asRole(
		`INSERT INTO S.curriculum_audit (occurred_at, operator_label, target, target_id, action) VALUES (now() - interval '1 day', 'x', 'level', `+L+`, 'rename')`),
		"its own transaction")
	wantGuard(t, "audit of nothing", asRole(
		`INSERT INTO S.curriculum_audit (operator_label, target, target_id, action) VALUES ('x', 'level', 987654, 'create')`),
		"no existing level")
	wantSQLState(t, "audit with two actors", asRole(
		`INSERT INTO S.curriculum_audit (actor_id, operator_label, target, target_id, action) VALUES (`+itoa(e.adminID)+`, 'x', 'level', `+L+`, 'create')`),
		"23514")
	wantSQLState(t, "audit action for target", asRole(
		`INSERT INTO S.curriculum_audit (operator_label, target, target_id, action) VALUES ('x', 'subject', `+itoa(e.math)+`, 'create')`),
		"23514")
}

// The guards read curriculum_editions, curriculum_assignments,
// academic_sections, education_levels, subjects, chapters and
// curriculum_audit. Temporary tables of the calling role with those names
// must not fool them, nor curriculum_assignment_selectable.
func TestGuardsIgnoreTemporaryShadowTables(t *testing.T) {
	e := newEnv(t)
	l, s, ed := e.hierarchy()
	c := e.chapter(e.math, "ch-01")
	ed, a, err := e.store.AddAssignment(e.ctx, e.admin, ed.ID, ed.Version, e.math)
	if err != nil {
		t.Fatal(err)
	}
	ed, _, err = e.store.MakeCurrent(e.ctx, e.admin, ed.ID, ed.Version, nil)
	if err != nil {
		t.Fatal(err)
	}
	retired := e.chapter(e.math, "ch-02")
	if err := e.store.RetireChapter(e.ctx, e.admin, retired); err != nil {
		t.Fatal(err)
	}
	asRole := e.leastPrivilegeRole()
	E, A := itoa(ed.ID), itoa(a.ID)

	wantGuard(t, "current edition via shadow editions", asRole(
		`CREATE TEMP TABLE curriculum_editions (id bigint, state text)`,
		`INSERT INTO curriculum_editions VALUES (`+E+`, 'draft')`,
		`INSERT INTO S.curriculum_assignment_chapters (assignment_id, subject_id, chapter_id, position) VALUES (`+A+`, `+itoa(e.math)+`, `+itoa(c)+`, 1)`),
		"is not a draft")
	wantGuard(t, "missing assignment via shadow assignments", asRole(
		`CREATE TEMP TABLE curriculum_assignments (id bigint, edition_id bigint)`,
		`INSERT INTO S.curriculum_assignment_chapters (assignment_id, subject_id, chapter_id, position) VALUES (`+A+`, `+itoa(e.math)+`, `+itoa(c)+`, 1)`),
		"is not a draft")

	// A draft edition, to test the remaining guards in isolation.
	d := e.edition(s.ID, "test-draft")
	d, da, err := e.store.AddAssignment(e.ctx, e.admin, d.ID, d.Version, e.math)
	if err != nil {
		t.Fatal(err)
	}
	wantGuard(t, "retired chapter via shadow chapters", asRole(
		`CREATE TEMP TABLE chapters (id bigint, retired_at timestamptz)`,
		`INSERT INTO chapters VALUES (`+itoa(retired)+`, NULL)`,
		auditSQL("assignment", da.ID, &d.ID, "set_chapters", chaptersJSON(), chaptersJSON(retired)),
		`INSERT INTO S.curriculum_assignment_chapters (assignment_id, subject_id, chapter_id, position) VALUES (`+itoa(da.ID)+`, `+itoa(e.math)+`, `+itoa(retired)+`, 1)`),
		"retired chapter")
	if err := e.store.RetireSubject(e.ctx, e.admin, e.physics); err != nil {
		t.Fatal(err)
	}
	wantGuard(t, "retired subject via shadow subjects", asRole(
		`CREATE TEMP TABLE subjects (id bigint, retired_at timestamptz)`,
		`INSERT INTO subjects VALUES (`+itoa(e.physics)+`, NULL)`,
		`INSERT INTO S.curriculum_assignments (edition_id, subject_id) VALUES (`+itoa(d.ID)+`, `+itoa(e.physics)+`)`),
		"retired subject")
	if _, err := e.store.RetireLevel(e.ctx, e.admin, l.ID, l.Version); err != nil {
		t.Fatal(err)
	}
	wantGuard(t, "retired level via shadow hierarchy", asRole(
		`CREATE TEMP TABLE education_levels (id bigint, retired_at timestamptz)`,
		`CREATE TEMP TABLE academic_sections (id bigint, level_id bigint, retired_at timestamptz)`,
		`INSERT INTO education_levels VALUES (`+itoa(l.ID)+`, NULL)`,
		`INSERT INTO academic_sections VALUES (`+itoa(s.ID)+`, `+itoa(l.ID)+`, NULL)`,
		`INSERT INTO S.academic_sections (level_id, code, name_fr, name_ar) VALUES (`+itoa(l.ID)+`, 'test-z', 'z', 'z')`),
		"is retired")
	wantGuard(t, "unaudited change via shadow audit", asRole(
		`CREATE TEMP TABLE curriculum_audit (target text, target_id bigint, action text, xact_id xid8)`,
		`INSERT INTO curriculum_audit VALUES ('edition', `+itoa(d.ID)+`, 'rename', pg_current_xact_id())`,
		`UPDATE S.curriculum_editions SET label_fr = 'TEST ombre', version = version + 1 WHERE id = `+itoa(d.ID)),
		"curriculum_audit row of the same transaction")

	// The selection rule binds its tables at creation.
	var selectable bool
	conn, err := pgx.ConnectConfig(e.ctx, e.db.Config().ConnConfig.Copy())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(context.Background()) }()
	for _, sql := range []string{
		`CREATE TEMP TABLE education_levels (id bigint, retired_at timestamptz)`,
		`INSERT INTO education_levels VALUES (` + itoa(l.ID) + `, NULL)`,
		`SET search_path = pg_temp, pg_catalog`,
	} {
		if _, err := conn.Exec(e.ctx, sql); err != nil {
			t.Fatal(err)
		}
	}
	var schema string
	if err := e.db.QueryRow(e.ctx, `SELECT current_schema()`).Scan(&schema); err != nil {
		t.Fatal(err)
	}
	if err := conn.QueryRow(e.ctx, `SELECT `+pgx.Identifier{schema, "curriculum_assignment_selectable"}.Sanitize()+`($1)`, a.ID).Scan(&selectable); err != nil {
		t.Fatal(err)
	}
	if selectable {
		t.Fatal("selectable under a retired level via a shadow table")
	}
}

// The draft-only guards lock the edition FOR SHARE, so an assignment edit
// cannot commit after a concurrent make-current took the edition FOR UPDATE.
func TestAssignmentEditRacingMakeCurrent(t *testing.T) {
	for _, editFirst := range []bool{true, false} {
		t.Run(map[bool]string{true: "edit first", false: "make-current first"}[editFirst], func(t *testing.T) {
			e := newEnv(t)
			_, _, ed := e.hierarchy()
			asRole := e.leastPrivilegeRole()
			edit := func() error {
				return asRole(
					`INSERT INTO S.curriculum_assignments (edition_id, subject_id) VALUES (`+itoa(ed.ID)+`, `+itoa(e.math)+`)`,
					`INSERT INTO S.curriculum_audit (operator_label, target, target_id, edition_id, action, after)
					 SELECT 'test-operator@test-host', 'assignment', id, edition_id, 'add_assignment', jsonb_build_object('subjectId', subject_id)
					   FROM S.curriculum_assignments WHERE edition_id = `+itoa(ed.ID),
					`SELECT pg_sleep(0.3)`)
			}
			makeCurrent := func() error {
				_, _, err := e.store.MakeCurrent(e.ctx, e.admin, ed.ID, ed.Version, nil)
				return err
			}
			first, second := edit, makeCurrent
			if !editFirst {
				// Hold the edition FOR UPDATE in a make-current transaction
				// that commits only after the edit started.
				first = func() error {
					return pgx.BeginFunc(e.ctx, e.db, func(tx pgx.Tx) error {
						if _, err := tx.Exec(e.ctx, `SELECT 1 FROM curriculum_editions WHERE id = $1 FOR UPDATE`, ed.ID); err != nil {
							return err
						}
						if _, err := tx.Exec(e.ctx, `INSERT INTO curriculum_audit (operator_label, target, target_id, edition_id, action, before, after)
							VALUES ('test-operator@test-host', 'edition', $1, $1, 'make_current', '{"state": "draft"}', '{"state": "current"}')`, ed.ID); err != nil {
							return err
						}
						if _, err := tx.Exec(e.ctx, `UPDATE curriculum_editions SET state = 'current', current_since = now(), version = version + 1 WHERE id = $1`, ed.ID); err != nil {
							return err
						}
						_, err := tx.Exec(e.ctx, `SELECT pg_sleep(0.3)`)
						return err
					})
				}
				second = edit
			}
			done := make(chan error, 1)
			go func() { done <- first() }()
			time.Sleep(100 * time.Millisecond)
			started := time.Now()
			secondErr := second()
			// The second transaction waited for the first one's edition lock.
			if waited := time.Since(started); waited < 150*time.Millisecond {
				t.Errorf("second transaction did not wait for the edition lock (%v)", waited)
			}
			if err := <-done; err != nil {
				t.Fatalf("first: %v", err)
			}
			var state curriculum.EditionState
			if err := e.db.QueryRow(e.ctx, `SELECT state FROM curriculum_editions WHERE id = $1`, ed.ID).Scan(&state); err != nil {
				t.Fatal(err)
			}
			n := e.count(`SELECT count(*) FROM curriculum_assignments WHERE edition_id = $1`, ed.ID)
			if editFirst {
				// make-current waited for the edit's FOR SHARE lock; the direct
				// SQL edit did not bump the version, so it then succeeds.
				if secondErr != nil {
					t.Fatalf("make-current after edit: %v", secondErr)
				}
				if state != curriculum.Current || n != 1 {
					t.Fatalf("state %s, %d assignments", state, n)
				}
			} else {
				wantGuard(t, "edit after make-current", secondErr, "is not a draft")
				if state != curriculum.Current || n != 0 {
					t.Fatalf("state %s, %d assignments", state, n)
				}
			}
		})
	}
}
