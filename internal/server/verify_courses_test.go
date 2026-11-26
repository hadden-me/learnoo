package server_test

import (
	"context"
	"testing"

	"github.com/hadden-me/learnoo/internal/courses"
	"github.com/hadden-me/learnoo/internal/coursetest"
)

// verify-restore's course checks (internal/backupcheck) report nothing for
// the legitimate course states the workflow produces: a published course
// with a published amendment, a suspended course, an archived course, a
// course whose successor in review was withdrawn, and courses never
// published (one in review, one a draft).
func TestVerifyRestoreAcceptsEveryCourseState(t *testing.T) {
	f := newReader(t)
	ctx := context.Background()
	store := courses.NewStore(f.db)
	lesson := f.publishNew("TEST Leçon de cours vérifiée", "fr", nil)
	spec := func(title string) coursetest.Spec {
		return coursetest.Spec{Admin: f.admin.id, Owner: f.author.id, Reviewer: f.reviewer.id, Publisher: f.publisher.id,
			SubjectID: f.subject, Title: title,
			Modules: []coursetest.Module{{Title: "TEST Module", Steps: []coursetest.Step{{LessonID: lesson.lessonID, RevisionID: lesson.revisionID, Title: "TEST Étape"}}}}}
	}
	submit := func(rev courses.Revision, d courses.Draft) courses.Revision {
		t.Helper()
		rev, err := store.SaveDraft(ctx, f.author.id, rev.ID, rev.Version, d)
		if err != nil {
			t.Fatal(err)
		}
		if rev, err = store.Submit(ctx, f.author.id, rev.ID, rev.Version); err != nil {
			t.Fatal(err)
		}
		return rev
	}

	// Published with a published amendment (a newer revision of its lesson).
	amended := coursetest.Publish(t, f.db, spec("TEST Cours amendé"))
	newer := f.publish(f.approved(lesson.lessonID, readerDoc("TEST Leçon de cours vérifiée, révision 2", "fr"), "fr", nil))
	am, err := store.CreateAmendment(ctx, f.author.id, amended.ID, []courses.Replacement{{LessonID: lesson.lessonID, RevisionID: newer.revisionID}})
	if err != nil {
		t.Fatal(err)
	}
	if am, err = store.Submit(ctx, f.author.id, am.ID, am.Version); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Approve(ctx, f.reviewer.id, am.ID, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Publish(ctx, f.publisher.id, am.ID, &amended.RevisionID); err != nil {
		t.Fatal(err)
	}

	coursetest.Suspend(t, f.db, f.admin.id, coursetest.Publish(t, f.db, spec("TEST Cours suspendu")))
	coursetest.Archive(t, f.db, f.admin.id, coursetest.Publish(t, f.db, spec("TEST Cours archivé")))

	// A successor in review, withdrawn by an administrator.
	withdrawn := coursetest.Publish(t, f.db, spec("TEST Cours avec successeur retiré"))
	draft, err := store.CreateDraft(ctx, f.author.id, withdrawn.ID)
	if err != nil {
		t.Fatal(err)
	}
	successor := submit(draft, courses.Draft{Title: "TEST Successeur", Summary: "Résumé de test.", Objectives: "Objectifs de test.",
		AssignmentIDs: []int64{withdrawn.AssignmentID}, Modules: []courses.ModuleInput{{Title: "TEST Module",
			Steps: []courses.StepInput{{LessonID: lesson.lessonID, RevisionID: newer.revisionID, Title: "TEST Étape"}}}}})
	if _, err := store.Withdraw(ctx, f.admin.id, successor.ID, "TEST retrait du successeur."); err != nil {
		t.Fatal(err)
	}

	// Never published: one in review, one still a draft.
	_, inReview, err := store.CreateCourse(ctx, f.author.id, f.subject, "fr")
	if err != nil {
		t.Fatal(err)
	}
	submit(inReview, courses.Draft{Title: "TEST Cours jamais publié", Summary: "Résumé de test.", Objectives: "Objectifs de test.",
		AssignmentIDs: []int64{withdrawn.AssignmentID}, Modules: []courses.ModuleInput{{Title: "TEST Module",
			Steps: []courses.StepInput{{LessonID: lesson.lessonID, RevisionID: newer.revisionID, Title: "TEST Étape"}}}}})
	if _, _, err := store.CreateCourse(ctx, f.author.id, f.subject, "fr"); err != nil {
		t.Fatal(err)
	}

	r := verifyRestore(t, f.db, t.TempDir())
	if len(r.Problems) != 0 {
		t.Fatalf("verify-restore problems on legitimate course states: %q", r.Problems)
	}
	if r.Courses != 2 { // the amended course and the one with a withdrawn successor
		t.Fatalf("published courses: %d", r.Courses)
	}
	var states struct{ suspended, archived, withdrawn, amendments, inReview, drafts int }
	if err := f.db.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM course_publication_audit WHERE action = 'suspend'),
		(SELECT count(*) FROM courses WHERE archived_at IS NOT NULL),
		(SELECT count(*) FROM course_revisions WHERE withdrawn_at IS NOT NULL),
		(SELECT count(*) FROM course_revisions r JOIN courses c ON c.published_revision_id = r.id WHERE r.kind = 'amendment'),
		(SELECT count(*) FROM course_revisions WHERE status = 'in_review' AND withdrawn_at IS NULL),
		(SELECT count(*) FROM course_revisions WHERE status = 'draft')`).
		Scan(&states.suspended, &states.archived, &states.withdrawn, &states.amendments, &states.inReview, &states.drafts); err != nil {
		t.Fatal(err)
	}
	if states != (struct{ suspended, archived, withdrawn, amendments, inReview, drafts int }{1, 1, 1, 1, 1, 1}) {
		t.Fatalf("course states not all produced: %+v", states)
	}
}
