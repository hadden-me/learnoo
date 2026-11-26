package courses_test

import (
	"fmt"
	"testing"

	"github.com/hadden-me/learnoo/internal/courses"
	"github.com/hadden-me/learnoo/internal/database/dbtest"
	"github.com/hadden-me/learnoo/internal/permissions"
)

// A draft created while suspended compares with the last published
// revision; if a restore changes the comparison before publication, the
// draft is refused as stale (its review applied rule 3 against another
// revision), and a new draft is reviewed against the right comparison.
func TestPublishRefusesStaleComparison(t *testing.T) {
	e := newEnv(t)
	lessonAuthor := e.user("lesson-author", permissions.Author)
	if _, err := e.perms.GrantReviewer(e.ctx, testOperator, lessonAuthor, e.math); err != nil {
		t.Fatal(err)
	}
	a1 := e.publishedLesson(lessonAuthor, e.math, "fr", "A")
	r := e.publishedCourse(a1) // v1 pins a1
	a2 := e.newerLessonRevision(a1, "A v2")
	v2, err := e.store.CreateAmendment(e.ctx, e.owner, r.course.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	v2 = e.submit(v2)
	e.approveCourse(v2.ID)
	e.publishCourse(v2.ID) // v2 pins a2
	if _, err := e.store.Suspend(e.ctx, e.adminID(), r.course.ID, &v2.ID, "Simulated."); err != nil {
		t.Fatal(err)
	}
	// Drafted while suspended: compared with v2, where a2 is unchanged, so
	// the author of a2 may approve it.
	d3, err := e.store.CreateDraft(e.ctx, e.owner, r.course.ID)
	if err != nil || *d3.CompareRevisionID != v2.ID || d3.BaseRevisionID != nil {
		t.Fatalf("draft while suspended: %+v %v", d3, err)
	}
	dd := e.completeDraft(r.p.assignment, r.ch, []courses.StepInput{step(a2, "Renamed")})
	dd.Modules[0].Key = d3.Modules[0].Key
	d3 = e.submit(e.mustSave(d3, dd))
	if _, err := e.store.Approve(e.ctx, lessonAuthor, d3.ID, ""); err != nil {
		t.Fatalf("approval against v2: %v", err)
	}
	// Restore v1, suspend again: the comparison is now v1.
	if _, err := e.store.Restore(e.ctx, e.publisher, r.course.ID, r.rev.ID, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.Suspend(e.ctx, e.adminID(), r.course.ID, &r.rev.ID, "Simulated."); err != nil {
		t.Fatal(err)
	}
	q, err := e.store.PublishQueue(e.ctx, e.publisher)
	if err != nil || len(q) != 1 || !q[0].StaleBase() {
		t.Fatalf("queue: %+v %v", q, err)
	}
	_, err = e.store.Publish(e.ctx, e.publisher, d3.ID, nil)
	wantErr(t, "stale comparison", err, courses.ErrStaleBase)
	// The guard refuses it too.
	tx, err := e.db.Begin(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, err = tx.Exec(e.ctx, `INSERT INTO course_publication_audit (course_id, revision_id, actor_id, action) VALUES ($1, $2, $3, 'publish')`,
		r.course.ID, d3.ID, e.publisher)
	_ = tx.Rollback(e.ctx)
	wantGuard(t, "guard", err, "stale base")
	if e.pointer(r.course.ID) != nil {
		t.Fatal("published")
	}
	// Released, a new draft compares with v1: a2 is a replacement there,
	// so its author may not approve.
	if _, err := e.store.Withdraw(e.ctx, e.adminID(), d3.ID, "Simulated release."); err != nil {
		t.Fatal(err)
	}
	d4, err := e.store.CreateDraft(e.ctx, e.owner, r.course.ID)
	if err != nil || *d4.CompareRevisionID != r.rev.ID {
		t.Fatalf("new draft: %+v %v", d4, err)
	}
	d4 = e.submit(d4)
	_, err = e.store.Approve(e.ctx, lessonAuthor, d4.ID, "")
	wantErr(t, "author of a replaced lesson revision", err, courses.ErrNotEligibleReviewer)
}

// After the last published revision is withdrawn, the comparison falls
// back to the previous published revision, and an amendment amends it.
func TestAmendAfterWithdrawingLastPublished(t *testing.T) {
	e := newEnv(t)
	a := e.publishedLesson(e.other, e.math, "fr", "A")
	r := e.publishedCourse(a)
	d, err := e.store.CreateDraft(e.ctx, e.owner, r.course.ID)
	if err != nil {
		t.Fatal(err)
	}
	dd := e.completeDraft(r.p.assignment, r.ch, []courses.StepInput{step(a, "Renamed")})
	dd.Modules[0].Key = d.Modules[0].Key
	v2 := e.submit(e.mustSave(d, dd))
	e.approveCourse(v2.ID)
	e.publishCourse(v2.ID)
	if _, err := e.store.Suspend(e.ctx, e.adminID(), r.course.ID, &v2.ID, "Simulated."); err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.Withdraw(e.ctx, e.adminID(), v2.ID, "Simulated."); err != nil {
		t.Fatal(err)
	}
	a2 := e.newerLessonRevision(a, "A v2")
	n, err := e.store.NewerRevisions(e.ctx, e.owner, r.course.ID)
	if err != nil || *n.ComparisonRevision != r.rev.ID || len(n.Steps) != 1 {
		t.Fatalf("notice: %+v %v", n, err)
	}
	am, err := e.store.CreateAmendment(e.ctx, e.owner, r.course.ID, nil)
	if err != nil || *am.CompareRevisionID != r.rev.ID || am.Modules[0].Steps[0].Title != "Step 1" || am.Modules[0].Steps[0].RevisionID != a2.ID {
		t.Fatalf("amendment: %+v %v", am, err)
	}
	am = e.submit(am)
	e.approveCourse(am.ID)
	if _, err := e.store.Publish(e.ctx, e.publisher, am.ID, nil); err != nil {
		t.Fatalf("publish the amendment: %v", err)
	}
	_, err = e.store.NewerRevisions(e.ctx, e.adminID(), 987654)
	wantErr(t, "notice of a missing course", err, courses.ErrNotFound)
}

// The review payload reports moved and retitled steps.
func TestReviewPayloadReportsStepStructure(t *testing.T) {
	e := newEnv(t)
	a := e.publishedLesson(e.other, e.math, "fr", "A")
	b := e.publishedLesson(e.other, e.math, "fr", "B")
	r := e.publishedCourse(a, b)
	d, err := e.store.CreateDraft(e.ctx, e.owner, r.course.ID)
	if err != nil {
		t.Fatal(err)
	}
	// Swap the steps and retitle B.
	dd := e.completeDraft(r.p.assignment, r.ch, []courses.StepInput{step(b, "B retitled"), step(a, "Step 1")})
	dd.Modules[0].Key = d.Modules[0].Key
	d = e.submit(e.mustSave(d, dd))
	p, err := e.store.ReviewRevision(e.ctx, e.reviewer, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !p.MetadataChanged || len(p.Steps) != 2 {
		t.Fatalf("payload: %+v", p)
	}
	for _, s := range p.Steps {
		if s.Change != "moved" || !s.StructureChanged || s.PreviousPosition == nil {
			t.Fatalf("step %+v", s)
		}
	}
	if *p.Steps[0].PreviousTitle != "Step 2" || *p.Steps[0].PreviousPosition != 2 || *p.Steps[1].PreviousPosition != 1 {
		t.Fatalf("previous structure: %+v", p.Steps)
	}
}

// BlockedReviews filters in SQL and pages with a cursor.
func TestBlockedReviewsPaging(t *testing.T) {
	e := newEnv(t)
	defer func(n int) { courses.BlockedPage = n }(courses.BlockedPage)
	courses.BlockedPage = 2
	chem := e.user("chem-temp-reviewer")
	subject := dbtest.AddSubject(t, e.db, "test-chem")
	if _, err := e.perms.GrantReviewer(e.ctx, testOperator, chem, subject); err != nil {
		t.Fatal(err)
	}
	ch := e.chapter(subject, "chem-1")
	pl := e.edition(subject, []int64{ch}, true)
	var lessonsChem []int64
	for i := 0; i < 3; i++ {
		l := e.lessonDraft(e.other, subject, "fr", fmt.Sprintf("Chem %d", i))
		e.approveBy(l, chem)
		e.publish(l)
		_, rev := e.course(subject, "fr")
		rev = e.mustSave(rev, e.completeDraft(pl.assignment, ch, []courses.StepInput{step(l, "s")}))
		e.submit(rev)
		lessonsChem = append(lessonsChem, l.ID)
	}
	if _, err := e.perms.RevokeReviewer(e.ctx, testOperator, chem, subject); err != nil {
		t.Fatal(err)
	}
	// A math revision a reviewer may decide is never listed.
	m := e.readyCourse(e.publishedLesson(e.other, e.math, "fr", "Math"))
	e.submit(m.rev)
	page1, next, err := e.store.BlockedReviews(e.ctx, e.adminID(), 0)
	if err != nil || len(page1) != 2 || next == nil || page1[0].Blocked == nil {
		t.Fatalf("page 1: %+v %v %v", page1, next, err)
	}
	page2, next2, err := e.store.BlockedReviews(e.ctx, e.adminID(), *next)
	if err != nil || len(page2) != 1 || next2 != nil || page2[0].SubjectID != subject {
		t.Fatalf("page 2: %+v %v %v", page2, next2, err)
	}
	if page2[0].Blocked.Reasons[0].Reason != "no_subject_reviewer" {
		t.Fatalf("reasons: %+v", page2[0].Blocked)
	}
}

// Direct SQL: a revision stored as an amendment whose overview changed is
// refused at submission; a transfer that leaves a draft open at commit is
// refused.
func TestKindAndTransferGuards(t *testing.T) {
	e := newEnv(t)
	a := e.publishedLesson(e.other, e.math, "fr", "A")
	r := e.publishedCourse(a)
	e.newerLessonRevision(a, "A v2")
	am, err := e.store.CreateAmendment(e.ctx, e.owner, r.course.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	asRole := e.leastPrivilegeRole()
	C, R, O := itoa(r.course.ID), itoa(am.ID), itoa(e.owner)
	wantGuard(t, "changed amendment submitted", asRole(
		`UPDATE S.course_revisions SET title = 'Changed', version = version + 1 WHERE id = `+R,
		`INSERT INTO S.course_submission_audit (course_id, revision_id, actor_id) VALUES (`+C+`, `+R+`, `+O+`)`,
		`UPDATE S.course_revisions SET status = 'in_review', submitted_at = now(), updated_at = now(), version = version + 1 WHERE id = `+R),
		"stored as amendment but is ordinary")
	if err := e.store.DiscardDraft(e.ctx, e.owner, am.ID, am.Version); err != nil {
		t.Fatal(err)
	}
	wantGuard(t, "transfer leaving a draft", asRole(
		`INSERT INTO S.course_lifecycle_audit (course_id, actor_id, action, reason, from_owner_id, to_owner_id)
		 VALUES (`+C+`, `+itoa(e.adminID())+`, 'transfer', 'x', `+O+`, `+itoa(e.other)+`)`,
		`UPDATE S.courses SET owner_id = `+itoa(e.other)+` WHERE id = `+C,
		`INSERT INTO S.course_revisions (course_id, subject_id, language, revision_number, author_id, base_revision_id, compare_revision_id)
		 SELECT id, subject_id, language, last_revision_number + 1, owner_id, published_revision_id, published_revision_id
		   FROM S.courses WHERE id = `+C), "was not applied")
}
