package courses_test

import (
	"errors"
	"testing"

	"github.com/hadden-me/learnoo/internal/courses"
	"github.com/hadden-me/learnoo/internal/permissions"
)

// Submission (LMS-B04): completeness, eligibility, placement, the audit
// row of the same transaction and the content left unchanged.
func TestSubmit(t *testing.T) {
	e := newEnv(t)
	pub := e.publishedLesson(e.other, e.math, "fr", "Published")
	ownDraft := e.lessonDraft(e.owner, e.math, "fr", "Own draft")

	// Incomplete: nothing filled in.
	_, empty := e.course(e.math, "fr")
	_, err := e.store.Submit(e.ctx, e.owner, empty.ID, empty.Version)
	var inc *courses.IncompleteError
	if !errors.As(err, &inc) || len(inc.Fields) != 4 {
		t.Fatalf("empty: %v", err)
	}
	// A module without a step is incomplete too.
	r := e.readyCourse(pub)
	d := e.completeDraft(r.p.assignment, r.ch, []courses.StepInput{step(pub, "s")})
	d.Modules = append(d.Modules, courses.ModuleInput{Title: "Empty"})
	rev := e.mustSave(r.rev, d)
	_, err = e.store.Submit(e.ctx, e.owner, rev.ID, rev.Version)
	if !errors.As(err, &inc) || len(inc.Fields) != 1 {
		t.Fatalf("empty module: %v", err)
	}
	// Ineligible: the owner's own draft lesson.
	rev = e.mustSave(rev, e.completeDraft(r.p.assignment, r.ch, []courses.StepInput{step(pub, "a"), step(ownDraft, "b")}))
	_, err = e.store.Submit(e.ctx, e.owner, rev.ID, rev.Version)
	var ine *courses.IneligibleError
	if !errors.As(err, &ine) || len(ine.Steps) != 1 || ine.Steps[0].LessonID != ownDraft.LessonID {
		t.Fatalf("ineligible: %v", err)
	}
	// Stale version, another user's revision.
	rev = e.mustSave(rev, e.completeDraft(r.p.assignment, r.ch, []courses.StepInput{step(pub, "a")}))
	_, err = e.store.Submit(e.ctx, e.owner, rev.ID, rev.Version-1)
	wantErr(t, "stale version", err, courses.ErrVersionConflict)
	_, err = e.store.Submit(e.ctx, e.other, rev.ID, rev.Version)
	wantErr(t, "another author", err, courses.ErrNotFound)

	sub := e.submit(rev)
	if sub.Status != courses.StatusInReview || sub.Version != rev.Version+1 || sub.Title != rev.Title || sub.Kind != "ordinary" {
		t.Fatalf("submitted: %+v", sub)
	}
	if n := e.count(`SELECT count(*) FROM course_submission_audit WHERE revision_id = $1 AND actor_id = $2 AND submitted_at = $3`,
		sub.ID, e.owner, *sub.SubmittedAt); n != 1 {
		t.Fatalf("audit rows %d", n)
	}
	_, err = e.store.Submit(e.ctx, e.owner, sub.ID, sub.Version)
	wantErr(t, "twice", err, courses.ErrNotDraft)
	_, err = e.store.SaveDraft(e.ctx, e.owner, sub.ID, sub.Version, courses.Draft{})
	wantErr(t, "save submitted", err, courses.ErrNotDraft)
	// A submitted revision is history: the course is never deleted.
	wantErr(t, "delete", e.store.DeleteCourse(e.ctx, e.owner, r.course.ID), courses.ErrHasHistory)
	if e.count(`SELECT count(*) FROM lessons WHERE id = $1`, pub.LessonID) != 1 {
		t.Fatal("lesson lost")
	}
}

// A04 / A12: independent review.
func TestIndependentReview(t *testing.T) {
	e := newEnv(t)
	// A reviewer of math who also authors lessons.
	authorReviewer := e.user("author-reviewer", permissions.Author)
	if _, err := e.perms.GrantReviewer(e.ctx, testOperator, authorReviewer, e.math); err != nil {
		t.Fatal(err)
	}
	// The course owner is a reviewer of math too.
	if _, err := e.perms.GrantReviewer(e.ctx, testOperator, e.owner, e.math); err != nil {
		t.Fatal(err)
	}
	physicsOnly := e.user("physics-reviewer")
	if _, err := e.perms.GrantReviewer(e.ctx, testOperator, physicsOnly, e.physics); err != nil {
		t.Fatal(err)
	}
	theirs := e.publishedLesson(authorReviewer, e.math, "fr", "By the author-reviewer")
	other := e.publishedLesson(e.other, e.math, "fr", "By other")

	r := e.readyCourse(theirs, other)
	sub := e.submit(r.rev)
	// Course author self-approval: refused even as a reviewer of the subject.
	_, err := e.store.Approve(e.ctx, e.owner, sub.ID, "")
	var re *courses.ReviewerError
	if !errors.As(err, &re) || re.Reasons[0].Reason != "course_author" {
		t.Fatalf("self approval: %v", err)
	}
	// Author of an added lesson revision: refused.
	_, err = e.store.Approve(e.ctx, authorReviewer, sub.ID, "")
	if !errors.As(err, &re) || re.Reasons[0].Reason != "lesson_author" || *re.Reasons[0].LessonID != theirs.LessonID {
		t.Fatalf("lesson author: %v", err)
	}
	// Wrong subject: indistinguishable from a missing revision.
	_, err = e.store.Approve(e.ctx, physicsOnly, sub.ID, "")
	wantErr(t, "wrong subject", err, courses.ErrNotFound)
	// Change requests need a comment.
	_, err = e.store.RequestChanges(e.ctx, e.reviewer, sub.ID, "  ")
	wantErr(t, "no comment", err, courses.ErrCommentRequired)
	// The queue: eligible for the independent reviewer, not for the others.
	q, err := e.store.ReviewQueue(e.ctx, e.reviewer, 0)
	if err != nil || len(q) != 1 || !q[0].Eligible || q[0].Blocked != nil {
		t.Fatalf("queue: %+v %v", q, err)
	}
	q, err = e.store.ReviewQueue(e.ctx, authorReviewer, 0)
	if err != nil || len(q) != 1 || q[0].Eligible {
		t.Fatalf("queue of the lesson author: %+v %v", q, err)
	}
	// Changes requested, then a correction draft can start.
	if _, err := e.store.RequestChanges(e.ctx, e.reviewer, sub.ID, "Simulated feedback."); err != nil {
		t.Fatal(err)
	}
	_, err = e.store.Approve(e.ctx, e.reviewer, sub.ID, "")
	wantErr(t, "decided twice", err, courses.ErrNotInReview)
	fix, err := e.store.CreateDraft(e.ctx, e.owner, r.course.ID)
	if err != nil || len(fix.Modules[0].Steps) != 2 {
		t.Fatalf("correction draft: %+v %v", fix, err)
	}
	owner, _ := e.store.Revision(e.ctx, e.owner, sub.ID)
	if owner.Decision == nil || owner.Decision.Action != "request_changes" || *owner.Decision.Comment != "Simulated feedback." {
		t.Fatalf("owner sees the feedback: %+v", owner.Decision)
	}
	fix = e.submit(fix)
	e.approveCourse(fix.ID)
	e.publishCourse(fix.ID)

	// An amendment replacing only the other author's step: the lesson
	// author of the unchanged step may now approve it.
	newer := e.newerLessonRevision(other, "By other, corrected")
	am, err := e.store.CreateAmendment(e.ctx, e.owner, r.course.ID, nil)
	if err != nil || am.Kind != "amendment" {
		t.Fatalf("amendment: %+v %v", am, err)
	}
	am = e.submit(am)
	p, err := e.store.ReviewRevision(e.ctx, authorReviewer, am.ID)
	if err != nil || !p.Eligible || p.MetadataChanged {
		t.Fatalf("payload: %+v %v", p, err)
	}
	var changed int
	for _, s := range p.Steps {
		switch s.Change {
		case "replaced":
			changed++
			if s.LessonID != other.LessonID || *s.PreviousRevisionID != other.ID || s.RevisionID != newer.ID || s.Diff == nil || len(s.Diff.Fields) == 0 {
				t.Fatalf("replaced step: %+v", s)
			}
		case "unchanged":
		default:
			t.Fatalf("step %+v", s)
		}
	}
	if changed != 1 {
		t.Fatalf("changed steps %d", changed)
	}
	if _, err := e.store.Approve(e.ctx, authorReviewer, am.ID, ""); err != nil {
		t.Fatalf("author of an unchanged lesson approves an amendment: %v", err)
	}
	e.publishCourse(am.ID)
}

// No eligible reviewer: the revision stays in review, is reported blocked
// with the reasons to the author, reviewers and administrators, and no role
// can publish it.
func TestNoEligibleReviewerBlocks(t *testing.T) {
	e := newEnv(t)
	// The only reviewer of physics authored the lesson.
	lone := e.user("lone", permissions.Author)
	if _, err := e.perms.GrantReviewer(e.ctx, testOperator, lone, e.physics); err != nil {
		t.Fatal(err)
	}
	if _, err := e.perms.RevokeReviewer(e.ctx, testOperator, e.reviewer, e.physics); err != nil {
		t.Fatal(err)
	}
	lesson := e.lessonDraft(lone, e.physics, "fr", "Physics lesson")
	other := e.user("other-reviewer")
	if _, err := e.perms.GrantReviewer(e.ctx, testOperator, other, e.physics); err != nil {
		t.Fatal(err)
	}
	e.approveBy(lesson, other)
	e.publish(lesson)
	if _, err := e.perms.RevokeReviewer(e.ctx, testOperator, other, e.physics); err != nil {
		t.Fatal(err)
	}

	ch := e.chapter(e.physics, "ph-1")
	pl := e.edition(e.physics, []int64{ch}, true)
	c, rev := e.course(e.physics, "fr")
	rev = e.mustSave(rev, e.completeDraft(pl.assignment, ch, []courses.StepInput{step(lesson, "s")}))
	sub := e.submit(rev)
	if sub.Blocked == nil || len(sub.Blocked.Reasons) != 1 || sub.Blocked.Reasons[0].Reason != "lesson_author" ||
		sub.Blocked.Reasons[0].ReviewerID != lone {
		t.Fatalf("author view: %+v", sub.Blocked)
	}
	items, _, err := e.store.BlockedReviews(e.ctx, e.adminID(), 0)
	if err != nil || len(items) != 1 || items[0].RevisionID != sub.ID {
		t.Fatalf("administrators: %+v %v", items, err)
	}
	q, err := e.store.ReviewQueue(e.ctx, lone, 0)
	if err != nil || len(q) != 1 || q[0].Eligible || q[0].Blocked == nil {
		t.Fatalf("queue: %+v %v", q, err)
	}
	_, err = e.store.Approve(e.ctx, lone, sub.ID, "")
	wantErr(t, "lone approves", err, courses.ErrNotEligibleReviewer)
	for _, actor := range []int64{e.publisher, e.adminID()} {
		_, err = e.store.Publish(e.ctx, actor, sub.ID, nil)
		if err == nil {
			t.Fatal("published without approval")
		}
	}
	// With nobody assigned at all, the reason says so.
	if _, err := e.perms.RevokeReviewer(e.ctx, testOperator, lone, e.physics); err != nil {
		t.Fatal(err)
	}
	got, _ := e.store.Revision(e.ctx, e.owner, sub.ID)
	if got.Blocked == nil || got.Blocked.Reasons[0].Reason != "no_subject_reviewer" {
		t.Fatalf("no reviewer: %+v", got.Blocked)
	}
	if e.pointer(c.ID) != nil {
		t.Fatal("published")
	}
}
