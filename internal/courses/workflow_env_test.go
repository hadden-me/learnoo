package courses_test

import (
	"fmt"

	"github.com/hadden-me/learnoo/internal/courses"
	"github.com/hadden-me/learnoo/internal/lessons"
	"github.com/hadden-me/learnoo/internal/review"
)

// Helpers for the LMS-B04 tests: a ready course (complete overview, a
// current placement, one module with a chapter and the given steps),
// submission, approval and publication through the store.

type ready struct {
	course courses.Course
	rev    courses.Revision
	p      placement
	ch     int64
}

var readySeq int

// readyCourse creates a course of e.owner in math/fr whose draft is
// complete and pins the given lesson revisions, one step each.
func (e *env) readyCourse(steps ...lessons.Revision) ready {
	e.t.Helper()
	readySeq++
	ch := e.chapter(e.math, fmt.Sprintf("rc-%d", readySeq))
	p := e.edition(e.math, []int64{ch}, true)
	c, rev := e.course(e.math, "fr")
	var ss []courses.StepInput
	for i, s := range steps {
		ss = append(ss, step(s, fmt.Sprintf("Step %d", i+1)))
	}
	rev = e.mustSave(rev, e.completeDraft(p.assignment, ch, ss))
	return ready{course: c, rev: rev, p: p, ch: ch}
}

func (e *env) completeDraft(assignment, ch int64, steps []courses.StepInput) courses.Draft {
	return courses.Draft{Title: "TEST course", Summary: "Simulated summary.", Objectives: "Simulated objectives.",
		AssignmentIDs: []int64{assignment},
		Modules:       []courses.ModuleInput{{Title: "Module 1", ChapterID: &ch, Steps: steps}}}
}

func (e *env) submit(rev courses.Revision) courses.Revision {
	e.t.Helper()
	out, err := e.store.Submit(e.ctx, e.owner, rev.ID, rev.Version)
	if err != nil {
		e.t.Fatalf("submit: %v", err)
	}
	return out
}

func (e *env) approveCourse(revID int64) {
	e.t.Helper()
	if _, err := e.store.Approve(e.ctx, e.reviewer, revID, ""); err != nil {
		e.t.Fatalf("approve course: %v", err)
	}
}

func (e *env) pointer(courseID int64) *int64 {
	e.t.Helper()
	var p *int64
	if err := e.db.QueryRow(e.ctx, `SELECT published_revision_id FROM courses WHERE id = $1`, courseID).Scan(&p); err != nil {
		e.t.Fatal(err)
	}
	return p
}

func (e *env) publishCourse(revID int64) {
	e.t.Helper()
	var course int64
	if err := e.db.QueryRow(e.ctx, `SELECT course_id FROM course_revisions WHERE id = $1`, revID).Scan(&course); err != nil {
		e.t.Fatal(err)
	}
	if _, err := e.store.Publish(e.ctx, e.publisher, revID, e.pointer(course)); err != nil {
		e.t.Fatalf("publish course: %v", err)
	}
}

// publishedCourse runs a ready course through submission, approval and
// publication.
func (e *env) publishedCourse(steps ...lessons.Revision) ready {
	e.t.Helper()
	r := e.readyCourse(steps...)
	r.rev = e.submit(r.rev)
	e.approveCourse(r.rev.ID)
	e.publishCourse(r.rev.ID)
	return r
}

// newerLessonRevision publishes a new revision of a published lesson.
func (e *env) newerLessonRevision(of lessons.Revision, title string) lessons.Revision {
	e.t.Helper()
	rev, err := e.lessons.CreateDraft(e.ctx, of.AuthorID, of.LessonID, lessons.Fields{Content: doc(title, of.Language)})
	if err != nil {
		e.t.Fatal(err)
	}
	e.approve(rev)
	e.publish(rev)
	return rev
}

func (e *env) adminID() int64 { return *e.admin.AuditID() }

func (e *env) reviewStore() *review.Store { return review.NewStore(e.db) }

// readable reports reader_published_courses for the course and the
// readable flag of each step of reader_course_steps, by lesson.
func (e *env) readable(courseID int64) (bool, map[int64]bool) {
	e.t.Helper()
	ok := e.count(`SELECT count(*) FROM reader_published_courses WHERE course_id = $1`, courseID) == 1
	steps := map[int64]bool{}
	rows, err := e.db.Query(e.ctx, `SELECT lesson_id, readable FROM reader_course_steps WHERE course_id = $1`, courseID)
	if err != nil {
		e.t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var l int64
		var r bool
		if err := rows.Scan(&l, &r); err != nil {
			e.t.Fatal(err)
		}
		steps[l] = r
	}
	return ok, steps
}

// approveBy submits rev (by its author) and approves it as reviewer.
func (e *env) approveBy(rev lessons.Revision, reviewer int64) {
	e.t.Helper()
	if _, err := e.lessons.Submit(e.ctx, rev.AuthorID, rev.ID, rev.Version); err != nil {
		e.t.Fatal(err)
	}
	tx, err := e.db.Begin(e.ctx)
	if err != nil {
		e.t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(e.ctx) }()
	if _, err := tx.Exec(e.ctx, `INSERT INTO review_decisions (revision_id, reviewer_id, action) VALUES ($1, $2, 'approve')`, rev.ID, reviewer); err != nil {
		e.t.Fatal(err)
	}
	if _, err := tx.Exec(e.ctx, `UPDATE lesson_revisions SET status = 'approved' WHERE id = $1`, rev.ID); err != nil {
		e.t.Fatal(err)
	}
	if err := tx.Commit(e.ctx); err != nil {
		e.t.Fatal(err)
	}
}
