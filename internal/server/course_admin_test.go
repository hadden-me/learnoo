package server_test

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/hadden-me/learnoo/internal/permissions"
)

// The administrators' course reads (LMS-B06) through the real server
// handler: authentication, authorization before any lookup, strict query
// parameters, and route precedence over the fixed /api/admin/courses/...
// routes. TEST users and subjects only.
func TestAdminCourseReadsAPI(t *testing.T) {
	e := newEnv(t)
	math := e.newSubject("test-math")
	owner := e.newUser("ac-owner", 0, permissions.Author)
	other := e.newUser("ac-other-author", 0, permissions.Author)
	reviewer := e.newUser("ac-reviewer", math, permissions.Reviewer)
	publisher := e.newUser("ac-publisher", 0, permissions.Publisher)
	admin := e.newUser("ac-admin", 0, permissions.Administrator)
	student := e.newUser("ac-student", 0)

	w := e.api(owner, http.MethodPost, "/api/courses", map[string]any{"subjectId": math, "language": "fr"})
	wantStatus(t, "create course", w, http.StatusCreated, "")
	created := decodeBody[struct {
		Course struct {
			ID int64 `json:"id"`
		} `json:"course"`
	}](t, w)
	courseID := created.Course.ID
	detailPath := fmt.Sprintf("/api/admin/courses/%d", courseID)

	// Anonymous: 401. Every other role: 403 before any lookup, a missing course included.
	for _, path := range []string{"/api/admin/courses", detailPath, "/api/admin/courses/999999999"} {
		wantStatus(t, "anonymous "+path, e.api(apiUser{}, http.MethodGet, path, nil), http.StatusUnauthorized, "unauthenticated")
		for _, u := range []apiUser{owner, reviewer, publisher, student} {
			wantStatus(t, "non-administrator "+path, e.api(u, http.MethodGet, path, nil), http.StatusForbidden, "forbidden")
		}
	}

	// The list: the course, its owner's name and latest revision; strict cursor.
	w = e.api(admin, http.MethodGet, "/api/admin/courses", nil)
	wantStatus(t, "list", w, http.StatusOK, "")
	list := decodeBody[struct {
		Courses []struct {
			ID             int64  `json:"id"`
			OwnerName      string `json:"ownerName"`
			Suspended      bool   `json:"suspended"`
			LatestRevision *struct {
				Status        string `json:"status"`
				Version       int    `json:"version"`
				EverPublished bool   `json:"everPublished"`
			} `json:"latestRevision"`
		} `json:"courses"`
		NextCursor *string `json:"nextCursor"`
	}](t, w)
	if len(list.Courses) != 1 || list.Courses[0].ID != courseID || list.Courses[0].OwnerName == "" || list.Courses[0].LatestRevision == nil ||
		list.Courses[0].LatestRevision.Status != "draft" || list.Courses[0].LatestRevision.Version != 1 || list.NextCursor != nil {
		t.Fatalf("list: %+v", list)
	}
	w = e.api(admin, http.MethodGet, fmt.Sprintf("/api/admin/courses?cursor=%d", courseID), nil)
	wantStatus(t, "next page", w, http.StatusOK, "")
	if b := decodeBody[struct {
		Courses []any `json:"courses"`
	}](t, w); len(b.Courses) != 0 {
		t.Fatalf("page after the last course: %+v", b)
	}
	for _, q := range []string{"?cursor=abc", "?cursor=0", "?cursor=1&cursor=2", "?cursor=", "?mine=1", "?cursor=1&q=x"} {
		wantStatus(t, "list "+q, e.api(admin, http.MethodGet, "/api/admin/courses"+q, nil), http.StatusBadRequest, "invalid_request")
	}

	// The detail: revisions with their version, transfer targets without the owner; missing, malformed and extra query.
	w = e.api(admin, http.MethodGet, detailPath, nil)
	wantStatus(t, "detail", w, http.StatusOK, "")
	detail := decodeBody[struct {
		Course struct {
			ID             int64 `json:"id"`
			LatestRevision *struct {
				Version int `json:"version"`
			} `json:"latestRevision"`
		} `json:"course"`
		Revisions []struct {
			Status string `json:"status"`
		} `json:"revisions"`
		History   []any `json:"history"`
		Lifecycle []any `json:"lifecycle"`
		Authors   []struct {
			ID int64 `json:"id"`
		} `json:"authors"`
	}](t, w)
	if detail.Course.ID != courseID || detail.Course.LatestRevision == nil || detail.Course.LatestRevision.Version != 1 ||
		len(detail.Revisions) != 1 || detail.History == nil || detail.Lifecycle == nil {
		t.Fatalf("detail: %+v", detail)
	}
	foundOther := false
	for _, a := range detail.Authors {
		if a.ID == owner.id {
			t.Fatal("the owner is a transfer target")
		}
		foundOther = foundOther || a.ID == other.id || a.ID == admin.id
	}
	if !foundOther {
		t.Fatalf("authors: %+v", detail.Authors)
	}
	wantStatus(t, "missing", e.api(admin, http.MethodGet, "/api/admin/courses/999999999", nil), http.StatusNotFound, "not_found")
	wantStatus(t, "malformed", e.api(admin, http.MethodGet, "/api/admin/courses/abc", nil), http.StatusNotFound, "not_found")
	wantStatus(t, "detail query", e.api(admin, http.MethodGet, detailPath+"?x=1", nil), http.StatusBadRequest, "invalid_request")

	// The fixed routes keep precedence over /api/admin/courses/{courseId}.
	w = e.api(admin, http.MethodGet, "/api/admin/courses/archive-requests", nil)
	wantStatus(t, "archive requests", w, http.StatusOK, "")
	if b := decodeBody[map[string]any](t, w); b["requests"] == nil {
		t.Fatalf("archive requests answered by another route: %v", b)
	}
	w = e.api(admin, http.MethodGet, "/api/admin/courses/blocked-reviews", nil)
	wantStatus(t, "blocked reviews", w, http.StatusOK, "")
	if b := decodeBody[map[string]any](t, w); b["revisions"] == nil {
		t.Fatalf("blocked reviews answered by another route: %v", b)
	}
}
