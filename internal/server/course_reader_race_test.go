package server_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"testing"

	"github.com/hadden-me/learnoo/internal/coursetest"
	"github.com/hadden-me/learnoo/internal/database/dbtest"
	"github.com/hadden-me/learnoo/internal/reader"
)

// Lesson withdrawal racing course reads and course media reads (LMS-B08,
// contract §10 A07). Every read sees one snapshot: a readable step with its
// pinned content, or an unreadable step without any; once the withdrawal
// has committed, no read returns the content or the image.
func TestLessonWithdrawalRacingCourseReadAndMedia(t *testing.T) {
	rf := newReader(t)
	wf := withdrawFixture{reviewFixture: reviewFixture{studioFixture: studioFixture{env: rf.env, subject: rf.subject, author: rf.author},
		reviewer: rf.reviewer, publisher: rf.publisher}, admin: rf.admin}
	store := reader.NewStore(rf.db)
	ctx := context.Background()
	dbtest.Warm(t, rf.db, 6)
	for round := range 4 {
		img := rf.uploadAs(rf.author, uint8(150+round))
		w := rf.createWithMedia(rf.author, rf.subject, mediaDoc(fmt.Sprintf("TEST Course concurrente %d", round), false, img), []int64{img})
		wantStatus(t, "create", w, http.StatusCreated, "")
		rev := wf.submitRev(rf.author, decodeRevision(t, w))
		wf.approvedRev(rev)
		wf.publishedRev(rev)
		l := published{lessonID: rev.LessonID, revisionID: rev.ID, publicID: rf.publicID(rev.LessonID)}
		keep := rf.courseLesson(fmt.Sprintf("GARDÉE %d", round))
		c := coursetest.Publish(t, rf.db, rf.courseSpec(fmt.Sprintf("TEST Cours concurrent %d", round),
			coursetest.Module{Title: "TEST Module", Steps: []coursetest.Step{step(l, "TEST Étape retirée"), step(keep, "TEST Étape gardée")}}))

		var wg sync.WaitGroup
		errs := make(chan error, 64)
		withdrawn := make(chan struct{})
		read := func() {
			defer wg.Done()
			for i := 0; i < 8; i++ {
				after := false
				select {
				case <-withdrawn:
					after = true
				default:
				}
				cs, err := store.CourseStep(ctx, rf.student.id, c.PublicID, l.publicID)
				if err != nil {
					errs <- fmt.Errorf("course step: %w", err)
					return
				}
				if cs.Step.Readable != (cs.RevisionID == rev.ID) || cs.Step.Readable != (len(cs.Document.Blocks) > 0) {
					errs <- fmt.Errorf("inconsistent step: readable %v revision %d", cs.Step.Readable, cs.RevisionID)
					return
				}
				if after && cs.Step.Readable {
					errs <- errors.New("withdrawn step read after the withdrawal committed")
					return
				}
				_, err = store.CourseMedia(ctx, c.PublicID, l.publicID, img)
				switch {
				case err == nil && after:
					errs <- errors.New("withdrawn image served after the withdrawal committed")
					return
				case err != nil && !errors.Is(err, reader.ErrNotFound):
					errs <- fmt.Errorf("course media: %w", err)
					return
				}
				other, err := store.CourseStep(ctx, rf.student.id, c.PublicID, keep.publicID)
				if err != nil || !other.Step.Readable {
					errs <- fmt.Errorf("unaffected step: %v %v", other.Step.Readable, err)
					return
				}
			}
		}
		wg.Add(3)
		go read()
		go read()
		go func() {
			defer wg.Done()
			cur := rev.ID
			if _, err := rf.rs.Withdraw(ctx, rf.admin.id, rev.ID, "TEST retrait concurrent", &cur); err != nil {
				errs <- fmt.Errorf("withdraw: %w", err)
			}
			close(withdrawn)
		}()
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Fatalf("round %d: %v", round, err)
		}
		cs, err := store.CourseStep(ctx, rf.student.id, c.PublicID, l.publicID)
		if err != nil || cs.Step.Readable || cs.Document.Blocks != nil {
			t.Fatalf("round %d after: %+v %v", round, cs.Step, err)
		}
		if _, err := store.CourseMedia(ctx, c.PublicID, l.publicID, img); !errors.Is(err, reader.ErrNotFound) {
			t.Fatalf("round %d media after: %v", round, err)
		}
	}
}
