package server_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/hadden-me/learnoo/internal/permissions"
	"github.com/hadden-me/learnoo/internal/review"
)

// Read endpoints of the Studio's publisher and administrator views (issue
// #66 second slice) through the real server handler. Simulated users,
// subjects and unreviewed test documents only.

type publishQueueBody struct {
	Revisions []struct {
		RevisionID          int64  `json:"revisionId"`
		LessonID            int64  `json:"lessonId"`
		LessonPublicID      string `json:"lessonPublicId"`
		Number              int    `json:"number"`
		Title               string `json:"title"`
		Language            string `json:"language"`
		SubjectID           int64  `json:"subjectId"`
		SubjectCode         string `json:"subjectCode"`
		AuthorID            int64  `json:"authorId"`
		AuthorName          string `json:"authorName"`
		ApprovedBy          int64  `json:"approvedBy"`
		ApproverName        string `json:"approverName"`
		ApprovedAt          string `json:"approvedAt"`
		BaseRevisionID      *int64 `json:"baseRevisionId"`
		PublishedRevisionID *int64 `json:"publishedRevisionId"`
		StaleBase           bool   `json:"staleBase"`
	} `json:"revisions"`
}

type lessonPublicationBody struct {
	View                string `json:"view"`
	LessonID            int64  `json:"lessonId"`
	PublicID            string `json:"publicId"`
	SubjectID           int64  `json:"subjectId"`
	SubjectCode         string `json:"subjectCode"`
	Language            string `json:"language"`
	Archived            bool   `json:"archived"`
	PublishedRevisionID *int64 `json:"publishedRevisionId"`
	Revisions           []struct {
		ID             int64   `json:"id"`
		Number         int     `json:"number"`
		Status         string  `json:"status"`
		Title          string  `json:"title"`
		AuthorID       *int64  `json:"authorId"`
		AuthorName     *string `json:"authorName"`
		BaseRevisionID *int64  `json:"baseRevisionId"`
		SubmittedAt    *string `json:"submittedAt"`
		ApprovedBy     *int64  `json:"approvedBy"`
		ApproverName   *string `json:"approverName"`
		ApprovedAt     *string `json:"approvedAt"`
		EverPublished  bool    `json:"everPublished"`
		Candidate      bool    `json:"candidate"`
		Withdrawn      bool    `json:"withdrawn"`
		Withdrawal     *struct {
			WithdrawnBy     int64  `json:"withdrawnBy"`
			WithdrawnByName string `json:"withdrawnByName"`
			Reason          string `json:"reason"`
			WithdrawnAt     string `json:"withdrawnAt"`
		} `json:"withdrawal"`
		Submission *struct {
			SubmittedBy int64  `json:"submittedBy"`
			SubmittedAt string `json:"submittedAt"`
			Backfilled  bool   `json:"backfilled"`
		} `json:"submission"`
	} `json:"revisions"`
	History []struct {
		Action              string  `json:"action"`
		PublishedRevisionID *int64  `json:"publishedRevisionId"`
		PreviousRevisionID  *int64  `json:"previousRevisionId"`
		ActorID             int64   `json:"actorId"`
		ActorName           string  `json:"actorName"`
		Reason              *string `json:"reason"`
		OccurredAt          string  `json:"occurredAt"`
	} `json:"history"`
}

type adminLessonsBody struct {
	Lessons []struct {
		LessonID            int64  `json:"lessonId"`
		PublicID            string `json:"publicId"`
		SubjectCode         string `json:"subjectCode"`
		Language            string `json:"language"`
		Archived            bool   `json:"archived"`
		PublishedRevisionID *int64 `json:"publishedRevisionId"`
		Title               string `json:"title"`
		PendingRevisionID   *int64 `json:"pendingRevisionId"`
		LastChange          string `json:"lastChange"`
	} `json:"lessons"`
}

func setDisplayName(e *env, id int64, name string) {
	e.t.Helper()
	if _, err := e.db.Exec(context.Background(), `UPDATE users SET display_name = $2 WHERE id = $1`, id, name); err != nil {
		e.t.Fatal(err)
	}
}

type publishReadBody struct {
	Item struct {
		RevisionID          int64  `json:"revisionId"`
		LessonPublicID      string `json:"lessonPublicId"`
		AuthorName          string `json:"authorName"`
		ApproverName        string `json:"approverName"`
		PublishedRevisionID *int64 `json:"publishedRevisionId"`
		StaleBase           bool   `json:"staleBase"`
	} `json:"item"`
	Revision revisionBody `json:"revision"`
}

func (f withdrawFixture) publishRead(id int64) publishReadBody {
	f.t.Helper()
	w := f.api(f.publisher, http.MethodGet, publishRevisionPath(id), nil)
	wantStatus(f.t, "publish read", w, http.StatusOK, "")
	var b publishReadBody
	if err := json.Unmarshal(w.Body.Bytes(), &b); err != nil {
		f.t.Fatal(err)
	}
	return b
}

func lessonPublicationPath(ref any) string { return fmt.Sprintf("/api/lessons/%v/publication", ref) }

func publishRevisionPath(id int64) string { return fmt.Sprintf("/api/publish/revisions/%d", id) }

func (f withdrawFixture) publishQueue() publishQueueBody {
	f.t.Helper()
	w := f.api(f.publisher, http.MethodGet, "/api/publish/queue", nil)
	wantStatus(f.t, "publish queue", w, http.StatusOK, "")
	wantPrivateNoStore(f.t, "publish queue", w)
	return decodeInto[publishQueueBody](f.t, w)
}

func (f withdrawFixture) lessonPublication(u apiUser, ref any) lessonPublicationBody {
	f.t.Helper()
	w := f.api(u, http.MethodGet, lessonPublicationPath(ref), nil)
	wantStatus(f.t, "lesson publication", w, http.StatusOK, "")
	wantPrivateNoStore(f.t, "lesson publication", w)
	return decodeInto[lessonPublicationBody](f.t, w)
}

func TestPublishingReadsRequirePermissions(t *testing.T) {
	f := newWithdraw(t)
	rev := f.approvedRev(f.submittedLesson(f.author, "Approved edition"))
	lesson := rev.LessonID
	student := f.newUser("student-one", f.subject)
	paths := []string{"/api/publish/queue", publishRevisionPath(rev.ID), lessonPublicationPath(lesson), "/api/admin/lessons"}
	for _, p := range paths {
		wantStatus(t, "anonymous "+p, f.api(apiUser{}, http.MethodGet, p, nil), http.StatusUnauthorized, "unauthenticated")
		for who, u := range map[string]apiUser{"student": student, "author": f.author, "reviewer": f.reviewer} {
			wantStatus(t, who+" "+p, f.api(u, http.MethodGet, p, nil), http.StatusForbidden, "forbidden")
		}
		// Not routes for writes.
		if w := f.api(f.admin, http.MethodPost, p, map[string]any{}); w.Code != http.StatusNotFound && w.Code != http.StatusMethodNotAllowed {
			t.Errorf("POST %s: %d", p, w.Code)
		}
	}
	// Denials come before any lookup: a missing ID looks the same.
	wantStatus(t, "author missing revision", f.api(f.author, http.MethodGet, publishRevisionPath(rev.ID+1000), nil), http.StatusForbidden, "forbidden")
	wantStatus(t, "reviewer missing lesson", f.api(f.reviewer, http.MethodGet, lessonPublicationPath("nope"), nil), http.StatusForbidden, "forbidden")
	// Publisher only: the queue and the revision read; administrator only: the list.
	wantStatus(t, "admin queue", f.api(f.admin, http.MethodGet, "/api/publish/queue", nil), http.StatusForbidden, "forbidden")
	wantStatus(t, "admin revision", f.api(f.admin, http.MethodGet, publishRevisionPath(rev.ID), nil), http.StatusForbidden, "forbidden")
	wantStatus(t, "publisher list", f.api(f.publisher, http.MethodGet, "/api/admin/lessons", nil), http.StatusForbidden, "forbidden")
	// Both read a lesson's publication state (both may restore).
	f.lessonPublication(f.publisher, lesson)
	f.lessonPublication(f.admin, lesson)
	wantStatus(t, "admin list", f.api(f.admin, http.MethodGet, "/api/admin/lessons", nil), http.StatusOK, "")
	wantStatus(t, "queue query", f.api(f.publisher, http.MethodGet, "/api/publish/queue?x=1", nil), http.StatusBadRequest, "invalid_request")
	wantStatus(t, "list query", f.api(f.admin, http.MethodGet, "/api/admin/lessons?x=1", nil), http.StatusBadRequest, "invalid_request")
	wantStatus(t, "revision query", f.api(f.publisher, http.MethodGet, publishRevisionPath(rev.ID)+"?x=1", nil), http.StatusBadRequest, "invalid_request")
	wantStatus(t, "lesson query", f.api(f.admin, http.MethodGet, lessonPublicationPath(lesson)+"?x=1", nil), http.StatusBadRequest, "invalid_request")

	// The stores recheck inside their transaction, independently of the
	// middleware: callers without the permission are denied.
	ctx := context.Background()
	rs := review.NewStore(f.db)
	if _, err := rs.PublishQueue(ctx, f.author.id); !errors.Is(err, review.ErrDenied) {
		t.Errorf("store queue as author: %v", err)
	}
	if _, _, err := rs.PublishRevision(ctx, f.admin.id, rev.ID); !errors.Is(err, review.ErrDenied) {
		t.Errorf("store revision as admin: %v", err)
	}
	if _, err := rs.LessonPublication(ctx, f.reviewer.id, fmt.Sprint(lesson)); !errors.Is(err, review.ErrDenied) {
		t.Errorf("store lesson as reviewer: %v", err)
	}
	if _, err := rs.AdminLessons(ctx, f.publisher.id); !errors.Is(err, review.ErrDenied) {
		t.Errorf("store list as publisher: %v", err)
	}
	// Revocation applies to the next request.
	if _, err := permissions.NewStore(f.db).Revoke(ctx, testOperator, f.publisher.id, permissions.Publisher); err != nil {
		t.Fatal(err)
	}
	wantStatus(t, "revoked publisher", f.api(f.publisher, http.MethodGet, "/api/publish/queue", nil), http.StatusForbidden, "forbidden")
	if _, err := rs.PublishQueue(ctx, f.publisher.id); !errors.Is(err, review.ErrDenied) {
		t.Errorf("store queue as revoked publisher: %v", err)
	}
}

func TestPublishQueueListsApprovedCandidates(t *testing.T) {
	f := newWithdraw(t)
	setDisplayName(f.env, f.author.id, "TEST Autrice")
	setDisplayName(f.env, f.reviewer.id, "TEST Relecteur")
	inReview := f.submittedLesson(f.author, "In review")
	first := f.approvedRev(f.submittedLesson(f.author, "First edition"))
	withdrawn := f.approvedRev(f.submittedLesson(f.author, "Withdrawn edition"))
	wantStatus(t, "withdraw", f.api(f.admin, http.MethodPost, withdrawPath(withdrawn.ID),
		map[string]any{"reason": "Simulated reason.", "expectedPublishedRevisionId": nil}), http.StatusOK, "")

	q := f.publishQueue()
	if len(q.Revisions) != 1 {
		t.Fatalf("queue: %+v", q)
	}
	it := q.Revisions[0]
	if it.RevisionID != first.ID || it.LessonID != first.LessonID || it.Number != 1 || it.Title != "First edition" ||
		it.Language != "fr" || it.SubjectID != f.subject || it.SubjectCode != "simulated-math" || it.AuthorID != f.author.id ||
		it.AuthorName != "TEST Autrice" || it.ApprovedBy != f.reviewer.id || it.ApproverName != "TEST Relecteur" ||
		it.ApprovedAt == "" || it.BaseRevisionID != nil || it.PublishedRevisionID != nil || it.StaleBase || len(it.LessonPublicID) != 36 {
		t.Fatalf("queue item: %+v", it)
	}
	// The publisher reads what they publish; nothing else.
	// The publisher reads what they publish, with its queue facts (whatever
	// the queue's bound): first publication, current pointer, stale base.
	read := f.publishRead(first.ID)
	if read.Revision.ID != first.ID || read.Revision.Status != "approved" || !strings.Contains(string(read.Revision.Content), "First edition") ||
		read.Item.RevisionID != first.ID || read.Item.AuthorName != "TEST Autrice" || read.Item.PublishedRevisionID != nil || read.Item.StaleBase {
		t.Fatalf("publisher read: %+v", read)
	}
	for what, id := range map[string]int64{"in review": inReview.ID, "withdrawn": withdrawn.ID, "missing": first.ID + 1000} {
		wantStatus(t, "publisher read "+what, f.api(f.publisher, http.MethodGet, publishRevisionPath(id), nil), http.StatusNotFound, "not_found")
	}
	wantStatus(t, "malformed", f.api(f.publisher, http.MethodGet, "/api/publish/revisions/01", nil), http.StatusNotFound, "not_found")

	// Published: it leaves the queue and can no longer be read there.
	f.publishedRev(first)
	if q := f.publishQueue(); len(q.Revisions) != 0 {
		t.Fatalf("queue after publication: %+v", q)
	}
	wantStatus(t, "publisher read published", f.api(f.publisher, http.MethodGet, publishRevisionPath(first.ID), nil), http.StatusNotFound, "not_found")

	// A successor approved, then the lesson unpublished: listed as stale,
	// and publication refuses it exactly as the flag says.
	second := f.approvedRev(f.successorRev(first.LessonID, "Second edition"))
	q = f.publishQueue()
	if len(q.Revisions) != 1 || q.Revisions[0].RevisionID != second.ID || q.Revisions[0].StaleBase ||
		*q.Revisions[0].BaseRevisionID != first.ID || *q.Revisions[0].PublishedRevisionID != first.ID {
		t.Fatalf("queue with successor: %+v", q)
	}
	wantStatus(t, "unpublish", f.api(f.admin, http.MethodPost, unpublishPath(first.LessonID),
		map[string]any{"reason": "Simulated emergency.", "expectedPublishedRevisionId": first.ID}), http.StatusOK, "")
	q = f.publishQueue()
	if len(q.Revisions) != 1 || !q.Revisions[0].StaleBase || q.Revisions[0].PublishedRevisionID != nil {
		t.Fatalf("stale queue item: %+v", q)
	}
	if read := f.publishRead(second.ID); !read.Item.StaleBase || read.Item.PublishedRevisionID != nil {
		t.Fatalf("stale publish read: %+v", read.Item)
	}
	wantStatus(t, "publish stale", f.api(f.publisher, http.MethodPost, publishPath(second.ID), map[string]any{}), http.StatusConflict, "stale_base")
}

func TestLessonPublicationAndAdminList(t *testing.T) {
	f := newWithdraw(t)
	setDisplayName(f.env, f.admin.id, "TEST Admin")
	setDisplayName(f.env, f.publisher.id, "TEST Éditeur")
	first, second := f.firstAndSecond()
	lesson := first.LessonID
	// Unpublish, then restore the first edition: the history records both.
	wantStatus(t, "unpublish", f.api(f.admin, http.MethodPost, unpublishPath(lesson),
		map[string]any{"reason": "TEST urgence", "expectedPublishedRevisionId": second.ID}), http.StatusOK, "")
	wantStatus(t, "restore", f.api(f.publisher, http.MethodPost, restorePath(first.ID),
		map[string]any{"expectedPublishedRevisionId": nil}), http.StatusOK, "")
	wantStatus(t, "withdraw second", f.api(f.admin, http.MethodPost, withdrawPath(second.ID),
		map[string]any{"reason": "TEST erreur", "expectedPublishedRevisionId": first.ID}), http.StatusOK, "")
	// A lesson only in review is not in the administrator's list.
	onlyReview := f.submittedLesson(f.author, "Only in review")

	byID := f.lessonPublication(f.admin, lesson)
	byPublic := f.lessonPublication(f.admin, byID.PublicID)
	a, _ := json.Marshal(byID)
	b, _ := json.Marshal(byPublic)
	if string(a) != string(b) {
		t.Fatalf("by ID and by public ID differ:\n%s\n%s", a, b)
	}
	if byID.LessonID != lesson || byID.SubjectCode != "simulated-math" || byID.Language != "fr" || byID.Archived ||
		byID.PublishedRevisionID == nil || *byID.PublishedRevisionID != first.ID || len(byID.Revisions) != 2 || byID.View != "administrator" {
		t.Fatalf("lesson: %s", a)
	}
	r2, r1 := byID.Revisions[0], byID.Revisions[1]
	if r2.ID != second.ID || r2.Number != 2 || r2.Status != "approved" || !r2.EverPublished || r2.Candidate ||
		!r2.Withdrawn || r2.Withdrawal == nil || r2.Withdrawal.Reason != "TEST erreur" || r2.Withdrawal.WithdrawnByName != "TEST Admin" ||
		r2.BaseRevisionID == nil || *r2.BaseRevisionID != first.ID {
		t.Fatalf("second: %+v", r2)
	}
	if r1.ID != first.ID || !r1.EverPublished || r1.Withdrawn || r1.Withdrawal != nil || r1.AuthorName == nil || r1.ApproverName == nil || r1.ApprovedAt == nil || r1.SubmittedAt == nil {
		t.Fatalf("first: %+v", r1)
	}
	// Each submitted revision carries its submission audit row (0011).
	for _, r := range byID.Revisions {
		if r.Submission == nil || r.Submission.SubmittedBy != f.author.id || r.Submission.Backfilled ||
			r.SubmittedAt == nil || r.Submission.SubmittedAt != *r.SubmittedAt {
			t.Fatalf("revision %d submission: %+v (submittedAt %v)", r.ID, r.Submission, r.SubmittedAt)
		}
	}
	var actions []string
	for _, e := range byID.History {
		actions = append(actions, e.Action)
	}
	if strings.Join(actions, ",") != "restore,unpublish,publish,publish" {
		t.Fatalf("history (newest first): %v", actions)
	}
	if h := byID.History[1]; h.Reason == nil || *h.Reason != "TEST urgence" || h.ActorName != "TEST Admin" || h.PublishedRevisionID != nil {
		t.Fatalf("unpublication event: %+v", h)
	}
	if h := byID.History[0]; h.ActorName != "TEST Éditeur" || h.Reason != nil || *h.PublishedRevisionID != first.ID {
		t.Fatalf("restoration event: %+v", h)
	}
	// Publishers get only what restoration needs: no names, reasons,
	// history, or revisions that are in review or were sent back.
	third := f.successorRev(lesson, "Third in review")
	if full := f.lessonPublication(f.admin, lesson); len(full.Revisions) != 3 || full.Revisions[0].ID != third.ID {
		t.Fatalf("administrator view with a revision in review: %+v", full.Revisions)
	}
	w := f.api(f.publisher, http.MethodGet, lessonPublicationPath(lesson), nil)
	wantStatus(t, "publisher view", w, http.StatusOK, "")
	for _, secret := range []string{"TEST erreur", "TEST urgence", "TEST Admin", "TEST Éditeur", "Simulated Student", "Third in review"} {
		if strings.Contains(w.Body.String(), secret) {
			t.Errorf("publisher view reveals %q: %s", secret, w.Body)
		}
	}
	pv := decodeInto[lessonPublicationBody](t, w)
	if pv.View != "publisher" || len(pv.History) != 0 || len(pv.Revisions) != 2 || *pv.PublishedRevisionID != first.ID {
		t.Fatalf("publisher view: %+v", pv)
	}
	p2, p1 := pv.Revisions[0], pv.Revisions[1]
	if p2.ID != second.ID || !p2.Withdrawn || p2.Withdrawal != nil || p2.Submission != nil || p1.Submission != nil || p2.AuthorID != nil || p2.AuthorName != nil || p2.ApproverName != nil ||
		p2.ApprovedBy != nil || p2.SubmittedAt != nil || p2.BaseRevisionID != nil || p2.ApprovedAt == nil || !p2.EverPublished ||
		p1.ID != first.ID || p1.Withdrawn || !p1.EverPublished || p1.Title != "First edition" || p1.Candidate {
		t.Fatalf("publisher revisions: %+v", pv.Revisions)
	}
	if lp, err := review.NewStore(f.db).LessonPublication(context.Background(), f.publisher.id, fmt.Sprint(lesson)); err != nil || lp.Full || lp.History != nil && len(lp.History) != 0 {
		t.Fatalf("store publisher view: %+v %v", lp, err)
	}

	// Drafts are never listed.
	draft := f.createLesson(f.author, "Draft only")
	if lp := f.lessonPublication(f.admin, draft.LessonID); len(lp.Revisions) != 0 || len(lp.History) != 0 {
		t.Fatalf("draft lesson: %+v", lp)
	}
	for _, ref := range []string{"999999", "00000000-0000-0000-0000-000000000000", "0", "01", "nope", strings.ToUpper(byID.PublicID)} {
		wantStatus(t, "ref "+ref, f.api(f.admin, http.MethodGet, lessonPublicationPath(ref), nil), http.StatusNotFound, "not_found")
	}

	w = f.api(f.admin, http.MethodGet, "/api/admin/lessons", nil)
	wantStatus(t, "admin list", w, http.StatusOK, "")
	wantPrivateNoStore(t, "admin list", w)
	list := decodeInto[adminLessonsBody](t, w)
	if len(list.Lessons) != 1 {
		t.Fatalf("admin list: %s", w.Body)
	}
	l := list.Lessons[0]
	if l.LessonID != lesson || l.PublicID != byID.PublicID || l.Title != "First edition" || l.PublishedRevisionID == nil ||
		*l.PublishedRevisionID != first.ID || l.PendingRevisionID != nil || l.LastChange == "" {
		t.Fatalf("admin lesson: %+v", l)
	}
	// An approved candidate shows as pending.
	f.approvedRev(onlyReview)
	list = decodeInto[adminLessonsBody](t, f.api(f.admin, http.MethodGet, "/api/admin/lessons", nil))
	if len(list.Lessons) != 2 || list.Lessons[0].LessonID != onlyReview.LessonID || list.Lessons[0].PendingRevisionID == nil ||
		*list.Lessons[0].PendingRevisionID != onlyReview.ID || list.Lessons[0].PublishedRevisionID != nil || list.Lessons[0].Title != "Only in review" {
		t.Fatalf("admin list with candidate: %+v", list)
	}
}

// An archived lesson's approved revision is never offered for publication
// or restoration (archival has no code path yet; set directly).
func TestArchivedLessonIsNotPublishable(t *testing.T) {
	f := newWithdraw(t)
	pending := f.approvedRev(f.submittedLesson(f.author, "Pending then archived"))
	first, second := f.firstAndSecond()
	if q := f.publishQueue(); len(q.Revisions) != 1 || q.Revisions[0].RevisionID != pending.ID {
		t.Fatalf("queue before archival: %+v", q)
	}
	for _, l := range []int64{pending.LessonID, first.LessonID} {
		if _, err := f.db.Exec(context.Background(), `UPDATE lessons SET archived_at = now() WHERE id = $1`, l); err != nil {
			t.Fatal(err)
		}
	}
	if q := f.publishQueue(); len(q.Revisions) != 0 {
		t.Fatalf("queue lists an archived lesson: %+v", q)
	}
	wantStatus(t, "publish read archived", f.api(f.publisher, http.MethodGet, publishRevisionPath(pending.ID), nil), http.StatusNotFound, "not_found")
	// The publication view says archived, which hides restore offers in the
	// Studio; the server refuses restoration anyway.
	for _, u := range []apiUser{f.publisher, f.admin} {
		if lp := f.lessonPublication(u, first.LessonID); !lp.Archived {
			t.Fatalf("archived flag: %+v", lp)
		}
	}
	wantStatus(t, "restore archived", f.api(f.publisher, http.MethodPost, restorePath(first.ID),
		map[string]any{"expectedPublishedRevisionId": second.ID}), http.StatusConflict, "archived")
}
