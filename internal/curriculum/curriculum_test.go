package curriculum_test

import (
	"sync"
	"testing"

	"github.com/hadden-me/learnoo/internal/curriculum"
	"github.com/hadden-me/learnoo/internal/permissions"
)

func TestLifecycleIsAudited(t *testing.T) {
	e := newEnv(t)
	l, s, ed := e.hierarchy()
	if ed.State != curriculum.Draft || ed.Version != 1 || ed.CurrentSince != nil {
		t.Fatalf("new edition = %+v", ed)
	}
	c1, c2 := e.chapter(e.math, "ch-01"), e.chapter(e.math, "ch-02")

	ed, a, err := e.store.AddAssignment(e.ctx, e.admin, ed.ID, 1, e.math)
	if err != nil {
		t.Fatal(err)
	}
	if a.Selectable || ed.Version != 2 {
		t.Fatalf("draft assignment selectable %v, version %d", a.Selectable, ed.Version)
	}
	// Assignment chapter order is independent of codes.
	ed, err = e.store.SetChapters(e.ctx, e.admin, ed.ID, a.ID, ed.Version, []int64{c2, c1})
	if err != nil {
		t.Fatal(err)
	}
	got := ed.Assignments[0].Chapters
	if len(got) != 2 || got[0].ChapterID != c2 || got[0].Position != 1 || got[1].ChapterID != c1 || got[1].Position != 2 {
		t.Fatalf("chapters = %+v", got)
	}
	ed, prev, err := e.store.MakeCurrent(e.ctx, e.admin, ed.ID, ed.Version, nil)
	if err != nil {
		t.Fatal(err)
	}
	if prev != nil || ed.State != curriculum.Current || ed.CurrentSince == nil || !ed.Assignments[0].Selectable {
		t.Fatalf("after make-current: %+v, previous %v", ed, prev)
	}

	// A successor supersedes it atomically.
	next := e.edition(s.ID, "test-2027")
	next, prev, err = e.store.MakeCurrent(e.ctx, e.admin, next.ID, next.Version, &ed.ID)
	if err != nil {
		t.Fatal(err)
	}
	if prev == nil || *prev != ed.ID || next.State != curriculum.Current {
		t.Fatalf("successor: %+v, previous %v", next, prev)
	}
	old, err := e.store.Edition(e.ctx, ed.ID)
	if err != nil {
		t.Fatal(err)
	}
	if old.State != curriculum.Superseded || old.SupersededAt == nil || old.Assignments[0].Selectable || len(old.Assignments[0].Chapters) != 2 {
		t.Fatalf("superseded edition = %+v", old)
	}
	if _, err := e.store.RetireEdition(e.ctx, e.admin, old.ID, old.Version); err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.UpdateLevel(e.ctx, e.admin, l.ID, l.Version, curriculum.Display{NameFR: "TEST Bac", NameAR: "باكالوريا", DisplayOrder: 3}); err != nil {
		t.Fatal(err)
	}

	type row struct{ target, action string }
	rows, err := e.db.Query(e.ctx, `SELECT target, action FROM curriculum_audit WHERE actor_id = $1 AND operator_label IS NULL ORDER BY id`, e.adminID)
	if err != nil {
		t.Fatal(err)
	}
	var trail []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.target, &r.action); err != nil {
			t.Fatal(err)
		}
		trail = append(trail, r)
	}
	want := []row{{"level", "create"}, {"section", "create"}, {"edition", "create"}, {"assignment", "add_assignment"},
		{"assignment", "set_chapters"}, {"edition", "make_current"}, {"edition", "create"}, {"edition", "supersede"},
		{"edition", "make_current"}, {"edition", "retire"}, {"level", "rename"}, {"level", "reorder"}}
	if len(trail) != len(want) {
		t.Fatalf("audit trail = %v, want %v", trail, want)
	}
	for i := range want {
		if trail[i] != want[i] {
			t.Fatalf("audit trail = %v, want %v", trail, want)
		}
	}
	if n := e.count(`SELECT count(*) FROM curriculum_audit WHERE action = 'set_chapters' AND after = jsonb_build_object('chapterIds', jsonb_build_array($1::bigint, $2::bigint))`, c2, c1); n != 1 {
		t.Fatalf("set_chapters after values: %d rows", n)
	}

	// Operators (bootstrap CLI) are audited with their label.
	if _, err := e.store.CreateLevel(e.ctx, testOperator, curriculum.Entry{Code: "test-op", NameFR: "TEST op", NameAR: "عملية"}); err != nil {
		t.Fatal(err)
	}
	if n := e.count(`SELECT count(*) FROM curriculum_audit WHERE operator_label = 'test-operator@test-host' AND actor_id IS NULL`); n != 1 {
		t.Fatalf("operator audit rows = %d", n)
	}
}

func TestWritesNeedAnActiveAdministrator(t *testing.T) {
	e := newEnv(t)
	_, _, ed := e.hierarchy()
	author := permissions.UserActor(e.authorID)
	_, err := e.store.CreateLevel(e.ctx, author, curriculum.Entry{Code: "test-x", NameFR: "x", NameAR: "x"})
	wantErr(t, "author creates a level", err, permissions.ErrDenied)
	_, _, err = e.store.AddAssignment(e.ctx, author, ed.ID, ed.Version, e.math)
	wantErr(t, "author assigns", err, permissions.ErrDenied)
	_, _, err = e.store.MakeCurrent(e.ctx, author, ed.ID, ed.Version, nil)
	wantErr(t, "author makes current", err, permissions.ErrDenied)
	wantErr(t, "author retires a subject", e.store.RetireSubject(e.ctx, author, e.math), permissions.ErrDenied)

	if err := e.exec(`UPDATE users SET disabled_at = now() WHERE id = $1`, e.adminID); err != nil {
		t.Fatal(err)
	}
	_, err = e.store.CreateLevel(e.ctx, e.admin, curriculum.Entry{Code: "test-y", NameFR: "y", NameAR: "y"})
	wantErr(t, "disabled administrator", err, permissions.ErrDenied)
	if n := e.count(`SELECT count(*) FROM education_levels`); n != 1 {
		t.Fatalf("levels = %d", n)
	}
}

func TestValidationAndConflicts(t *testing.T) {
	e := newEnv(t)
	l, s, ed := e.hierarchy()
	for name, in := range map[string]curriculum.Entry{
		"bad code":       {Code: "Test", NameFR: "a", NameAR: "b"},
		"empty name":     {Code: "test-a", NameFR: "", NameAR: "b"},
		"bidi control":   {Code: "test-a", NameFR: "a‮b", NameAR: "b"},
		"order too high": {Code: "test-a", NameFR: "a", NameAR: "b", DisplayOrder: 10000},
	} {
		_, err := e.store.CreateLevel(e.ctx, e.admin, in)
		wantErr(t, name, err, curriculum.ErrInvalidInput)
	}
	// Names are trimmed like the catalogue's display names.
	padded, err := e.store.CreateLevel(e.ctx, e.admin, curriculum.Entry{Code: "test-padded", NameFR: "  TEST a ", NameAR: "\tب\n"})
	if err != nil || padded.NameFR != "TEST a" || padded.NameAR != "ب" {
		t.Fatalf("padded names: %+v, %v", padded, err)
	}
	_, err = e.store.CreateLevel(e.ctx, e.admin, curriculum.Entry{Code: l.Code, NameFR: "a", NameAR: "b"})
	wantErr(t, "duplicate level code", err, curriculum.ErrCodeExists)
	_, err = e.store.CreateSection(e.ctx, e.admin, l.ID, curriculum.Entry{Code: s.Code, NameFR: "a", NameAR: "b"})
	wantErr(t, "duplicate section code", err, curriculum.ErrCodeExists)
	_, err = e.store.CreateEdition(e.ctx, e.admin, s.ID, curriculum.NewEdition{Code: ed.Code, LabelFR: "a", LabelAR: "b"})
	wantErr(t, "duplicate edition code", err, curriculum.ErrCodeExists)
	// The same codes are free in another scope.
	other := e.level("test-other")
	e.section(other.ID, s.Code)

	_, err = e.store.UpdateSection(e.ctx, e.admin, s.ID, s.Version+1, curriculum.Display{NameFR: "a", NameAR: "b"})
	wantErr(t, "stale section version", err, curriculum.ErrVersionConflict)
	_, _, err = e.store.AddAssignment(e.ctx, e.admin, ed.ID, ed.Version+1, e.math)
	wantErr(t, "stale edition version", err, curriculum.ErrVersionConflict)
	_, _, err = e.store.AddAssignment(e.ctx, e.admin, ed.ID, ed.Version, 999999)
	wantErr(t, "unknown subject", err, curriculum.ErrUnknownSubject)
	ed, a, err := e.store.AddAssignment(e.ctx, e.admin, ed.ID, ed.Version, e.math)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = e.store.AddAssignment(e.ctx, e.admin, ed.ID, ed.Version, e.math)
	wantErr(t, "subject assigned twice", err, curriculum.ErrAssignmentExists)
	foreign := e.chapter(e.physics, "ch-01")
	mine := e.chapter(e.math, "ch-01")
	_, err = e.store.SetChapters(e.ctx, e.admin, ed.ID, a.ID, ed.Version, []int64{foreign})
	wantErr(t, "chapter of another subject", err, curriculum.ErrPlacementInvalid)
	_, err = e.store.SetChapters(e.ctx, e.admin, ed.ID, a.ID, ed.Version, []int64{mine, mine})
	wantErr(t, "duplicate chapter", err, curriculum.ErrPlacementInvalid)
	_, err = e.store.SetChapters(e.ctx, e.admin, ed.ID, a.ID, ed.Version, []int64{424242})
	wantErr(t, "unknown chapter", err, curriculum.ErrPlacementInvalid)
	_, err = e.store.SetChapters(e.ctx, e.admin, ed.ID, 424242, ed.Version, nil)
	wantErr(t, "unknown assignment", err, curriculum.ErrNotFound)
	_, err = e.store.RetireEdition(e.ctx, e.admin, ed.ID, ed.Version)
	wantErr(t, "retire a draft", err, curriculum.ErrStateConflict)
	// Nothing above changed the edition.
	now, err := e.store.Edition(e.ctx, ed.ID)
	if err != nil {
		t.Fatal(err)
	}
	if now.Version != ed.Version || len(now.Assignments[0].Chapters) != 0 {
		t.Fatalf("edition changed by refused writes: %+v", now)
	}
}

func TestAssignmentsFreezeWhenTheEditionLeavesDraft(t *testing.T) {
	e := newEnv(t)
	_, _, ed := e.hierarchy()
	c := e.chapter(e.math, "ch-01")
	ed, a, err := e.store.AddAssignment(e.ctx, e.admin, ed.ID, ed.Version, e.math)
	if err != nil {
		t.Fatal(err)
	}
	ed, _, err = e.store.MakeCurrent(e.ctx, e.admin, ed.ID, ed.Version, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = e.store.AddAssignment(e.ctx, e.admin, ed.ID, ed.Version, e.physics)
	wantErr(t, "add after current", err, curriculum.ErrEditionNotDraft)
	_, err = e.store.SetChapters(e.ctx, e.admin, ed.ID, a.ID, ed.Version, []int64{c})
	wantErr(t, "chapters after current", err, curriculum.ErrEditionNotDraft)
	_, err = e.store.RemoveAssignment(e.ctx, e.admin, ed.ID, a.ID, ed.Version)
	wantErr(t, "remove after current", err, curriculum.ErrEditionNotDraft)
	// A stale version still reports the state, not a version conflict.
	_, _, err = e.store.AddAssignment(e.ctx, e.admin, ed.ID, 1, e.physics)
	wantErr(t, "stale add after current", err, curriculum.ErrEditionNotDraft)
	_, _, err = e.store.MakeCurrent(e.ctx, e.admin, ed.ID, ed.Version, nil)
	wantErr(t, "make current twice", err, curriculum.ErrEditionNotDraft)

	// The guards refuse the same edits for every writer.
	asRole := e.leastPrivilegeRole()
	wantGuard(t, "insert assignment", asRole(
		`INSERT INTO S.curriculum_assignments (edition_id, subject_id) VALUES (`+itoa(ed.ID)+`, `+itoa(e.physics)+`)`),
		"is not a draft")
	wantGuard(t, "delete assignment", asRole(`DELETE FROM S.curriculum_assignments WHERE id = `+itoa(a.ID)), "is not a draft")
	wantGuard(t, "insert chapter", asRole(
		`INSERT INTO S.curriculum_assignment_chapters (assignment_id, subject_id, chapter_id, position) VALUES (`+
			itoa(a.ID)+`, `+itoa(e.math)+`, `+itoa(c)+`, 1)`), "is not a draft")
	wantGuard(t, "back to draft", asRole(`UPDATE S.curriculum_editions SET state = 'draft', current_since = NULL, version = version + 1 WHERE id = `+itoa(ed.ID)),
		"cannot change from current to draft")
}

func TestRetirement(t *testing.T) {
	e := newEnv(t)
	l, s, ed := e.hierarchy()
	c := e.chapter(e.math, "ch-01")
	ed, a, err := e.store.AddAssignment(e.ctx, e.admin, ed.ID, ed.Version, e.math)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.store.RetireChapter(e.ctx, e.admin, c); err != nil {
		t.Fatal(err)
	}
	wantErr(t, "retire chapter twice", e.store.RetireChapter(e.ctx, e.admin, c), curriculum.ErrAlreadyRetired)
	_, err = e.store.SetChapters(e.ctx, e.admin, ed.ID, a.ID, ed.Version, []int64{c})
	wantErr(t, "retired chapter", err, curriculum.ErrPlacementInvalid)
	if err := e.store.RetireSubject(e.ctx, e.admin, e.physics); err != nil {
		t.Fatal(err)
	}
	_, _, err = e.store.AddAssignment(e.ctx, e.admin, ed.ID, ed.Version, e.physics)
	wantErr(t, "retired subject", err, curriculum.ErrPlacementInvalid)

	ed, _, err = e.store.MakeCurrent(e.ctx, e.admin, ed.ID, ed.Version, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !ed.Assignments[0].Selectable {
		t.Fatal("current assignment not selectable")
	}
	// Retiring the subject keeps the assignment resolving but unselectable.
	if err := e.store.RetireSubject(e.ctx, e.admin, e.math); err != nil {
		t.Fatal(err)
	}
	ed, err = e.store.Edition(e.ctx, ed.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(ed.Assignments) != 1 || ed.Assignments[0].Selectable {
		t.Fatalf("assignment after subject retirement = %+v", ed.Assignments)
	}

	l2 := e.level("test-l2")
	s2 := e.section(l2.ID, "test-s2")
	ed2 := e.edition(s2.ID, "test-e2")
	ed2, _, err = e.store.AddAssignment(e.ctx, e.admin, ed2.ID, ed2.Version, mustSubject(e, "test-chem"))
	if err != nil {
		t.Fatal(err)
	}
	ed2, _, err = e.store.MakeCurrent(e.ctx, e.admin, ed2.ID, ed2.Version, nil)
	if err != nil {
		t.Fatal(err)
	}
	l2, err = e.store.RetireLevel(e.ctx, e.admin, l2.ID, l2.Version)
	if err != nil {
		t.Fatal(err)
	}
	_, err = e.store.RetireLevel(e.ctx, e.admin, l2.ID, l2.Version)
	wantErr(t, "retire level twice", err, curriculum.ErrAlreadyRetired)
	ed2, err = e.store.Edition(e.ctx, ed2.ID)
	if err != nil {
		t.Fatal(err)
	}
	if ed2.Assignments[0].Selectable {
		t.Fatal("assignment under a retired level is selectable")
	}
	_, err = e.store.CreateSection(e.ctx, e.admin, l2.ID, curriculum.Entry{Code: "test-new", NameFR: "a", NameAR: "b"})
	wantErr(t, "section under retired level", err, curriculum.ErrParentRetired)
	_, err = e.store.CreateEdition(e.ctx, e.admin, s2.ID, curriculum.NewEdition{Code: "test-new", LabelFR: "a", LabelAR: "b"})
	wantErr(t, "edition under retired level", err, curriculum.ErrParentRetired)

	draft := e.edition(s.ID, "test-next")
	bio := mustSubject(e, "test-bio")
	draft, _, err = e.store.AddAssignment(e.ctx, e.admin, draft.ID, draft.Version, bio)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.store.RetireSubject(e.ctx, e.admin, bio); err != nil {
		t.Fatal(err)
	}
	_, _, err = e.store.MakeCurrent(e.ctx, e.admin, draft.ID, draft.Version, &ed.ID)
	wantErr(t, "make current with a retired subject", err, curriculum.ErrPlacementInvalid)
	if _, err := e.store.RetireSection(e.ctx, e.admin, s.ID, s.Version); err != nil {
		t.Fatal(err)
	}
	_, _, err = e.store.MakeCurrent(e.ctx, e.admin, draft.ID, draft.Version, &ed.ID)
	wantErr(t, "make current under retired section", err, curriculum.ErrParentRetired)
	_ = l

	// Retirement is one-way, for every writer.
	asRole := e.leastPrivilegeRole()
	for _, sql := range []string{
		`UPDATE S.education_levels SET retired_at = NULL, version = version + 1 WHERE id = ` + itoa(l2.ID),
		`UPDATE S.subjects SET retired_at = NULL WHERE id = ` + itoa(e.math),
		`UPDATE S.chapters SET retired_at = NULL WHERE id = ` + itoa(c),
	} {
		wantGuard(t, sql, asRole(sql), "never undone")
	}
	wantGuard(t, "backdated retirement", asRole(
		`UPDATE S.academic_sections SET retired_at = now() - interval '1 day', version = version + 1 WHERE id = `+itoa(s2.ID),
		auditSQL("section", s2.ID, nil, "retire", notRetired, isRetired)), "never undone")
	wantGuard(t, "subject added retired", asRole(
		`WITH s AS (INSERT INTO S.subjects (code, retired_at) VALUES ('test-dead', now()) RETURNING id)
		 INSERT INTO S.catalogue_audit (operator_label, action, subject_id) SELECT 'test-operator@test-host', 'add_subject', id FROM s`),
		"is not retired")
}

func mustSubject(e *env, code string) int64 {
	e.t.Helper()
	var id int64
	if err := e.db.QueryRow(e.ctx, `
		WITH s AS (INSERT INTO subjects (code) VALUES ($1) RETURNING id)
		INSERT INTO catalogue_audit (operator_label, action, subject_id) SELECT 'test-operator@test-host', 'add_subject', id FROM s
		RETURNING subject_id`, code).Scan(&id); err != nil {
		e.t.Fatal(err)
	}
	return id
}

// Racing make-current of two drafts of one section: exactly one becomes
// current; the loser changes nothing.
func TestRacingMakeCurrentYieldsOneCurrent(t *testing.T) {
	e := newEnv(t)
	_, s, first := e.hierarchy()
	first, _, err := e.store.MakeCurrent(e.ctx, e.admin, first.ID, first.Version, nil)
	if err != nil {
		t.Fatal(err)
	}
	current := first.ID
	for round := 0; round < 10; round++ {
		a := e.edition(s.ID, "test-a"+itoa(int64(round)))
		b := e.edition(s.ID, "test-b"+itoa(int64(round)))
		var wg sync.WaitGroup
		errs := make([]error, 2)
		for i, ed := range []curriculum.Edition{a, b} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, _, errs[i] = e.store.MakeCurrent(e.ctx, e.admin, ed.ID, ed.Version, &current)
			}()
		}
		wg.Wait()
		ok := 0
		for i, err := range errs {
			switch {
			case err == nil:
				ok++
				current = []int64{a.ID, b.ID}[i]
			default:
				wantErr(t, "losing make-current", err, curriculum.ErrStateConflict)
			}
		}
		if ok != 1 {
			t.Fatalf("round %d: %d winners (%v)", round, ok, errs)
		}
		if n := e.count(`SELECT count(*) FROM curriculum_editions WHERE section_id = $1 AND state = 'current'`, s.ID); n != 1 {
			t.Fatalf("round %d: %d current editions", round, n)
		}
		if n := e.count(`SELECT count(*) FROM curriculum_editions WHERE section_id = $1 AND state = 'draft' AND id IN ($2, $3)`, s.ID, a.ID, b.ID); n != 1 {
			t.Fatalf("round %d: loser not left as draft", round)
		}
		if n := e.count(`SELECT count(*) FROM curriculum_audit WHERE action = 'make_current' AND target_id IN ($1, $2)`, a.ID, b.ID); n != 1 {
			t.Fatalf("round %d: %d make_current audit rows", round, n)
		}
	}
	// The same edition raced twice: one wins, the other finds it current.
	c := e.edition(s.ID, "test-same")
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, errs[i] = e.store.MakeCurrent(e.ctx, e.admin, c.ID, c.Version, &current)
		}()
	}
	wg.Wait()
	if (errs[0] == nil) == (errs[1] == nil) {
		t.Fatalf("same edition race: %v", errs)
	}
	if n := e.count(`SELECT count(*) FROM curriculum_audit WHERE action = 'supersede'`); n != 11 {
		t.Fatalf("supersede rows = %d, want 11", n)
	}
}

// Concurrent edits of one draft edition with the same expected version:
// exactly one is written.
func TestConcurrentDraftEditsConflict(t *testing.T) {
	e := newEnv(t)
	_, s, ed := e.hierarchy()
	c1, c2 := e.chapter(e.math, "ch-01"), e.chapter(e.math, "ch-02")
	ed, a, err := e.store.AddAssignment(e.ctx, e.admin, ed.ID, ed.Version, e.math)
	if err != nil {
		t.Fatal(err)
	}
	for round := 0; round < 10; round++ {
		cur, err := e.store.Edition(e.ctx, ed.ID)
		if err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		errs := make([]error, 2)
		for i, list := range [][]int64{{c1, c2}, {c2}} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, errs[i] = e.store.SetChapters(e.ctx, e.admin, ed.ID, a.ID, cur.Version, list)
			}()
		}
		wg.Wait()
		if (errs[0] == nil) == (errs[1] == nil) {
			t.Fatalf("round %d: %v", round, errs)
		}
		for _, err := range errs {
			if err != nil {
				wantErr(t, "losing edit", err, curriculum.ErrVersionConflict)
			}
		}
	}
	// Concurrent renames of one section.
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, name := range []string{"TEST un", "TEST deux"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = e.store.UpdateSection(e.ctx, e.admin, s.ID, s.Version, curriculum.Display{NameFR: name, NameAR: "اسم"})
		}()
	}
	wg.Wait()
	if (errs[0] == nil) == (errs[1] == nil) {
		t.Fatalf("renames: %v", errs)
	}
	if n := e.count(`SELECT count(*) FROM curriculum_audit WHERE target = 'section' AND action = 'rename'`); n != 1 {
		t.Fatalf("rename audit rows = %d", n)
	}
}
