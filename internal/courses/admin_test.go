package courses_test

import (
	"errors"
	"testing"

	"github.com/hadden-me/learnoo/internal/courses"
	"github.com/hadden-me/learnoo/internal/permissions"
)

// LMS-B06 reads: the administrators' course list and detail, the suspended
// flag, the reviewer's name on decisions, earlier feedback and the pinned
// lesson content of the review payload (only while eligible).
func TestAdminReadsAndReviewContext(t *testing.T) {
	e := newEnv(t)
	pub := e.publishedLesson(e.other, e.math, "fr", "Published")
	r := e.readyCourse(pub)

	// Request changes, then a corrected revision: the reviewer sees the earlier feedback.
	sub := e.submit(r.rev)
	if _, err := e.store.RequestChanges(e.ctx, e.reviewer, sub.ID, "Simulated feedback."); err != nil {
		t.Fatal(err)
	}
	got, err := e.store.Revision(e.ctx, e.owner, sub.ID)
	if err != nil || got.Decision == nil || got.Decision.ReviewerName == "" || got.Decision.Action != "request_changes" {
		t.Fatalf("decision: %+v %v", got.Decision, err)
	}
	draft, err := e.store.CreateDraft(e.ctx, e.owner, r.course.ID)
	if err != nil {
		t.Fatal(err)
	}
	second := e.submit(draft)
	p, err := e.store.ReviewRevision(e.ctx, e.reviewer, second.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Earlier) != 1 || p.Earlier[0].RevisionID != sub.ID || p.Earlier[0].Comment == nil || *p.Earlier[0].Comment != "Simulated feedback." {
		t.Fatalf("earlier: %+v", p.Earlier)
	}
	if len(p.Steps) != 1 || !p.Steps[0].Eligible || len(p.Steps[0].Content) == 0 {
		t.Fatalf("step content: %+v", p.Steps)
	}
	e.approveCourse(second.ID)
	e.publishCourse(second.ID)

	// Administrators only.
	_, _, err = e.store.AdminCourses(e.ctx, e.owner, 0)
	wantErr(t, "author lists", err, permissions.ErrDenied)
	_, err = e.store.AdminCourseByID(e.ctx, e.reviewer, r.course.ID)
	wantErr(t, "reviewer reads", err, permissions.ErrDenied)

	admin := e.adminID()
	list, _, err := e.store.AdminCourses(e.ctx, admin, 0)
	if err != nil {
		t.Fatal(err)
	}
	var found *courses.AdminCourse
	for i := range list {
		if list[i].ID == r.course.ID {
			found = &list[i]
		}
	}
	if found == nil || found.Suspended || found.OwnerName == "" || found.Latest == nil || found.Latest.ID != second.ID {
		t.Fatalf("list: %+v", found)
	}

	// Suspension shows in the list and the owner's listing; the detail has revisions, history and transfer targets.
	if _, err := e.store.Suspend(e.ctx, admin, r.course.ID, &second.ID, "Simulated suspension."); err != nil {
		t.Fatal(err)
	}
	d, err := e.store.AdminCourseByID(e.ctx, admin, r.course.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !d.Suspended || d.PublishedRevisionID != nil || len(d.Revisions) != 2 || !d.Revisions[0].EverPublished || d.Revisions[1].EverPublished {
		t.Fatalf("detail: %+v", d)
	}
	if d.Revisions[1].Decision == nil || d.Revisions[1].Decision.Action != "request_changes" {
		t.Fatalf("revision decision: %+v", d.Revisions[1].Decision)
	}
	if len(d.History) != 2 || d.History[0].Action != "suspend" || d.History[0].ActorName == "" {
		t.Fatalf("history: %+v", d.History)
	}
	for _, a := range d.Authors {
		if a.ID == e.owner {
			t.Fatal("the owner is offered as a transfer target")
		}
	}
	if len(d.Authors) == 0 {
		t.Fatal("no transfer target")
	}
	owned, _, err := e.store.Owned(e.ctx, e.owner, 0)
	if err != nil || len(owned) == 0 || !owned[0].Suspended || owned[0].Latest == nil || !owned[0].Latest.EverPublished {
		t.Fatalf("owned: %+v %v", owned, err)
	}
	_, err = e.store.AdminCourseByID(e.ctx, admin, r.course.ID+1000)
	if !errors.Is(err, courses.ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}

	// A withdrawn pinned lesson revision is never sent as content.
	third, err := e.store.CreateDraft(e.ctx, e.owner, r.course.ID)
	if err != nil {
		t.Fatal(err)
	}
	third = e.submit(third)
	if _, err := e.reviewStore().Withdraw(e.ctx, admin, pub.ID, "Simulated withdrawal.", &pub.ID); err != nil {
		t.Fatal(err)
	}
	p, err = e.store.ReviewRevision(e.ctx, e.reviewer, third.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Steps) != 1 || p.Steps[0].Eligible || p.Steps[0].Content != nil {
		t.Fatalf("withdrawn step: %+v", p.Steps[0])
	}
}
