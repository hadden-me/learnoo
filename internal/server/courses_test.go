package server_test

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/hadden-me/learnoo/internal/permissions"
)

// TEST courses only: simulated users, TEST subjects and unreviewed lessons.

type courseRevisionResp struct {
	ID       int64  `json:"id"`
	CourseID int64  `json:"courseId"`
	Number   int    `json:"number"`
	Status   string `json:"status"`
	Version  int    `json:"version"`
	Title    string `json:"title"`
	Modules  []struct {
		Key   string `json:"key"`
		Steps []struct {
			LessonID   int64 `json:"lessonId"`
			RevisionID int64 `json:"revisionId"`
			OwnLesson  bool  `json:"ownLesson"`
		} `json:"steps"`
	} `json:"modules"`
}

func TestCourseDraftAPI(t *testing.T) {
	e := newEnv(t)
	math := e.newSubject("test-math")
	owner := e.newUser("course-owner", 0, permissions.Author)
	other := e.newUser("course-other", 0, permissions.Author)
	reviewer := e.newUser("course-reviewer", math, permissions.Reviewer)
	student := e.newUser("course-student", 0)

	w := e.api(owner, http.MethodPost, "/api/lessons", map[string]any{"subjectId": math, "language": "fr", "content": testDoc("Own lesson", "fr")})
	wantStatus(t, "own lesson", w, http.StatusCreated, "")
	ownLesson := decodeRevision(t, w)
	w = e.api(other, http.MethodPost, "/api/lessons", map[string]any{"subjectId": math, "language": "fr", "content": testDoc("Other lesson", "fr")})
	wantStatus(t, "other lesson", w, http.StatusCreated, "")
	otherLesson := decodeRevision(t, w)

	w = e.api(owner, http.MethodPost, "/api/courses", map[string]any{"subjectId": math, "language": "fr"})
	wantStatus(t, "create course", w, http.StatusCreated, "")
	created := decodeBody[struct {
		Course struct {
			ID int64 `json:"id"`
		} `json:"course"`
		Revision courseRevisionResp `json:"revision"`
	}](t, w)
	if loc := w.Header().Get("Location"); loc != fmt.Sprintf("/api/course-revisions/%d", created.Revision.ID) {
		t.Errorf("Location %q", loc)
	}
	courseID, rev := created.Course.ID, created.Revision
	revPath := fmt.Sprintf("/api/course-revisions/%d", rev.ID)
	coursePath := fmt.Sprintf("/api/courses/%d", courseID)
	wantStatus(t, "unknown subject", e.api(owner, http.MethodPost, "/api/courses", map[string]any{"subjectId": 999999, "language": "fr"}), http.StatusUnprocessableEntity, "unknown_subject")
	wantStatus(t, "bad language", e.api(owner, http.MethodPost, "/api/courses", map[string]any{"subjectId": math, "language": "en"}), http.StatusUnprocessableEntity, "invalid_input")

	body := func(version int, title string, steps ...map[string]any) map[string]any {
		if steps == nil {
			steps = []map[string]any{}
		}
		return map[string]any{"expectedVersion": version, "title": title, "summary": "", "objectives": "", "prerequisites": "",
			"coverMediaId": nil, "coverAlt": nil, "assignmentIds": []int64{},
			"modules": []map[string]any{{"key": nil, "title": "TEST module", "chapterId": nil, "steps": steps}}}
	}
	ownStep := map[string]any{"lessonId": ownLesson.LessonID, "revisionId": ownLesson.ID, "title": "TEST step"}
	w = e.api(owner, http.MethodPut, revPath, body(rev.Version, "TEST course", ownStep))
	wantStatus(t, "save", w, http.StatusOK, "")
	saved := decodeBody[courseRevisionResp](t, w)
	if saved.Version != 2 || len(saved.Modules) != 1 || len(saved.Modules[0].Steps) != 1 || !saved.Modules[0].Steps[0].OwnLesson {
		t.Fatalf("saved: %+v", saved)
	}
	wantStatus(t, "stale save", e.api(owner, http.MethodPut, revPath, body(rev.Version, "TEST stale")), http.StatusConflict, "version_conflict")
	wantStatus(t, "other's unpublished lesson", e.api(owner, http.MethodPut, revPath,
		body(saved.Version, "x", map[string]any{"lessonId": otherLesson.LessonID, "revisionId": otherLesson.ID, "title": "s"})), http.StatusUnprocessableEntity, "invalid_step")
	wantStatus(t, "duplicate lesson", e.api(owner, http.MethodPut, revPath, body(saved.Version, "x", ownStep, ownStep)), http.StatusUnprocessableEntity, "invalid_step")
	wantStatus(t, "multi-line title", e.api(owner, http.MethodPut, revPath, body(saved.Version, "a\nb")), http.StatusUnprocessableEntity, "invalid_input")
	chapter := e.chapter(math, "ch-01")
	noPlace := body(saved.Version, "x")
	noPlace["modules"] = []map[string]any{{"title": "m", "chapterId": chapter, "steps": []any{}}}
	wantStatus(t, "chapter outside assignments", e.api(owner, http.MethodPut, revPath, noPlace), http.StatusUnprocessableEntity, "placement_invalid")
	wantStatus(t, "someone else's cover", e.api(owner, http.MethodPut, revPath, func() map[string]any {
		b := body(saved.Version, "x")
		b["coverMediaId"], b["coverAlt"] = 999999, "alt"
		return b
	}()), http.StatusUnprocessableEntity, "unknown_media")

	// Strict JSON, at every level.
	for name, raw := range map[string]string{
		"unknown key":        `{"expectedVersion":2,"title":"","summary":"","objectives":"","prerequisites":"","assignmentIds":[],"modules":[],"extra":1}`,
		"missing modules":    `{"expectedVersion":2,"title":"","summary":"","objectives":"","prerequisites":"","assignmentIds":[]}`,
		"null title":         `{"expectedVersion":2,"title":null,"summary":"","objectives":"","prerequisites":"","assignmentIds":[],"modules":[]}`,
		"modules object":     `{"expectedVersion":2,"title":"","summary":"","objectives":"","prerequisites":"","assignmentIds":[],"modules":{}}`,
		"module unknown key": `{"expectedVersion":2,"title":"","summary":"","objectives":"","prerequisites":"","assignmentIds":[],"modules":[{"title":"m","steps":[],"x":1}]}`,
		"module null steps":  `{"expectedVersion":2,"title":"","summary":"","objectives":"","prerequisites":"","assignmentIds":[],"modules":[{"title":"m","steps":null}]}`,
		"module not object":  `{"expectedVersion":2,"title":"","summary":"","objectives":"","prerequisites":"","assignmentIds":[],"modules":[1]}`,
		"step case key":      `{"expectedVersion":2,"title":"","summary":"","objectives":"","prerequisites":"","assignmentIds":[],"modules":[{"title":"m","steps":[{"LessonId":1,"revisionId":1,"title":"s"}]}]}`,
		"step string id":     `{"expectedVersion":2,"title":"","summary":"","objectives":"","prerequisites":"","assignmentIds":[],"modules":[{"title":"m","steps":[{"lessonId":"1","revisionId":1,"title":"s"}]}]}`,
		"step duplicate key": `{"expectedVersion":2,"title":"","summary":"","objectives":"","prerequisites":"","assignmentIds":[],"modules":[{"title":"m","steps":[{"lessonId":1,"lessonId":1,"revisionId":1,"title":"s"}]}]}`,
	} {
		wantStatus(t, name, e.api(owner, http.MethodPut, revPath, []byte(raw)), http.StatusBadRequest, "invalid_request")
	}
	wantStatus(t, "drafts body", e.api(owner, http.MethodPost, coursePath+"/drafts", []byte(`{"x":1}`)), http.StatusBadRequest, "invalid_request")
	wantStatus(t, "list without mine", e.api(owner, http.MethodGet, "/api/courses", nil), http.StatusBadRequest, "invalid_request")
	wantStatus(t, "bad cursor", e.api(owner, http.MethodGet, coursePath+"/attachable-lessons?cursor=abc", nil), http.StatusBadRequest, "invalid_request")

	wantStatus(t, "get with query", e.api(owner, http.MethodGet, revPath+"?x=1", nil), http.StatusBadRequest, "invalid_request")
	wantStatus(t, "save with query", e.api(owner, http.MethodPut, revPath+"?expectedVersion=2", body(saved.Version, "x")), http.StatusBadRequest, "invalid_request")
	wantStatus(t, "drafts with query", e.api(owner, http.MethodPost, coursePath+"/drafts?x=1", map[string]any{}), http.StatusBadRequest, "invalid_request")
	wantStatus(t, "list with bad cursor", e.api(owner, http.MethodGet, "/api/courses?mine=1&cursor=0", nil), http.StatusBadRequest, "invalid_request")
	wantStatus(t, "list with extra parameter", e.api(owner, http.MethodGet, "/api/courses?mine=1&x=1", nil), http.StatusBadRequest, "invalid_request")

	// Reads.
	w = e.api(owner, http.MethodGet, "/api/courses?mine=1", nil)
	wantStatus(t, "list", w, http.StatusOK, "")
	if list := decodeBody[struct {
		Courses []struct {
			ID int64 `json:"id"`
		} `json:"courses"`
		NextCursor *string `json:"nextCursor"`
	}](t, w); len(list.Courses) != 1 || list.Courses[0].ID != courseID || list.NextCursor != nil {
		t.Fatalf("list: %+v", list)
	}
	wantStatus(t, "get", e.api(owner, http.MethodGet, revPath, nil), http.StatusOK, "")
	w = e.api(owner, http.MethodGet, coursePath+"/attachable-lessons?q=own", nil)
	wantStatus(t, "attachable", w, http.StatusOK, "")
	if got := decodeBody[struct {
		Lessons []struct {
			LessonID int64 `json:"lessonId"`
		} `json:"lessons"`
		NextCursor *string `json:"nextCursor"`
	}](t, w); len(got.Lessons) != 1 || got.Lessons[0].LessonID != ownLesson.LessonID || got.NextCursor != nil {
		t.Fatalf("attachable: %+v", got)
	}
	wantStatus(t, "own lesson revisions", e.api(owner, http.MethodGet, fmt.Sprintf("/api/lessons/%d/eligible-revisions", ownLesson.LessonID), nil), http.StatusOK, "")

	// Conflicts.
	wantStatus(t, "second draft", e.api(owner, http.MethodPost, coursePath+"/drafts", map[string]any{}), http.StatusConflict, "active_candidate_exists")
	wantStatus(t, "discard referenced lesson draft", e.api(owner, http.MethodDelete,
		fmt.Sprintf("/api/revisions/%d?expectedVersion=%d", ownLesson.ID, ownLesson.Version), nil), http.StatusConflict, "referenced_by_course")

	// Enumeration-safe 404 for another author, identical to a missing ID.
	for _, rq := range []struct {
		method, path string
		body         any
	}{
		{http.MethodGet, revPath, nil},
		{http.MethodPut, revPath, body(saved.Version, "x")},
		{http.MethodDelete, revPath + fmt.Sprintf("?expectedVersion=%d", saved.Version), nil},
		{http.MethodDelete, coursePath, nil},
		{http.MethodPost, coursePath + "/drafts", map[string]any{}},
		{http.MethodGet, coursePath + "/attachable-lessons", nil},
		{http.MethodGet, fmt.Sprintf("/api/lessons/%d/eligible-revisions", ownLesson.LessonID), nil},
	} {
		wantStatus(t, "other "+rq.method+" "+rq.path, e.api(other, rq.method, rq.path, rq.body), http.StatusNotFound, "not_found")
	}
	wantStatus(t, "missing revision", e.api(owner, http.MethodGet, "/api/course-revisions/987654", nil), http.StatusNotFound, "not_found")
	wantStatus(t, "malformed id", e.api(owner, http.MethodGet, "/api/course-revisions/01", nil), http.StatusNotFound, "not_found")

	// Authorization before any lookup: 401, 403, CSRF, cross-origin.
	routes := []struct {
		method, path string
		body         any
	}{
		{http.MethodGet, "/api/courses?mine=1", nil},
		{http.MethodPost, "/api/courses", map[string]any{"subjectId": math, "language": "fr"}},
		{http.MethodDelete, coursePath, nil},
		{http.MethodPost, coursePath + "/drafts", map[string]any{}},
		{http.MethodGet, coursePath + "/attachable-lessons", nil},
		{http.MethodGet, revPath, nil},
		{http.MethodPut, revPath, body(saved.Version, "x")},
		{http.MethodDelete, revPath + "?expectedVersion=2", nil},
		{http.MethodGet, fmt.Sprintf("/api/lessons/%d/eligible-revisions", ownLesson.LessonID), nil},
	}
	before := e.count(`SELECT (SELECT count(*) FROM courses) + (SELECT count(*) FROM course_revisions) + (SELECT sum(version) FROM course_revisions)`)
	for _, rq := range routes {
		wantStatus(t, "anonymous "+rq.path, e.api(apiUser{}, rq.method, rq.path, rq.body), http.StatusUnauthorized, "unauthenticated")
		for name, u := range map[string]apiUser{"student": student, "reviewer": reviewer} {
			wantStatus(t, name+" "+rq.method+" "+rq.path, e.api(u, rq.method, rq.path, rq.body), http.StatusForbidden, "forbidden")
		}
		if rq.method != http.MethodGet {
			wantStatus(t, "no csrf "+rq.path, e.api(owner, rq.method, rq.path, rq.body, withCSRF("")), http.StatusForbidden, "csrf_rejected")
			wantStatus(t, "cross-site "+rq.path, e.api(owner, rq.method, rq.path, rq.body, withHeader("Sec-Fetch-Site", "cross-site")), http.StatusForbidden, "cross_origin_rejected")
		}
	}
	if after := e.count(`SELECT (SELECT count(*) FROM courses) + (SELECT count(*) FROM course_revisions) + (SELECT sum(version) FROM course_revisions)`); after != before {
		t.Fatal("refused requests changed courses")
	}

	// A course with submitted history is kept (submission simulated with
	// triggers off: B04 adds it).
	w = e.api(owner, http.MethodPost, "/api/courses", map[string]any{"subjectId": math, "language": "fr"})
	wantStatus(t, "second course", w, http.StatusCreated, "")
	second := decodeBody[struct {
		Course   struct{ ID int64 } `json:"course"`
		Revision courseRevisionResp `json:"revision"`
	}](t, w)
	tx, err := e.db.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, sql := range []string{`SET LOCAL session_replication_role = replica`,
		fmt.Sprintf(`UPDATE course_revisions SET status = 'in_review', submitted_at = now() WHERE id = %d`, second.Revision.ID)} {
		if _, err := tx.Exec(context.Background(), sql); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	wantStatus(t, "delete with history", e.api(owner, http.MethodDelete, fmt.Sprintf("/api/courses/%d", second.Course.ID), nil), http.StatusConflict, "has_history")
	wantStatus(t, "save submitted", e.api(owner, http.MethodPut, fmt.Sprintf("/api/course-revisions/%d", second.Revision.ID), body(2, "x")), http.StatusConflict, "not_draft")

	// Discard the draft, then delete the never-submitted course.
	wantStatus(t, "discard", e.api(owner, http.MethodDelete, fmt.Sprintf("%s?expectedVersion=%d", revPath, saved.Version), nil), http.StatusNoContent, "")
	wantStatus(t, "delete course", e.api(owner, http.MethodDelete, coursePath, nil), http.StatusNoContent, "")
	wantStatus(t, "deleted course", e.api(owner, http.MethodDelete, coursePath, nil), http.StatusNotFound, "not_found")
	wantStatus(t, "lesson draft still there", e.api(owner, http.MethodGet, fmt.Sprintf("/api/revisions/%d", ownLesson.ID), nil), http.StatusOK, "")
}
