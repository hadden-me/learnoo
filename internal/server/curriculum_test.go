package server_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hadden-me/learnoo/internal/permissions"
)

// TEST curriculum fixtures only: no real level, section or edition names.

type editionResp struct {
	ID          int64  `json:"id"`
	SectionID   int64  `json:"sectionId"`
	State       string `json:"state"`
	Version     int    `json:"version"`
	Assignments []struct {
		ID         int64 `json:"id"`
		SubjectID  int64 `json:"subjectId"`
		Selectable bool  `json:"selectable"`
		Chapters   []struct {
			ChapterID int64 `json:"chapterId"`
			Position  int   `json:"position"`
		} `json:"chapters"`
	} `json:"assignments"`
}

func decodeBody[T any](t *testing.T, w *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode %s: %v", w.Body, err)
	}
	return v
}

func TestCurriculumAdministrationAPI(t *testing.T) {
	e := newEnv(t)
	math := e.newSubject("test-math")
	ch1, ch2 := e.chapter(math, "ch-01"), e.chapter(math, "ch-02")
	admin := e.newUser("cur-admin", 0, permissions.Administrator)
	author := e.newUser("cur-author", 0, permissions.Author)
	reviewer := e.newUser("cur-reviewer", math, permissions.Reviewer)
	student := e.newUser("cur-student", 0)
	const base = "/api/admin/curriculum"

	w := e.api(admin, http.MethodPost, base+"/levels", map[string]any{"code": "test-bac", "nameFr": "TEST Bac", "nameAr": "اختبار"})
	wantStatus(t, "create level", w, http.StatusCreated, "")
	if cc := w.Header().Get("Cache-Control"); cc != "private, no-store" {
		t.Errorf("Cache-Control %q", cc)
	}
	level := decodeBody[struct {
		ID      int64 `json:"id"`
		Version int   `json:"version"`
	}](t, w)
	w = e.api(admin, http.MethodPost, fmt.Sprintf("%s/levels/%d/sections", base, level.ID), map[string]any{"code": "test-sec", "nameFr": "TEST Section", "nameAr": "شعبة", "displayOrder": 2})
	wantStatus(t, "create section", w, http.StatusCreated, "")
	section := decodeBody[struct{ ID int64 }](t, w)
	w = e.api(admin, http.MethodPost, fmt.Sprintf("%s/sections/%d/editions", base, section.ID), map[string]any{"code": "test-2026", "labelFr": "TEST 2026", "labelAr": "طبعة"})
	wantStatus(t, "create edition", w, http.StatusCreated, "")
	ed := decodeBody[editionResp](t, w)
	edPath := fmt.Sprintf("%s/editions/%d", base, ed.ID)

	w = e.api(admin, http.MethodPost, edPath+"/assignments", map[string]any{"expectedVersion": ed.Version, "subjectId": math})
	wantStatus(t, "add assignment", w, http.StatusCreated, "")
	ed = decodeBody[editionResp](t, w)
	asg := ed.Assignments[0].ID
	chPath := fmt.Sprintf("%s/assignments/%d/chapters", edPath, asg)
	wantStatus(t, "stale chapters", e.api(admin, http.MethodPut, chPath, map[string]any{"expectedVersion": 1, "chapterIds": []int64{ch1}}), http.StatusConflict, "version_conflict")
	wantStatus(t, "foreign chapter", e.api(admin, http.MethodPut, chPath, map[string]any{"expectedVersion": ed.Version, "chapterIds": []int64{999999}}), http.StatusUnprocessableEntity, "placement_invalid")
	wantStatus(t, "missing chapterIds", e.api(admin, http.MethodPut, chPath, map[string]any{"expectedVersion": ed.Version}), http.StatusBadRequest, "invalid_request")
	w = e.api(admin, http.MethodPut, chPath, map[string]any{"expectedVersion": ed.Version, "chapterIds": []int64{ch2, ch1}})
	wantStatus(t, "set chapters", w, http.StatusOK, "")
	ed = decodeBody[editionResp](t, w)
	if c := ed.Assignments[0].Chapters; len(c) != 2 || c[0].ChapterID != ch2 {
		t.Fatalf("chapters %+v", c)
	}

	wantStatus(t, "make current without expected current", e.api(admin, http.MethodPost, edPath+"/make-current", map[string]any{"expectedVersion": ed.Version}), http.StatusBadRequest, "invalid_request")
	wantStatus(t, "make current, wrong expected current", e.api(admin, http.MethodPost, edPath+"/make-current", map[string]any{"expectedVersion": ed.Version, "expectedCurrentEditionId": 12345}), http.StatusConflict, "state_conflict")
	w = e.api(admin, http.MethodPost, edPath+"/make-current", map[string]any{"expectedVersion": ed.Version, "expectedCurrentEditionId": nil})
	wantStatus(t, "make current", w, http.StatusOK, "")
	mc := decodeBody[struct {
		Edition    editionResp `json:"edition"`
		Superseded *int64      `json:"supersededEditionId"`
	}](t, w)
	if mc.Edition.State != "current" || mc.Superseded != nil || !mc.Edition.Assignments[0].Selectable {
		t.Fatalf("make current: %+v", mc)
	}
	ed = mc.Edition
	wantStatus(t, "edit after current", e.api(admin, http.MethodPut, chPath, map[string]any{"expectedVersion": ed.Version, "chapterIds": []int64{ch1}}), http.StatusConflict, "edition_not_draft")
	wantStatus(t, "remove after current", e.api(admin, http.MethodDelete, fmt.Sprintf("%s/assignments/%d?expectedVersion=%d", edPath, asg, ed.Version), nil), http.StatusConflict, "edition_not_draft")
	wantStatus(t, "duplicate level", e.api(admin, http.MethodPost, base+"/levels", map[string]any{"code": "test-bac", "nameFr": "x", "nameAr": "y"}), http.StatusConflict, "code_exists")
	wantStatus(t, "invalid code", e.api(admin, http.MethodPost, base+"/levels", map[string]any{"code": "Bad Code", "nameFr": "x", "nameAr": "y"}), http.StatusUnprocessableEntity, "invalid_input")
	wantStatus(t, "retire subject", e.api(admin, http.MethodPost, fmt.Sprintf("%s/subjects/%d/retire", base, math), map[string]any{}), http.StatusNoContent, "")
	w = e.api(author, http.MethodGet, "/api/catalogue/subjects", nil)
	wantStatus(t, "subjects after retirement", w, http.StatusOK, "")
	subjects := decodeBody[struct {
		Subjects []struct {
			ID        int64   `json:"id"`
			RetiredAt *string `json:"retiredAt"`
		} `json:"subjects"`
	}](t, w)
	if len(subjects.Subjects) != 1 || subjects.Subjects[0].RetiredAt == nil {
		t.Fatalf("retiredAt not exposed: %s", w.Body)
	}
	wantStatus(t, "delete with a body", e.api(admin, http.MethodDelete, fmt.Sprintf("%s/assignments/%d?expectedVersion=%d", edPath, asg, ed.Version), map[string]any{}),
		http.StatusBadRequest, "invalid_request")
	wantStatus(t, "retire subject twice", e.api(admin, http.MethodPost, fmt.Sprintf("%s/subjects/%d/retire", base, math), map[string]any{}), http.StatusConflict, "already_retired")

	// Making an edition current that assigns a retired subject is refused.
	w = e.api(admin, http.MethodPost, fmt.Sprintf("%s/sections/%d/editions", base, section.ID), map[string]any{"code": "test-2027", "labelFr": "TEST 2027", "labelAr": "طبعة"})
	wantStatus(t, "second edition", w, http.StatusCreated, "")
	next := decodeBody[editionResp](t, w)
	nextPath := fmt.Sprintf("%s/editions/%d", base, next.ID)
	wantStatus(t, "assign a retired subject", e.api(admin, http.MethodPost, nextPath+"/assignments", map[string]any{"expectedVersion": next.Version, "subjectId": math}),
		http.StatusUnprocessableEntity, "placement_invalid")

	// Strict JSON.
	wantStatus(t, "unknown key", e.api(admin, http.MethodPost, base+"/levels", []byte(`{"code":"test-x","nameFr":"x","nameAr":"y","extra":1}`)), http.StatusBadRequest, "invalid_request")
	wantStatus(t, "case variant key", e.api(admin, http.MethodPost, base+"/levels", []byte(`{"Code":"test-x","nameFr":"x","nameAr":"y"}`)), http.StatusBadRequest, "invalid_request")
	wantStatus(t, "duplicate key", e.api(admin, http.MethodPost, base+"/levels", []byte(`{"code":"test-x","code":"test-y","nameFr":"x","nameAr":"y"}`)), http.StatusBadRequest, "invalid_request")
	wantStatus(t, "null name", e.api(admin, http.MethodPost, base+"/levels", []byte(`{"code":"test-x","nameFr":null,"nameAr":"y"}`)), http.StatusBadRequest, "invalid_request")
	wantStatus(t, "string version", e.api(admin, http.MethodPost, edPath+"/retire", []byte(`{"expectedVersion":"1"}`)), http.StatusBadRequest, "invalid_request")

	// Reads for staff, with the full edition.
	for _, u := range []apiUser{admin, author, reviewer} {
		wantStatus(t, "levels", e.api(u, http.MethodGet, "/api/catalogue/levels", nil), http.StatusOK, "")
		wantStatus(t, "editions", e.api(u, http.MethodGet, fmt.Sprintf("/api/catalogue/sections/%d/editions", section.ID), nil), http.StatusOK, "")
		w = e.api(u, http.MethodGet, fmt.Sprintf("/api/catalogue/editions/%d", ed.ID), nil)
		wantStatus(t, "edition", w, http.StatusOK, "")
		got := decodeBody[editionResp](t, w)
		if len(got.Assignments) != 1 || got.Assignments[0].Selectable || len(got.Assignments[0].Chapters) != 2 {
			t.Fatalf("edition read: %+v", got)
		}
	}
	wantStatus(t, "unknown edition", e.api(admin, http.MethodGet, "/api/catalogue/editions/987654", nil), http.StatusNotFound, "not_found")
	wantStatus(t, "malformed edition id", e.api(admin, http.MethodGet, "/api/catalogue/editions/01", nil), http.StatusNotFound, "not_found")

	// Authorization: anonymous 401, students 403 everywhere, staff without
	// administrator 403 on writes, before any lookup; CSRF on writes.
	writes := []struct{ method, path string }{
		{http.MethodPost, base + "/levels"},
		{http.MethodPut, fmt.Sprintf("%s/levels/%d", base, level.ID)},
		{http.MethodPost, fmt.Sprintf("%s/levels/%d/retire", base, level.ID)},
		{http.MethodPost, fmt.Sprintf("%s/levels/%d/sections", base, level.ID)},
		{http.MethodPut, fmt.Sprintf("%s/sections/%d", base, section.ID)},
		{http.MethodPost, fmt.Sprintf("%s/sections/%d/retire", base, section.ID)},
		{http.MethodPost, fmt.Sprintf("%s/sections/%d/editions", base, section.ID)},
		{http.MethodPut, edPath},
		{http.MethodPost, edPath + "/assignments"},
		{http.MethodDelete, fmt.Sprintf("%s/assignments/%d?expectedVersion=1", edPath, asg)},
		{http.MethodPut, chPath},
		{http.MethodPost, edPath + "/make-current"},
		{http.MethodPost, edPath + "/retire"},
		{http.MethodPost, fmt.Sprintf("%s/subjects/%d/retire", base, math)},
		{http.MethodPost, fmt.Sprintf("%s/chapters/%d/retire", base, ch1)},
	}
	before := e.count(`SELECT count(*) FROM curriculum_audit`)
	for _, wr := range writes {
		body := map[string]any{}
		wantStatus(t, "anonymous "+wr.path, e.api(apiUser{}, wr.method, wr.path, body), http.StatusUnauthorized, "unauthenticated")
		for name, u := range map[string]apiUser{"author": author, "reviewer": reviewer, "student": student} {
			wantStatus(t, name+" "+wr.method+" "+wr.path, e.api(u, wr.method, wr.path, body), http.StatusForbidden, "forbidden")
		}
		wantStatus(t, "no csrf "+wr.path, e.api(admin, wr.method, wr.path, body, withCSRF("")), http.StatusForbidden, "csrf_rejected")
		wantStatus(t, "cross-site "+wr.path, e.api(admin, wr.method, wr.path, body, withHeader("Sec-Fetch-Site", "cross-site")), http.StatusForbidden, "cross_origin_rejected")
	}
	if after := e.count(`SELECT count(*) FROM curriculum_audit`); after != before {
		t.Fatalf("refused requests wrote %d audit rows", after-before)
	}
	for _, path := range []string{"/api/catalogue/levels", fmt.Sprintf("/api/catalogue/editions/%d", ed.ID), fmt.Sprintf("/api/catalogue/sections/%d/editions", section.ID)} {
		wantStatus(t, "anonymous "+path, e.api(apiUser{}, http.MethodGet, path, nil), http.StatusUnauthorized, "unauthenticated")
		wantStatus(t, "student "+path, e.api(student, http.MethodGet, path, nil), http.StatusForbidden, "forbidden")
	}

}
