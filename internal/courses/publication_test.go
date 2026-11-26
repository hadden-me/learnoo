package courses_test

import (
	"errors"
	"testing"

	"github.com/hadden-me/learnoo/internal/courses"
	"github.com/jackc/pgx/v5"
)

// Publication: the reader views, stale base and pointer, ineligible steps,
// atomicity (nothing changes on refusal).
func TestPublish(t *testing.T) {
	e := newEnv(t)
	a := e.publishedLesson(e.other, e.math, "fr", "A")
	b := e.publishedLesson(e.owner, e.math, "fr", "B")
	r := e.readyCourse(a, b)
	if ok, _ := e.readable(r.course.ID); ok {
		t.Fatal("draft readable")
	}
	sub := e.submit(r.rev)
	_, err := e.store.Publish(e.ctx, e.publisher, sub.ID, nil)
	wantErr(t, "in review", err, courses.ErrNotApproved)
	e.approveCourse(sub.ID)
	_, err = e.store.Publish(e.ctx, e.owner, sub.ID, nil)
	wantErr(t, "author is no publisher", err, courses.ErrDenied)
	_, err = e.store.Publish(e.ctx, e.publisher, sub.ID, &sub.ID)
	wantErr(t, "stale expected pointer", err, courses.ErrStalePointer)
	pc, err := e.store.Publish(e.ctx, e.publisher, sub.ID, nil)
	if err != nil || *pc.RevisionID != sub.ID || pc.PreviousRevisionID != nil {
		t.Fatalf("publish: %+v %v", pc, err)
	}
	ok, steps := e.readable(r.course.ID)
	if !ok || len(steps) != 2 || !steps[a.LessonID] || !steps[b.LessonID] {
		t.Fatalf("reader: %v %v", ok, steps)
	}
	_, err = e.store.Publish(e.ctx, e.publisher, sub.ID, &sub.ID)
	wantErr(t, "again", err, courses.ErrAlreadyPublished)

	// A successor drafted before a competing publication has a stale base.
	d1, err := e.store.CreateDraft(e.ctx, e.owner, r.course.ID)
	if err != nil {
		t.Fatal(err)
	}
	d1 = e.mustSave(d1, e.completeDraft(r.p.assignment, r.ch, []courses.StepInput{step(a, "Renamed")}))
	d1 = e.submit(d1)
	e.approveCourse(d1.ID)
	// Simulate a competing publication: suspend and restore changes nothing
	// for the base, so suspend only (pointer NULL, base = sub).
	if _, err := e.store.Suspend(e.ctx, e.adminID(), r.course.ID, &sub.ID, "Simulated suspension."); err != nil {
		t.Fatal(err)
	}
	before := e.count(`SELECT count(*) FROM course_publication_audit`)
	_, err = e.store.Publish(e.ctx, e.publisher, d1.ID, nil)
	wantErr(t, "stale base", err, courses.ErrStaleBase)
	if e.count(`SELECT count(*) FROM course_publication_audit`) != before || e.pointer(r.course.ID) != nil {
		t.Fatal("a refused publication changed something")
	}
	// Restoring makes the base current again.
	if _, err := e.store.Restore(e.ctx, e.publisher, r.course.ID, sub.ID, nil); err != nil {
		t.Fatal(err)
	}
	// A pinned lesson unpublished before publication: refused, with the step.
	e.unpublish(a.LessonID)
	_, err = e.store.Publish(e.ctx, e.publisher, d1.ID, &sub.ID)
	var ine *courses.IneligibleError
	if !errors.As(err, &ine) || ine.Steps[0].LessonID != a.LessonID {
		t.Fatalf("ineligible: %v", err)
	}
	if *e.pointer(r.course.ID) != sub.ID {
		t.Fatal("pointer moved")
	}
}

// A06: amendments.
func TestAmendments(t *testing.T) {
	e := newEnv(t)
	student := e.user("student")
	a := e.publishedLesson(e.other, e.math, "fr", "A")
	b := e.publishedLesson(e.other, e.math, "fr", "B")
	r := e.publishedCourse(a, b)
	_, err := e.store.CreateAmendment(e.ctx, e.owner, r.course.ID, nil)
	wantErr(t, "nothing newer", err, courses.ErrNothingToAmend)

	a2 := e.newerLessonRevision(a, "A v2")
	b2 := e.newerLessonRevision(b, "B v2")
	// The notice: staff only.
	for _, who := range []int64{e.owner, e.reviewer, e.publisher, e.adminID()} {
		n, err := e.store.NewerRevisions(e.ctx, who, r.course.ID)
		if err != nil || len(n.Steps) != 2 {
			t.Fatalf("notice for %d: %+v %v", who, n, err)
		}
	}
	for _, who := range []int64{student, e.other} {
		_, err := e.store.NewerRevisions(e.ctx, who, r.course.ID)
		wantErr(t, "notice for a student or another author", err, courses.ErrNotFound)
	}
	// An ordinary draft is open: draft_exists.
	open, err := e.store.CreateDraft(e.ctx, e.owner, r.course.ID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = e.store.CreateAmendment(e.ctx, e.owner, r.course.ID, nil)
	wantErr(t, "draft open", err, courses.ErrDraftExists)
	if err := e.store.DiscardDraft(e.ctx, e.owner, open.ID, open.Version); err != nil {
		t.Fatal(err)
	}
	// A choice that is no newer revision of a step's lesson is refused.
	_, err = e.store.CreateAmendment(e.ctx, e.owner, r.course.ID, []courses.Replacement{{LessonID: a.LessonID, RevisionID: b2.ID}})
	wantErr(t, "wrong lesson", err, courses.ErrInvalidStep)
	_, err = e.store.CreateAmendment(e.ctx, e.owner, r.course.ID, []courses.Replacement{{LessonID: a.LessonID, RevisionID: a.ID}})
	wantErr(t, "not newer", err, courses.ErrInvalidStep)

	// Only the chosen step changes.
	am, err := e.store.CreateAmendment(e.ctx, e.owner, r.course.ID, []courses.Replacement{{LessonID: a.LessonID, RevisionID: a2.ID}})
	if err != nil || am.Kind != "amendment" || am.Modules[0].Steps[0].RevisionID != a2.ID || am.Modules[0].Steps[1].RevisionID != b.ID {
		t.Fatalf("amendment: %+v %v", am, err)
	}
	// Any other change makes it ordinary; undoing it makes it an amendment
	// again.
	d := e.completeDraft(r.p.assignment, r.ch, []courses.StepInput{step(a2, "Step 1"), step(b, "Step 2")})
	d.Modules[0].Key = am.Modules[0].Key
	d.Title = "Retitled"
	saved := e.mustSave(am, d)
	if saved.Kind != "ordinary" {
		t.Fatalf("retitled amendment is %s", saved.Kind)
	}
	d.Title = "TEST course"
	saved = e.mustSave(saved, d)
	if saved.Kind != "amendment" {
		t.Fatalf("restored amendment is %s", saved.Kind)
	}
	// Going back to an older revision is ordinary too (save via the owner's
	// own draft path is not possible for another author's older revision,
	// so check the function directly with b2 replaced by b after b's
	// newer revision is pinned elsewhere).

	// The replacement withdrawn before publication: refused, nothing changes.
	sub := e.submit(saved)
	e.approveCourse(sub.ID)
	if _, err := e.reviewStore().Withdraw(e.ctx, e.adminID(), a2.ID, "Simulated withdrawal.", &a2.ID); err != nil {
		t.Fatal(err)
	}
	_, err = e.store.Publish(e.ctx, e.publisher, sub.ID, &r.rev.ID)
	wantErr(t, "withdrawn replacement", err, courses.ErrIneligible)
	if *e.pointer(r.course.ID) != r.rev.ID {
		t.Fatal("pointer changed")
	}
	// The stale candidate is released by withdrawal. Withdrawing a2 also
	// unpublished lesson a, so its step is unreadable and an amendment
	// cannot carry it until a publisher restores a's first revision; then a
	// new amendment takes b2 (a has no newer eligible revision now).
	if _, err := e.store.Withdraw(e.ctx, e.adminID(), sub.ID, "Simulated release."); err != nil {
		t.Fatal(err)
	}
	_, err = e.store.CreateAmendment(e.ctx, e.owner, r.course.ID, nil)
	wantErr(t, "amendment carrying an unreadable step", err, courses.ErrInvalidStep)
	if _, err := e.reviewStore().Restore(e.ctx, e.adminID(), a.ID, nil); err != nil {
		t.Fatal(err)
	}
	am2, err := e.store.CreateAmendment(e.ctx, e.owner, r.course.ID, nil)
	if err != nil || am2.Modules[0].Steps[1].RevisionID != b2.ID || am2.Modules[0].Steps[0].RevisionID != a.ID {
		t.Fatalf("second amendment: %+v %v", am2, err)
	}
	am2 = e.submit(am2)
	e.approveCourse(am2.ID)
	// A stale base: published by someone else meanwhile is impossible with
	// one candidate; publish atomically.
	e.publishCourse(am2.ID)
	ok, steps := e.readable(r.course.ID)
	if !ok || !steps[a.LessonID] || !steps[b.LessonID] {
		t.Fatalf("reader after amendment: %v %v", ok, steps)
	}
}

// An amendment is accepted on a retired edition, and a course suspended
// after its last publication is republished from a draft created while
// suspended.
func TestAmendmentRetiredEditionAndRepublication(t *testing.T) {
	e := newEnv(t)
	a := e.publishedLesson(e.other, e.math, "fr", "A")
	r := e.publishedCourse(a)
	// Retire the course's edition.
	ed, err := e.cur.RetireEdition(e.ctx, e.admin, r.p.edition.ID, e.editionVersion(r.p.edition.ID))
	if err != nil {
		t.Fatalf("retire: %+v %v", ed, err)
	}
	// Suspend, then draft while suspended: the comparison is the last
	// published revision.
	if _, err := e.store.Suspend(e.ctx, e.adminID(), r.course.ID, &r.rev.ID, "Simulated suspension."); err != nil {
		t.Fatal(err)
	}
	ok, _ := e.readable(r.course.ID)
	if ok {
		t.Fatal("suspended course readable")
	}
	a2 := e.newerLessonRevision(a, "A v2")
	am, err := e.store.CreateAmendment(e.ctx, e.owner, r.course.ID, nil)
	if err != nil || *am.CompareRevisionID != r.rev.ID || am.BaseRevisionID != nil {
		t.Fatalf("amendment while suspended: %+v %v", am, err)
	}
	am = e.submit(am)
	e.approveCourse(am.ID)
	if _, err := e.store.Publish(e.ctx, e.publisher, am.ID, nil); err != nil {
		t.Fatalf("republication: %v", err)
	}
	ok, steps := e.readable(r.course.ID)
	if !ok || !steps[a.LessonID] {
		t.Fatal("republished course not readable")
	}
	var pinned int64
	if err := e.db.QueryRow(e.ctx, `SELECT lesson_revision_id FROM reader_course_steps WHERE course_id = $1`, r.course.ID).Scan(&pinned); err != nil || pinned != a2.ID {
		t.Fatalf("pinned %d %v", pinned, err)
	}
	// An ordinary revision on the retired edition is refused at submission.
	d, err := e.store.CreateDraft(e.ctx, e.owner, r.course.ID)
	if err != nil {
		t.Fatal(err)
	}
	dd := e.completeDraft(r.p.assignment, r.ch, []courses.StepInput{step(a2, "Renamed")})
	dd.Modules[0].Key = d.Modules[0].Key
	d = e.mustSave(d, dd)
	_, err = e.store.Submit(e.ctx, e.owner, d.ID, d.Version)
	wantErr(t, "ordinary on a retired edition", err, courses.ErrPlacementInvalid)
}

func (e *env) editionVersion(id int64) int {
	e.t.Helper()
	var v int
	if err := e.db.QueryRow(e.ctx, `SELECT version FROM curriculum_editions WHERE id = $1`, id).Scan(&v); err != nil {
		e.t.Fatal(err)
	}
	return v
}

// A07: lesson withdrawal, unpublication and archive affect published
// courses: the step becomes unreadable everywhere at once, the overview and
// other steps stay, a dependency event is recorded; suspension hides the
// course; restore with an ineligible step is refused; archive then
// unarchive leaves the course unreadable until restore.
func TestDependencyEffects(t *testing.T) {
	e := newEnv(t)
	a := e.publishedLesson(e.other, e.math, "fr", "A")
	b := e.publishedLesson(e.other, e.math, "fr", "B")
	c := e.publishedLesson(e.other, e.math, "fr", "C")
	r := e.publishedCourse(a, b, c)

	if _, err := e.reviewStore().Withdraw(e.ctx, e.adminID(), a.ID, "Simulated withdrawal.", &a.ID); err != nil {
		t.Fatal(err)
	}
	e.unpublish(b.LessonID)
	ok, steps := e.readable(r.course.ID)
	if !ok || steps[a.LessonID] || steps[b.LessonID] || !steps[c.LessonID] {
		t.Fatalf("after withdrawal and unpublication: %v %v", ok, steps)
	}
	// Nothing of the unreadable steps but their course-authored titles.
	if n := e.count(`SELECT count(*) FROM reader_course_steps WHERE course_id = $1 AND NOT readable
		AND (lesson_title IS NOT NULL OR content IS NOT NULL)`, r.course.ID); n != 0 {
		t.Fatalf("unreadable step exposes %d bodies", n)
	}
	// Archive a lesson (no application path: direct SQL as the owner).
	e.mustExec(`UPDATE lessons SET archived_at = now() WHERE id = $1`, c.LessonID)
	if _, steps := e.readable(r.course.ID); steps[c.LessonID] {
		t.Fatal("archived lesson readable in the course")
	}
	rows := e.count(`SELECT count(*) FROM course_dependency_events WHERE course_id = $1`, r.course.ID)
	var causes []string
	if err := e.db.QueryRow(e.ctx, `SELECT array_agg(cause ORDER BY id) FROM course_dependency_events WHERE course_id = $1`, r.course.ID).Scan(&causes); err != nil {
		t.Fatal(err)
	}
	// Withdrawing a's published revision also unpublishes a.
	if rows != 4 || causes[0] != "revision_withdrawn" || causes[1] != "lesson_unpublished" || causes[2] != "lesson_unpublished" ||
		causes[3] != "lesson_archived" || e.count(`SELECT dependency_count FROM courses WHERE id = $1`, r.course.ID) != 4 {
		t.Fatalf("events %d %v", rows, causes)
	}

	// Suspension hides the course; restore is refused while a step is
	// ineligible.
	if _, err := e.store.Suspend(e.ctx, e.adminID(), r.course.ID, &r.rev.ID, "Simulated suspension."); err != nil {
		t.Fatal(err)
	}
	if ok, steps := e.readable(r.course.ID); ok || len(steps) != 0 {
		t.Fatal("suspended course readable")
	}
	_, err := e.store.Restore(e.ctx, e.adminID(), r.course.ID, r.rev.ID, nil)
	wantErr(t, "restore with ineligible steps", err, courses.ErrIneligible)
	_, err = e.store.Restore(e.ctx, e.owner, r.course.ID, r.rev.ID, nil)
	wantErr(t, "owner restores", err, courses.ErrDenied)
}

// Withdrawal under REPEATABLE READ is refused even when no course is
// affected; so are unpublication and archive.
func TestDependencyTriggersRequireReadCommitted(t *testing.T) {
	e := newEnv(t)
	a := e.publishedLesson(e.other, e.math, "fr", "Unused by courses")
	for _, iso := range []pgx.TxIsoLevel{pgx.RepeatableRead, pgx.Serializable} {
		for name, sql := range map[string]string{
			"withdraw": `INSERT INTO revision_withdrawals (lesson_id, revision_id, actor_id, reason) VALUES ($1, $2, $3, 'Simulated.')`,
			"unpublish": `INSERT INTO publication_audit (lesson_id, revision_id, previous_revision_id, actor_id, action, reason)
				VALUES ($1, NULL, $2, $3, 'unpublish', 'Simulated.')`,
			"archive": `UPDATE lessons SET archived_at = now() WHERE id = $1 AND $2::bigint > 0 AND $3::bigint > 0`,
		} {
			tx, err := e.db.BeginTx(e.ctx, pgx.TxOptions{IsoLevel: iso})
			if err != nil {
				t.Fatal(err)
			}
			_, err = tx.Exec(e.ctx, sql, a.LessonID, a.ID, e.adminID())
			_ = tx.Rollback(e.ctx)
			wantGuard(t, name+" "+string(iso), err, "READ COMMITTED")
		}
	}
}

// Archive then unarchive leaves the course unreadable until a restore;
// restore is refused while archived.
func TestArchiveUnarchive(t *testing.T) {
	e := newEnv(t)
	a := e.publishedLesson(e.other, e.math, "fr", "A")
	r := e.publishedCourse(a)
	_, err := e.store.Archive(e.ctx, e.adminID(), r.course.ID, nil, "Simulated archive.")
	wantErr(t, "stale pointer", err, courses.ErrStalePointer)
	_, err = e.store.Archive(e.ctx, e.adminID(), r.course.ID, &r.rev.ID, "")
	wantErr(t, "no reason", err, courses.ErrReasonRequired)
	if _, err := e.store.Archive(e.ctx, e.adminID(), r.course.ID, &r.rev.ID, "Simulated archive."); err != nil {
		t.Fatal(err)
	}
	if ok, _ := e.readable(r.course.ID); ok {
		t.Fatal("archived course readable")
	}
	_, err = e.store.Restore(e.ctx, e.publisher, r.course.ID, r.rev.ID, nil)
	wantErr(t, "restore archived", err, courses.ErrArchived)
	_, err = e.store.CreateDraft(e.ctx, e.owner, r.course.ID)
	wantErr(t, "draft of archived", err, courses.ErrArchived)
	if _, err := e.store.Unarchive(e.ctx, e.adminID(), r.course.ID, "Simulated unarchive."); err != nil {
		t.Fatal(err)
	}
	if ok, _ := e.readable(r.course.ID); ok {
		t.Fatal("unarchiving republished")
	}
	if _, err := e.store.Restore(e.ctx, e.publisher, r.course.ID, r.rev.ID, nil); err != nil {
		t.Fatal(err)
	}
	if ok, _ := e.readable(r.course.ID); !ok {
		t.Fatal("restored course unreadable")
	}
	// Archiving a suspended course writes only a lifecycle row.
	if _, err := e.store.Suspend(e.ctx, e.adminID(), r.course.ID, &r.rev.ID, "Simulated."); err != nil {
		t.Fatal(err)
	}
	before := e.count(`SELECT count(*) FROM course_publication_audit WHERE course_id = $1`, r.course.ID)
	if _, err := e.store.Archive(e.ctx, e.adminID(), r.course.ID, nil, "Simulated archive."); err != nil {
		t.Fatal(err)
	}
	if e.count(`SELECT count(*) FROM course_publication_audit WHERE course_id = $1`, r.course.ID) != before ||
		e.count(`SELECT count(*) FROM course_lifecycle_audit WHERE course_id = $1 AND action = 'archive'`, r.course.ID) != 1 {
		t.Fatal("archive of a suspended course")
	}
	// Archive requests.
	_, err = e.store.RequestArchive(e.ctx, e.owner, r.course.ID, "Please.")
	wantErr(t, "request on archived", err, courses.ErrArchived)
}

// A08: lifecycle.
func TestLifecycle(t *testing.T) {
	e := newEnv(t)
	a := e.publishedLesson(e.other, e.math, "fr", "A")
	r := e.publishedCourse(a)

	// Transfer refused with an open candidate.
	d, err := e.store.CreateDraft(e.ctx, e.owner, r.course.ID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = e.store.Transfer(e.ctx, e.adminID(), r.course.ID, e.other, "Simulated transfer.")
	wantErr(t, "transfer with a draft", err, courses.ErrActiveCandidateExists)
	_, err = e.store.Transfer(e.ctx, e.adminID(), r.course.ID, e.reviewer, "Simulated transfer.")
	wantErr(t, "transfer to a non-author", err, courses.ErrUnknownUser)

	// Withdrawal of a revision in review: frees the slot, blocks decisions.
	d = e.mustSave(d, e.completeDraft(r.p.assignment, r.ch, []courses.StepInput{step(a, "Renamed")}))
	sub := e.submit(d)
	_, err = e.store.CreateDraft(e.ctx, e.owner, r.course.ID)
	wantErr(t, "slot taken", err, courses.ErrActiveCandidateExists)
	_, err = e.store.Withdraw(e.ctx, e.adminID(), r.rev.ID, "Simulated.")
	wantErr(t, "withdraw the published revision", err, courses.ErrPublished)
	_, err = e.store.Withdraw(e.ctx, e.owner, sub.ID, "Simulated.")
	wantErr(t, "owner withdraws", err, courses.ErrDenied)
	if _, err := e.store.Withdraw(e.ctx, e.adminID(), sub.ID, "Simulated withdrawal."); err != nil {
		t.Fatal(err)
	}
	_, err = e.store.Approve(e.ctx, e.reviewer, sub.ID, "")
	wantErr(t, "decide withdrawn", err, courses.ErrWithdrawn)
	_, err = e.store.Withdraw(e.ctx, e.adminID(), sub.ID, "Again.")
	wantErr(t, "withdraw twice", err, courses.ErrWithdrawn)
	if q, _ := e.store.ReviewQueue(e.ctx, e.reviewer, 0); len(q) != 0 {
		t.Fatal("withdrawn revision still queued")
	}

	// Withdrawal of an approved revision frees the slot and blocks publication.
	d2, err := e.store.CreateDraft(e.ctx, e.owner, r.course.ID)
	if err != nil {
		t.Fatalf("slot after withdrawal: %v", err)
	}
	d2 = e.submit(d2)
	e.approveCourse(d2.ID)
	if _, err := e.store.Withdraw(e.ctx, e.adminID(), d2.ID, "Simulated withdrawal."); err != nil {
		t.Fatal(err)
	}
	_, err = e.store.Publish(e.ctx, e.publisher, d2.ID, &r.rev.ID)
	wantErr(t, "publish withdrawn", err, courses.ErrWithdrawn)

	// Administrator draft discard, then transfer.
	d3, err := e.store.CreateDraft(e.ctx, e.owner, r.course.ID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = e.store.DiscardDraftAsAdmin(e.ctx, e.owner, d3.ID, "Simulated.")
	wantErr(t, "owner as admin", err, courses.ErrDenied)
	if _, err := e.store.DiscardDraftAsAdmin(e.ctx, e.adminID(), d3.ID, "Simulated recovery."); err != nil {
		t.Fatal(err)
	}
	ev, err := e.store.Transfer(e.ctx, e.adminID(), r.course.ID, e.other, "Simulated transfer.")
	if err != nil || *ev.FromOwnerID != e.owner || *ev.ToOwnerID != e.other {
		t.Fatalf("transfer: %+v %v", ev, err)
	}
	// The new owner drafts; the old one cannot; history keeps its authors.
	if _, err := e.store.CreateDraft(e.ctx, e.other, r.course.ID); err != nil {
		t.Fatalf("new owner draft: %v", err)
	}
	_, err = e.store.CreateDraft(e.ctx, e.owner, r.course.ID)
	wantErr(t, "old owner", err, courses.ErrNotFound)
	if e.count(`SELECT count(*) FROM course_revisions WHERE course_id = $1 AND author_id = $2`, r.course.ID, e.owner) != 3 {
		t.Fatal("history authors changed")
	}
	// Previously published revision withdrawn: no restore.
	if _, err := e.store.Suspend(e.ctx, e.adminID(), r.course.ID, &r.rev.ID, "Simulated."); err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.Withdraw(e.ctx, e.adminID(), r.rev.ID, "Simulated."); err != nil {
		t.Fatal(err)
	}
	_, err = e.store.Restore(e.ctx, e.adminID(), r.course.ID, r.rev.ID, nil)
	wantErr(t, "restore withdrawn", err, courses.ErrWithdrawn)
	if n := e.count(`SELECT count(*) FROM course_lifecycle_audit WHERE course_id = $1`, r.course.ID); n != 2 {
		t.Fatalf("lifecycle rows %d", n)
	}
	// Archive requests are listed for administrators.
	if _, err := e.store.RequestArchive(e.ctx, e.other, r.course.ID, "Simulated request."); err != nil {
		t.Fatal(err)
	}
	reqs, err := e.store.ArchiveRequests(e.ctx, e.adminID())
	if err != nil || len(reqs) != 1 || reqs[0].ActorID != e.other {
		t.Fatalf("requests: %+v %v", reqs, err)
	}
	_, err = e.store.ArchiveRequests(e.ctx, e.other)
	wantErr(t, "requests for an author", err, courses.ErrDenied)
	_, err = e.store.RequestArchive(e.ctx, e.owner, r.course.ID, "Not mine.")
	wantErr(t, "former owner request", err, courses.ErrNotFound)
}
