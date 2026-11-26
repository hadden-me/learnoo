package curriculum_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// Two-connection races by direct SQL. A first transaction (the table owner)
// makes a state change and holds its locks for a moment; a second one, as
// the runtime role at each isolation level, makes a conflicting change. The
// second must wait for the first, then fail: with the guard's error at READ
// COMMITTED (it re-reads the committed state), with a serialization failure
// (40001) at REPEATABLE READ and SERIALIZABLE.
func TestDirectSQLRacesAtEveryIsolationLevel(t *testing.T) {
	e := newEnv(t)
	asRoleAt := e.leastPrivilegeRoleAt()
	n := 0
	fresh := func() (sectionID, levelID, editionID, assignmentID, subjectID, chapterID int64) {
		n++
		l := e.level(fmt.Sprintf("test-rl%d", n))
		s := e.section(l.ID, "test-rs")
		ed := e.edition(s.ID, "test-re")
		subject := mustSubject(e, fmt.Sprintf("test-rsub%d", n))
		chapter := e.chapter(subject, "ch-01")
		ed, a, err := e.store.AddAssignment(e.ctx, e.admin, ed.ID, ed.Version, subject)
		if err != nil {
			t.Fatal(err)
		}
		return s.ID, l.ID, ed.ID, a.ID, subject, chapter
	}
	first := func(stmts ...string) func() error {
		return func() error {
			return pgx.BeginFunc(e.ctx, e.db, func(tx pgx.Tx) error {
				for _, s := range stmts {
					if _, err := tx.Exec(e.ctx, s); err != nil {
						return err
					}
				}
				_, err := tx.Exec(e.ctx, `SELECT pg_sleep(0.3)`)
				return err
			})
		}
	}
	unqualified := func(s string) string { return strings.ReplaceAll(s, "S.", "") }
	makeCurrent := func(edition int64) []string {
		return []string{
			auditSQL("edition", edition, &edition, "make_current", stateJSON("draft"), stateJSON("current")),
			`UPDATE S.curriculum_editions SET state = 'current', current_since = now(), version = version + 1 WHERE id = ` + itoa(edition),
		}
	}
	retire := func(target, table string, id int64) []string {
		version := ""
		if table == "education_levels" || table == "academic_sections" {
			version = ", version = version + 1"
		}
		return []string{
			auditSQL(target, id, nil, "retire", notRetired, isRetired),
			`UPDATE S.` + table + ` SET retired_at = now()` + version + ` WHERE id = ` + itoa(id),
		}
	}
	type scenario struct {
		name     string
		first    func(section, level, edition, assignment, subject, chapter int64) []string
		second   func(section, level, edition, assignment, subject, chapter int64) []string
		guardMsg string
	}
	scenarios := []scenario{
		{"assignment insert vs make-current",
			func(_, _, ed, _, _, _ int64) []string { return makeCurrent(ed) },
			func(_, _, ed, _, _, _ int64) []string {
				other := mustSubject(e, fmt.Sprintf("test-rother%d", n))
				return []string{`INSERT INTO S.curriculum_assignments (edition_id, subject_id) VALUES (` + itoa(ed) + `, ` + itoa(other) + `)`}
			}, "is not a draft"},
		{"chapter insert vs make-current",
			func(_, _, ed, _, _, _ int64) []string { return makeCurrent(ed) },
			func(_, _, ed, a, sj, c int64) []string {
				return []string{`INSERT INTO S.curriculum_assignment_chapters (assignment_id, subject_id, chapter_id, position) VALUES (` +
					itoa(a) + `, ` + itoa(sj) + `, ` + itoa(c) + `, 1)`}
			}, "is not a draft"},
		{"assignment insert vs subject retirement",
			func(_, _, _, _, _, _ int64) []string {
				return retire("subject", "subjects", e.physics)
			},
			func(_, _, ed, _, _, _ int64) []string {
				return []string{`INSERT INTO S.curriculum_assignments (edition_id, subject_id) VALUES (` + itoa(ed) + `, ` + itoa(e.physics) + `)`}
			}, "retired subject"},
		{"chapter insert vs chapter retirement",
			func(_, _, _, _, _, c int64) []string { return retire("chapter", "chapters", c) },
			func(_, _, _, a, sj, c int64) []string {
				return []string{`INSERT INTO S.curriculum_assignment_chapters (assignment_id, subject_id, chapter_id, position) VALUES (` +
					itoa(a) + `, ` + itoa(sj) + `, ` + itoa(c) + `, 1)`}
			}, "retired chapter"},
		{"make-current vs section retirement",
			func(s, _, _, _, _, _ int64) []string { return retire("section", "academic_sections", s) },
			func(_, _, ed, _, _, _ int64) []string { return makeCurrent(ed) }, "is retired"},
		{"make-current vs level retirement",
			func(_, l, _, _, _, _ int64) []string { return retire("level", "education_levels", l) },
			func(_, _, ed, _, _, _ int64) []string { return makeCurrent(ed) }, "is retired"},
	}
	for _, sc := range scenarios {
		for _, iso := range []pgx.TxIsoLevel{pgx.ReadCommitted, pgx.RepeatableRead, pgx.Serializable} {
			t.Run(sc.name+"/"+string(iso), func(t *testing.T) {
				if sc.name == "assignment insert vs subject retirement" {
					// Each run needs a subject that is not yet retired.
					e.physics = mustSubject(e, fmt.Sprintf("test-rphys%d", n+1))
				}
				s, l, ed, a, sj, c := fresh()
				stmts := sc.first(s, l, ed, a, sj, c)
				for i := range stmts {
					stmts[i] = unqualified(stmts[i])
				}
				second := sc.second(s, l, ed, a, sj, c)
				done := make(chan error, 1)
				go func() { done <- first(stmts...)() }()
				time.Sleep(100 * time.Millisecond)
				started := time.Now()
				err := asRoleAt(iso, second...)
				waited := time.Since(started)
				if ferr := <-done; ferr != nil {
					t.Fatalf("first transaction: %v", ferr)
				}
				if waited < 150*time.Millisecond {
					t.Errorf("second transaction did not wait (%v)", waited)
				}
				if iso == pgx.ReadCommitted {
					wantGuard(t, sc.name, err, sc.guardMsg)
				} else {
					wantSQLState(t, sc.name, err, "40001")
				}
			})
		}
	}
}
