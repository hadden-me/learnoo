package server_test

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/hadden-me/learnoo/internal/courses"
	"github.com/hadden-me/learnoo/internal/coursetest"
)

// LMS-A06: while a successor revision of a published course is drafted, in
// review and approved, students keep reading the published revision; a
// refused publication (stale expected pointer) changes nothing; only the
// successor's publication changes what students read.
func TestStudentsKeepThePublishedCourseUntilTheSuccessorIsPublished(t *testing.T) {
	f := newReader(t)
	ctx := context.Background()
	store := courses.NewStore(f.db)
	lesson := f.publishNew("TEST Leçon du cours successeur", "fr", nil)
	c := coursetest.Publish(t, f.db, coursetest.Spec{Admin: f.admin.id, Owner: f.author.id, Reviewer: f.reviewer.id, Publisher: f.publisher.id,
		SubjectID: f.subject, Title: "TEST Cours version publiée",
		Modules: []coursetest.Module{{Title: "TEST Module publié", Steps: []coursetest.Step{{LessonID: lesson.lessonID, RevisionID: lesson.revisionID, Title: "TEST Étape publiée"}}}}})
	reads := func(what string, want, absent string) {
		t.Helper()
		w := f.get(f.student, coursePath(c.PublicID))
		wantPage(t, what, w, http.StatusOK, want)
		wantAbsent(t, what, w, absent)
	}

	rev, err := store.CreateDraft(ctx, f.author.id, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rev, err = store.SaveDraft(ctx, f.author.id, rev.ID, rev.Version, courses.Draft{
		Title: "TEST Cours version suivante", Summary: "Résumé de test.", Objectives: "Objectifs de test.", AssignmentIDs: []int64{c.AssignmentID},
		Modules: []courses.ModuleInput{{Title: "TEST Module suivant", Steps: []courses.StepInput{{LessonID: lesson.lessonID, RevisionID: lesson.revisionID, Title: "TEST Étape suivante"}}}},
	}); err != nil {
		t.Fatal(err)
	}
	reads("successor drafted", "TEST Cours version publiée", "TEST Cours version suivante")
	if rev, err = store.Submit(ctx, f.author.id, rev.ID, rev.Version); err != nil {
		t.Fatal(err)
	}
	reads("successor in review", "TEST Étape publiée", "TEST Étape suivante")
	if _, err := store.Approve(ctx, f.reviewer.id, rev.ID, ""); err != nil {
		t.Fatal(err)
	}
	reads("successor approved", "TEST Module publié", "TEST Module suivant")

	// A publication against a stale expected pointer is refused and changes nothing.
	stale := rev.ID
	if _, err := store.Publish(ctx, f.publisher.id, rev.ID, &stale); !errors.Is(err, courses.ErrStalePointer) {
		t.Fatalf("publication with a stale expected pointer: %v", err)
	}
	reads("refused publication", "TEST Cours version publiée", "TEST Cours version suivante")
	var pointer int64
	if err := f.db.QueryRow(ctx, `SELECT published_revision_id FROM courses WHERE id = $1`, c.ID).Scan(&pointer); err != nil || pointer != c.RevisionID {
		t.Fatalf("pointer after the refused publication: %d %v", pointer, err)
	}

	if _, err := store.Publish(ctx, f.publisher.id, rev.ID, &c.RevisionID); err != nil {
		t.Fatal(err)
	}
	reads("successor published", "TEST Cours version suivante", "TEST Cours version publiée")
}
