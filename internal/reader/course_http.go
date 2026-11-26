package reader

// Course reading routes (LMS-B08, docs/API.md "Course reading"). Every page
// needs a session and is private, no-store (headers); state changes need the
// session's CSRF token. A course that is not readable, an unknown course and
// a lesson that is not a step of its published revision all get the same
// generic notice.

import (
	"errors"
	"html/template"
	"net/http"
	"strconv"
	"strings"

	"github.com/hadden-me/learnoo/internal/auth"
	"github.com/hadden-me/learnoo/internal/rendering"
	"github.com/hadden-me/learnoo/internal/reports"
)

// coursePages are the templates of this file, parsed by New.
var coursePages = []string{"course", "course_step", "course_reported", "preferences"}

func (h *Handlers) parseCoursePages(base *template.Template, pages map[string]*template.Template) {
	for _, name := range coursePages {
		pages[name] = template.Must(template.Must(base.Clone()).ParseFS(files, "templates/"+name+".html"))
	}
}

// registerCourses mounts the course reading and preference routes.
func (h *Handlers) registerCourses(page func(string, http.HandlerFunc)) {
	page("GET /learn/preferences", h.requireUser(h.preferencesPage))
	page("POST /learn/preferences", h.requireUser(h.requireCSRF(h.savePreferences)))
	page("GET /learn/courses/{publicId}", h.requireUser(h.course))
	page("POST /learn/courses/{publicId}/bookmark", h.requireUser(h.requireCSRF(h.addCourseBookmark)))
	page("POST /learn/courses/{publicId}/unbookmark", h.requireUser(h.requireCSRF(h.removeCourseBookmark)))
	page("GET /learn/courses/{publicId}/lessons/{lessonId}", h.requireUser(h.courseStep))
	page("POST /learn/courses/{publicId}/lessons/{lessonId}/report", h.requireUser(h.requireCSRFBody(maxReportFormBytes, h.courseReport, h.courseReportTooLarge)))
	page("GET /learn/courses/{publicId}/lessons/{lessonId}/reported", h.requireUser(h.courseReported))
	page("GET /learn/courses/{publicId}/lessons/{lessonId}/media/{mediaId}", h.courseMedia)
}

func coursePath(publicID string) string { return "/learn/courses/" + publicID }

func courseStepPath(coursePublicID, lessonPublicID string) string {
	return coursePath(coursePublicID) + "/lessons/" + lessonPublicID
}

// CourseMediaURL is the reader's MediaURL for a course step: embedded
// uploaded images are fetched through courseMedia, scoped to the course and
// the step's pinned revision.
func CourseMediaURL(coursePublicID, lessonPublicID string) rendering.MediaURL {
	return rendering.MediaPrefix(courseStepPath(coursePublicID, lessonPublicID) + "/media/")
}

// courseUnavailable is the generic notice for a course that cannot be read:
// unknown, never published, suspended, archived or withdrawn alike, and for
// a lesson that is not a step of it. It keeps navigation back (error.html).
func (h *Handlers) courseUnavailable(w http.ResponseWriter, r *http.Request) {
	h.errorPage(w, r, http.StatusNotFound, "Cours indisponible",
		"Ce cours n’existe pas ou n’est pas disponible pour le moment. Vos signets sont conservés.")
}

type courseData struct {
	Course
	First *OutlineStep
}

func (h *Handlers) course(w http.ResponseWriter, r *http.Request) {
	c, err := h.Store.Course(r.Context(), userID(r.Context()), r.PathValue("publicId"))
	if errors.Is(err, ErrNotFound) {
		h.courseUnavailable(w, r)
		return
	}
	if err != nil {
		h.internal(w, r, "course", err)
		return
	}
	d := courseData{Course: c}
	if steps := c.Steps(); len(steps) > 0 {
		d.First = &steps[0]
	}
	v := view{Title: c.Title, Data: d}
	if c.Language != "fr" {
		v.TitleLang = c.Language
	}
	h.render(w, r, http.StatusOK, "course", v)
}

type courseStepData struct {
	CourseStep
	Parts  rendering.LessonParts
	Report reportForm
}

func (h *Handlers) courseStep(w http.ResponseWriter, r *http.Request) {
	h.showCourseStep(w, r, http.StatusOK, reportForm{})
}

// showCourseStep renders a course step: the pinned revision through the
// canonical renderer when the step is readable, otherwise only the
// unavailable-step notice, the outline and previous/next.
func (h *Handlers) showCourseStep(w http.ResponseWriter, r *http.Request, status int, form reportForm) {
	courseID, lessonID := r.PathValue("publicId"), r.PathValue("lessonId")
	cs, err := h.Store.CourseStep(r.Context(), userID(r.Context()), courseID, lessonID)
	if errors.Is(err, ErrNotFound) {
		h.courseUnavailable(w, r)
		return
	}
	if err != nil {
		h.internal(w, r, "course step", err)
		return
	}
	d := courseStepData{CourseStep: cs}
	v := view{Title: cs.Step.Title + " · " + cs.Course.Title}
	if cs.Course.Language != "fr" {
		v.TitleLang = cs.Course.Language
	}
	if cs.Step.Readable {
		parts, err := rendering.RenderLesson(cs.Document, CourseMediaURL(courseID, lessonID))
		if err != nil {
			h.internal(w, r, "render course step", err)
			return
		}
		d.Parts = parts
		form.Max = reports.MaxMessage
		d.Report = form
		v.Lesson = true
		w.Header().Set("Content-Security-Policy", cspLesson)
	}
	v.Data = d
	h.render(w, r, status, "course_step", v)
}

// courseMedia serves an uploaded image embedded in the pinned revision of a
// readable step (Store.CourseMedia) to any signed-in account. Every other
// request gets the JSON 404 of lessonMedia.
func (h *Handlers) courseMedia(w http.ResponseWriter, r *http.Request) {
	courseID, lessonID, raw := r.PathValue("publicId"), r.PathValue("lessonId"), r.PathValue("mediaId")
	if _, ok := auth.SessionFrom(r.Context()); !ok || !ValidPublicID(courseID) || !ValidPublicID(lessonID) || !mediaIDPattern.MatchString(raw) {
		staticNotFound(w)
		return
	}
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		staticNotFound(w)
		return
	}
	key, err := h.Store.CourseMedia(r.Context(), courseID, lessonID, id)
	if errors.Is(err, ErrNotFound) {
		staticNotFound(w)
		return
	}
	if err != nil {
		h.Log.Error("reader failure", "op", "course media", "err", err)
		auth.WriteError(w, http.StatusInternalServerError, "internal", "Something went wrong. Try again later.")
		return
	}
	if err := h.Media.Serve(w, r, key); err != nil {
		h.Log.Error("reader media unavailable", "id", id, "err", err)
		staticNotFound(w)
	}
}

// courseReport files an error report on the pinned revision of a course
// step, as displayed: the form carries the course revision number and the
// lesson revision number. Throttles and text rules are those of report.
func (h *Handlers) courseReport(w http.ResponseWriter, r *http.Request) {
	courseID, lessonID := r.PathValue("publicId"), r.PathValue("lessonId")
	if !ValidPublicID(courseID) || !ValidPublicID(lessonID) {
		h.courseUnavailable(w, r)
		return
	}
	err := h.Service.ThrottleErrorReport(r.Context(), userID(r.Context()), auth.ClientAddr(r))
	var limited *auth.RateLimitedError
	switch {
	case errors.As(err, &limited):
		h.rateLimited(w, r, limited)
		return
	case err != nil:
		h.internal(w, r, "throttle error report", err)
		return
	}
	raw, rawCourse := r.PostForm.Get("revision"), r.PostForm.Get("course_revision")
	if len(r.PostForm["revision"]) != 1 || len(r.PostForm["course_revision"]) != 1 || len(r.PostForm["message"]) != 1 ||
		!revisionField.MatchString(raw) || !revisionField.MatchString(rawCourse) {
		h.errorPage(w, r, http.StatusBadRequest, "Requête invalide", "Le formulaire est incomplet. Rechargez la leçon et réessayez.")
		return
	}
	number, _ := strconv.Atoi(raw)
	courseNumber, _ := strconv.Atoi(rawCourse)
	message := r.PostForm.Get("message")
	form := reportForm{Open: true, Message: truncateRunes(message, reports.MaxMessage+100)}
	_, err = h.Reports.CreateInCourse(r.Context(), userID(r.Context()), courseID, courseNumber, lessonID, number, message)
	switch {
	case err == nil:
		http.Redirect(w, r, courseStepPath(courseID, lessonID)+"/reported", http.StatusSeeOther)
	case errors.Is(err, reports.ErrTextRequired):
		form.Error = "Décrivez l’erreur avant d’envoyer le signalement."
		h.showCourseStep(w, r, http.StatusUnprocessableEntity, form)
	case errors.Is(err, reports.ErrInvalidText):
		form.Error = "Le signalement doit contenir au plus " + strconv.Itoa(reports.MaxMessage) +
			" caractères, sans caractères de contrôle ni de mise en forme de direction."
		h.showCourseStep(w, r, http.StatusUnprocessableEntity, form)
	case errors.Is(err, reports.ErrNotFound):
		// The course or the step is no longer readable: show the step page,
		// which explains which (or the generic notice).
		h.showCourseStep(w, r, http.StatusNotFound, reportForm{})
	case errors.Is(err, reports.ErrRevisionChanged):
		form.Error = "Ce cours ou cette leçon a été modifié depuis son affichage. Relisez-la, puis renvoyez votre signalement s’il s’applique encore."
		h.showCourseStep(w, r, http.StatusConflict, form)
	case errors.Is(err, reports.ErrRejected), isRetryable(err):
		form.Error = "Le signalement n’a pas pu être enregistré. Réessayez."
		h.showCourseStep(w, r, http.StatusConflict, form)
	default:
		h.internal(w, r, "course error report", err)
	}
}

// courseReportTooLarge answers an oversized course report form, as
// reportTooLarge does; nothing is stored or throttled.
func (h *Handlers) courseReportTooLarge(w http.ResponseWriter, r *http.Request) {
	if !ValidPublicID(r.PathValue("publicId")) || !ValidPublicID(r.PathValue("lessonId")) {
		h.courseUnavailable(w, r)
		return
	}
	h.showCourseStep(w, r, http.StatusRequestEntityTooLarge, reportForm{Open: true,
		Error: "Le texte est trop long (au plus " + strconv.Itoa(reports.MaxMessage) + " caractères). Il n’a pas pu être conservé : saisissez une version plus courte."})
}

// courseReported confirms a filed course-context report. It shows no
// course or lesson data.
func (h *Handlers) courseReported(w http.ResponseWriter, r *http.Request) {
	courseID, lessonID := r.PathValue("publicId"), r.PathValue("lessonId")
	if !ValidPublicID(courseID) || !ValidPublicID(lessonID) {
		h.courseUnavailable(w, r)
		return
	}
	h.render(w, r, http.StatusOK, "course_reported", view{Title: "Signalement envoyé", Data: courseStepPath(courseID, lessonID)})
}

// courseBookmarkReturn is where a course bookmark form returns: the home or
// the course outline.
func courseBookmarkReturn(r *http.Request, publicID string) string {
	if r.PostForm.Get("return") == "dashboard" {
		return "/learn"
	}
	return coursePath(publicID)
}

func (h *Handlers) addCourseBookmark(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("publicId")
	err := h.Store.AddCourseBookmark(r.Context(), userID(r.Context()), id)
	switch {
	case errors.Is(err, ErrNotFound):
		h.courseUnavailable(w, r)
	case errors.Is(err, ErrTooManyBookmarks):
		h.errorPage(w, r, http.StatusConflict, "Trop de signets",
			"Vous avez atteint la limite de "+strconv.Itoa(MaxBookmarks)+" cours enregistrés. Retirez-en avant d’en ajouter.")
	case err != nil:
		h.internal(w, r, "add course bookmark", err)
	default:
		http.Redirect(w, r, courseBookmarkReturn(r, id), http.StatusSeeOther)
	}
}

func (h *Handlers) removeCourseBookmark(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("publicId")
	err := h.Store.RemoveCourseBookmark(r.Context(), userID(r.Context()), id)
	switch {
	case errors.Is(err, ErrNotFound):
		h.courseUnavailable(w, r)
	case err != nil:
		h.internal(w, r, "remove course bookmark", err)
	default:
		http.Redirect(w, r, courseBookmarkReturn(r, id), http.StatusSeeOther)
	}
}

// ---- level and section preference ----

type preferencesData struct {
	Preference Preference
	Levels     []LevelOption
	Error      string
}

// preferenceValue encodes a choice in the form's select: "level:<id>" or
// "section:<level>:<id>"; "" clears the preference.
func preferenceValue(levelID, sectionID int64) string {
	if sectionID == 0 {
		return "level:" + strconv.FormatInt(levelID, 10)
	}
	return "section:" + strconv.FormatInt(levelID, 10) + ":" + strconv.FormatInt(sectionID, 10)
}

func parsePreference(v string) (levelID, sectionID int64, ok bool) {
	parts := strings.Split(v, ":")
	num := func(s string) (int64, bool) {
		if !mediaIDPattern.MatchString(s) {
			return 0, false
		}
		n, err := strconv.ParseInt(s, 10, 64)
		return n, err == nil
	}
	switch {
	case len(parts) == 2 && parts[0] == "level":
		l, ok := num(parts[1])
		return l, 0, ok
	case len(parts) == 3 && parts[0] == "section":
		l, ok1 := num(parts[1])
		s, ok2 := num(parts[2])
		return l, s, ok1 && ok2
	}
	return 0, 0, false
}

func (h *Handlers) preferencesPage(w http.ResponseWriter, r *http.Request) {
	h.showPreferences(w, r, http.StatusOK, "")
}

func (h *Handlers) showPreferences(w http.ResponseWriter, r *http.Request, status int, msg string) {
	p, err := h.Store.Preference(r.Context(), userID(r.Context()))
	if err != nil {
		h.internal(w, r, "preference", err)
		return
	}
	levels, err := h.Store.LevelOptions(r.Context())
	if err != nil {
		h.internal(w, r, "level options", err)
		return
	}
	h.render(w, r, status, "preferences", view{Title: "Mon niveau et ma section",
		Data: preferencesData{Preference: p, Levels: levels, Error: msg}})
}

// savePreferences stores or clears the level/section preference. The
// choice is optional: "skip" (or an empty choice) clears it.
func (h *Handlers) savePreferences(w http.ResponseWriter, r *http.Request) {
	uid := userID(r.Context())
	choice := r.PostForm.Get("choice")
	if len(r.PostForm["choice"]) > 1 {
		h.showPreferences(w, r, http.StatusBadRequest, "Choisissez une seule option.")
		return
	}
	if r.PostForm.Get("skip") != "" || choice == "" {
		if err := h.Store.ClearPreference(r.Context(), uid); err != nil {
			h.internal(w, r, "clear preference", err)
			return
		}
		http.Redirect(w, r, "/learn", http.StatusSeeOther)
		return
	}
	levelID, sectionID, ok := parsePreference(choice)
	if !ok {
		h.showPreferences(w, r, http.StatusUnprocessableEntity, "Ce choix n’est pas proposé. Choisissez dans la liste.")
		return
	}
	err := h.Store.SetPreference(r.Context(), uid, levelID, sectionID)
	switch {
	case errors.Is(err, ErrInvalidPreference):
		h.showPreferences(w, r, http.StatusUnprocessableEntity, "Ce choix n’est plus proposé. Choisissez dans la liste.")
	case err != nil:
		h.internal(w, r, "save preference", err)
	default:
		http.Redirect(w, r, "/learn", http.StatusSeeOther)
	}
}

// ---- student home ----

// homeData is the course part of the student home (/learn).
type homeData struct {
	Preference      Preference
	Courses         []CourseSummary
	CourseBookmarks []CourseBookmark
	Recent          []RecentItem
}

// home loads the course part of the student home for the signed-in user.
func (h *Handlers) home(r *http.Request) (homeData, []Subject, error) {
	ctx, uid := r.Context(), userID(r.Context())
	var d homeData
	var err error
	if d.Preference, err = h.Store.Preference(ctx, uid); err != nil {
		return d, nil, err
	}
	if d.Courses, err = h.Store.HomeCourses(ctx, d.Preference); err != nil {
		return d, nil, err
	}
	if d.CourseBookmarks, err = h.Store.CourseBookmarks(ctx, uid); err != nil {
		return d, nil, err
	}
	if d.Recent, err = h.Store.Recent(ctx); err != nil {
		return d, nil, err
	}
	subjects, err := h.Store.HomeSubjects(ctx, d.Preference)
	return d, subjects, err
}
