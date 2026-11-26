package server_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/hadden-me/learnoo/internal/courses"
	"github.com/hadden-me/learnoo/internal/coursetest"
)

// Course reading (LMS-B08) through the real server handler: outline,
// pinned revisions, unavailable steps, suspension, course media, course
// bookmarks, the home preference, course search and course-context
// reports. TEST users, curriculum entries and documents only.

// courseLesson publishes a TEST lesson whose body names a unique marker,
// with a lesson title distinct from the course-authored step title.
func (f readerFixture) courseLesson(marker string) published {
	f.t.Helper()
	return f.publish(f.approved(0, readerDoc("TEST Titre leçon "+marker, "fr",
		para(text("Corps de test "+marker+"."))), "fr", nil))
}

func (f readerFixture) courseSpec(title string, modules ...coursetest.Module) coursetest.Spec {
	return coursetest.Spec{Admin: f.admin.id, Owner: f.author.id, Reviewer: f.reviewer.id, Publisher: f.publisher.id,
		SubjectID: f.subject, Title: title, Summary: "Résumé de test " + title + ".", Modules: modules}
}

func step(p published, title string) coursetest.Step {
	return coursetest.Step{LessonID: p.lessonID, RevisionID: p.revisionID, Title: title}
}

func courseURL(c coursetest.Course) string { return "/learn/courses/" + c.PublicID }

func courseStepURL(c coursetest.Course, p published) string {
	return courseURL(c) + "/lessons/" + p.publicID
}

func (f readerFixture) withdrawRevision(lessonID, revisionID int64) {
	f.t.Helper()
	var ptr *int64
	if err := f.db.QueryRow(context.Background(), `SELECT published_revision_id FROM lessons WHERE id = $1`, lessonID).Scan(&ptr); err != nil {
		f.t.Fatal(err)
	}
	if _, err := f.rs.Withdraw(context.Background(), f.admin.id, revisionID, "TEST retrait", ptr); err != nil {
		f.t.Fatalf("withdraw: %v", err)
	}
}

func TestCourseOutlineNavigationAndUnavailableSteps(t *testing.T) {
	f := newReader(t)
	l1, l2, l3, l4 := f.courseLesson("ALPHA"), f.courseLesson("BRAVO"), f.courseLesson("CHARLIE"), f.courseLesson("DELTA")
	c := coursetest.Publish(t, f.db, f.courseSpec("TEST Cours complexe",
		coursetest.Module{Title: "TEST Module un", Steps: []coursetest.Step{step(l1, "TEST Étape alpha"), step(l2, "TEST Étape bravo")}},
		coursetest.Module{Title: "TEST Module deux", Steps: []coursetest.Step{step(l3, "TEST Étape charlie"), step(l4, "TEST Étape delta")}}))
	s := f.student

	// Anonymous readers are sent to sign in and come back.
	w := f.get(apiUser{}, courseURL(c))
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/login?next="+url.QueryEscape(courseURL(c)) {
		t.Fatalf("anonymous: %d %v", w.Code, w.Header())
	}
	w = f.get(apiUser{}, courseStepURL(c, l1))
	if w.Code != http.StatusSeeOther || !strings.Contains(w.Header().Get("Location"), url.QueryEscape(courseStepURL(c, l1))) {
		t.Fatalf("anonymous step: %d %v", w.Code, w.Header())
	}

	// Outline: modules and steps in order, with the course-authored titles.
	w = f.get(s, courseURL(c))
	wantPage(t, "outline", w, http.StatusOK, "TEST Cours complexe", "Plan du cours", "Commencer le cours", "TEST Niveau › TEST Section")
	wantInOrder(t, "outline order", w.Body.String(), "TEST Module un", "TEST Étape alpha", "TEST Étape bravo", "TEST Module deux", "TEST Étape charlie", "TEST Étape delta")
	wantAbsent(t, "outline", w, "temporairement indisponible", "Corps de test")

	// Steps: the pinned revision through the canonical renderer, breadcrumbs,
	// previous and next.
	w = f.get(s, courseStepURL(c, l2))
	wantPage(t, "step 2", w, http.StatusOK, "Corps de test BRAVO.", "TEST Titre leçon BRAVO", "Leçon 2 sur 4", `aria-label="Fil d’Ariane"`,
		`rel="prev" href="`+courseStepURL(c, l1)+`"`, `rel="next" href="`+courseStepURL(c, l3)+`"`, `aria-current="page"`,
		`name="course_revision" value="`+strconv.Itoa(c.RevisionNumber)+`"`)
	if csp := w.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "script-src 'self'") {
		t.Fatalf("step CSP %q", csp)
	}
	wantAbsent(t, "step 2", w, "Corps de test ALPHA", "Corps de test CHARLIE", "Ce cours présente la version")
	w = f.get(s, courseStepURL(c, l1))
	wantPage(t, "step 1", w, http.StatusOK, "Corps de test ALPHA.", `rel="next"`)
	wantAbsent(t, "step 1", w, `rel="prev"`)
	w = f.get(s, courseStepURL(c, l4))
	wantPage(t, "last step", w, http.StatusOK, "Corps de test DELTA.", `rel="prev"`, "Retour au cours")
	wantAbsent(t, "last step", w, `rel="next"`, "cours incomplet", "le cours est incomplet")

	// A lesson that is not a step, malformed IDs: the generic notice.
	other := f.courseLesson("ECHO")
	for _, p := range []string{courseURL(c) + "/lessons/" + other.publicID, courseURL(c) + "/lessons/x", "/learn/courses/x",
		"/learn/courses/" + strings.ToUpper(c.PublicID), "/learn/courses/" + l1.publicID} {
		wantPage(t, p, f.get(s, p), http.StatusNotFound, "Cours indisponible")
	}

	// Withdraw the pinned revision of step 2, unpublish the lesson of step
	// 3 and archive the lesson of step 4: each step stays in the outline with
	// the notice, shows only its course-authored title, and navigation stops
	// at it rather than skipping it.
	f.withdrawRevision(l2.lessonID, l2.revisionID)
	f.unpublish(l3.lessonID, l3.revisionID)
	if _, err := f.db.Exec(context.Background(), `UPDATE lessons SET archived_at = now() WHERE id = $1`, l4.lessonID); err != nil {
		t.Fatal(err)
	}
	w = f.get(s, courseURL(c))
	wantPage(t, "outline after", w, http.StatusOK, "3 leçons de ce cours sont temporairement indisponibles", "Le cours est incomplet",
		"TEST Étape bravo</span> <span class=\"step-state\">(temporairement indisponible)")
	wantAbsent(t, "outline after", w, "TEST Titre leçon BRAVO", "TEST Titre leçon CHARLIE", "TEST Titre leçon DELTA")
	w = f.get(s, courseStepURL(c, l1))
	wantPage(t, "step 1 after", w, http.StatusOK, "Corps de test ALPHA.",
		`rel="next" href="`+courseStepURL(c, l2)+`"`, "TEST Étape bravo</span> <span class=\"step-state\">(temporairement indisponible)", "le cours est incomplet pour le moment")
	for _, tc := range []struct {
		p              published
		title, prev    string
		next, absentRe string
	}{
		{l2, "TEST Étape bravo", courseStepURL(c, l1), courseStepURL(c, l3), "BRAVO"},
		{l3, "TEST Étape charlie", courseStepURL(c, l2), courseStepURL(c, l4), "CHARLIE"},
		{l4, "TEST Étape delta", courseStepURL(c, l3), "", "DELTA"},
	} {
		w = f.get(s, courseStepURL(c, tc.p))
		wantPage(t, "unavailable "+tc.title, w, http.StatusOK, "Cette leçon est temporairement indisponible", tc.title, `rel="prev" href="`+tc.prev+`"`)
		if tc.next != "" {
			wantPage(t, "unavailable next "+tc.title, w, http.StatusOK, `rel="next" href="`+tc.next+`"`)
		} else {
			// The end of the outline never presents the course as complete.
			wantPage(t, "end of plan "+tc.title, w, http.StatusOK, "Fin du plan · cours incomplet",
				"3 leçons de ce cours sont temporairement indisponibles : le cours est incomplet pour le moment.")
		}
		wantAbsent(t, "unavailable "+tc.title, w, "Corps de test "+tc.absentRe, "TEST Titre leçon "+tc.absentRe, "Signaler une erreur", "katex")
	}
	// The standalone pages agree.
	wantPage(t, "standalone withdrawn", f.get(s, lessonPath(l2.publicID)), http.StatusNotFound)
	wantPage(t, "standalone unpublished", f.get(s, lessonPath(l3.publicID)), http.StatusNotFound)

	// The unpublished lesson is restored: its step returns.
	if _, err := f.rs.Restore(context.Background(), f.admin.id, l3.revisionID, nil); err != nil {
		t.Fatal(err)
	}
	wantPage(t, "restored step", f.get(s, courseStepURL(c, l3)), http.StatusOK, "Corps de test CHARLIE.")
}

func TestCoursePinsItsRevisionAndSaysWhenItDiffers(t *testing.T) {
	f := newReader(t)
	l1 := f.courseLesson("PREMIÈRE")
	c := coursetest.Publish(t, f.db, f.courseSpec("TEST Cours épinglé",
		coursetest.Module{Title: "TEST Module", Steps: []coursetest.Step{step(l1, "TEST Étape épinglée")}}))
	// A newer standalone revision: the course keeps the pinned one.
	rev2 := f.publish(f.approved(l1.lessonID, readerDoc("TEST Titre leçon SECONDE", "fr", para(text("Corps de test SECONDE."))), "fr", nil))
	w := f.get(f.student, courseStepURL(c, l1))
	wantPage(t, "pinned", w, http.StatusOK, "Corps de test PREMIÈRE.", "Ce cours présente la version 1 de cette leçon",
		"La version publiée seule est la version 2", `href="`+lessonPath(l1.publicID)+`"`)
	wantAbsent(t, "pinned", w, "Corps de test SECONDE")
	wantPage(t, "standalone", f.get(f.student, lessonPath(l1.publicID)), http.StatusOK, "Corps de test SECONDE.")

	// Withdrawing the pinned (no longer published) revision makes the step
	// unavailable; no other revision is substituted.
	f.withdrawRevision(l1.lessonID, l1.revisionID)
	w = f.get(f.student, courseStepURL(c, l1))
	wantPage(t, "pinned withdrawn", w, http.StatusOK, "Cette leçon est temporairement indisponible")
	wantAbsent(t, "pinned withdrawn", w, "Corps de test PREMIÈRE", "Corps de test SECONDE", "TEST Titre leçon")
	wantPage(t, "standalone still readable", f.get(f.student, lessonPath(rev2.publicID)), http.StatusOK, "Corps de test SECONDE.")
}

func TestSuspendedArchivedAndUnpublishedCoursesAreHidden(t *testing.T) {
	f := newReader(t)
	l1 := f.courseLesson("SUSPENDU")
	spec := f.courseSpec("TEST Cours suspendu", coursetest.Module{Title: "TEST Module", Steps: []coursetest.Step{step(l1, "TEST Étape suspendue")}})
	c := coursetest.Publish(t, f.db, spec)
	archivedSpec := spec
	archivedSpec.Title = "TEST Cours archivé"
	archived := coursetest.Publish(t, f.db, archivedSpec)
	s := f.student
	wantStatus(t, "bookmark", f.post(s, courseURL(c)+"/bookmark", nil), http.StatusSeeOther, "")
	wantPage(t, "search before", f.get(s, "/learn/search?q=suspendu"), http.StatusOK, "TEST Cours suspendu")

	coursetest.Suspend(t, f.db, f.admin.id, c)
	coursetest.Archive(t, f.db, f.admin.id, archived)
	for name, co := range map[string]coursetest.Course{"suspended": c, "archived": archived} {
		for _, p := range []string{courseURL(co), courseStepURL(co, l1)} {
			w := f.get(s, p)
			wantPage(t, name+" "+p, w, http.StatusNotFound, "Cours indisponible", "Retour à mes leçons")
			wantAbsent(t, name+" "+p, w, "TEST Cours", "Corps de test", "TEST Étape")
		}
		wantStatus(t, name+" bookmark", f.post(s, courseURL(co)+"/bookmark", nil), http.StatusNotFound, "")
	}
	// The bookmark survives, shown as unavailable; search and the home no
	// longer list the course; the standalone lesson is unaffected.
	w := f.get(s, "/learn")
	wantPage(t, "home", w, http.StatusOK, "Mes cours enregistrés (1)", "Cours indisponible pour le moment", courseURL(c)+"/unbookmark")
	wantAbsent(t, "home", w, "TEST Cours suspendu", "TEST Cours archivé")
	w = f.get(s, "/learn/search?q=TEST+Cours")
	wantPage(t, "search after", w, http.StatusOK, "Cours (0)")
	wantPage(t, "standalone", f.get(s, lessonPath(l1.publicID)), http.StatusOK, "Corps de test SUSPENDU.")

	// A course that was never published: the same notice.
	var draftID string
	if err := f.db.QueryRow(context.Background(), `INSERT INTO courses (owner_id, subject_id, language, created_at, created_by)
		VALUES ($1, $2, 'fr', now(), $1) RETURNING public_id::text`, f.author.id, f.subject).Scan(&draftID); err != nil {
		t.Fatal(err)
	}
	wantPage(t, "never published", f.get(s, "/learn/courses/"+draftID), http.StatusNotFound, "Cours indisponible")
	// Lifting the suspension (a restore) brings it back.
	if _, err := courses.NewStore(f.db).Restore(context.Background(), f.publisher.id, c.ID, c.RevisionID, nil); err != nil {
		t.Fatal(err)
	}
	wantPage(t, "restored", f.get(s, courseURL(c)), http.StatusOK, "TEST Cours suspendu")
}

func TestCourseMediaServesOnlyReadablePinnedRevisions(t *testing.T) {
	rf := newReader(t)
	wf := withdrawFixture{reviewFixture: reviewFixture{studioFixture: studioFixture{env: rf.env, subject: rf.subject, author: rf.author},
		reviewer: rf.reviewer, publisher: rf.publisher}, admin: rf.admin}
	img, attachedOnly, later := rf.uploadAs(rf.author, 121), rf.uploadAs(rf.author, 122), rf.uploadAs(rf.author, 123)
	w := rf.createWithMedia(rf.author, rf.subject, mediaDoc("TEST Image de cours", false, img), []int64{img, attachedOnly})
	wantStatus(t, "create", w, http.StatusCreated, "")
	rev1 := wf.submitRev(rf.author, decodeRevision(t, w))
	wf.approvedRev(rev1)
	wf.publishedRev(rev1)
	l := published{lessonID: rev1.LessonID, revisionID: rev1.ID, publicID: rf.publicID(rev1.LessonID)}
	plain := rf.courseLesson("SANS IMAGE")
	c := coursetest.Publish(t, rf.db, rf.courseSpec("TEST Cours illustré",
		coursetest.Module{Title: "TEST Module", Steps: []coursetest.Step{step(l, "TEST Étape illustrée"), step(plain, "TEST Étape simple")}}))
	cmedia := func(p published, id int64) string { return fmt.Sprintf("%s/media/%d", courseStepURL(c, p), id) }
	s := rf.student

	wantPage(t, "step", rf.get(s, courseStepURL(c, l)), http.StatusOK,
		fmt.Sprintf(`<img src="%s" alt="TEST image %d" loading="lazy">`, cmedia(l, img), img))
	wantMedia(t, "course media", rf.get(s, cmedia(l, img)))
	wantNoMedia(t, "anonymous", rf.get(apiUser{}, cmedia(l, img)))
	wantNoMedia(t, "attached only", rf.get(s, cmedia(l, attachedOnly)))
	wantNoMedia(t, "another step", rf.get(s, cmedia(plain, img)))
	wantNoMedia(t, "malformed", rf.get(s, courseStepURL(c, l)+"/media/0"+strconv.FormatInt(img, 10)))

	// A newer standalone revision embeds another image: the course keeps
	// serving only the pinned revision's.
	w = rf.api(rf.author, http.MethodPost, fmt.Sprintf("/api/lessons/%d/drafts", l.lessonID), map[string]any{
		"content": mediaDoc("TEST Image de cours v2", false, later), "mediaIds": []int64{later}})
	wantStatus(t, "draft", w, http.StatusCreated, "")
	rev2 := wf.submitRev(rf.author, decodeRevision(t, w))
	wf.approvedRev(rev2)
	wf.publishedRev(rev2)
	wantMedia(t, "standalone v2", rf.get(s, mediaPath(l.publicID, later)))
	wantNoMedia(t, "course, newer image", rf.get(s, cmedia(l, later)))
	wantMedia(t, "course, pinned image", rf.get(s, cmedia(l, img)))

	// Withdrawal of the pinned revision, then suspension: nothing served.
	rf.withdrawRevision(l.lessonID, rev1.ID)
	wantNoMedia(t, "withdrawn pinned", rf.get(s, cmedia(l, img)))
	coursetest.Suspend(t, rf.db, rf.admin.id, c)
	wantNoMedia(t, "suspended", rf.get(s, cmedia(l, img)))
}

func TestCourseBookmarksAndPreferencesArePrivate(t *testing.T) {
	f := newReader(t)
	l1 := f.courseLesson("SIGNET")
	lvl, sec, _, _ := coursetest.Section(t, f.db, f.admin.id, f.subject, "TEST Bac", "TEST Mathématiques")
	otherLvl, otherSec, _, _ := coursetest.Section(t, f.db, f.admin.id, f.subject, "TEST Bac autre", "TEST Sciences")
	spec := f.courseSpec("TEST Cours signet", coursetest.Module{Title: "TEST Module", Steps: []coursetest.Step{step(l1, "TEST Étape signet")}})
	spec.SectionID = sec
	c := coursetest.Publish(t, f.db, spec)
	spec.Title, spec.SectionID = "TEST Cours ailleurs", otherSec
	elsewhere := coursetest.Publish(t, f.db, spec)
	a, b := f.student, f.invitedStudent("test-student-b@example.test", "TEST Élève B")

	// Home without a preference: every readable course, recent material.
	w := f.get(a, "/learn")
	wantPage(t, "home", w, http.StatusOK, "Vous parcourez tout le programme", "TEST Cours signet", "TEST Cours ailleurs",
		"Récemment publié", "Mes cours enregistrés (0)", "Mes signets (0)")

	// Bookmarks: per account; a lesson bookmark is independent.
	wantStatus(t, "bookmark course", f.post(a, courseURL(c)+"/bookmark", nil), http.StatusSeeOther, "")
	wantStatus(t, "bookmark again", f.post(a, courseURL(c)+"/bookmark", nil), http.StatusSeeOther, "")
	wantStatus(t, "bookmark lesson", f.post(a, lessonPath(l1.publicID)+"/bookmark", nil), http.StatusSeeOther, "")
	wantStatus(t, "no csrf", f.page(a, http.MethodPost, courseURL(elsewhere)+"/bookmark", url.Values{}), http.StatusForbidden, "")
	wantStatus(t, "anonymous", f.page(apiUser{}, http.MethodPost, courseURL(elsewhere)+"/bookmark", url.Values{}), http.StatusUnauthorized, "")
	wantPage(t, "outline bookmarked", f.get(a, courseURL(c)), http.StatusOK, "Retirer des signets")
	w = f.get(a, "/learn")
	wantPage(t, "home a", w, http.StatusOK, "Mes cours enregistrés (1)", "Mes signets (1)", courseURL(c)+"/unbookmark")
	w = f.get(b, "/learn")
	wantPage(t, "home b", w, http.StatusOK, "Mes cours enregistrés (0)", "Mes signets (0)")
	wantPage(t, "outline b", f.get(b, courseURL(c)), http.StatusOK, "Enregistrer le cours")
	if n := f.count(`SELECT count(*) FROM bookmarks`); n != 1 {
		t.Fatalf("%d lesson bookmarks", n)
	}

	// Preference: optional, per account, narrows the home to the section's
	// current edition.
	wantPage(t, "preferences", f.get(a, "/learn/preferences"), http.StatusOK, "TEST Bac", "TEST Mathématiques", "Passer et tout parcourir")
	wantStatus(t, "prefer section", f.post(a, "/learn/preferences", url.Values{"choice": {fmt.Sprintf("section:%d:%d", lvl, sec)}}), http.StatusSeeOther, "")
	w = f.get(a, "/learn")
	wantPage(t, "home a pref", w, http.StatusOK, "TEST Bac › TEST Mathématiques", "TEST Cours signet")
	body := w.Body.String()
	if i, j := strings.Index(body, `id="courses-heading"`), strings.Index(body, `id="recent-heading"`); i < 0 || j < i ||
		!strings.Contains(body[i:j], courseURL(c)) || strings.Contains(body[i:j], courseURL(elsewhere)) {
		t.Fatal("home lists a course of another section")
	}
	wantPage(t, "home b no pref", f.get(b, "/learn"), http.StatusOK, "Vous parcourez tout le programme", "TEST Cours ailleurs")
	wantStatus(t, "level of another section", f.post(a, "/learn/preferences",
		url.Values{"choice": {fmt.Sprintf("section:%d:%d", lvl, otherSec)}}), http.StatusUnprocessableEntity, "")
	wantStatus(t, "garbage", f.post(a, "/learn/preferences", url.Values{"choice": {"section:x"}}), http.StatusUnprocessableEntity, "")
	wantStatus(t, "prefer level", f.post(a, "/learn/preferences", url.Values{"choice": {fmt.Sprintf("level:%d", otherLvl)}}), http.StatusSeeOther, "")
	wantPage(t, "home a level", f.get(a, "/learn"), http.StatusOK, "TEST Bac autre</strong>", "TEST Cours ailleurs")
	var stored int
	if err := f.db.QueryRow(context.Background(), `SELECT count(*) FROM student_preferences WHERE user_id = $1 AND level_id = $2 AND section_id IS NULL`,
		a.id, otherLvl).Scan(&stored); err != nil || stored != 1 {
		t.Fatalf("stored preference: %d %v", stored, err)
	}
	wantStatus(t, "skip", f.post(a, "/learn/preferences", url.Values{"skip": {"1"}}), http.StatusSeeOther, "")
	if n := f.count(`SELECT count(*) FROM student_preferences`); n != 0 {
		t.Fatalf("%d preferences after skip", n)
	}

	// Removal; removing again is harmless.
	wantStatus(t, "unbookmark", f.post(a, courseURL(c)+"/unbookmark", url.Values{"return": {"dashboard"}}), http.StatusSeeOther, "")
	wantStatus(t, "unbookmark again", f.post(a, courseURL(c)+"/unbookmark", nil), http.StatusSeeOther, "")
	wantPage(t, "home a after", f.get(a, "/learn"), http.StatusOK, "Mes cours enregistrés (0)", "Mes signets (1)")
}

func TestCourseSearchMatchesOnlyReadableMaterial(t *testing.T) {
	f := newReader(t)
	l1, l2 := f.courseLesson("RECHERCHE"), f.courseLesson("CACHÉE")
	c := coursetest.Publish(t, f.db, f.courseSpec("TEST Cours géométrie",
		coursetest.Module{Title: "TEST Module", Steps: []coursetest.Step{step(l1, "TEST Étape trigonométrie"), step(l2, "TEST Étape xylophone")}}))
	s := f.student
	for _, q := range []string{"geometrie", "Résumé de test TEST Cours", "trigonometrie", "xylophone", "Titre leçon CACHEE"} {
		wantPage(t, "search "+q, f.get(s, "/learn/search?q="+url.QueryEscape(q)), http.StatusOK, "Cours (1)", courseURL(c))
	}
	f.withdrawRevision(l2.lessonID, l2.revisionID)
	for _, q := range []string{"xylophone", "Titre leçon CACHEE"} {
		w := f.get(s, "/learn/search?q="+url.QueryEscape(q))
		wantPage(t, "unreadable "+q, w, http.StatusOK, "Cours (0)")
		wantAbsent(t, "unreadable "+q, w, courseURL(c))
	}
	wantPage(t, "still found", f.get(s, "/learn/search?q=trigonometrie"), http.StatusOK, "Cours (1)")
}

func TestCourseContextReportsNameTheDisplayedRevision(t *testing.T) {
	f := newReader(t)
	l1, l2 := f.courseLesson("RAPPORT"), f.courseLesson("AUTRE")
	spec := f.courseSpec("TEST Cours signalé", coursetest.Module{Title: "TEST Module",
		Steps: []coursetest.Step{step(l1, "TEST Étape signalée"), step(l2, "TEST Étape autre")}})
	c := coursetest.Publish(t, f.db, spec)
	// The standalone lesson moves on; the course still pins revision 1.
	rev2 := f.publish(f.approved(l1.lessonID, readerDoc("TEST Titre leçon RAPPORT v2", "fr", para(text("Corps v2."))), "fr", nil))
	s := f.student
	report := func(u apiUser, p published, courseNumber, number int, msg string) *httptest.ResponseRecorder {
		return f.post(u, courseStepURL(c, p)+"/report", url.Values{"course_revision": {strconv.Itoa(courseNumber)},
			"revision": {strconv.Itoa(number)}, "message": {msg}})
	}
	w := report(s, l1, c.RevisionNumber, 1, "Erreur dans la version du cours.")
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != courseStepURL(c, l1)+"/reported" {
		t.Fatalf("report: %d %s", w.Code, w.Body)
	}
	wantPage(t, "reported", f.get(s, courseStepURL(c, l1)+"/reported"), http.StatusOK, "Signalement envoyé", courseStepURL(c, l1))
	var revID, courseRev *int64
	if err := f.db.QueryRow(context.Background(), `SELECT revision_id, course_revision_id FROM error_reports WHERE lesson_id = $1`, l1.lessonID).
		Scan(&revID, &courseRev); err != nil {
		t.Fatal(err)
	}
	if *revID != l1.revisionID || courseRev == nil || *courseRev != c.RevisionID {
		t.Fatalf("stored revision %d course revision %v, want %d %d", *revID, courseRev, l1.revisionID, c.RevisionID)
	}
	// A standalone report still names the current revision.
	w = f.post(s, lessonPath(l1.publicID)+"/report", url.Values{"revision": {"2"}, "message": {"Erreur seule."}})
	wantStatus(t, "standalone report", w, http.StatusSeeOther, "")
	if n := f.count(`SELECT count(*) FROM error_reports WHERE revision_id = $1 AND course_revision_id IS NULL`, rev2.revisionID); n != 1 {
		t.Fatalf("%d standalone reports", n)
	}

	before := f.count(`SELECT count(*) FROM error_reports`)
	// Stale numbers: refused, text kept, nothing stored.
	w = report(s, l1, c.RevisionNumber, 2, "Mauvaise version.")
	wantPage(t, "stale lesson revision", w, http.StatusConflict, "a été modifié depuis son affichage", "Mauvaise version.")
	w = report(s, l1, c.RevisionNumber+1, 1, "Mauvaise version du cours.")
	wantPage(t, "unknown course revision", w, http.StatusConflict, "a été modifié depuis son affichage")
	spec.Title = "TEST Cours signalé v2"
	c2 := coursetest.Republish(t, f.db, c, spec)
	w = report(s, l1, c.RevisionNumber, 1, "Ancienne version du cours.")
	wantPage(t, "stale course revision", w, http.StatusConflict, "a été modifié depuis son affichage")
	w = report(s, l1, 0, 1, "Numéro invalide.")
	wantPage(t, "invalid number", w, http.StatusBadRequest, "Requête invalide")
	w = report(s, l1, c2.RevisionNumber, 1, " ")
	wantPage(t, "blank", w, http.StatusUnprocessableEntity, "Décrivez l’erreur")
	// An unavailable step cannot be reported.
	f.withdrawRevision(l2.lessonID, l2.revisionID)
	w = report(s, l2, c2.RevisionNumber, 1, "Étape retirée.")
	wantPage(t, "unavailable step", w, http.StatusNotFound, "Cette leçon est temporairement indisponible")
	wantAbsent(t, "unavailable step", w, "Corps de test AUTRE")
	coursetest.Suspend(t, f.db, f.admin.id, c2)
	w = report(s, l1, c2.RevisionNumber, 1, "Cours suspendu.")
	wantPage(t, "suspended", w, http.StatusNotFound, "Cours indisponible")
	if after := f.count(`SELECT count(*) FROM error_reports`); after != before {
		t.Fatalf("refused reports stored: %d -> %d", before, after)
	}
}

// Course routes refuse anonymous and CSRF-less posts, keep every response
// private, and never accept an oversized report body.
func TestCourseRoutesRefuseForgedPostsAndStayPrivate(t *testing.T) {
	rf := newReader(t)
	wf := withdrawFixture{reviewFixture: reviewFixture{studioFixture: studioFixture{env: rf.env, subject: rf.subject, author: rf.author},
		reviewer: rf.reviewer, publisher: rf.publisher}, admin: rf.admin}
	img := rf.uploadAs(rf.author, 131)
	w := rf.createWithMedia(rf.author, rf.subject, mediaDoc("TEST Image privée", false, img), []int64{img})
	wantStatus(t, "create", w, http.StatusCreated, "")
	rev := wf.submitRev(rf.author, decodeRevision(t, w))
	wf.approvedRev(rev)
	wf.publishedRev(rev)
	l := published{lessonID: rev.LessonID, revisionID: rev.ID, publicID: rf.publicID(rev.LessonID)}
	c := coursetest.Publish(t, rf.db, rf.courseSpec("TEST Cours privé",
		coursetest.Module{Title: "TEST Module", Steps: []coursetest.Step{step(l, "TEST Étape privée")}}))
	s := rf.student
	wantStatus(t, "bookmark", rf.post(s, courseURL(c)+"/bookmark", nil), http.StatusSeeOther, "")
	report := url.Values{"course_revision": {strconv.Itoa(c.RevisionNumber)}, "revision": {"1"}, "message": {"Erreur."}}
	posts := map[string]url.Values{
		"/learn/preferences":            {"skip": {"1"}},
		courseURL(c) + "/unbookmark":    {},
		courseStepURL(c, l) + "/report": report,
	}
	for path, form := range posts {
		// rf.page skips the reader header check; it is asserted below.
		w := rf.page(apiUser{}, http.MethodPost, path, form)
		wantPage(t, "anonymous "+path, w, http.StatusUnauthorized, "Connexion requise")
		checkReaderHeaders(t, "anonymous "+path, w)
		w = rf.page(s, http.MethodPost, path, form)
		wantPage(t, "no csrf "+path, w, http.StatusForbidden, "Requête refusée")
		checkReaderHeaders(t, "no csrf "+path, w)
		bad := url.Values{}
		for k, v := range form {
			bad[k] = v
		}
		bad.Set("csrf_token", "wrong-token")
		wantPage(t, "wrong csrf "+path, rf.post(s, path, bad), http.StatusForbidden, "Requête refusée")
	}
	if n := rf.count(`SELECT count(*) FROM course_bookmarks`); n != 1 {
		t.Fatalf("%d course bookmarks after refused posts", n)
	}
	if n := rf.count(`SELECT count(*) FROM error_reports`); n != 0 {
		t.Fatalf("%d reports after refused posts", n)
	}

	// Successful pages and media are private, no-store (rf.get checks
	// Cache-Control and Vary on every page).
	wantPage(t, "course", rf.get(s, courseURL(c)), http.StatusOK, "TEST Cours privé")
	wantPage(t, "step", rf.get(s, courseStepURL(c, l)), http.StatusOK, "TEST Image privée")
	wantMedia(t, "course media", rf.get(s, fmt.Sprintf("%s/media/%d", courseStepURL(c, l), img)))

	// An oversized report body is refused, read-only: nothing is stored or
	// throttled, and the text is not kept.
	big := url.Values{"csrf_token": {s.csrf}, "course_revision": {strconv.Itoa(c.RevisionNumber)}, "revision": {"1"},
		"message": {strings.Repeat("é", 40000)}}
	w = rf.post(s, courseStepURL(c, l)+"/report", big)
	wantPage(t, "oversized", w, http.StatusRequestEntityTooLarge, "Le texte est trop long")
	if strings.Contains(w.Body.String(), strings.Repeat("é", 100)) {
		t.Fatal("oversized text echoed back")
	}
	if n := rf.count(`SELECT count(*) FROM error_reports`); n != 0 {
		t.Fatalf("%d reports after an oversized body", n)
	}
	wantStatus(t, "report after", rf.post(s, courseStepURL(c, l)+"/report", report), http.StatusSeeOther, "")
}
