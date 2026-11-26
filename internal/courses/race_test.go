package courses_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/hadden-me/learnoo/internal/courses"
)

// race runs a and b concurrently and returns both results.
func race(a, b func() error) (error, error) {
	ea, eb := make(chan error, 1), make(chan error, 1)
	start := make(chan struct{})
	go func() { <-start; ea <- a() }()
	go func() { <-start; eb <- b() }()
	close(start)
	return <-ea, <-eb
}

// A save pinning another author's eligible revision racing that lesson's
// withdrawal, its unpublication and its archive (LMS-B04): the lesson change always
// applies; the save either commits first or, seeing the change, is refused
// with ErrInvalidStep. Never a deadlock, a retry or a guard error.
func TestSaveRacesLessonWithdrawalAndUnpublish(t *testing.T) {
	e := newEnv(t)
	for _, kind := range []string{"withdraw", "unpublish", "archive"} {
		saved, refused := 0, 0
		for i := 0; i < 8; i++ {
			foreign := e.publishedLesson(e.other, e.math, "fr", fmt.Sprintf("Foreign %s %d", kind, i))
			_, rev := e.course(e.math, "fr")
			d := courses.Draft{Modules: []courses.ModuleInput{{Title: "m", Steps: []courses.StepInput{step(foreign, "s")}}}}
			saveErr, lessonErr := race(
				func() error { _, err := e.save(rev, d); return err },
				func() error {
					if i%2 == 0 { // let the save start first every other round
						time.Sleep(20 * time.Millisecond)
					}
					return e.lessonEffect(kind, foreign)
				})
			if lessonErr != nil {
				t.Fatalf("%s: %v", kind, lessonErr)
			}
			steps := e.count(`SELECT count(*) FROM course_steps WHERE course_revision_id = $1`, rev.ID)
			switch {
			case saveErr == nil && steps == 1:
				saved++
			case errors.Is(saveErr, courses.ErrInvalidStep) && steps == 0:
				refused++
			default:
				t.Fatalf("%s: save %v with %d steps", kind, saveErr, steps)
			}
			// Afterwards the step can no longer be pinned.
			cur, _ := e.store.Revision(e.ctx, e.owner, rev.ID)
			_, err := e.save(cur, d)
			wantErr(t, kind+" then save", err, courses.ErrInvalidStep)
			e.mustSave(cur, courses.Draft{})
		}
		t.Logf("%s: save first %d, refused %d", kind, saved, refused)
	}
}

// A save naming an assignment racing MakeCurrent and RetireEdition of its
// edition: the edition change always applies; the save sees the edition
// either before (draft: refused; current: saved) or after (current: saved;
// retired: refused), never a mix.
func TestSaveRacesEditionStateChange(t *testing.T) {
	e := newEnv(t)
	for i := 0; i < 6; i++ {
		ch := e.chapter(e.math, fmt.Sprintf("race-%d", i))
		p := e.edition(e.math, []int64{ch}, false)
		_, rev := e.course(e.math, "fr")
		d := courses.Draft{AssignmentIDs: []int64{p.assignment}, Modules: []courses.ModuleInput{{Title: "m", ChapterID: &ch}}}
		saveErr, edErr := race(
			func() error { _, err := e.save(rev, d); return err },
			func() error {
				_, _, err := e.cur.MakeCurrent(context.Background(), e.admin, p.edition.ID, p.edition.Version, nil)
				return err
			})
		if edErr != nil {
			t.Fatalf("make current: %v", edErr)
		}
		if saveErr != nil && !errors.Is(saveErr, courses.ErrPlacementInvalid) {
			t.Fatalf("save racing make-current: %v", saveErr)
		}
		// Now current: a save succeeds; then race retirement.
		cur, _ := e.store.Revision(e.ctx, e.owner, rev.ID)
		cur = e.mustSave(cur, d)
		saveErr, edErr = race(
			func() error { _, err := e.save(cur, d); return err },
			func() error {
				_, err := e.cur.RetireEdition(context.Background(), e.admin, p.edition.ID, e.versionOf(p.edition.ID))
				return err
			})
		if edErr != nil {
			t.Fatalf("retire: %v", edErr)
		}
		if saveErr != nil && !errors.Is(saveErr, courses.ErrPlacementInvalid) {
			t.Fatalf("save racing retirement: %v", saveErr)
		}
		got, _ := e.store.Revision(e.ctx, e.owner, rev.ID)
		if saveErr == nil && got.Version != cur.Version+1 {
			t.Fatalf("save reported success but version %d", got.Version)
		}
	}
}

// A chapter retirement in flight holds the chapter row; a save newly
// selecting it waits on its share lock, then sees the retirement and is
// refused, instead of committing a retired selection.
func TestSaveWaitsForChapterRetirement(t *testing.T) {
	e := newEnv(t)
	ch := e.chapter(e.math, "ch-01")
	p := e.edition(e.math, []int64{ch}, true)
	_, rev := e.course(e.math, "fr")
	tx, err := e.db.Begin(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(e.ctx) }()
	for _, sql := range []string{
		fmt.Sprintf(`INSERT INTO curriculum_audit (operator_label, target, target_id, action, before, after) VALUES ('test-operator@test-host', 'chapter', %d, 'retire', '{"retired": false}', '{"retired": true}')`, ch),
		fmt.Sprintf(`UPDATE chapters SET retired_at = now() WHERE id = %d`, ch),
	} {
		if _, err := tx.Exec(e.ctx, sql); err != nil {
			t.Fatal(err)
		}
	}
	done := make(chan error, 1)
	go func() {
		_, err := e.store.SaveDraft(context.Background(), e.owner, rev.ID, rev.Version,
			courses.Draft{AssignmentIDs: []int64{p.assignment}, Modules: []courses.ModuleInput{{Title: "m", ChapterID: &ch}}})
		done <- err
	}()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var waiting bool
		if err := e.db.QueryRow(e.ctx, `SELECT EXISTS (SELECT 1 FROM pg_locks WHERE locktype = 'tuple' AND relation = 'chapters'::regclass)`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("save did not wait for the retirement: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("save never waited on the chapter")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := tx.Commit(e.ctx); err != nil {
		t.Fatal(err)
	}
	wantErr(t, "save after retirement", <-done, courses.ErrPlacementInvalid)
}

// Owned pages by course ID, newest first.
func TestOwnedPages(t *testing.T) {
	e := newEnv(t)
	for i := 0; i < courses.OwnedPage+1; i++ {
		e.course(e.math, "fr")
	}
	first, next, err := e.store.Owned(e.ctx, e.owner, 0)
	if err != nil || len(first) != courses.OwnedPage || next == nil || *next != first[len(first)-1].ID || first[0].ID < first[1].ID {
		t.Fatalf("first page: %d %v %v", len(first), next, err)
	}
	second, next2, err := e.store.Owned(e.ctx, e.owner, *next)
	if err != nil || len(second) != 1 || next2 != nil || second[0].ID >= *next {
		t.Fatalf("second page: %+v %v %v", second, next2, err)
	}
}

// A correction draft leaves out steps whose foreign lesson is no longer
// attachable, and lists them.
func TestCreateDraftDropsUnattachableSteps(t *testing.T) {
	e := newEnv(t)
	own := e.lessonDraft(e.owner, e.math, "fr", "Own")
	foreign := e.publishedLesson(e.other, e.math, "fr", "Foreign")
	c, rev := e.course(e.math, "fr")
	saved := e.mustSave(rev, courses.Draft{Modules: []courses.ModuleInput{{Title: "m", Steps: []courses.StepInput{step(own, "a"), step(foreign, "b")}}}})
	e.forceStatus(saved.ID, `status = 'changes_requested', submitted_at = now()`)
	e.unpublish(foreign.LessonID)
	next, err := e.store.CreateDraft(e.ctx, e.owner, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(next.Modules[0].Steps) != 1 || next.Modules[0].Steps[0].LessonID != own.LessonID ||
		len(next.DroppedSteps) != 1 || next.DroppedSteps[0].LessonID != foreign.LessonID || next.DroppedSteps[0].Title != "b" {
		t.Fatalf("draft: %+v dropped %+v", next.Modules, next.DroppedSteps)
	}
}
