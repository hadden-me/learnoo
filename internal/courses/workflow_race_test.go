package courses_test

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hadden-me/learnoo/internal/courses"
	"github.com/hadden-me/learnoo/internal/lessons"
)

// lessonEffect applies a lesson withdrawal (of its published revision,
// which also unpublishes it), an unpublication or an archive.
func (e *env) lessonEffect(kind string, rev lessons.Revision) error {
	ctx := context.Background()
	switch kind {
	case "withdraw":
		_, err := e.reviewStore().Withdraw(ctx, e.adminID(), rev.ID, "Simulated withdrawal.", &rev.ID)
		return err
	case "unpublish":
		_, err := e.reviewStore().Unpublish(ctx, e.adminID(), rev.LessonID, "Simulated unpublication.", rev.ID)
		return err
	default: // archive: no application path; direct SQL at READ COMMITTED
		_, err := e.db.Exec(ctx, `UPDATE lessons SET archived_at = now() WHERE id = $1`, rev.LessonID)
		return err
	}
}

// A07: a lesson withdrawal, unpublication or archive racing a course
// publication or restoration that pins it. The lesson change always
// applies; the course change either commits first (and the dependency
// events then name it) or sees the lesson change and is refused with
// ErrIneligible, nothing changed. Never a deadlock or a retry.
func TestLessonEffectsRaceCoursePublicationAndRestore(t *testing.T) {
	e := newEnv(t)
	for _, kind := range []string{"withdraw", "unpublish", "archive"} {
		for _, op := range []string{"publish", "restore"} {
			first, refused := 0, 0
			for i := 0; i < 4; i++ {
				l := e.publishedLesson(e.other, e.math, "fr", fmt.Sprintf("%s %s %d", kind, op, i))
				stable := e.publishedLesson(e.other, e.math, "fr", fmt.Sprintf("stable %s %s %d", kind, op, i))
				r := e.publishedCourse(l, stable)
				var target int64
				var expected *int64
				if op == "publish" {
					d, err := e.store.CreateDraft(e.ctx, e.owner, r.course.ID)
					if err != nil {
						t.Fatal(err)
					}
					dd := e.completeDraft(r.p.assignment, r.ch, []courses.StepInput{step(l, "Renamed"), step(stable, "Step 2")})
					dd.Modules[0].Key = d.Modules[0].Key
					d = e.submit(e.mustSave(d, dd))
					e.approveCourse(d.ID)
					target, expected = d.ID, &r.rev.ID
				} else {
					if _, err := e.store.Suspend(e.ctx, e.adminID(), r.course.ID, &r.rev.ID, "Simulated."); err != nil {
						t.Fatal(err)
					}
					target = r.rev.ID
				}
				courseErr, lessonErr := race(
					func() error {
						var err error
						if op == "publish" {
							_, err = e.store.Publish(context.Background(), e.publisher, target, expected)
						} else {
							_, err = e.store.Restore(context.Background(), e.publisher, r.course.ID, target, nil)
						}
						return err
					},
					func() error {
						// Every other round lets the course side start first,
						// so both orders are exercised.
						if i%2 == 0 {
							time.Sleep(20 * time.Millisecond)
						}
						return e.lessonEffect(kind, l)
					})
				if lessonErr != nil {
					t.Fatalf("%s/%s: lesson: %v", kind, op, lessonErr)
				}
				ptr := e.pointer(r.course.ID)
				events := e.count(`SELECT count(*) FROM course_dependency_events WHERE course_id = $1`, r.course.ID)
				stale := e.count(`SELECT count(*) FROM course_dependency_events e JOIN courses c ON c.id = e.course_id
					WHERE e.course_id = $1 AND e.course_revision_id IS DISTINCT FROM c.published_revision_id`, r.course.ID)
				want := 1
				if kind == "withdraw" {
					want = 2 // revision_withdrawn and lesson_unpublished
				}
				switch {
				case courseErr == nil && ptr != nil && *ptr == target && events == want && stale == 0:
					first++
				case errors.Is(courseErr, courses.ErrIneligible) && ((op == "publish" && *ptr == r.rev.ID && events == want) ||
					(op == "restore" && ptr == nil && events == 0)):
					refused++
				default:
					t.Fatalf("%s/%s: course %v, pointer %v, events %d (stale %d)", kind, op, courseErr, ptr, events, stale)
				}
				if _, steps := e.readable(r.course.ID); steps[l.LessonID] {
					t.Fatalf("%s/%s: the affected step is readable", kind, op)
				}
			}
			t.Logf("%s/%s: course first %d, refused %d", kind, op, first, refused)
		}
	}
}

// A withdrawal, unpublication or archive racing readers: a reader never sees a step marked readable
// without its body or a body for an unreadable step, and once the
// withdrawal committed no reader sees the step readable.
func TestWithdrawalRacesCourseRead(t *testing.T) {
	e := newEnv(t)
	for _, kind := range []string{"withdraw", "unpublish", "archive"} {
		raceRead(t, e, kind)
	}
}

func raceRead(t *testing.T, e *env, kind string) {
	l := e.publishedLesson(e.other, e.math, "fr", "Read race "+kind)
	other := e.publishedLesson(e.other, e.math, "fr", "Unaffected "+kind)
	r := e.publishedCourse(l, other)
	var done atomic.Bool
	readErr := make(chan error, 1)
	go func() {
		for {
			finished := done.Load()
			rows, err := e.db.Query(context.Background(), `SELECT lesson_id, readable, content IS NOT NULL, lesson_title IS NOT NULL
				FROM reader_course_steps WHERE course_id = $1`, r.course.ID)
			if err != nil {
				readErr <- err
				return
			}
			n := 0
			for rows.Next() {
				var lesson int64
				var readable, body, title bool
				if err := rows.Scan(&lesson, &readable, &body, &title); err != nil {
					readErr <- err
					return
				}
				n++
				if readable != body || readable != title || (finished && lesson == l.LessonID && readable) {
					readErr <- fmt.Errorf("lesson %d readable %v body %v title %v after %v", lesson, readable, body, title, finished)
					return
				}
			}
			if n != 2 {
				readErr <- fmt.Errorf("outline has %d steps", n)
				return
			}
			if finished {
				readErr <- nil
				return
			}
		}
	}()
	if err := e.lessonEffect(kind, l); err != nil {
		t.Fatal(err)
	}
	done.Store(true)
	if err := <-readErr; err != nil {
		t.Fatal(err)
	}
	if ok, steps := e.readable(r.course.ID); !ok || steps[l.LessonID] || !steps[other.LessonID] {
		t.Fatalf("after: %v %v", ok, steps)
	}
}

// Two publications of one revision, and a publication racing a course
// revision withdrawal: exactly one applies.
func TestPublishRaces(t *testing.T) {
	e := newEnv(t)
	for i := 0; i < 4; i++ {
		l := e.publishedLesson(e.other, e.math, "fr", fmt.Sprintf("Publish race %d", i))
		r := e.readyCourse(l)
		sub := e.submit(r.rev)
		e.approveCourse(sub.ID)
		a, b := race(
			func() error { _, err := e.store.Publish(context.Background(), e.publisher, sub.ID, nil); return err },
			func() error { _, err := e.store.Publish(context.Background(), e.publisher, sub.ID, nil); return err })
		if (a == nil) == (b == nil) {
			t.Fatalf("publish/publish: %v, %v", a, b)
		}
		loser := a
		if loser == nil {
			loser = b
		}
		if !errors.Is(loser, courses.ErrStalePointer) && !errors.Is(loser, courses.ErrAlreadyPublished) {
			t.Fatalf("loser: %v", loser)
		}
		if n := e.count(`SELECT count(*) FROM course_publication_audit WHERE course_id = $1`, r.course.ID); n != 1 {
			t.Fatalf("audit rows %d", n)
		}

		// Publication racing withdrawal of the same approved revision.
		d, err := e.store.CreateDraft(e.ctx, e.owner, r.course.ID)
		if err != nil {
			t.Fatal(err)
		}
		dd := e.completeDraft(r.p.assignment, r.ch, []courses.StepInput{step(l, "Renamed")})
		dd.Modules[0].Key = d.Modules[0].Key
		d = e.submit(e.mustSave(d, dd))
		e.approveCourse(d.ID)
		pubErr, wErr := race(
			func() error { _, err := e.store.Publish(context.Background(), e.publisher, d.ID, &sub.ID); return err },
			func() error {
				if i%2 == 0 {
					time.Sleep(20 * time.Millisecond)
				}
				_, err := e.store.Withdraw(context.Background(), e.adminID(), d.ID, "Simulated.")
				return err
			})
		ptr := e.pointer(r.course.ID)
		switch {
		case pubErr == nil && errors.Is(wErr, courses.ErrPublished) && *ptr == d.ID:
		case wErr == nil && errors.Is(pubErr, courses.ErrWithdrawn) && *ptr == sub.ID:
		default:
			t.Fatalf("publish/withdraw: %v, %v, pointer %v", pubErr, wErr, *ptr)
		}
	}
}

// A review decision racing the revocation of the reviewer's assignment:
// either the decision commits first (and stays valid, as for lessons) or
// the revocation does and the decision is refused; never both refused or a
// decision recorded by a reviewer revoked before it.
func TestReviewRacesRevocation(t *testing.T) {
	e := newEnv(t)
	decided, refused := 0, 0
	for i := 0; i < 6; i++ {
		reviewer := e.user(fmt.Sprintf("racing-reviewer-%d", i))
		if _, err := e.perms.GrantReviewer(e.ctx, testOperator, reviewer, e.math); err != nil {
			t.Fatal(err)
		}
		l := e.publishedLesson(e.other, e.math, "fr", fmt.Sprintf("Review race %d", i))
		r := e.readyCourse(l)
		sub := e.submit(r.rev)
		approveErr, revokeErr := race(
			func() error { _, err := e.store.Approve(context.Background(), reviewer, sub.ID, ""); return err },
			func() error {
				if i%2 == 0 {
					time.Sleep(20 * time.Millisecond)
				}
				_, err := e.perms.RevokeReviewer(context.Background(), testOperator, reviewer, e.math)
				return err
			})
		if revokeErr != nil {
			t.Fatal(revokeErr)
		}
		decisions := e.count(`SELECT count(*) FROM course_review_decisions WHERE revision_id = $1`, sub.ID)
		switch {
		case approveErr == nil && decisions == 1:
			decided++
		case errors.Is(approveErr, courses.ErrNotFound) && decisions == 0:
			refused++
		default:
			t.Fatalf("approve %v with %d decisions", approveErr, decisions)
		}
	}
	t.Logf("decided %d, refused %d", decided, refused)
}
