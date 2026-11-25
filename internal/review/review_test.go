package review_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hadden-me/learnoo/internal/auth"
	"github.com/hadden-me/learnoo/internal/database/dbtest"
	"github.com/hadden-me/learnoo/internal/lessons"
	"github.com/hadden-me/learnoo/internal/permissions"
	"github.com/hadden-me/learnoo/internal/review"
	"github.com/hadden-me/learnoo/internal/users"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Simulated users, subjects and unreviewed test documents only. Nothing here
// is curriculum data or reviewed educational content.

var testArgon2 = auth.Argon2Params{MemoryKiB: 1024, Iterations: 1, Lanes: 1, SaltLen: 16, KeyLen: 32}

type env struct {
	t       *testing.T
	ctx     context.Context
	db      *pgxpool.Pool
	perms   *permissions.Store
	lessons *lessons.Store
	review  *review.Store
	subject int64
	author  int64
}

// testOperator is the local operator actor the tests grant and revoke with.
var testOperator = func() permissions.Actor {
	a, err := permissions.OperatorAs("test")
	if err != nil {
		panic(err)
	}
	return a
}()

func newEnv(t *testing.T) *env {
	t.Helper()
	db := dbtest.Migrated(t)
	e := &env{t: t, ctx: context.Background(), db: db, perms: permissions.NewStore(db),
		lessons: lessons.NewStore(db, nil), review: review.NewStore(db)}
	e.subject = e.newSubject("simulated-math")
	e.author = e.user("author", permissions.Author)
	return e
}

func (e *env) newSubject(code string) int64 {
	e.t.Helper()
	var id int64
	if err := e.db.QueryRow(e.ctx, `INSERT INTO subjects (code) VALUES ($1) RETURNING id`, code).Scan(&id); err != nil {
		e.t.Fatal(err)
	}
	return id
}

// user creates a simulated user with global permissions perms.
func (e *env) user(name string, perms ...permissions.Permission) int64 {
	e.t.Helper()
	hash, err := auth.HashPassword("simulated-password-1", testArgon2)
	if err != nil {
		e.t.Fatal(err)
	}
	u, err := users.Create(e.ctx, e.db, name+"@example.test", name, hash)
	if err != nil {
		e.t.Fatal(err)
	}
	for _, p := range perms {
		if _, err := e.perms.Grant(e.ctx, testOperator, u.ID, p); err != nil {
			e.t.Fatal(err)
		}
	}
	return u.ID
}

// reviewer creates a user assigned to review subjects, plus global perms.
func (e *env) reviewer(name string, subjects []int64, perms ...permissions.Permission) int64 {
	e.t.Helper()
	id := e.user(name, perms...)
	for _, s := range subjects {
		if _, err := e.perms.GrantReviewer(e.ctx, testOperator, id, s); err != nil {
			e.t.Fatal(err)
		}
	}
	return id
}

func doc(title string) []byte {
	d := map[string]any{
		"schemaVersion": 1, "title": title, "language": "fr", "status": "unreviewed",
		"objective": "Simulated objective.",
		"blocks": []any{map[string]any{"type": "paragraph", "children": []any{
			map[string]any{"type": "text", "text": "Simulated text: " + title}}}},
	}
	b, err := json.Marshal(d)
	if err != nil {
		panic(err)
	}
	return b
}

// submitted creates a lesson by author in subject and submits its first draft.
func (e *env) submitted(author, subject int64, title string) lessons.Revision {
	e.t.Helper()
	rev, err := e.lessons.CreateLesson(e.ctx, author, lessons.NewLesson{SubjectID: subject, Language: "fr",
		Fields: lessons.Fields{Content: doc(title)}})
	if err != nil {
		e.t.Fatal(err)
	}
	return e.submit(author, rev)
}

func (e *env) submit(author int64, rev lessons.Revision) lessons.Revision {
	e.t.Helper()
	rev, err := e.lessons.Submit(e.ctx, author, rev.ID, rev.Version)
	if err != nil {
		e.t.Fatal(err)
	}
	return rev
}

// successor creates and submits the next draft of a lesson.
func (e *env) successor(author, lessonID int64, title string) lessons.Revision {
	e.t.Helper()
	rev, err := e.lessons.CreateDraft(e.ctx, author, lessonID, lessons.Fields{Content: doc(title)})
	if err != nil {
		e.t.Fatal(err)
	}
	return e.submit(author, rev)
}

func (e *env) count(sql string, args ...any) int {
	e.t.Helper()
	var n int
	if err := e.db.QueryRow(e.ctx, sql, args...).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

func (e *env) exec(sql string, args ...any) error {
	_, err := e.db.Exec(e.ctx, sql, args...)
	return err
}

// inTx runs stmts in one transaction and commits it.
func (e *env) inTx(stmts ...string) error {
	tx, err := e.db.Begin(e.ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(e.ctx) }()
	for _, s := range stmts {
		if _, err := tx.Exec(e.ctx, s); err != nil {
			return err
		}
	}
	return tx.Commit(e.ctx)
}

// state is the revision's status, its decision count, the lesson's pointer
// and its publication audit count, for "nothing changed" checks.
func (e *env) state(rev lessons.Revision) string {
	e.t.Helper()
	var s string
	err := e.db.QueryRow(e.ctx, `
		SELECT r.status || '|' || r.updated_at || '|' || coalesce(r.candidate_closed_at::text, '-') || '|' ||
		       (SELECT count(*) FROM review_decisions WHERE revision_id = r.id) || '|' ||
		       coalesce(l.published_revision_id::text, '-') || '|' ||
		       (SELECT count(*) FROM publication_audit WHERE lesson_id = l.id)
		  FROM lesson_revisions r JOIN lessons l ON l.id = r.lesson_id WHERE r.id = $1`, rev.ID).Scan(&s)
	if err != nil {
		e.t.Fatal(err)
	}
	return s
}

func (e *env) pointer(lessonID int64) *int64 {
	e.t.Helper()
	l, err := e.lessons.Lesson(e.ctx, lessonID)
	if err != nil {
		e.t.Fatal(err)
	}
	return l.PublishedRevisionID
}

func wantErr(t *testing.T, what string, err, want error) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("%s: want %v, got %v", what, want, err)
	}
}

func wantSQLState(t *testing.T, what string, err error, code string) {
	t.Helper()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != code {
		t.Errorf("%s: want SQLSTATE %s, got %v", what, code, err)
	}
}

// wantGuard asserts an LRN01 error whose message contains msg.
func wantGuard(t *testing.T, what string, err error, msg string) {
	t.Helper()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "LRN01" || !strings.Contains(pgErr.Message, msg) {
		t.Errorf("%s: want LRN01 %q, got %v", what, msg, err)
	}
}

// waitBlockedBy waits until some backend waits on a lock held by pid.
func (e *env) waitBlockedBy(pid uint32) {
	e.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var blocked bool
		err := e.db.QueryRow(e.ctx, `SELECT EXISTS (SELECT 1 FROM pg_stat_activity WHERE $1::int = ANY (pg_blocking_pids(pid)))`, int32(pid)).Scan(&blocked)
		if err != nil {
			e.t.Fatal(err)
		}
		if blocked {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	e.t.Fatal("no backend waited on the held lock")
}

func TestApproveRecordsReviewer(t *testing.T) {
	e := newEnv(t)
	reviewer := e.reviewer("reviewer", []int64{e.subject})
	rev := e.submitted(e.author, e.subject, "Approved")

	got, err := e.review.Approve(e.ctx, reviewer, rev.ID, "  Clear and correct.  ")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != lessons.StatusApproved || got.AuthorID != e.author || got.Decision == nil ||
		got.Decision.Action != review.ActionApprove || got.Decision.ReviewerID != reviewer ||
		got.Decision.Comment != "Clear and correct." || got.ApprovedBy() == nil || *got.ApprovedBy() != reviewer {
		t.Fatalf("approved revision: %+v %+v", got, got.Decision)
	}
	// The snapshot is unchanged apart from status and updated_at.
	if string(got.Content) != string(rev.Content) || got.Version != rev.Version || got.Title != rev.Title {
		t.Fatalf("approval changed the snapshot: %+v", got)
	}
	// The author reads the decision too.
	again, err := e.lessons.Revision(e.ctx, rev.ID)
	if err != nil || again.Decision == nil || again.Decision.ReviewerID != reviewer {
		t.Fatalf("stored decision: %+v %v", again, err)
	}
	// An approval without comment stores none.
	other := e.submitted(e.author, e.subject, "No comment")
	got, err = e.review.Approve(e.ctx, reviewer, other.ID, " ")
	if err != nil || got.Decision.Comment != "" {
		t.Fatalf("approve without comment: %+v %v", got.Decision, err)
	}
	if n := e.count(`SELECT count(*) FROM review_decisions WHERE revision_id = $1 AND comment IS NULL`, other.ID); n != 1 {
		t.Fatalf("blank approval comment stored: %d", n)
	}
}

func TestRequestChangesThenSuccessor(t *testing.T) {
	e := newEnv(t)
	reviewer := e.reviewer("reviewer", []int64{e.subject})
	rev := e.submitted(e.author, e.subject, "First")

	for _, c := range []string{"", "   \n\t"} {
		_, err := e.review.RequestChanges(e.ctx, reviewer, rev.ID, c)
		wantErr(t, fmt.Sprintf("comment %q", c), err, review.ErrCommentRequired)
	}
	_, err := e.review.RequestChanges(e.ctx, reviewer, rev.ID, strings.Repeat("é", review.MaxComment+1))
	wantErr(t, "long comment", err, review.ErrInvalidComment)
	_, err = e.review.Approve(e.ctx, reviewer, rev.ID, "bad\x00comment")
	wantErr(t, "NUL in comment", err, review.ErrInvalidComment)
	if n := e.count(`SELECT count(*) FROM review_decisions`); n != 0 {
		t.Fatalf("rejected decisions stored: %d", n)
	}

	got, err := e.review.RequestChanges(e.ctx, reviewer, rev.ID, strings.Repeat("é", review.MaxComment))
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != lessons.StatusChangesRequested || got.Decision == nil || got.Decision.Action != review.ActionRequestChanges ||
		got.ApprovedBy() != nil {
		t.Fatalf("changes requested: %+v", got)
	}
	_, err = e.review.Approve(e.ctx, reviewer, rev.ID, "")
	wantErr(t, "approve after changes requested", err, review.ErrNotInReview)

	// The author corrects in a successor, which is reviewed again.
	next := e.successor(e.author, rev.LessonID, "Corrected")
	if next.Number != 2 {
		t.Fatalf("successor %+v", next)
	}
	if _, err := e.review.Approve(e.ctx, reviewer, next.ID, ""); err != nil {
		t.Fatal(err)
	}
	if n := e.count(`SELECT count(*) FROM review_decisions WHERE revision_id IN ($1, $2)`, rev.ID, next.ID); n != 2 {
		t.Fatalf("%d decisions, want 2", n)
	}
}

func TestDecisionDenials(t *testing.T) {
	e := newEnv(t)
	physics := e.newSubject("simulated-physics")
	rev := e.submitted(e.author, e.subject, "Denied")
	before := e.state(rev)

	disabled := e.reviewer("disabled-reviewer", []int64{e.subject})
	if err := e.exec(`UPDATE users SET disabled_at = now() WHERE id = $1`, disabled); err != nil {
		t.Fatal(err)
	}
	for who, id := range map[string]int64{
		"anonymous":                   0,
		"unknown user":                999999,
		"author only":                 e.user("other-author", permissions.Author),
		"reviewer of another subject": e.reviewer("physics-reviewer", []int64{physics}),
		"publisher without reviewer":  e.user("publisher", permissions.Publisher),
		"administrator without assignment": e.user("admin", permissions.Administrator, permissions.Publisher,
			permissions.Author),
		"disabled reviewer": disabled,
	} {
		_, err := e.review.Approve(e.ctx, id, rev.ID, "")
		wantErr(t, who+" approve", err, review.ErrDenied)
		_, err = e.review.RequestChanges(e.ctx, id, rev.ID, "Simulated comment.")
		wantErr(t, who+" request changes", err, review.ErrDenied)
		_, err = e.review.ReviewRevision(e.ctx, id, rev.ID)
		wantErr(t, who+" read", err, review.ErrNotFound)
	}
	if after := e.state(rev); after != before {
		t.Fatalf("denied decisions changed state: %s -> %s", before, after)
	}
	_, err := e.review.Approve(e.ctx, e.reviewer("reviewer", []int64{e.subject}), 999999, "")
	wantErr(t, "unknown revision", err, review.ErrNotFound)
}

// Nobody approves their own revision, administrators included, and the
// database refuses it even when the Store is bypassed.
func TestSelfApprovalIsRejected(t *testing.T) {
	e := newEnv(t)
	admin := e.reviewer("admin", []int64{e.subject}, permissions.Administrator, permissions.Publisher, permissions.Author)
	rev := e.submitted(admin, e.subject, "Own lesson")
	before := e.state(rev)

	_, err := e.review.Approve(e.ctx, admin, rev.ID, "")
	wantErr(t, "self-approval", err, review.ErrSelfReview)
	_, err = e.review.RequestChanges(e.ctx, admin, rev.ID, "Simulated comment.")
	wantErr(t, "self change request", err, review.ErrSelfReview)
	// Reading is allowed (it is their own), deciding is not.
	if _, err := e.review.ReviewRevision(e.ctx, admin, rev.ID); err != nil {
		t.Fatalf("own revision read: %v", err)
	}
	wantGuard(t, "direct self-approval", e.inTx(
		fmt.Sprintf(`INSERT INTO review_decisions (revision_id, reviewer_id, action) VALUES (%d, %d, 'approve')`, rev.ID, admin),
		fmt.Sprintf(`UPDATE lesson_revisions SET status = 'approved' WHERE id = %d`, rev.ID)), "own revision")
	if after := e.state(rev); after != before {
		t.Fatalf("self-approval changed state: %s -> %s", before, after)
	}
	// A different reviewer may approve it; publishing it is then allowed for
	// the author too, because publishing is a separate permission.
	other := e.reviewer("other-reviewer", []int64{e.subject})
	if _, err := e.review.Approve(e.ctx, other, rev.ID, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := e.review.Publish(e.ctx, admin, rev.ID); err != nil {
		t.Fatalf("publisher publishing own approved revision: %v", err)
	}
}

func TestDecisionsOnlyInReview(t *testing.T) {
	e := newEnv(t)
	reviewer := e.reviewer("reviewer", []int64{e.subject})
	draft, err := e.lessons.CreateLesson(e.ctx, e.author, lessons.NewLesson{SubjectID: e.subject, Language: "fr",
		Fields: lessons.Fields{Content: doc("Draft")}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = e.review.Approve(e.ctx, reviewer, draft.ID, "")
	wantErr(t, "approve draft", err, review.ErrNotInReview)
	_, err = e.review.ReviewRevision(e.ctx, reviewer, draft.ID)
	wantErr(t, "read draft", err, review.ErrNotFound)

	rev := e.submitted(e.author, e.subject, "Approved twice")
	if _, err := e.review.Approve(e.ctx, reviewer, rev.ID, ""); err != nil {
		t.Fatal(err)
	}
	_, err = e.review.Approve(e.ctx, reviewer, rev.ID, "")
	wantErr(t, "approve twice", err, review.ErrNotInReview)
	_, err = e.review.RequestChanges(e.ctx, reviewer, rev.ID, "Too late.")
	wantErr(t, "request changes after approval", err, review.ErrNotInReview)
	_, err = e.review.ReviewRevision(e.ctx, reviewer, rev.ID)
	wantErr(t, "read approved", err, review.ErrNotFound)
}

// Concurrent approve and request-changes on one revision: exactly one
// decision is applied.
func TestConcurrentDecisionsExactlyOne(t *testing.T) {
	e := newEnv(t)
	first := e.reviewer("reviewer-one", []int64{e.subject})
	second := e.reviewer("reviewer-two", []int64{e.subject})
	for i := 0; i < 15; i++ {
		rev := e.submitted(e.author, e.subject, fmt.Sprintf("Race %d", i))
		start := make(chan struct{})
		errs := make(chan error, 4)
		var wg sync.WaitGroup
		for _, f := range []func() error{
			func() error { _, err := e.review.Approve(e.ctx, first, rev.ID, ""); return err },
			func() error {
				_, err := e.review.RequestChanges(e.ctx, second, rev.ID, "Simulated comment.")
				return err
			},
			func() error { _, err := e.review.Approve(e.ctx, second, rev.ID, ""); return err },
			func() error {
				_, err := e.review.RequestChanges(e.ctx, first, rev.ID, "Simulated comment.")
				return err
			},
		} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				errs <- f()
			}()
		}
		close(start)
		wg.Wait()
		close(errs)
		ok := 0
		for err := range errs {
			switch {
			case err == nil:
				ok++
			case !errors.Is(err, review.ErrNotInReview):
				t.Fatalf("iteration %d: %v", i, err)
			}
		}
		if ok != 1 {
			t.Fatalf("iteration %d: %d decisions applied, want 1", i, ok)
		}
		var status, action string
		if err := e.db.QueryRow(e.ctx, `SELECT r.status, d.action FROM lesson_revisions r JOIN review_decisions d ON d.revision_id = r.id
			WHERE r.id = $1`, rev.ID).Scan(&status, &action); err != nil {
			t.Fatalf("iteration %d: %v", i, err)
		}
		if (action == review.ActionApprove) != (status == "approved") {
			t.Fatalf("iteration %d: status %s with decision %s", i, status, action)
		}
	}
}

func TestPublish(t *testing.T) {
	e := newEnv(t)
	reviewer := e.reviewer("reviewer", []int64{e.subject})
	publisher := e.user("publisher", permissions.Publisher)
	rev := e.submitted(e.author, e.subject, "First edition")
	if _, err := e.review.Approve(e.ctx, reviewer, rev.ID, ""); err != nil {
		t.Fatal(err)
	}
	// The approved revision blocks drafts until published.
	_, err := e.lessons.CreateDraft(e.ctx, e.author, rev.LessonID, lessons.Fields{Content: doc("Blocked")})
	wantErr(t, "draft before publication", err, lessons.ErrActiveCandidateExists)

	pub, err := e.review.Publish(e.ctx, publisher, rev.ID)
	if err != nil {
		t.Fatal(err)
	}
	if pub.LessonID != rev.LessonID || pub.RevisionID != rev.ID || pub.PreviousRevisionID != nil || pub.ActorID != publisher || pub.PublishedAt.IsZero() {
		t.Fatalf("publication %+v", pub)
	}
	if p := e.pointer(rev.LessonID); p == nil || *p != rev.ID {
		t.Fatalf("pointer %v", p)
	}
	_, err = e.review.Publish(e.ctx, publisher, rev.ID)
	wantErr(t, "publish twice", err, review.ErrAlreadyPublished)

	// The next edition replaces it, with the previous pointer audited.
	next := e.successor(e.author, rev.LessonID, "Second edition")
	if next.BaseRevisionID == nil || *next.BaseRevisionID != rev.ID {
		t.Fatalf("base %v", next.BaseRevisionID)
	}
	if p := e.pointer(rev.LessonID); p == nil || *p != rev.ID {
		t.Fatal("published revision changed while the next edition is in review")
	}
	_, err = e.review.Publish(e.ctx, publisher, next.ID)
	wantErr(t, "publish in review", err, review.ErrNotApproved)
	if _, err := e.review.Approve(e.ctx, reviewer, next.ID, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := e.review.Publish(e.ctx, publisher, next.ID); err != nil {
		t.Fatal(err)
	}
	hist, err := e.review.PublicationHistory(e.ctx, rev.LessonID)
	if err != nil {
		t.Fatal(err)
	}
	if len(hist) != 2 || hist[0].RevisionID != rev.ID || hist[0].PreviousRevisionID != nil ||
		hist[1].RevisionID != next.ID || hist[1].PreviousRevisionID == nil || *hist[1].PreviousRevisionID != rev.ID ||
		hist[1].ActorID != publisher {
		t.Fatalf("history %+v", hist)
	}
	// Publishing an earlier revision again is restoration (LRN-009), not Publish.
	_, err = e.review.Publish(e.ctx, publisher, rev.ID)
	wantErr(t, "republish earlier revision", err, review.ErrAlreadyPublished)
}

func TestPublishDenialsAndEligibility(t *testing.T) {
	e := newEnv(t)
	reviewer := e.reviewer("reviewer", []int64{e.subject})
	rev := e.submitted(e.author, e.subject, "To publish")
	if _, err := e.review.Approve(e.ctx, reviewer, rev.ID, ""); err != nil {
		t.Fatal(err)
	}
	before := e.state(rev)
	disabled := e.user("disabled-publisher", permissions.Publisher)
	if err := e.exec(`UPDATE users SET disabled_at = now() WHERE id = $1`, disabled); err != nil {
		t.Fatal(err)
	}
	for who, id := range map[string]int64{
		"anonymous":                  0,
		"author only":                e.author,
		"reviewer without publisher": reviewer,
		"administrator only":         e.user("admin", permissions.Administrator),
		"disabled publisher":         disabled,
	} {
		_, err := e.review.Publish(e.ctx, id, rev.ID)
		wantErr(t, who, err, review.ErrDenied)
	}
	if after := e.state(rev); after != before {
		t.Fatalf("denied publications changed state: %s -> %s", before, after)
	}

	publisher := e.user("publisher", permissions.Publisher)
	_, err := e.review.Publish(e.ctx, publisher, 999999)
	wantErr(t, "unknown revision", err, review.ErrNotFound)
	draft, err := e.lessons.CreateLesson(e.ctx, e.author, lessons.NewLesson{SubjectID: e.subject, Language: "fr",
		Fields: lessons.Fields{Content: doc("Draft")}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = e.review.Publish(e.ctx, publisher, draft.ID)
	wantErr(t, "draft", err, review.ErrNotApproved)
	requested := e.submitted(e.author, e.subject, "Changes requested")
	if _, err := e.review.RequestChanges(e.ctx, reviewer, requested.ID, "Simulated comment."); err != nil {
		t.Fatal(err)
	}
	_, err = e.review.Publish(e.ctx, publisher, requested.ID)
	wantErr(t, "changes requested", err, review.ErrNotApproved)

	// Archived lessons are not published.
	archived := e.submitted(e.author, e.subject, "Archived")
	if _, err := e.review.Approve(e.ctx, reviewer, archived.ID, ""); err != nil {
		t.Fatal(err)
	}
	if err := e.exec(`UPDATE lessons SET archived_at = now() WHERE id = $1`, archived.LessonID); err != nil {
		t.Fatal(err)
	}
	_, err = e.review.Publish(e.ctx, publisher, archived.ID)
	wantErr(t, "archived", err, review.ErrArchived)
	if n := e.count(`SELECT count(*) FROM publication_audit`); n != 0 {
		t.Fatalf("rejected publications audited: %d", n)
	}
}

// A revision drafted against a published revision that is no longer
// current is not published, and nothing changes.
func TestPublishStaleBaseConflicts(t *testing.T) {
	e := newEnv(t)
	reviewer := e.reviewer("reviewer", []int64{e.subject})
	publisher := e.user("publisher", permissions.Publisher)
	approve := func(r lessons.Revision) {
		t.Helper()
		if _, err := e.review.Approve(e.ctx, reviewer, r.ID, ""); err != nil {
			t.Fatal(err)
		}
	}
	first := e.submitted(e.author, e.subject, "First")
	approve(first)
	if _, err := e.review.Publish(e.ctx, publisher, first.ID); err != nil {
		t.Fatal(err)
	}
	second := e.successor(e.author, first.LessonID, "Second")
	approve(second)

	// Unpublication (LRN-009) simulated with direct SQL: the pointer is cleared.
	if err := e.exec(`UPDATE lessons SET published_revision_id = NULL WHERE id = $1`, first.LessonID); err != nil {
		t.Fatal(err)
	}
	before := e.state(second)
	_, err := e.review.Publish(e.ctx, publisher, second.ID)
	wantErr(t, "stale after unpublication", err, review.ErrStaleBase)
	if after := e.state(second); after != before {
		t.Fatalf("stale publication changed state: %s -> %s", before, after)
	}

	// Restoration (LRN-009) simulated with direct SQL: the pointer goes back
	// to the first edition, the second edition's base; it publishes again.
	if err := e.inTx(
		fmt.Sprintf(`INSERT INTO publication_audit (lesson_id, revision_id, actor_id, action) VALUES (%d, %d, %d, 'publish')`, first.LessonID, first.ID, publisher),
		fmt.Sprintf(`UPDATE lessons SET published_revision_id = %d WHERE id = %d`, first.ID, first.LessonID)); err != nil {
		t.Fatal(err)
	}
	if _, err := e.review.Publish(e.ctx, publisher, second.ID); err != nil {
		t.Fatalf("publish with matching base: %v", err)
	}
	// A third edition drafted on the second, while the pointer is moved back
	// to the first: stale.
	third := e.successor(e.author, first.LessonID, "Third")
	approve(third)
	if err := e.inTx(
		fmt.Sprintf(`INSERT INTO publication_audit (lesson_id, revision_id, previous_revision_id, actor_id, action) VALUES (%d, %d, %d, %d, 'publish')`,
			first.LessonID, first.ID, second.ID, publisher),
		fmt.Sprintf(`UPDATE lessons SET published_revision_id = %d WHERE id = %d`, first.ID, first.LessonID)); err != nil {
		t.Fatal(err)
	}
	before = e.state(third)
	_, err = e.review.Publish(e.ctx, publisher, third.ID)
	wantErr(t, "stale after restoration", err, review.ErrStaleBase)
	if after := e.state(third); after != before {
		t.Fatalf("stale publication changed state: %s -> %s", before, after)
	}
	if p := e.pointer(first.LessonID); p == nil || *p != first.ID {
		t.Fatalf("pointer overwritten: %v", p)
	}
}

// Concurrent publications of one revision: one wins, the others see it
// published; the pointer is set once and audited once. Concurrent
// publications of different lessons do not interfere.
func TestConcurrentPublishes(t *testing.T) {
	e := newEnv(t)
	reviewer := e.reviewer("reviewer", []int64{e.subject})
	publishers := []int64{e.user("publisher-one", permissions.Publisher), e.user("publisher-two", permissions.Publisher)}
	var lessonIDs []int64
	for i := 0; i < 10; i++ {
		rev := e.submitted(e.author, e.subject, fmt.Sprintf("Race %d", i))
		if _, err := e.review.Approve(e.ctx, reviewer, rev.ID, ""); err != nil {
			t.Fatal(err)
		}
		lessonIDs = append(lessonIDs, rev.LessonID)
		const n = 6
		start := make(chan struct{})
		errs := make(chan error, n)
		for w := 0; w < n; w++ {
			go func() {
				<-start
				_, err := e.review.Publish(e.ctx, publishers[w%2], rev.ID)
				errs <- err
			}()
		}
		close(start)
		ok := 0
		for w := 0; w < n; w++ {
			err := <-errs
			switch {
			case err == nil:
				ok++
			case !errors.Is(err, review.ErrAlreadyPublished):
				t.Fatalf("iteration %d: %v", i, err)
			}
		}
		if ok != 1 {
			t.Fatalf("iteration %d: %d publications succeeded, want 1", i, ok)
		}
		if n := e.count(`SELECT count(*) FROM publication_audit WHERE lesson_id = $1`, rev.LessonID); n != 1 {
			t.Fatalf("iteration %d: %d audit rows", i, n)
		}
		if p := e.pointer(rev.LessonID); p == nil || *p != rev.ID {
			t.Fatalf("iteration %d: pointer %v", i, p)
		}
	}

	// Successors of every lesson, approved, then all published at once
	// from two publishers each: every lesson ends on its successor with one
	// new audit row whose previous revision is the first edition.
	var next []lessons.Revision
	for i, l := range lessonIDs {
		r := e.successor(e.author, l, fmt.Sprintf("Successor %d", i))
		if _, err := e.review.Approve(e.ctx, reviewer, r.ID, ""); err != nil {
			t.Fatal(err)
		}
		next = append(next, r)
	}
	start := make(chan struct{})
	var wg sync.WaitGroup
	results := make(chan error, 2*len(next))
	for _, r := range next {
		for _, p := range publishers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				_, err := e.review.Publish(e.ctx, p, r.ID)
				results <- err
			}()
		}
	}
	close(start)
	wg.Wait()
	close(results)
	ok := 0
	for err := range results {
		switch {
		case err == nil:
			ok++
		case !errors.Is(err, review.ErrAlreadyPublished):
			t.Fatal(err)
		}
	}
	if ok != len(next) {
		t.Fatalf("%d successor publications, want %d", ok, len(next))
	}
	for i, r := range next {
		if p := e.pointer(r.LessonID); p == nil || *p != r.ID {
			t.Fatalf("lesson %d: pointer %v, want %d", i, p, r.ID)
		}
		hist, err := e.review.PublicationHistory(e.ctx, r.LessonID)
		if err != nil {
			t.Fatal(err)
		}
		if len(hist) != 2 || hist[1].RevisionID != r.ID || hist[1].PreviousRevisionID == nil || *hist[1].PreviousRevisionID != hist[0].RevisionID {
			t.Fatalf("lesson %d: history %+v", i, hist)
		}
	}
}

// A revocation committed after the request started but before the
// in-transaction check denies the write; nothing is recorded.
func TestRevocationBeforeCheckDenies(t *testing.T) {
	e := newEnv(t)
	reviewer := e.reviewer("reviewer", []int64{e.subject})
	publisher := e.user("publisher", permissions.Publisher)
	rev := e.submitted(e.author, e.subject, "Revoked reviewer")
	before := e.state(rev)

	restore := review.SetTestHooks(func() {
		if _, err := e.perms.RevokeReviewer(e.ctx, testOperator, reviewer, e.subject); err != nil {
			t.Error(err)
		}
	}, nil)
	_, err := e.review.Approve(e.ctx, reviewer, rev.ID, "")
	restore()
	wantErr(t, "approve after revocation", err, review.ErrDenied)
	if after := e.state(rev); after != before {
		t.Fatalf("revoked approval changed state: %s -> %s", before, after)
	}

	// Disablement of a reviewer works the same way.
	second := e.reviewer("second-reviewer", []int64{e.subject})
	restore = review.SetTestHooks(func() {
		if err := e.exec(`UPDATE users SET disabled_at = now() WHERE id = $1`, second); err != nil {
			t.Error(err)
		}
	}, nil)
	_, err = e.review.RequestChanges(e.ctx, second, rev.ID, "Simulated comment.")
	restore()
	wantErr(t, "request changes after disablement", err, review.ErrDenied)

	third := e.reviewer("third-reviewer", []int64{e.subject})
	if _, err := e.review.Approve(e.ctx, third, rev.ID, ""); err != nil {
		t.Fatal(err)
	}
	before = e.state(rev)
	restore = review.SetTestHooks(func() {
		if _, err := e.perms.Revoke(e.ctx, testOperator, publisher, permissions.Publisher); err != nil {
			t.Error(err)
		}
	}, nil)
	_, err = e.review.Publish(e.ctx, publisher, rev.ID)
	restore()
	wantErr(t, "publish after revocation", err, review.ErrDenied)
	if after := e.state(rev); after != before {
		t.Fatalf("revoked publication changed state: %s -> %s", before, after)
	}
}

// A revocation started after the in-transaction check waits for the write
// to commit (the grant and account rows are share-locked), then applies to
// the next request.
func TestRevocationWaitsForInFlightWrite(t *testing.T) {
	e := newEnv(t)
	reviewer := e.reviewer("reviewer", []int64{e.subject})
	publisher := e.user("publisher", permissions.Publisher)
	first := e.submitted(e.author, e.subject, "First")
	second := e.submitted(e.author, e.subject, "Second")

	hold := func(revoke func() error) (restore func(), done chan error) {
		done = make(chan error, 1)
		restore = review.SetTestHooks(nil, func(tx pgx.Tx) {
			go func() { done <- revoke() }()
			e.waitBlockedBy(tx.Conn().PgConn().PID())
			select {
			case err := <-done:
				t.Errorf("revocation completed during the write: %v", err)
			default:
			}
		})
		return restore, done
	}

	restore, done := hold(func() error {
		_, err := e.perms.RevokeReviewer(e.ctx, testOperator, reviewer, e.subject)
		return err
	})
	if _, err := e.review.Approve(e.ctx, reviewer, first.ID, ""); err != nil {
		t.Fatalf("approval holding the grant: %v", err)
	}
	restore()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	_, err := e.review.Approve(e.ctx, reviewer, second.ID, "")
	wantErr(t, "next approval", err, review.ErrDenied)

	restore, done = hold(func() error {
		_, err := e.perms.Revoke(e.ctx, testOperator, publisher, permissions.Publisher)
		return err
	})
	if _, err := e.review.Publish(e.ctx, publisher, first.ID); err != nil {
		t.Fatalf("publication holding the grant: %v", err)
	}
	restore()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	other := e.reviewer("other", []int64{e.subject})
	if _, err := e.review.Approve(e.ctx, other, second.ID, ""); err != nil {
		t.Fatal(err)
	}
	_, err = e.review.Publish(e.ctx, publisher, second.ID)
	wantErr(t, "next publication", err, review.ErrDenied)
}

func TestQueueAndReviewerReads(t *testing.T) {
	e := newEnv(t)
	physics := e.newSubject("simulated-physics")
	reviewer := e.reviewer("reviewer", []int64{e.subject}, permissions.Author)
	both := e.reviewer("both", []int64{e.subject, physics})

	a := e.submitted(e.author, e.subject, "Math A")
	b := e.submitted(e.author, physics, "Physics B")
	own := e.submitted(reviewer, e.subject, "Reviewer's own")
	draft, err := e.lessons.CreateLesson(e.ctx, e.author, lessons.NewLesson{SubjectID: e.subject, Language: "fr",
		Fields: lessons.Fields{Content: doc("Draft")}})
	if err != nil {
		t.Fatal(err)
	}
	ids := func(items []review.QueueItem) []int64 {
		var out []int64
		for _, it := range items {
			out = append(out, it.RevisionID)
		}
		return out
	}
	q, err := e.review.ReviewQueue(e.ctx, reviewer, 0)
	if err != nil || fmt.Sprint(ids(q)) != fmt.Sprint([]int64{a.ID}) {
		t.Fatalf("reviewer queue %v %v (own %d excluded, draft %d excluded)", ids(q), err, own.ID, draft.ID)
	}
	if q[0].AuthorID != e.author || q[0].Title != "Math A" || q[0].SubjectID != e.subject {
		t.Fatalf("queue item %+v", q[0])
	}
	q, err = e.review.ReviewQueue(e.ctx, both, 0)
	if err != nil || fmt.Sprint(ids(q)) != fmt.Sprint([]int64{a.ID, b.ID, own.ID}) {
		t.Fatalf("two-subject queue %v %v", ids(q), err)
	}
	q, err = e.review.ReviewQueue(e.ctx, both, physics)
	if err != nil || fmt.Sprint(ids(q)) != fmt.Sprint([]int64{b.ID}) {
		t.Fatalf("physics queue %v %v", ids(q), err)
	}
	_, err = e.review.ReviewQueue(e.ctx, reviewer, physics)
	wantErr(t, "queue of an unassigned subject", err, review.ErrNotFound)
	_, err = e.review.ReviewQueue(e.ctx, reviewer, 999999)
	wantErr(t, "queue of an unknown subject", err, review.ErrNotFound)
	if q, err := e.review.ReviewQueue(e.ctx, e.author, 0); err != nil || len(q) != 0 {
		t.Fatalf("non-reviewer queue %v %v", q, err)
	}

	got, err := e.review.ReviewRevision(e.ctx, reviewer, a.ID)
	if err != nil || got.ID != a.ID || string(got.Content) != string(a.Content) {
		t.Fatalf("reviewer read %+v %v", got, err)
	}
	_, err = e.review.ReviewRevision(e.ctx, reviewer, b.ID)
	wantErr(t, "other subject read", err, review.ErrNotFound)
	_, err = e.review.ReviewRevision(e.ctx, reviewer, 999999)
	wantErr(t, "unknown read", err, review.ErrNotFound)

	// Decided revisions leave the queue.
	if _, err := e.review.Approve(e.ctx, both, a.ID, ""); err != nil {
		t.Fatal(err)
	}
	if q, err := e.review.ReviewQueue(e.ctx, reviewer, 0); err != nil || len(q) != 0 {
		t.Fatalf("queue after decision %v %v", ids(q), err)
	}
	// Revoked reviewers see nothing.
	if _, err := e.perms.RevokeReviewer(e.ctx, testOperator, both, physics); err != nil {
		t.Fatal(err)
	}
	_, err = e.review.ReviewRevision(e.ctx, both, b.ID)
	wantErr(t, "read after revocation", err, review.ErrNotFound)
	q, err = e.review.ReviewQueue(e.ctx, both, 0)
	if err != nil || fmt.Sprint(ids(q)) != fmt.Sprint([]int64{own.ID}) {
		t.Fatalf("queue after revocation %v %v", ids(q), err)
	}
}
