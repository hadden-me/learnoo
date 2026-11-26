package review_test

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/hadden-me/learnoo/internal/lessons"
	"github.com/hadden-me/learnoo/internal/permissions"
	"github.com/hadden-me/learnoo/internal/review"
)

// Withdrawal, emergency unpublication and restoration (LRN-009) through the
// Store. Simulated users and unreviewed test documents only.

// world captures every row a rejected operation must leave unchanged:
// revisions (status, closing time), pointers, the pointer audit and
// withdrawals.
func (e *env) world() string {
	e.t.Helper()
	var s string
	err := e.db.QueryRow(e.ctx, `
		SELECT coalesce((SELECT string_agg(id || ':' || status || ':' || coalesce(candidate_closed_at::text, '-'), ',' ORDER BY id) FROM lesson_revisions), '') || '|' ||
		       coalesce((SELECT string_agg(id || ':' || coalesce(published_revision_id::text, '-'), ',' ORDER BY id) FROM lessons), '') || '|' ||
		       coalesce((SELECT jsonb_agg(p ORDER BY p.id)::text FROM publication_audit p), '') || '|' ||
		       coalesce((SELECT jsonb_agg(w ORDER BY w.id)::text FROM revision_withdrawals w), '')`).Scan(&s)
	if err != nil {
		e.t.Fatal(err)
	}
	return s
}

func (e *env) unchanged(t *testing.T, what, before string) {
	t.Helper()
	if after := e.world(); after != before {
		t.Fatalf("%s changed state:\n%s\n%s", what, before, after)
	}
}

func ptr(id int64) *int64 { return &id }

// withdrawEnv adds a reviewer, a publisher and an administrator (who holds
// administrator only, not publisher).
type withdrawEnv struct {
	*env
	reviewer, publisher, admin int64
}

func newWithdrawEnv(t *testing.T) withdrawEnv {
	e := newEnv(t)
	return withdrawEnv{env: e,
		reviewer:  e.reviewer("reviewer", []int64{e.subject}),
		publisher: e.user("publisher", permissions.Publisher),
		admin:     e.user("admin", permissions.Administrator)}
}

func (w withdrawEnv) approved(r lessons.Revision) lessons.Revision {
	w.t.Helper()
	got, err := w.review.Approve(w.ctx, w.reviewer, r.ID, "")
	if err != nil {
		w.t.Fatal(err)
	}
	return got
}

func (w withdrawEnv) published(r lessons.Revision) {
	w.t.Helper()
	if _, err := w.review.Publish(w.ctx, w.publisher, r.ID); err != nil {
		w.t.Fatal(err)
	}
}

// firstAndSecond publishes a first edition, then a second edition on it.
func (w withdrawEnv) firstAndSecond() (first, second lessons.Revision) {
	w.t.Helper()
	first = w.approved(w.submitted(w.author, w.subject, "First edition"))
	w.published(first)
	second = w.approved(w.successor(w.author, first.LessonID, "Second edition"))
	w.published(second)
	return first, second
}

func TestWithdrawAndUnpublishRequireAdministrator(t *testing.T) {
	w := newWithdrawEnv(t)
	first, second := w.firstAndSecond()
	disabled := w.user("disabled-admin", permissions.Administrator)
	if err := w.exec(`UPDATE users SET disabled_at = now() WHERE id = $1`, disabled); err != nil {
		t.Fatal(err)
	}
	before := w.world()
	for who, id := range map[string]int64{
		"anonymous":        0,
		"unknown user":     999999,
		"author":           w.author,
		"reviewer":         w.reviewer,
		"publisher":        w.publisher,
		"disabled admin":   disabled,
		"author+publisher": w.user("both", permissions.Author, permissions.Publisher),
	} {
		_, err := w.review.Withdraw(w.ctx, id, second.ID, "Simulated reason.", ptr(second.ID))
		wantErr(t, who+" withdraws", err, review.ErrDenied)
		_, err = w.review.Withdraw(w.ctx, id, 999999, "Simulated reason.", nil)
		wantErr(t, who+" withdraws an unknown revision", err, review.ErrDenied)
		_, err = w.review.Unpublish(w.ctx, id, first.LessonID, "Simulated reason.", second.ID)
		wantErr(t, who+" unpublishes", err, review.ErrDenied)
	}
	w.unchanged(t, "denied withdrawals", before)
}

func TestRestoreRequiresPublisherOrAdministrator(t *testing.T) {
	w := newWithdrawEnv(t)
	first, second := w.firstAndSecond()
	disabled := w.user("disabled-publisher", permissions.Publisher)
	if err := w.exec(`UPDATE users SET disabled_at = now() WHERE id = $1`, disabled); err != nil {
		t.Fatal(err)
	}
	before := w.world()
	for who, id := range map[string]int64{
		"anonymous":          0,
		"unknown user":       999999,
		"author":             w.author,
		"reviewer":           w.reviewer,
		"disabled publisher": disabled,
	} {
		_, err := w.review.Restore(w.ctx, id, first.ID, ptr(second.ID))
		wantErr(t, who+" restores", err, review.ErrDenied)
	}
	w.unchanged(t, "denied restorations", before)

	// A publisher restores; an administrator without publisher restores too.
	ch, err := w.review.Restore(w.ctx, w.publisher, first.ID, ptr(second.ID))
	if err != nil {
		t.Fatal(err)
	}
	if ch.Action != "restore" || ch.RevisionID == nil || *ch.RevisionID != first.ID || ch.PreviousRevisionID == nil ||
		*ch.PreviousRevisionID != second.ID || ch.ActorID != w.publisher || ch.OccurredAt.IsZero() {
		t.Fatalf("restoration %+v", ch)
	}
	if _, err := w.review.Restore(w.ctx, w.admin, second.ID, ptr(first.ID)); err != nil {
		t.Fatalf("administrator restore: %v", err)
	}
	if p := w.pointer(first.LessonID); p == nil || *p != second.ID {
		t.Fatalf("pointer %v", p)
	}
}

// Withdrawing the published revision unpublishes the lesson in the same
// transaction; the revision is then blocked from publication and restoration.
func TestWithdrawPublishedRevisionUnpublishes(t *testing.T) {
	w := newWithdrawEnv(t)
	first, second := w.firstAndSecond()

	got, err := w.review.Withdraw(w.ctx, w.admin, second.ID, "  Simulated: incorrect worked solution.  ", ptr(second.ID))
	if err != nil {
		t.Fatal(err)
	}
	if got.RevisionID != second.ID || got.LessonID != second.LessonID || got.ActorID != w.admin ||
		got.Reason != "Simulated: incorrect worked solution." || got.WithdrawnAt.IsZero() || got.Unpublished == nil ||
		got.Unpublished.PreviousRevisionID == nil || *got.Unpublished.PreviousRevisionID != second.ID ||
		got.Unpublished.RevisionID != nil || got.Unpublished.Reason != got.Reason || !got.Unpublished.OccurredAt.Equal(got.WithdrawnAt) {
		t.Fatalf("withdrawal %+v %+v", got, got.Unpublished)
	}
	if p := w.pointer(first.LessonID); p != nil {
		t.Fatalf("withdrawn revision still published: %v", *p)
	}
	hist, err := w.review.PointerHistory(w.ctx, first.LessonID)
	if err != nil {
		t.Fatal(err)
	}
	if len(hist) != 3 || hist[2].Action != "unpublish" || hist[2].ActorID != w.admin || hist[2].RevisionID != nil ||
		*hist[2].PreviousRevisionID != second.ID || hist[2].Reason != got.Reason {
		t.Fatalf("history %+v", hist)
	}
	ws, err := w.review.Withdrawals(w.ctx, first.LessonID)
	if err != nil || len(ws) != 1 || ws[0].RevisionID != second.ID || ws[0].Reason != got.Reason || ws[0].ActorID != w.admin {
		t.Fatalf("withdrawals %+v %v", ws, err)
	}
	// Publication history keeps the two publications only.
	if pubs, err := w.review.PublicationHistory(w.ctx, first.LessonID); err != nil || len(pubs) != 2 {
		t.Fatalf("publication history %+v %v", pubs, err)
	}

	before := w.world()
	_, err = w.review.Publish(w.ctx, w.publisher, second.ID)
	wantErr(t, "publish withdrawn", err, review.ErrWithdrawn)
	_, err = w.review.Restore(w.ctx, w.publisher, second.ID, nil)
	wantErr(t, "restore withdrawn", err, review.ErrWithdrawn)
	_, err = w.review.Withdraw(w.ctx, w.admin, second.ID, "Again.", nil)
	wantErr(t, "withdraw twice", err, review.ErrAlreadyWithdrawn)
	w.unchanged(t, "blocked withdrawn revision", before)

	// The earlier edition can still be restored.
	if _, err := w.review.Restore(w.ctx, w.publisher, first.ID, nil); err != nil {
		t.Fatal(err)
	}
	if p := w.pointer(first.LessonID); p == nil || *p != first.ID {
		t.Fatalf("pointer %v", p)
	}
	// Withdrawing the restored revision unpublishes the lesson again.
	_, err = w.review.Withdraw(w.ctx, w.admin, first.ID, "Simulated reason.", ptr(first.ID))
	if err != nil {
		t.Fatal(err)
	}
	if p := w.pointer(first.LessonID); p != nil {
		t.Fatalf("pointer %v", p)
	}
}

// Withdrawing a pending approved candidate releases the lesson: a new draft
// may be created, and the withdrawn revision is never publishable.
func TestWithdrawPendingCandidateReleasesLesson(t *testing.T) {
	w := newWithdrawEnv(t)
	first := w.approved(w.submitted(w.author, w.subject, "First edition"))
	w.published(first)
	pending := w.approved(w.successor(w.author, first.LessonID, "Pending"))
	_, err := w.lessons.CreateDraft(w.ctx, w.author, first.LessonID, lessons.Fields{Content: doc("Blocked")})
	wantErr(t, "draft while approved candidate pending", err, lessons.ErrActiveCandidateExists)

	got, err := w.review.Withdraw(w.ctx, w.admin, pending.ID, "Simulated: incorrect.", ptr(first.ID))
	if err != nil {
		t.Fatal(err)
	}
	if got.Unpublished != nil {
		t.Fatalf("withdrawing an unpublished revision unpublished the lesson: %+v", got.Unpublished)
	}
	if p := w.pointer(first.LessonID); p == nil || *p != first.ID {
		t.Fatalf("pointer %v", p)
	}
	if n := w.count(`SELECT count(*) FROM lesson_revisions WHERE id = $1 AND candidate_closed_at IS NOT NULL`, pending.ID); n != 1 {
		t.Fatal("withdrawal did not close the candidate")
	}
	before := w.world()
	_, err = w.review.Publish(w.ctx, w.publisher, pending.ID)
	wantErr(t, "publish withdrawn candidate", err, review.ErrWithdrawn)
	_, err = w.review.Restore(w.ctx, w.publisher, pending.ID, ptr(first.ID))
	wantErr(t, "restore withdrawn candidate", err, review.ErrWithdrawn)
	w.unchanged(t, "blocked withdrawn candidate", before)

	next, err := w.lessons.CreateDraft(w.ctx, w.author, first.LessonID, lessons.Fields{Content: doc("Corrected")})
	if err != nil {
		t.Fatalf("draft after withdrawal: %v", err)
	}
	if next.BaseRevisionID == nil || *next.BaseRevisionID != first.ID {
		t.Fatalf("base %v", next.BaseRevisionID)
	}
	// The corrected successor goes through review and publication normally.
	next = w.approved(w.submit(w.author, next))
	w.published(next)
}

func TestWithdrawRules(t *testing.T) {
	w := newWithdrawEnv(t)
	first := w.approved(w.submitted(w.author, w.subject, "First edition"))
	w.published(first)
	inReview := w.successor(w.author, first.LessonID, "In review")
	before := w.world()

	for _, r := range []string{"", " \n\t"} {
		_, err := w.review.Withdraw(w.ctx, w.admin, first.ID, r, ptr(first.ID))
		wantErr(t, fmt.Sprintf("reason %q", r), err, review.ErrReasonRequired)
	}
	_, err := w.review.Withdraw(w.ctx, w.admin, first.ID, strings.Repeat("é", review.MaxReason+1), ptr(first.ID))
	wantErr(t, "long reason", err, review.ErrInvalidReason)
	_, err = w.review.Withdraw(w.ctx, w.admin, first.ID, "bad\x00reason", ptr(first.ID))
	wantErr(t, "NUL reason", err, review.ErrInvalidReason)
	_, err = w.review.Withdraw(w.ctx, w.admin, 999999, "Simulated reason.", nil)
	wantErr(t, "unknown revision", err, review.ErrNotFound)
	_, err = w.review.Withdraw(w.ctx, w.admin, inReview.ID, "Simulated reason.", ptr(first.ID))
	wantErr(t, "revision in review", err, review.ErrNotApproved)
	// Stale expected pointers: none, or another revision.
	_, err = w.review.Withdraw(w.ctx, w.admin, first.ID, "Simulated reason.", nil)
	wantErr(t, "expected unpublished", err, review.ErrStalePointer)
	_, err = w.review.Withdraw(w.ctx, w.admin, first.ID, "Simulated reason.", ptr(inReview.ID))
	wantErr(t, "expected another revision", err, review.ErrStalePointer)
	w.unchanged(t, "rejected withdrawals", before)

	if _, err := w.review.Withdraw(w.ctx, w.admin, first.ID, strings.Repeat("é", review.MaxReason), ptr(first.ID)); err != nil {
		t.Fatalf("withdrawal at the reason limit: %v", err)
	}
}

func TestUnpublishAndRestoreWithExpectedPointer(t *testing.T) {
	w := newWithdrawEnv(t)
	first, second := w.firstAndSecond()
	lesson := first.LessonID
	before := w.world()

	_, err := w.review.Unpublish(w.ctx, w.admin, lesson, "Simulated reason.", first.ID)
	wantErr(t, "unpublish with a stale pointer", err, review.ErrStalePointer)
	_, err = w.review.Unpublish(w.ctx, w.admin, lesson, " ", second.ID)
	wantErr(t, "unpublish without reason", err, review.ErrReasonRequired)
	_, err = w.review.Unpublish(w.ctx, w.admin, 999999, "Simulated reason.", second.ID)
	wantErr(t, "unpublish unknown lesson", err, review.ErrNotFound)
	_, err = w.review.Restore(w.ctx, w.publisher, first.ID, nil)
	wantErr(t, "restore expecting no pointer", err, review.ErrStalePointer)
	_, err = w.review.Restore(w.ctx, w.publisher, first.ID, ptr(first.ID))
	wantErr(t, "restore expecting the target", err, review.ErrStalePointer)
	_, err = w.review.Restore(w.ctx, w.publisher, second.ID, ptr(second.ID))
	wantErr(t, "restore the current revision", err, review.ErrAlreadyCurrent)
	_, err = w.review.Restore(w.ctx, w.publisher, 999999, nil)
	wantErr(t, "restore unknown revision", err, review.ErrNotFound)
	w.unchanged(t, "rejected unpublications and restorations", before)

	ch, err := w.review.Unpublish(w.ctx, w.admin, lesson, "  Simulated: unlawful image.  ", second.ID)
	if err != nil {
		t.Fatal(err)
	}
	if ch.Action != "unpublish" || ch.RevisionID != nil || *ch.PreviousRevisionID != second.ID || ch.Reason != "Simulated: unlawful image." {
		t.Fatalf("unpublication %+v", ch)
	}
	if p := w.pointer(lesson); p != nil {
		t.Fatalf("pointer %v", *p)
	}
	before = w.world()
	_, err = w.review.Unpublish(w.ctx, w.admin, lesson, "Simulated reason.", second.ID)
	wantErr(t, "unpublish twice", err, review.ErrNotPublished)
	w.unchanged(t, "second unpublication", before)

	// Restoring from no pointer; then to the other edition. No base check:
	// the first edition's base (none) is not the pointer.
	if _, err := w.review.Restore(w.ctx, w.publisher, first.ID, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := w.review.Restore(w.ctx, w.publisher, second.ID, ptr(first.ID)); err != nil {
		t.Fatal(err)
	}
	hist, err := w.review.PointerHistory(w.ctx, lesson)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, h := range hist {
		got = append(got, fmt.Sprintf("%s:%v->%v", h.Action, deref(h.PreviousRevisionID), deref(h.RevisionID)))
	}
	want := fmt.Sprintf("[publish:-->%d publish:%d->%d unpublish:%d->- restore:-->%d restore:%d->%d]",
		first.ID, first.ID, second.ID, second.ID, first.ID, first.ID, second.ID)
	if fmt.Sprint(got) != want {
		t.Fatalf("history %v, want %s", got, want)
	}
	// Restoration does not reopen candidates: a new draft is allowed and
	// is based on the restored revision.
	draft, err := w.lessons.CreateDraft(w.ctx, w.author, lesson, lessons.Fields{Content: doc("Third")})
	if err != nil || draft.BaseRevisionID == nil || *draft.BaseRevisionID != second.ID {
		t.Fatalf("draft after restoration: %+v %v", draft, err)
	}
}

func deref(p *int64) string {
	if p == nil {
		return "-"
	}
	return fmt.Sprint(*p)
}

// Restoration is limited to revisions published before (open question in
// docs/DECISIONS.md): an approved, never-published candidate goes through
// Publish and its base check.
func TestRestoreNeverPublishedIsRejected(t *testing.T) {
	w := newWithdrawEnv(t)
	first := w.approved(w.submitted(w.author, w.subject, "First edition"))
	w.published(first)
	pending := w.approved(w.successor(w.author, first.LessonID, "Pending"))
	other := w.submitted(w.author, w.subject, "Other lesson")
	before := w.world()

	_, err := w.review.Restore(w.ctx, w.publisher, pending.ID, ptr(first.ID))
	wantErr(t, "restore pending candidate", err, review.ErrNeverPublished)
	_, err = w.review.Restore(w.ctx, w.publisher, other.ID, nil)
	wantErr(t, "restore revision in review", err, review.ErrNotApproved)
	w.unchanged(t, "rejected restorations", before)

	// After an unpublication the pending candidate is stale for Publish and
	// still cannot be restored.
	if _, err := w.review.Unpublish(w.ctx, w.admin, first.LessonID, "Simulated reason.", first.ID); err != nil {
		t.Fatal(err)
	}
	before = w.world()
	_, err = w.review.Restore(w.ctx, w.publisher, pending.ID, nil)
	wantErr(t, "restore pending after unpublication", err, review.ErrNeverPublished)
	_, err = w.review.Publish(w.ctx, w.publisher, pending.ID)
	wantErr(t, "publish pending after unpublication", err, review.ErrStaleBase)
	w.unchanged(t, "stale candidate", before)
}

// Restoration rechecks the revision's content against the current
// validator. Stored content is altered here as the table owner with the
// immutability guard disabled, standing in for a validator that became
// stricter since the revision was published.
func TestRestoreRechecksEligibility(t *testing.T) {
	w := newWithdrawEnv(t)
	first, second := w.firstAndSecond()
	tx, err := w.db.Begin(w.ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Roll back on every path: a transaction left open by t.Fatal would make
	// the pool's Close in the test cleanup wait forever (harmless after Commit).
	defer func() { _ = tx.Rollback(w.ctx) }()
	for _, s := range []string{
		`ALTER TABLE lesson_revisions DISABLE TRIGGER zz_guard_lesson_revisions_immutable_snapshot`,
		fmt.Sprintf(`UPDATE lesson_revisions SET content = content || '{"unexpected": true}' WHERE id = %d`, first.ID),
		`ALTER TABLE lesson_revisions ENABLE TRIGGER zz_guard_lesson_revisions_immutable_snapshot`,
	} {
		if _, err := tx.Exec(w.ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(w.ctx); err != nil {
		t.Fatal(err)
	}
	before := w.world()
	_, err = w.review.Restore(w.ctx, w.publisher, first.ID, ptr(second.ID))
	wantErr(t, "restore ineligible content", err, review.ErrIneligible)
	w.unchanged(t, "ineligible restoration", before)
}

// A revocation or disablement committed before the in-transaction check
// denies withdrawal, unpublication and restoration.
func TestWithdrawRevocationBeforeCheckDenies(t *testing.T) {
	w := newWithdrawEnv(t)
	first, second := w.firstAndSecond()
	// A second active administrator, so the last-administrator guard
	// (LRN-013) does not refuse the revocation of w.admin.
	admin2 := w.user("admin-two", permissions.Administrator)
	before := w.world()
	revokeAdmin := review.SetTestHooks(func() {
		if _, err := w.perms.Revoke(w.ctx, testOperator, w.admin, permissions.Administrator); err != nil {
			t.Error(err)
		}
	}, nil)
	_, err := w.review.Withdraw(w.ctx, w.admin, second.ID, "Simulated reason.", ptr(second.ID))
	revokeAdmin()
	wantErr(t, "withdraw after revocation", err, review.ErrDenied)
	disable := review.SetTestHooks(func() {
		if err := w.exec(`UPDATE users SET disabled_at = now() WHERE id = $1`, admin2); err != nil {
			t.Error(err)
		}
	}, nil)
	_, err = w.review.Unpublish(w.ctx, admin2, first.LessonID, "Simulated reason.", second.ID)
	disable()
	wantErr(t, "unpublish after disablement", err, review.ErrDenied)
	revokePub := review.SetTestHooks(func() {
		if _, err := w.perms.Revoke(w.ctx, testOperator, w.publisher, permissions.Publisher); err != nil {
			t.Error(err)
		}
	}, nil)
	_, err = w.review.Restore(w.ctx, w.publisher, first.ID, ptr(second.ID))
	revokePub()
	wantErr(t, "restore after revocation", err, review.ErrDenied)
	w.unchanged(t, "revoked actions", before)
}

// Restore racing Publish on one lesson: the first to lock the lesson wins,
// the other sees the changed pointer and changes nothing. Exactly one new
// audit row per iteration; no lost pointer update.
func TestConcurrentRestoreAndPublish(t *testing.T) {
	w := newWithdrawEnv(t)
	for i := 0; i < 8; i++ {
		first := w.approved(w.submitted(w.author, w.subject, fmt.Sprintf("First %d", i)))
		w.published(first)
		second := w.approved(w.successor(w.author, first.LessonID, fmt.Sprintf("Second %d", i)))
		w.published(second)
		third := w.approved(w.successor(w.author, first.LessonID, fmt.Sprintf("Third %d", i)))
		audits := w.count(`SELECT count(*) FROM publication_audit WHERE lesson_id = $1`, first.LessonID)

		start := make(chan struct{})
		var wg sync.WaitGroup
		var restoreErr, publishErr error
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			_, restoreErr = w.review.Restore(w.ctx, w.publisher, first.ID, ptr(second.ID))
		}()
		go func() {
			defer wg.Done()
			<-start
			_, publishErr = w.review.Publish(w.ctx, w.publisher, third.ID)
		}()
		close(start)
		wg.Wait()
		p := w.pointer(first.LessonID)
		switch {
		case restoreErr == nil && errors.Is(publishErr, review.ErrStaleBase):
			if p == nil || *p != first.ID {
				t.Fatalf("iteration %d: restore won but pointer %v", i, p)
			}
		case publishErr == nil && errors.Is(restoreErr, review.ErrStalePointer):
			if p == nil || *p != third.ID {
				t.Fatalf("iteration %d: publish won but pointer %v", i, p)
			}
		default:
			t.Fatalf("iteration %d: restore %v, publish %v", i, restoreErr, publishErr)
		}
		if n := w.count(`SELECT count(*) FROM publication_audit WHERE lesson_id = $1`, first.LessonID); n != audits+1 {
			t.Fatalf("iteration %d: %d new audit rows", i, n-audits)
		}
	}
}

// Withdraw racing Publish of the same approved revision: either the
// withdrawal wins (the revision is closed and never published) or the
// publication wins (the withdrawal's expected pointer is stale).
func TestConcurrentWithdrawAndPublish(t *testing.T) {
	w := newWithdrawEnv(t)
	outcomes := map[string]int{}
	for i := 0; i < 10; i++ {
		first := w.approved(w.submitted(w.author, w.subject, fmt.Sprintf("First %d", i)))
		w.published(first)
		next := w.approved(w.successor(w.author, first.LessonID, fmt.Sprintf("Next %d", i)))

		start := make(chan struct{})
		var wg sync.WaitGroup
		var withdrawErr, publishErr error
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			_, withdrawErr = w.review.Withdraw(w.ctx, w.admin, next.ID, "Simulated reason.", ptr(first.ID))
		}()
		go func() {
			defer wg.Done()
			<-start
			_, publishErr = w.review.Publish(w.ctx, w.publisher, next.ID)
		}()
		close(start)
		wg.Wait()
		p := w.pointer(first.LessonID)
		withdrawals := w.count(`SELECT count(*) FROM revision_withdrawals WHERE revision_id = $1`, next.ID)
		audits := w.count(`SELECT count(*) FROM publication_audit WHERE lesson_id = $1`, first.LessonID)
		switch {
		case withdrawErr == nil && errors.Is(publishErr, review.ErrWithdrawn):
			outcomes["withdraw"]++
			if p == nil || *p != first.ID || withdrawals != 1 || audits != 1 {
				t.Fatalf("iteration %d: withdraw won but pointer %v, %d withdrawals, %d audits", i, p, withdrawals, audits)
			}
		case publishErr == nil && errors.Is(withdrawErr, review.ErrStalePointer):
			outcomes["publish"]++
			if p == nil || *p != next.ID || withdrawals != 0 || audits != 2 {
				t.Fatalf("iteration %d: publish won but pointer %v, %d withdrawals, %d audits", i, p, withdrawals, audits)
			}
		default:
			t.Fatalf("iteration %d: withdraw %v, publish %v", i, withdrawErr, publishErr)
		}
	}
	t.Logf("outcomes %v", outcomes)
}

// Concurrent restorations of different revisions expecting the same
// pointer: exactly one applies.
func TestConcurrentRestorations(t *testing.T) {
	w := newWithdrawEnv(t)
	first, second := w.firstAndSecond()
	third := w.approved(w.successor(w.author, first.LessonID, "Third"))
	w.published(third)
	audits := w.count(`SELECT count(*) FROM publication_audit WHERE lesson_id = $1`, first.LessonID)
	start := make(chan struct{})
	errs := make(chan error, 6)
	for i := 0; i < 6; i++ {
		target := first.ID
		if i%2 == 1 {
			target = second.ID
		}
		go func() {
			<-start
			_, err := w.review.Restore(w.ctx, w.publisher, target, ptr(third.ID))
			errs <- err
		}()
	}
	close(start)
	ok := 0
	for i := 0; i < 6; i++ {
		err := <-errs
		switch {
		case err == nil:
			ok++
		case errors.Is(err, review.ErrStalePointer), errors.Is(err, review.ErrAlreadyCurrent):
		default:
			t.Fatal(err)
		}
	}
	if ok != 1 {
		t.Fatalf("%d restorations applied, want 1", ok)
	}
	if n := w.count(`SELECT count(*) FROM publication_audit WHERE lesson_id = $1`, first.LessonID); n != audits+1 {
		t.Fatalf("%d new audit rows", n-audits)
	}
}
