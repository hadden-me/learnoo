package server_test

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/hadden-me/learnoo/internal/curriculum"
	"github.com/hadden-me/learnoo/internal/permissions"
)

// Course submission, review, amendments, publication and lifecycle
// (LMS-B04) through the real server handler. TEST users, subjects,
// curriculum entries and unreviewed test documents only.

type courseErrBody struct {
	Error struct {
		Code   string `json:"code"`
		Fields []string
		Steps  []struct {
			LessonID int64 `json:"lessonId"`
		} `json:"steps"`
		Reasons []struct {
			Reason   string `json:"reason"`
			LessonID *int64 `json:"lessonId"`
		} `json:"reasons"`
	} `json:"error"`
}

func TestCourseReviewPublicationAPI(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	math := e.newSubject("test-math")
	owner := e.newUser("c-owner", 0, permissions.Author)
	lessonAuthor := e.newUser("c-lesson-author", math, permissions.Author, permissions.Reviewer)
	reviewer := e.newUser("c-reviewer", math, permissions.Reviewer)
	publisher := e.newUser("c-publisher", 0, permissions.Publisher)
	admin := e.newUser("c-admin", 0, permissions.Administrator)
	newOwner := e.newUser("c-new-owner", 0, permissions.Author)
	student := e.newUser("c-student", 0)

	// A published lesson of another author, through the lesson API.
	publishLesson := func(rev revisionBody) {
		t.Helper()
		w := e.api(lessonAuthor, http.MethodPost, fmt.Sprintf("/api/revisions/%d/submit", rev.ID), map[string]any{"expectedVersion": rev.Version})
		wantStatus(t, "submit lesson", w, http.StatusOK, "")
		wantStatus(t, "approve lesson", e.api(reviewer, http.MethodPost, fmt.Sprintf("/api/review/revisions/%d/approve", rev.ID), map[string]any{}), http.StatusOK, "")
		wantStatus(t, "publish lesson", e.api(publisher, http.MethodPost, fmt.Sprintf("/api/revisions/%d/publish", rev.ID), map[string]any{}), http.StatusOK, "")
	}
	w := e.api(lessonAuthor, http.MethodPost, "/api/lessons", map[string]any{"subjectId": math, "language": "fr", "content": testDoc("Lesson", "fr")})
	wantStatus(t, "lesson", w, http.StatusCreated, "")
	lesson := decodeRevision(t, w)
	publishLesson(lesson)

	// Curriculum: a current edition assigning math with one chapter.
	cur := curriculum.NewStore(e.db)
	adminActor := permissions.UserActor(admin.id)
	ch := e.chapter(math, "ch-01")
	lv, err := cur.CreateLevel(ctx, adminActor, curriculum.Entry{Code: "test-l", NameFR: "TEST", NameAR: "اختبار"})
	if err != nil {
		t.Fatal(err)
	}
	sec, err := cur.CreateSection(ctx, adminActor, lv.ID, curriculum.Entry{Code: "test-s", NameFR: "TEST", NameAR: "شعبة"})
	if err != nil {
		t.Fatal(err)
	}
	ed, err := cur.CreateEdition(ctx, adminActor, sec.ID, curriculum.NewEdition{Code: "test-e", LabelFR: "TEST", LabelAR: "طبعة"})
	if err != nil {
		t.Fatal(err)
	}
	ed, asg, err := cur.AddAssignment(ctx, adminActor, ed.ID, ed.Version, math)
	if err != nil {
		t.Fatal(err)
	}
	if ed, err = cur.SetChapters(ctx, adminActor, ed.ID, asg.ID, ed.Version, []int64{ch}); err != nil {
		t.Fatal(err)
	}
	if _, _, err = cur.MakeCurrent(ctx, adminActor, ed.ID, ed.Version, nil); err != nil {
		t.Fatal(err)
	}

	// The course draft.
	w = e.api(owner, http.MethodPost, "/api/courses", map[string]any{"subjectId": math, "language": "fr"})
	wantStatus(t, "create course", w, http.StatusCreated, "")
	created := decodeBody[struct {
		Course   struct{ ID int64 } `json:"course"`
		Revision courseRevisionResp `json:"revision"`
	}](t, w)
	courseID, rev := created.Course.ID, created.Revision
	coursePath := fmt.Sprintf("/api/courses/%d", courseID)
	revPath := func(id int64) string { return fmt.Sprintf("/api/course-revisions/%d", id) }
	save := func(r courseRevisionResp, title, key string, lessonID, revisionID int64) courseRevisionResp {
		t.Helper()
		var k any
		if key != "" {
			k = key
		}
		w := e.api(owner, http.MethodPut, revPath(r.ID), map[string]any{"expectedVersion": r.Version, "title": title,
			"summary": "TEST summary", "objectives": "TEST objectives", "prerequisites": "", "coverMediaId": nil, "coverAlt": nil,
			"assignmentIds": []int64{asg.ID}, "modules": []map[string]any{{"key": k, "title": "TEST module", "chapterId": ch,
				"steps": []map[string]any{{"lessonId": lessonID, "revisionId": revisionID, "title": "TEST step"}}}}})
		wantStatus(t, "save course", w, http.StatusOK, "")
		return decodeBody[courseRevisionResp](t, w)
	}
	submit := func(r courseRevisionResp) courseRevisionResp {
		t.Helper()
		w := e.api(owner, http.MethodPost, revPath(r.ID)+"/submit", map[string]any{"expectedVersion": r.Version})
		wantStatus(t, "submit course", w, http.StatusOK, "")
		return decodeBody[courseRevisionResp](t, w)
	}

	// Submission: incomplete, stale version, success.
	w = e.api(owner, http.MethodPost, revPath(rev.ID)+"/submit", map[string]any{"expectedVersion": rev.Version})
	wantStatus(t, "incomplete", w, http.StatusUnprocessableEntity, "incomplete")
	if b := decodeBody[courseErrBody](t, w); len(b.Error.Fields) != 4 {
		t.Fatalf("incomplete fields: %+v", b)
	}
	rev = save(rev, "TEST course", "", lesson.LessonID, lesson.ID)
	wantStatus(t, "stale submit", e.api(owner, http.MethodPost, revPath(rev.ID)+"/submit", map[string]any{"expectedVersion": rev.Version - 1}), http.StatusConflict, "version_conflict")
	wantStatus(t, "submit body", e.api(owner, http.MethodPost, revPath(rev.ID)+"/submit", map[string]any{"expectedVersion": rev.Version, "x": 1}), http.StatusBadRequest, "invalid_request")
	rev = submit(rev)
	if rev.Status != "in_review" {
		t.Fatalf("submitted: %+v", rev)
	}

	// Review: the author of an added lesson revision is refused with the
	// reason; the change request needs a comment; the queue tells who may.
	approvePath := fmt.Sprintf("/api/review/course-revisions/%d/approve", rev.ID)
	w = e.api(lessonAuthor, http.MethodPost, approvePath, map[string]any{})
	wantStatus(t, "lesson author approves", w, http.StatusForbidden, "not_eligible_reviewer")
	if b := decodeBody[courseErrBody](t, w); len(b.Error.Reasons) != 1 || b.Error.Reasons[0].Reason != "lesson_author" || *b.Error.Reasons[0].LessonID != lesson.LessonID {
		t.Fatalf("reasons: %+v", b)
	}
	wantStatus(t, "owner approves", e.api(owner, http.MethodPost, approvePath, map[string]any{}), http.StatusForbidden, "forbidden")
	wantStatus(t, "no comment", e.api(reviewer, http.MethodPost, fmt.Sprintf("/api/review/course-revisions/%d/request-changes", rev.ID), map[string]any{}),
		http.StatusUnprocessableEntity, "comment_required")
	w = e.api(lessonAuthor, http.MethodGet, "/api/review/queue?type=course", nil)
	wantStatus(t, "queue", w, http.StatusOK, "")
	if q := decodeBody[struct {
		Revisions []struct {
			RevisionID int64 `json:"revisionId"`
			Eligible   bool  `json:"eligible"`
			Blocked    *struct {
				Reason string `json:"reason"`
			} `json:"blocked"`
		} `json:"revisions"`
	}](t, w); len(q.Revisions) != 1 || q.Revisions[0].Eligible || q.Revisions[0].Blocked != nil {
		t.Fatalf("queue: %+v", q)
	}
	wantStatus(t, "queue bad type", e.api(reviewer, http.MethodGet, "/api/review/queue?type=x", nil), http.StatusBadRequest, "invalid_request")
	wantStatus(t, "lesson queue still served", e.api(reviewer, http.MethodGet, "/api/review/queue", nil), http.StatusOK, "")
	w = e.api(reviewer, http.MethodGet, fmt.Sprintf("/api/review/course-revisions/%d", rev.ID), nil)
	wantStatus(t, "payload", w, http.StatusOK, "")
	if p := decodeBody[struct {
		Eligible bool `json:"eligible"`
		Steps    []struct {
			Change string `json:"change"`
		} `json:"steps"`
	}](t, w); !p.Eligible || len(p.Steps) != 1 || p.Steps[0].Change != "added" {
		t.Fatalf("payload: %+v", p)
	}
	wantStatus(t, "approve", e.api(reviewer, http.MethodPost, approvePath, map[string]any{"comment": "Fine."}), http.StatusOK, "")
	wantStatus(t, "approve twice", e.api(reviewer, http.MethodPost, approvePath, map[string]any{}), http.StatusConflict, "not_in_review")

	// Publication.
	w = e.api(publisher, http.MethodGet, "/api/publish/queue?type=course", nil)
	wantStatus(t, "publish queue", w, http.StatusOK, "")
	if q := decodeBody[struct {
		Revisions []struct {
			RevisionID int64 `json:"revisionId"`
			StaleBase  bool  `json:"staleBase"`
		} `json:"revisions"`
	}](t, w); len(q.Revisions) != 1 || q.Revisions[0].RevisionID != rev.ID || q.Revisions[0].StaleBase {
		t.Fatalf("publish queue: %+v", q)
	}
	pubPath := revPath(rev.ID) + "/publish"
	wantStatus(t, "publish without expected", e.api(publisher, http.MethodPost, pubPath, map[string]any{}), http.StatusBadRequest, "invalid_request")
	wantStatus(t, "publish stale pointer", e.api(publisher, http.MethodPost, pubPath, map[string]any{"expectedPublishedRevisionId": rev.ID}), http.StatusConflict, "stale_pointer")
	wantStatus(t, "publish", e.api(publisher, http.MethodPost, pubPath, map[string]any{"expectedPublishedRevisionId": nil}), http.StatusOK, "")
	if e.count(`SELECT count(*) FROM reader_published_courses WHERE course_id = $1`, courseID) != 1 {
		t.Fatal("published course not readable")
	}

	// Newer revision notice and amendments.
	notice := coursePath + "/newer-revisions"
	wantStatus(t, "notice for a student", e.api(student, http.MethodGet, notice, nil), http.StatusForbidden, "forbidden")
	w = e.api(lessonAuthor, http.MethodPost, fmt.Sprintf("/api/lessons/%d/drafts", lesson.LessonID), map[string]any{"content": testDoc("Lesson v2", "fr")})
	wantStatus(t, "lesson draft", w, http.StatusCreated, "")
	lesson2 := decodeRevision(t, w)
	publishLesson(lesson2)
	for _, u := range []apiUser{owner, reviewer, publisher, admin} {
		w = e.api(u, http.MethodGet, notice, nil)
		wantStatus(t, "notice", w, http.StatusOK, "")
		if n := decodeBody[struct {
			Steps []struct {
				NewerRevisionID int64 `json:"newerRevisionId"`
			} `json:"steps"`
		}](t, w); len(n.Steps) != 1 || n.Steps[0].NewerRevisionID != lesson2.ID {
			t.Fatalf("notice: %+v", n)
		}
	}
	wantStatus(t, "notice for another author", e.api(newOwner, http.MethodGet, notice, nil), http.StatusNotFound, "not_found")
	w = e.api(owner, http.MethodPost, coursePath+"/drafts", map[string]any{})
	wantStatus(t, "ordinary draft", w, http.StatusCreated, "")
	draft := decodeBody[courseRevisionResp](t, w)
	wantStatus(t, "amendment with a draft", e.api(owner, http.MethodPost, coursePath+"/amendments", map[string]any{"steps": []any{}}), http.StatusConflict, "draft_exists")
	wantStatus(t, "discard draft", e.api(owner, http.MethodDelete, fmt.Sprintf("%s?expectedVersion=%d", revPath(draft.ID), draft.Version), nil), http.StatusNoContent, "")
	wantStatus(t, "amendment bad body", e.api(owner, http.MethodPost, coursePath+"/amendments", map[string]any{"steps": []any{map[string]any{"lessonId": 1}}}), http.StatusBadRequest, "invalid_request")
	w = e.api(owner, http.MethodPost, coursePath+"/amendments", map[string]any{"steps": []any{map[string]any{"lessonId": lesson.LessonID, "revisionId": lesson2.ID}}})
	wantStatus(t, "amendment", w, http.StatusCreated, "")
	am := decodeBody[struct {
		courseRevisionResp
		Kind string `json:"kind"`
	}](t, w)
	if am.Kind != "amendment" || am.Modules[0].Steps[0].RevisionID != lesson2.ID {
		t.Fatalf("amendment: %+v", am)
	}
	amRev := submit(am.courseRevisionResp)
	w = e.api(reviewer, http.MethodGet, fmt.Sprintf("/api/review/course-revisions/%d", amRev.ID), nil)
	wantStatus(t, "amendment payload", w, http.StatusOK, "")
	if p := decodeBody[struct {
		Steps []struct {
			Change             string `json:"change"`
			PreviousRevisionID *int64 `json:"previousRevisionId"`
			Diff               *struct {
				Blocks []struct {
					Op string `json:"op"`
				} `json:"blocks"`
			} `json:"diff"`
		} `json:"steps"`
	}](t, w); len(p.Steps) != 1 || p.Steps[0].Change != "replaced" || *p.Steps[0].PreviousRevisionID != lesson.ID || p.Steps[0].Diff == nil {
		t.Fatalf("amendment payload: %+v", p)
	}
	wantStatus(t, "approve amendment", e.api(reviewer, http.MethodPost, fmt.Sprintf("/api/review/course-revisions/%d/approve", amRev.ID), map[string]any{}), http.StatusOK, "")
	// The replacement is withdrawn before publication (this unpublishes the
	// lesson): publication is refused with the step.
	wantStatus(t, "withdraw lesson revision", e.api(admin, http.MethodPost, fmt.Sprintf("/api/revisions/%d/withdraw", lesson2.ID),
		map[string]any{"reason": "Simulated.", "expectedPublishedRevisionId": lesson2.ID}), http.StatusOK, "")
	w = e.api(publisher, http.MethodPost, revPath(amRev.ID)+"/publish", map[string]any{"expectedPublishedRevisionId": rev.ID})
	wantStatus(t, "publish ineligible", w, http.StatusConflict, "ineligible")
	if b := decodeBody[courseErrBody](t, w); len(b.Error.Steps) != 1 || b.Error.Steps[0].LessonID != lesson.LessonID {
		t.Fatalf("ineligible steps: %+v", b)
	}
	// Course revision withdrawal frees the slot.
	wantStatus(t, "withdraw published course revision", e.api(admin, http.MethodPost, revPath(rev.ID)+"/withdraw", map[string]any{"reason": "x"}), http.StatusConflict, "published")
	wantStatus(t, "withdraw without reason", e.api(admin, http.MethodPost, revPath(amRev.ID)+"/withdraw", map[string]any{"reason": " "}), http.StatusUnprocessableEntity, "reason_required")
	wantStatus(t, "withdraw amendment", e.api(admin, http.MethodPost, revPath(amRev.ID)+"/withdraw", map[string]any{"reason": "Simulated release."}), http.StatusOK, "")
	wantStatus(t, "publish withdrawn", e.api(publisher, http.MethodPost, revPath(amRev.ID)+"/publish", map[string]any{"expectedPublishedRevisionId": rev.ID}), http.StatusConflict, "withdrawn")
	if e.count(`SELECT count(*) FROM course_dependency_events WHERE course_id = $1`, courseID) == 0 {
		t.Fatal("no dependency event")
	}

	// Suspension, restore refused while ineligible, then allowed.
	wantStatus(t, "suspend stale", e.api(admin, http.MethodPost, coursePath+"/suspend", map[string]any{"reason": "x", "expectedPublishedRevisionId": nil}), http.StatusConflict, "stale_pointer")
	wantStatus(t, "suspend", e.api(admin, http.MethodPost, coursePath+"/suspend", map[string]any{"reason": "Simulated suspension.", "expectedPublishedRevisionId": rev.ID}), http.StatusOK, "")
	if e.count(`SELECT count(*) FROM reader_published_courses WHERE course_id = $1`, courseID) != 0 {
		t.Fatal("suspended course readable")
	}
	restore := map[string]any{"revisionId": rev.ID, "expectedPublishedRevisionId": nil}
	wantStatus(t, "restore ineligible", e.api(publisher, http.MethodPost, coursePath+"/restore", restore), http.StatusConflict, "ineligible")
	wantStatus(t, "restore lesson", e.api(admin, http.MethodPost, fmt.Sprintf("/api/revisions/%d/restore", lesson.ID), map[string]any{"expectedPublishedRevisionId": nil}), http.StatusOK, "")
	wantStatus(t, "owner restores", e.api(owner, http.MethodPost, coursePath+"/restore", restore), http.StatusForbidden, "forbidden")
	wantStatus(t, "restore", e.api(admin, http.MethodPost, coursePath+"/restore", restore), http.StatusOK, "")

	// Archive, unarchive, restore.
	wantStatus(t, "archive", e.api(admin, http.MethodPost, coursePath+"/archive", map[string]any{"reason": "Simulated.", "expectedPublishedRevisionId": rev.ID}), http.StatusOK, "")
	wantStatus(t, "archive twice", e.api(admin, http.MethodPost, coursePath+"/archive", map[string]any{"reason": "x", "expectedPublishedRevisionId": nil}), http.StatusConflict, "archived")
	wantStatus(t, "unarchive", e.api(admin, http.MethodPost, coursePath+"/unarchive", map[string]any{"reason": "Simulated."}), http.StatusOK, "")
	if e.count(`SELECT count(*) FROM reader_published_courses WHERE course_id = $1`, courseID) != 0 {
		t.Fatal("unarchive republished")
	}
	wantStatus(t, "restore after unarchive", e.api(publisher, http.MethodPost, coursePath+"/restore", restore), http.StatusOK, "")

	// Transfer, archive requests, administrator draft discard.
	w = e.api(owner, http.MethodPost, coursePath+"/drafts", map[string]any{})
	wantStatus(t, "draft before transfer", w, http.StatusCreated, "")
	open := decodeBody[courseRevisionResp](t, w)
	transfer := map[string]any{"toUserId": newOwner.id, "reason": "Simulated transfer."}
	wantStatus(t, "transfer with a draft", e.api(admin, http.MethodPost, coursePath+"/transfer", transfer), http.StatusConflict, "active_candidate_exists")
	wantStatus(t, "transfer to a student", e.api(admin, http.MethodPost, coursePath+"/transfer", map[string]any{"toUserId": student.id, "reason": "x"}), http.StatusUnprocessableEntity, "unknown_user")
	wantStatus(t, "admin discard", e.api(admin, http.MethodDelete, "/api/admin/course-revisions/"+fmt.Sprint(open.ID), map[string]any{"reason": "Simulated recovery."}), http.StatusOK, "")
	wantStatus(t, "transfer", e.api(admin, http.MethodPost, coursePath+"/transfer", transfer), http.StatusOK, "")
	wantStatus(t, "old owner request", e.api(owner, http.MethodPost, coursePath+"/archive-request", map[string]any{"reason": "x"}), http.StatusNotFound, "not_found")
	wantStatus(t, "new owner request", e.api(newOwner, http.MethodPost, coursePath+"/archive-request", map[string]any{"reason": "Simulated request."}), http.StatusCreated, "")
	w = e.api(admin, http.MethodGet, "/api/admin/courses/archive-requests", nil)
	wantStatus(t, "archive requests", w, http.StatusOK, "")
	if b := decodeBody[struct {
		Requests []struct {
			CourseID int64 `json:"courseId"`
		} `json:"requests"`
	}](t, w); len(b.Requests) != 1 || b.Requests[0].CourseID != courseID {
		t.Fatalf("requests: %+v", b)
	}
	wantStatus(t, "blocked reviews", e.api(admin, http.MethodGet, "/api/admin/courses/blocked-reviews", nil), http.StatusOK, "")

	// Authorization before any lookup.
	routes := []struct {
		method, path string
		body         any
	}{
		{http.MethodPost, revPath(rev.ID) + "/submit", map[string]any{"expectedVersion": 1}},
		{http.MethodPost, coursePath + "/amendments", map[string]any{"steps": []any{}}},
		{http.MethodPost, coursePath + "/archive-request", map[string]any{"reason": "x"}},
		{http.MethodGet, notice, nil},
		{http.MethodGet, fmt.Sprintf("/api/review/course-revisions/%d", rev.ID), nil},
		{http.MethodPost, approvePath, map[string]any{}},
		{http.MethodPost, pubPath, map[string]any{"expectedPublishedRevisionId": nil}},
		{http.MethodPost, coursePath + "/restore", restore},
		{http.MethodPost, coursePath + "/suspend", map[string]any{"reason": "x", "expectedPublishedRevisionId": nil}},
		{http.MethodPost, coursePath + "/archive", map[string]any{"reason": "x", "expectedPublishedRevisionId": nil}},
		{http.MethodPost, coursePath + "/unarchive", map[string]any{"reason": "x"}},
		{http.MethodPost, coursePath + "/transfer", transfer},
		{http.MethodPost, revPath(rev.ID) + "/withdraw", map[string]any{"reason": "x"}},
		{http.MethodDelete, "/api/admin/course-revisions/1", map[string]any{"reason": "x"}},
		{http.MethodGet, "/api/admin/courses/archive-requests", nil},
		{http.MethodGet, "/api/admin/courses/blocked-reviews", nil},
	}
	state := func() int {
		return e.count(`SELECT (SELECT count(*) FROM course_publication_audit) + (SELECT count(*) FROM course_lifecycle_audit)
			+ (SELECT count(*) FROM course_review_decisions) + (SELECT count(*) FROM course_revision_withdrawals)
			+ (SELECT count(*) FROM course_submission_audit) + (SELECT count(*) FROM course_revisions)`)
	}
	before := state()
	for _, rq := range routes {
		wantStatus(t, "anonymous "+rq.path, e.api(apiUser{}, rq.method, rq.path, rq.body), http.StatusUnauthorized, "unauthenticated")
		wantStatus(t, "student "+rq.method+" "+rq.path, e.api(student, rq.method, rq.path, rq.body), http.StatusForbidden, "forbidden")
		if rq.method != http.MethodGet {
			wantStatus(t, "no csrf "+rq.path, e.api(admin, rq.method, rq.path, rq.body, withCSRF("")), http.StatusForbidden, "csrf_rejected")
		}
	}
	if state() != before {
		t.Fatal("refused requests changed course state")
	}
}
