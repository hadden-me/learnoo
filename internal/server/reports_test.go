package server_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hadden-me/learnoo/internal/auth"
	"github.com/hadden-me/learnoo/internal/lessons"
	"github.com/hadden-me/learnoo/internal/permissions"
)

// Error reports (LRN-015) through the real server handler: the reader form
// and the review API. TEST lessons and simulated accounts only.

func reportPath(publicID string) string { return lessonPath(publicID) + "/report" }

// revisionNumber is the published revision's number, as the lesson page
// sends it back in the form.
func (f readerFixture) revisionNumber(revisionID int64) string {
	f.t.Helper()
	var n int
	if err := f.db.QueryRow(context.Background(), `SELECT revision_number FROM lesson_revisions WHERE id = $1`, revisionID).Scan(&n); err != nil {
		f.t.Fatal(err)
	}
	return strconv.Itoa(n)
}

// fileReport posts the report form as u with the displayed revision.
func (f readerFixture) fileReport(u apiUser, p published, message string, extra ...reqOpt) *httptest.ResponseRecorder {
	f.t.Helper()
	form := url.Values{"message": {message}, "revision": {f.revisionNumber(p.revisionID)}}
	if u.csrf != "" {
		form.Set("csrf_token", u.csrf)
	}
	w := f.page(u, http.MethodPost, reportPath(p.publicID), form, extra...)
	checkReaderHeaders(f.t, "POST report", w)
	return w
}

func (f readerFixture) reportCount() int {
	return f.count(`SELECT count(*) FROM error_reports`)
}

func TestErrorReportForm(t *testing.T) {
	f := newReaderConfig(t, func(c *auth.Config) { c.ErrorReportAccountLimit, c.ErrorReportAddressLimit = 100, 100 })
	p := f.publishNew("TEST Leçon signalée", "fr", nil)
	path := lessonPath(p.publicID)

	page := f.get(f.student, path)
	wantPage(t, "lesson page", page, http.StatusOK, "Signaler une erreur", `action="`+reportPath(p.publicID)+`"`,
		`name="revision" value="`+f.revisionNumber(p.revisionID)+`"`, `name="csrf_token" value="`+f.student.csrf+`"`, "Au plus 2000 caractères")
	wantAbsent(t, "lesson page", page, "maxlength=")
	wantAbsent(t, "lesson page", page, "<details class=\"report\" open")

	w := f.fileReport(f.student, p, "  La formule de la ligne 2 est fausse.\r\nMerci.  ")
	wantRedirect(t, "report", w, path+"/reported")
	var revision, reporter int64
	var message, status string
	if err := f.db.QueryRow(context.Background(), `SELECT revision_id, reporter_id, message, status FROM error_reports`).Scan(&revision, &reporter, &message, &status); err != nil {
		t.Fatal(err)
	}
	if revision != p.revisionID || reporter != f.student.id || message != "La formule de la ligne 2 est fausse.\nMerci." || status != "open" {
		t.Fatalf("stored report: %d %d %q %s", revision, reporter, message, status)
	}
	done := f.get(f.student, path+"/reported")
	wantPage(t, "confirmation", done, http.StatusOK, "Signalement envoyé", `href="`+path+`"`)
	wantAbsent(t, "confirmation", done, "TEST Leçon signalée", "La formule")

	// Invalid text re-displays the lesson with the (escaped) text and an error.
	for name, tc := range map[string]struct {
		message string
		error   string
	}{
		"blank":    {" \r\n ", "Décrivez l’erreur avant"},
		"too long": {strings.Repeat("é", 2001), "au plus 2000 caractères"},
		"arabic":   {strings.Repeat("ء", 2001), "au plus 2000 caractères"},
		"bidi":     {"<script>alert(1)</script>‮", "mise en forme de direction"},
		"control":  {"a\x1bb", "caractères de contrôle"},
	} {
		w := f.fileReport(f.student, p, tc.message)
		wantPage(t, name, w, http.StatusUnprocessableEntity, tc.error, `<details class="report" open>`, "TEST Leçon signalée")
		wantAbsent(t, name, w, "<script>alert")
	}
	if w := f.fileReport(f.student, p, "<script>alert(1)</script>‮"); !strings.Contains(w.Body.String(), "&lt;script&gt;alert(1)&lt;/script&gt;") {
		t.Errorf("kept text not escaped: %s", w.Body)
	}
	// Missing, repeated or malformed revision fields.
	for name, form := range map[string]url.Values{
		"no revision":       {"message": {"Erreur."}},
		"zero revision":     {"message": {"Erreur."}, "revision": {"0"}},
		"signed revision":   {"message": {"Erreur."}, "revision": {"+1"}},
		"two revisions":     {"message": {"Erreur."}, "revision": {"1", "2"}},
		"no message":        {"revision": {"1"}},
		"two messages":      {"message": {"a", "b"}, "revision": {"1"}},
		"huge revision":     {"message": {"Erreur."}, "revision": {"99999999999"}},
		"revision in query": {"message": {"Erreur."}},
	} {
		target := reportPath(p.publicID)
		if name == "revision in query" {
			target += "?revision=1"
		}
		form.Set("csrf_token", f.student.csrf)
		wantPage(t, name, f.page(f.student, http.MethodPost, target, form), http.StatusBadRequest, "Requête invalide")
	}
	// The longest valid text in 4-byte characters fits the form limit.
	wantRedirect(t, "longest report", f.fileReport(f.student, p, strings.Repeat("𝑥", 2000)), path+"/reported")
	if n := f.reportCount(); n != 2 {
		t.Fatalf("%d reports stored", n)
	}

	// A new published revision: the old form is refused and the current
	// lesson is shown with the text kept; the next submission names it.
	second := f.publish(f.approved(p.lessonID, readerDoc("TEST Leçon corrigée", "fr"), "fr", nil))
	w = f.fileReport(f.student, p, "Erreur vue sur l’ancienne version.")
	wantPage(t, "stale revision", w, http.StatusConflict, "La leçon a été modifiée", "TEST Leçon corrigée", "Erreur vue sur l’ancienne version.",
		`name="revision" value="`+f.revisionNumber(second.revisionID)+`"`)
	wantRedirect(t, "current revision", f.fileReport(f.student, second, "Erreur sur la version corrigée."), path+"/reported")
	if n := f.count(`SELECT count(*) FROM error_reports WHERE revision_id = $1`, second.revisionID); n != 1 {
		t.Fatalf("report against the new revision: %d", n)
	}

	// Unpublished and unknown lessons are 404, as on the lesson page.
	f.unpublish(p.lessonID, second.revisionID)
	wantPage(t, "unpublished", f.fileReport(f.student, second, "Erreur."), http.StatusNotFound, "Leçon indisponible")
	for _, id := range []string{"00000000-0000-0000-0000-000000000000", "not-a-uuid"} {
		w := f.post(f.student, reportPath(id), url.Values{"message": {"Erreur."}, "revision": {"1"}})
		wantPage(t, "unknown "+id, w, http.StatusNotFound, "Leçon indisponible")
	}
	if n := f.reportCount(); n != 3 {
		t.Fatalf("%d reports stored", n)
	}
	// Reports never changed the lessons' content.
	if n := f.count(`SELECT count(*) FROM lesson_revisions WHERE lesson_id = $1`, p.lessonID); n != 2 {
		t.Fatalf("%d revisions", n)
	}
}

func TestErrorReportDenials(t *testing.T) {
	f := newReader(t)
	p := f.publishNew("TEST Leçon protégée", "fr", nil)
	number := f.revisionNumber(p.revisionID)
	other := f.invitedStudent("test-student-two@example.test", "TEST Deux")
	form := func(csrf string) url.Values {
		v := url.Values{"message": {"Erreur."}, "revision": {number}}
		if csrf != "" {
			v.Set("csrf_token", csrf)
		}
		return v
	}

	for name, u := range map[string]apiUser{"anonymous": {}, "forged cookie": {cookie: strings.Repeat("A", 43)}} {
		w := f.page(u, http.MethodPost, reportPath(p.publicID), form("guess"))
		wantPage(t, name, w, http.StatusUnauthorized, "Connexion requise")
		wantAbsent(t, name, w, "TEST Leçon protégée")
		checkReaderHeaders(t, name, w)
		w = f.page(u, http.MethodGet, lessonPath(p.publicID)+"/reported", nil)
		if w.Code != http.StatusSeeOther || !strings.HasPrefix(w.Header().Get("Location"), "/login?next=") {
			t.Fatalf("%s confirmation: %d %s", name, w.Code, w.Header().Get("Location"))
		}
	}
	for name, req := range map[string]func() *httptest.ResponseRecorder{
		"missing CSRF": func() *httptest.ResponseRecorder {
			return f.page(f.student, http.MethodPost, reportPath(p.publicID), form(""))
		},
		"wrong CSRF": func() *httptest.ResponseRecorder {
			return f.page(f.student, http.MethodPost, reportPath(p.publicID), form("wrong"))
		},
		"other user's CSRF": func() *httptest.ResponseRecorder {
			return f.page(f.student, http.MethodPost, reportPath(p.publicID), form(other.csrf))
		},
		"CSRF in query only": func() *httptest.ResponseRecorder {
			return f.page(f.student, http.MethodPost, reportPath(p.publicID)+"?csrf_token="+f.student.csrf, form(""))
		},
		"JSON body": func() *httptest.ResponseRecorder {
			return f.api(f.student, http.MethodPost, reportPath(p.publicID), map[string]string{"message": "Erreur.", "revision": number, "csrf_token": f.student.csrf})
		},
	} {
		w := req()
		wantPage(t, name, w, http.StatusForbidden, "Requête refusée")
		checkReaderHeaders(t, name, w)
	}
	w := f.page(f.student, http.MethodPost, reportPath(p.publicID), form(f.student.csrf), withHeader("Sec-Fetch-Site", "cross-site"))
	wantStatus(t, "cross-site form post", w, http.StatusForbidden, "cross_origin_rejected")
	w = f.page(f.student, http.MethodPost, reportPath(p.publicID), form(f.student.csrf), withHeader("Origin", "https://evil.example"))
	wantStatus(t, "cross-origin form post", w, http.StatusForbidden, "cross_origin_rejected")
	if n := f.reportCount(); n != 0 {
		t.Fatalf("%d reports stored by denied requests", n)
	}
	// Denied requests are not throttled attempts: the student can still report.
	wantRedirect(t, "valid report", f.fileReport(f.student, p, "Erreur."), lessonPath(p.publicID)+"/reported")
}

// Reports are limited per account and per client address, under their own
// throttle scope.
func TestErrorReportThrottling(t *testing.T) {
	f := newReaderConfig(t, func(c *auth.Config) {
		c.ErrorReportAccountLimit = 2
		c.ErrorReportAddressLimit = 3
	})
	p := f.publishNew("TEST Leçon limitée", "fr", nil)
	other := f.invitedStudent("test-student-two@example.test", "TEST Deux")
	home, school := withAddr("203.0.113.50"), withAddr("203.0.113.51")
	path := lessonPath(p.publicID)
	limited := func(what string, w *httptest.ResponseRecorder) {
		t.Helper()
		wantPage(t, what, w, http.StatusTooManyRequests, "Trop de tentatives")
		if ra, err := strconv.Atoi(w.Header().Get("Retry-After")); err != nil || ra < 1 || ra > 15*60 {
			t.Errorf("%s: Retry-After %q", what, w.Header().Get("Retry-After"))
		}
	}

	wantRedirect(t, "first", f.fileReport(f.student, p, "Erreur 1.", home), path+"/reported")
	// An invalid submission counts too.
	wantPage(t, "invalid", f.fileReport(f.student, p, " ", home), http.StatusUnprocessableEntity)
	limited("account limit", f.fileReport(f.student, p, "Erreur 3.", home))
	limited("account limit, other address", f.fileReport(f.student, p, "Erreur 4.", school))
	// The address has seen three attempts (one throttled one included).
	limited("address limit", f.fileReport(other, p, "Erreur 5.", home))
	wantRedirect(t, "other account, other address", f.fileReport(other, p, "Erreur 6.", school), path+"/reported")
	if n := f.reportCount(); n != 2 {
		t.Fatalf("%d reports stored", n)
	}
	// Sign-in and invitations keep their own budgets.
	if w := f.login("test-student@example.test", testPassword, home); w.Code != http.StatusOK {
		t.Fatalf("login from a throttled address: %d %s", w.Code, w.Body)
	}

	f.clock.Advance(15*time.Minute + time.Second)
	wantRedirect(t, "after the window", f.fileReport(f.student, p, "Erreur 7.", home), path+"/reported")
}

type reportBody struct {
	ID                 int64  `json:"id"`
	LessonID           int64  `json:"lessonId"`
	LessonPublicID     string `json:"lessonPublicId"`
	SubjectID          int64  `json:"subjectId"`
	RevisionID         int64  `json:"revisionId"`
	RevisionNumber     int    `json:"revisionNumber"`
	RevisionTitle      string `json:"revisionTitle"`
	CurrentlyPublished bool   `json:"currentlyPublished"`
	Message            string `json:"message"`
	Status             string `json:"status"`
	CreatedAt          string `json:"createdAt"`
	Resolution         *struct {
		Action     string `json:"action"`
		ActorID    int64  `json:"actorId"`
		Note       string `json:"note"`
		OccurredAt string `json:"occurredAt"`
	} `json:"resolution"`
}

func decodeReports(t *testing.T, w *httptest.ResponseRecorder) []reportBody {
	t.Helper()
	var b struct{ Reports []reportBody }
	if err := json.Unmarshal(w.Body.Bytes(), &b); err != nil || b.Reports == nil {
		t.Fatalf("not a report list: %s", w.Body)
	}
	return b.Reports
}

func reportIDs(rs []reportBody) string {
	var ids []string
	for _, r := range rs {
		ids = append(ids, strconv.FormatInt(r.ID, 10))
	}
	return strings.Join(ids, ",")
}

func TestErrorReportReviewAPI(t *testing.T) {
	f := newReader(t)
	math := f.publishNew("TEST Leçon de mathématiques", "fr", nil)
	physics := f.newSubject("test-physics")
	physReviewer := f.newUser("test-physics-reviewer", physics, permissions.Reviewer)
	// A physics lesson, reviewed by the physics reviewer.
	f.setName(physReviewer.id, "TEST Relectrice Physique")
	physLesson := func() published {
		f.t.Helper()
		ctx := context.Background()
		rev, err := f.ls.CreateLesson(ctx, f.author.id, lessons.NewLesson{SubjectID: physics, Language: "fr",
			Fields: lessons.Fields{Content: readerDoc("TEST Leçon de physique", "fr")}})
		if err != nil {
			t.Fatal(err)
		}
		if rev, err = f.ls.Submit(ctx, f.author.id, rev.ID, rev.Version); err != nil {
			t.Fatal(err)
		}
		if _, err = f.rs.Approve(ctx, physReviewer.id, rev.ID, ""); err != nil {
			t.Fatal(err)
		}
		return f.publish(rev)
	}()

	wantRedirect(t, "math report", f.fileReport(f.student, math, "Erreur en mathématiques."), lessonPath(math.publicID)+"/reported")
	wantRedirect(t, "physics report", f.fileReport(f.student, physLesson, "Erreur en physique."), lessonPath(physLesson.publicID)+"/reported")
	var mathID, physID int64
	if err := f.db.QueryRow(context.Background(), `SELECT (SELECT id FROM error_reports WHERE lesson_id = $1), (SELECT id FROM error_reports WHERE lesson_id = $2)`,
		math.lessonID, physLesson.lessonID).Scan(&mathID, &physID); err != nil {
		t.Fatal(err)
	}
	reportURL := func(id int64) string { return "/api/review/error-reports/" + strconv.FormatInt(id, 10) }

	// Students, authors and publishers have no access to the review API.
	for name, u := range map[string]apiUser{"student": f.student, "author": f.author, "publisher": f.publisher} {
		wantStatus(t, name+" list", f.api(u, http.MethodGet, "/api/review/error-reports", nil), http.StatusForbidden, "forbidden")
		wantStatus(t, name+" get", f.api(u, http.MethodGet, reportURL(mathID), nil), http.StatusForbidden, "forbidden")
		wantStatus(t, name+" resolve", f.api(u, http.MethodPost, reportURL(mathID)+"/resolve", map[string]string{"note": "Note."}),
			http.StatusForbidden, "forbidden")
	}
	wantStatus(t, "anonymous list", f.api(apiUser{}, http.MethodGet, "/api/review/error-reports", nil), http.StatusUnauthorized, "unauthenticated")

	// Reviewers see their subject; administrators see all.
	w := f.api(f.reviewer, http.MethodGet, "/api/review/error-reports", nil)
	wantStatus(t, "math reviewer list", w, http.StatusOK, "")
	if got := reportIDs(decodeReports(t, w)); got != strconv.FormatInt(mathID, 10) {
		t.Fatalf("math reviewer sees %s", got)
	}
	if strings.Contains(w.Body.String(), "reporter") || strings.Contains(w.Body.String(), "test-student") {
		t.Fatalf("report list exposes the reporter: %s", w.Body)
	}
	w = f.api(physReviewer, http.MethodGet, "/api/review/error-reports?status=open", nil)
	if got := reportIDs(decodeReports(t, w)); got != strconv.FormatInt(physID, 10) {
		t.Fatalf("physics reviewer sees %s", got)
	}
	w = f.api(f.admin, http.MethodGet, "/api/review/error-reports", nil)
	if got, want := reportIDs(decodeReports(t, w)), fmt.Sprintf("%d,%d", physID, mathID); got != want {
		t.Fatalf("admin sees %s, want %s", got, want)
	}
	w = f.api(f.admin, http.MethodGet, fmt.Sprintf("/api/review/error-reports?subjectId=%d", physics), nil)
	if got := reportIDs(decodeReports(t, w)); got != strconv.FormatInt(physID, 10) {
		t.Fatalf("admin, physics only: %s", got)
	}
	wantStatus(t, "reviewer, other subject filter", f.api(f.reviewer, http.MethodGet,
		fmt.Sprintf("/api/review/error-reports?subjectId=%d", physics), nil), http.StatusNotFound, "not_found")
	for _, q := range []string{"?status=closed", "?status=", "?subjectId=0", "?subjectId=x", "?page=2", "?status=open&status=open"} {
		wantStatus(t, "query "+q, f.api(f.admin, http.MethodGet, "/api/review/error-reports"+q, nil), http.StatusBadRequest, "invalid_request")
	}

	// One report: enumeration-safe 404 across subjects.
	w = f.api(f.reviewer, http.MethodGet, reportURL(mathID), nil)
	wantStatus(t, "get own subject", w, http.StatusOK, "")
	var got reportBody
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.ID != mathID || got.LessonID != math.lessonID || got.LessonPublicID != math.publicID || got.RevisionID != math.revisionID ||
		got.RevisionTitle != "TEST Leçon de mathématiques" || !got.CurrentlyPublished || got.Message != "Erreur en mathématiques." ||
		got.Status != "open" || got.Resolution != nil || got.SubjectID != f.subject {
		t.Fatalf("report: %+v", got)
	}
	missing := f.api(f.reviewer, http.MethodGet, reportURL(physID+1000), nil)
	wantStatus(t, "missing", missing, http.StatusNotFound, "not_found")
	cross := f.api(f.reviewer, http.MethodGet, reportURL(physID), nil)
	wantStatus(t, "other subject", cross, http.StatusNotFound, "not_found")
	if cross.Body.String() != missing.Body.String() {
		t.Fatalf("cross-subject response differs from a missing report:\n%s\n%s", cross.Body, missing.Body)
	}
	for _, bad := range []string{"0", "-1", "01", "x", "99999999999999999999"} {
		wantStatus(t, "id "+bad, f.api(f.admin, http.MethodGet, "/api/review/error-reports/"+bad, nil), http.StatusNotFound, "not_found")
	}

	// Resolution: CSRF, strict body, required note, subject scope, once.
	noCSRF := apiUser{id: f.reviewer.id, cookie: f.reviewer.cookie}
	wantStatus(t, "resolve without CSRF", f.api(noCSRF, http.MethodPost, reportURL(mathID)+"/resolve", map[string]string{"note": "Note."}),
		http.StatusForbidden, "csrf_rejected")
	wantStatus(t, "resolve cross-site", f.api(f.reviewer, http.MethodPost, reportURL(mathID)+"/resolve", map[string]string{"note": "Note."},
		withHeader("Sec-Fetch-Site", "cross-site")), http.StatusForbidden, "cross_origin_rejected")
	for name, body := range map[string]any{
		"unknown field":   map[string]string{"note": "Note.", "status": "resolved"},
		"case variant":    []byte(`{"Note": "Note."}`),
		"duplicate":       []byte(`{"note": "a", "note": "b"}`),
		"not an object":   []byte(`["Note."]`),
		"trailing data":   []byte(`{"note": "Note."} {}`),
		"wrong note type": map[string]int{"note": 1},
	} {
		wantStatus(t, name, f.api(f.reviewer, http.MethodPost, reportURL(mathID)+"/resolve", body), http.StatusBadRequest, "invalid_request")
	}
	wantStatus(t, "no note", f.api(f.reviewer, http.MethodPost, reportURL(mathID)+"/resolve", map[string]any{}), http.StatusUnprocessableEntity, "note_required")
	wantStatus(t, "blank note", f.api(f.reviewer, http.MethodPost, reportURL(mathID)+"/dismiss", map[string]string{"note": " \n "}),
		http.StatusUnprocessableEntity, "note_required")
	wantStatus(t, "long note", f.api(f.reviewer, http.MethodPost, reportURL(mathID)+"/resolve", map[string]string{"note": strings.Repeat("n", 2001)}),
		http.StatusUnprocessableEntity, "invalid_note")
	wantStatus(t, "other subject resolve", f.api(physReviewer, http.MethodPost, reportURL(mathID)+"/resolve", map[string]string{"note": "Note."}),
		http.StatusNotFound, "not_found")
	if n := f.count(`SELECT count(*) FROM error_report_resolutions`); n != 0 {
		t.Fatalf("%d resolutions after refusals", n)
	}

	w = f.api(f.reviewer, http.MethodPost, reportURL(mathID)+"/resolve", map[string]string{"note": "Corrigé dans une nouvelle révision."})
	wantStatus(t, "resolve", w, http.StatusOK, "")
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Status != "resolved" || got.Resolution == nil || got.Resolution.Action != "resolve" || got.Resolution.ActorID != f.reviewer.id ||
		got.Resolution.Note != "Corrigé dans une nouvelle révision." || got.Resolution.OccurredAt == "" {
		t.Fatalf("resolved: %s", w.Body)
	}
	wantStatus(t, "dismiss after resolve", f.api(f.admin, http.MethodPost, reportURL(mathID)+"/dismiss", map[string]string{"note": "Note."}),
		http.StatusConflict, "not_open")
	// Authorization precedes the status check: a reviewer of another subject
	// cannot tell a closed report from a missing one.
	missingResolve := f.api(physReviewer, http.MethodPost, reportURL(physID+1000)+"/dismiss", map[string]string{"note": "Note."})
	wantStatus(t, "resolve missing", missingResolve, http.StatusNotFound, "not_found")
	for _, action := range []string{"/resolve", "/dismiss"} {
		w := f.api(physReviewer, http.MethodPost, reportURL(mathID)+action, map[string]string{"note": "Note."})
		wantStatus(t, "closed report, other subject "+action, w, http.StatusNotFound, "not_found")
		if w.Body.String() != missingResolve.Body.String() {
			t.Fatalf("closed other-subject report differs from a missing one:\n%s\n%s", w.Body, missingResolve.Body)
		}
	}
	w = f.api(f.admin, http.MethodPost, reportURL(physID)+"/dismiss", map[string]string{"note": "Pas une erreur."})
	wantStatus(t, "admin dismiss", w, http.StatusOK, "")
	w = f.api(f.admin, http.MethodGet, "/api/review/error-reports?status=resolved", nil)
	if got := reportIDs(decodeReports(t, w)); got != strconv.FormatInt(mathID, 10) {
		t.Fatalf("resolved filter: %s", got)
	}
	w = f.api(f.reviewer, http.MethodGet, "/api/review/error-reports?status=open", nil)
	if got := decodeReports(t, w); len(got) != 0 {
		t.Fatalf("open after resolution: %s", w.Body)
	}
}

// A body over the report form's 32 KiB limit (text pasted far beyond 2000
// characters) is never accepted and never throttled: the lesson page comes
// back with 413 and an explanation, the text is not kept, nothing is stored.
func TestErrorReportOversizedBody(t *testing.T) {
	f := newReader(t)
	p := f.publishNew("TEST Leçon longue", "fr", nil)
	attempts := func() int { return f.count(`SELECT count(*) FROM login_attempts`) }
	before := attempts()
	huge := strings.Repeat("x", 40<<10)
	for name, csrf := range map[string]string{"valid CSRF": f.student.csrf, "no CSRF": ""} {
		form := url.Values{"message": {huge}, "revision": {f.revisionNumber(p.revisionID)}}
		if csrf != "" {
			form.Set("csrf_token", csrf)
		}
		w := f.page(f.student, http.MethodPost, reportPath(p.publicID), form)
		checkReaderHeaders(t, name, w)
		wantPage(t, name, w, http.StatusRequestEntityTooLarge, "Le texte est trop long (au plus 2000 caractères)",
			"n’a pas pu être conservé", `<details class="report" open>`, "TEST Leçon longue")
		wantAbsent(t, name, w, "xxxxxxxxxx")
	}
	if n := f.reportCount(); n != 0 {
		t.Fatalf("%d reports stored from oversized bodies", n)
	}
	if n := attempts(); n != before {
		t.Fatalf("oversized bodies were throttled: %d attempts, want %d", n, before)
	}
	// Anonymous oversized posts are still refused first; other reader forms
	// keep the generic 403 for oversized bodies.
	w := f.page(apiUser{}, http.MethodPost, reportPath(p.publicID), url.Values{"message": {huge}})
	wantPage(t, "anonymous", w, http.StatusUnauthorized, "Connexion requise")
	w = f.page(f.student, http.MethodPost, lessonPath(p.publicID)+"/bookmark", url.Values{"csrf_token": {f.student.csrf}, "pad": {huge}})
	wantPage(t, "oversized bookmark", w, http.StatusForbidden, "Requête refusée")
	// The student can still report normally.
	wantRedirect(t, "valid report", f.fileReport(f.student, p, "Erreur."), lessonPath(p.publicID)+"/reported")
}
