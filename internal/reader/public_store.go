package reader

// Public discovery (LMS-B07): the homepage, the catalogue and course
// overviews. Everything here is readable without an account, so every query
// goes through the reader views (reader_published_lessons, 0008;
// reader_published_courses and reader_course_steps, 0015) and selects only
// public metadata: titles, summaries, course-authored outline titles and
// catalogue names. Lesson bodies, lesson titles of unreadable course steps,
// solutions and media never leave the database through this file.

import (
	"context"
	"errors"
	"fmt"

	"github.com/hadden-me/learnoo/internal/catalogue"
	"github.com/jackc/pgx/v5"
)

// Edition is a curriculum edition shown publicly: never a draft.
type Edition struct {
	ID      int64
	Code    string
	LabelFr string
	State   string // current, superseded or retired
}

// Current reports whether the edition is the section's current one.
func (e Edition) Current() bool { return e.State == "current" }

// StateLabel is the edition's state in the interface.
func (e Edition) StateLabel() string { return editionStateLabel(e.State) }

func editionStateLabel(state string) string {
	switch state {
	case "current":
		return "en vigueur"
	case "superseded":
		return "remplacée"
	case "retired":
		return "retirée"
	}
	return ""
}

// SubjectAvailability is a subject of an edition with what is readable in
// it: published courses placed in the edition's assignment and published
// lessons whose chapter the assignment lists. Counts are real.
type SubjectAvailability struct {
	Code    string
	Names   catalogue.Names
	Courses int
	Lessons int
}

// Label is the subject's name in the French interface.
func (s SubjectAvailability) Label() catalogue.Label { return s.Names.Label(s.Code, "fr") }

// Empty reports whether nothing is readable in the subject.
func (s SubjectAvailability) Empty() bool { return s.Courses == 0 && s.Lessons == 0 }

// PublicSection is an academic section in the catalogue with its current
// edition (nil when none) and that edition's subjects.
type PublicSection struct {
	LevelCode  string
	LevelName  string
	Code       string
	Name       string
	Current    *Edition
	HasEarlier bool
	Subjects   []SubjectAvailability
}

// Path is the section's catalogue URL (current edition).
func (s PublicSection) Path() string { return "/catalogue/" + s.LevelCode + "/" + s.Code }

// Available lists the subjects with readable content.
func (s PublicSection) Available() []SubjectAvailability {
	var out []SubjectAvailability
	for _, sj := range s.Subjects {
		if !sj.Empty() {
			out = append(out, sj)
		}
	}
	return out
}

// PublicLevel is an education level with its non-retired sections.
type PublicLevel struct {
	Code     string
	Name     string
	Sections []PublicSection
}

// PublicLesson is a published lesson listed publicly: its title and
// language only.
type PublicLesson struct {
	PublicID string
	Title    string
	Language string
}

// UnplacedSubject groups published lessons of one subject whose chapter is
// in no public edition's assignment, or that have no chapter.
type UnplacedSubject struct {
	Code    string
	Names   catalogue.Names
	Lessons []PublicLesson
}

// Label is the subject's name in the French interface.
func (s UnplacedSubject) Label() catalogue.Label { return s.Names.Label(s.Code, "fr") }

// Curriculum lists non-retired levels and sections, by display order then
// code, each section with its current edition and that edition's subjects.
func (s *Store) Curriculum(ctx context.Context) ([]PublicLevel, error) {
	rows, err := s.db.Query(ctx, `
		SELECT lv.code, lv.name_fr, sc.code, sc.name_fr, e.id, e.code, e.label_fr,
		       EXISTS (SELECT 1 FROM curriculum_editions o
		                WHERE o.section_id = sc.id AND o.state IN ('superseded', 'retired'))
		  FROM education_levels lv
		  JOIN academic_sections sc ON sc.level_id = lv.id
		  LEFT JOIN curriculum_editions e ON e.section_id = sc.id AND e.state = 'current'
		 WHERE lv.retired_at IS NULL AND sc.retired_at IS NULL
		 ORDER BY lv.display_order, lv.code, sc.display_order, sc.code
		 LIMIT $1`, maxListed)
	if err != nil {
		return nil, fmt.Errorf("curriculum: %w", err)
	}
	type row struct {
		sec       PublicSection
		editionID *int64
		code      *string
		label     *string
	}
	list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (row, error) {
		var x row
		err := r.Scan(&x.sec.LevelCode, &x.sec.LevelName, &x.sec.Code, &x.sec.Name, &x.editionID, &x.code, &x.label, &x.sec.HasEarlier)
		return x, err
	})
	if err != nil {
		return nil, fmt.Errorf("curriculum: %w", err)
	}
	var ids []int64
	for i := range list {
		if list[i].editionID != nil {
			list[i].sec.Current = &Edition{ID: *list[i].editionID, Code: *list[i].code, LabelFr: *list[i].label, State: "current"}
			ids = append(ids, *list[i].editionID)
		}
	}
	subjects, err := s.editionSubjects(ctx, ids)
	if err != nil {
		return nil, err
	}
	var levels []PublicLevel
	for _, x := range list {
		if x.sec.Current != nil {
			x.sec.Subjects = subjects[x.sec.Current.ID]
		}
		if n := len(levels); n == 0 || levels[n-1].Code != x.sec.LevelCode {
			levels = append(levels, PublicLevel{Code: x.sec.LevelCode, Name: x.sec.LevelName})
		}
		levels[len(levels)-1].Sections = append(levels[len(levels)-1].Sections, x.sec)
	}
	return levels, nil
}

// editionSubjects returns, per edition, its assigned subjects (by the
// catalogue's display order, then code) with their readable content.
func (s *Store) editionSubjects(ctx context.Context, editionIDs []int64) (map[int64][]SubjectAvailability, error) {
	out := map[int64][]SubjectAvailability{}
	if len(editionIDs) == 0 {
		return out, nil
	}
	rows, err := s.db.Query(ctx, `
		SELECT a.edition_id, sj.code, sj.name_fr, sj.name_ar,
		       (SELECT count(DISTINCT pc.course_id)
		          FROM course_revision_assignments cra
		          JOIN reader_published_courses pc ON pc.revision_id = cra.course_revision_id
		         WHERE cra.assignment_id = a.id),
		       (SELECT count(DISTINCT p.lesson_id)
		          FROM curriculum_assignment_chapters cac
		          JOIN reader_published_lessons p ON p.chapter_id = cac.chapter_id
		         WHERE cac.assignment_id = a.id)
		  FROM curriculum_assignments a
		  JOIN subjects sj ON sj.id = a.subject_id
		 WHERE a.edition_id = ANY($1)
		 ORDER BY sj.sort_order, sj.code
		 LIMIT $2`, editionIDs, maxListed)
	if err != nil {
		return nil, fmt.Errorf("edition subjects: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var sj SubjectAvailability
		if err := rows.Scan(&id, &sj.Code, &sj.Names.Fr, &sj.Names.Ar, &sj.Courses, &sj.Lessons); err != nil {
			return nil, fmt.Errorf("edition subjects: %w", err)
		}
		out[id] = append(out[id], sj)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("edition subjects: %w", err)
	}
	return out, nil
}

// unplacedCondition selects published lessons (alias p) whose chapter is in
// no assignment of a public (non-draft) edition, or that have no chapter.
const unplacedCondition = `(p.chapter_id IS NULL OR NOT EXISTS (
		SELECT 1 FROM curriculum_assignment_chapters cac
		  JOIN curriculum_assignments a ON a.id = cac.assignment_id
		  JOIN curriculum_editions e ON e.id = a.edition_id
		 WHERE cac.chapter_id = p.chapter_id AND e.state <> 'draft'))`

// Unplaced lists published lessons not yet placed in a curriculum edition,
// grouped by subject (display order, then code), lessons by folded title.
func (s *Store) Unplaced(ctx context.Context) ([]UnplacedSubject, error) {
	rows, err := s.db.Query(ctx, `
		SELECT sj.code, sj.name_fr, sj.name_ar, p.public_id::text, p.title, p.language
		  FROM reader_published_lessons p
		  JOIN subjects sj ON sj.id = p.subject_id
		 WHERE `+unplacedCondition+`
		 ORDER BY sj.sort_order, sj.code, reader_fold(p.title), p.public_id
		 LIMIT $1`, maxListed)
	if err != nil {
		return nil, fmt.Errorf("unplaced lessons: %w", err)
	}
	defer rows.Close()
	var out []UnplacedSubject
	for rows.Next() {
		var sj UnplacedSubject
		var l PublicLesson
		if err := rows.Scan(&sj.Code, &sj.Names.Fr, &sj.Names.Ar, &l.PublicID, &l.Title, &l.Language); err != nil {
			return nil, fmt.Errorf("unplaced lessons: %w", err)
		}
		if n := len(out); n == 0 || out[n-1].Code != sj.Code {
			out = append(out, sj)
		}
		out[len(out)-1].Lessons = append(out[len(out)-1].Lessons, l)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("unplaced lessons: %w", err)
	}
	return out, nil
}

// SectionPage is a section in one edition: the current one by default, or
// an earlier one named explicitly.
type SectionPage struct {
	LevelCode string
	LevelName string
	Code      string
	Name      string
	Retired   bool
	// Edition is the edition shown; nil when the section has no current
	// edition and none was named.
	Edition  *Edition
	Current  *Edition
	Earlier  []Edition
	Subjects []SubjectAvailability
}

// Path is the section's catalogue URL in its current edition.
func (p SectionPage) Path() string { return "/catalogue/" + p.LevelCode + "/" + p.Code }

// EditionQuery is "?edition=<code>" for an earlier edition shown, "" for
// the current one. Catalogue links within the page carry it, so an earlier
// edition is never left silently.
func (p SectionPage) EditionQuery() string {
	if p.Edition == nil || p.Edition.Current() {
		return ""
	}
	return "?edition=" + p.Edition.Code
}

// codePattern is the format of level, section, edition and subject codes.
var codePattern = subjectCodePattern

// Section loads a section by level and section code in the edition named by
// editionCode ("" for the current one). Unknown codes, malformed codes and
// draft editions are ErrNotFound. Retired levels and sections still
// resolve, so that the labelled path of their editions keeps working.
func (s *Store) Section(ctx context.Context, levelCode, sectionCode, editionCode string) (SectionPage, error) {
	p, _, err := s.section(ctx, s.db, levelCode, sectionCode, editionCode)
	if err != nil {
		return SectionPage{}, err
	}
	if p.Edition != nil {
		subjects, err := s.editionSubjects(ctx, []int64{p.Edition.ID})
		if err != nil {
			return SectionPage{}, err
		}
		p.Subjects = subjects[p.Edition.ID]
	}
	return p, nil
}

type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func (s *Store) section(ctx context.Context, q querier, levelCode, sectionCode, editionCode string) (SectionPage, int64, error) {
	if !codePattern.MatchString(levelCode) || !codePattern.MatchString(sectionCode) ||
		(editionCode != "" && !codePattern.MatchString(editionCode)) {
		return SectionPage{}, 0, ErrNotFound
	}
	var p SectionPage
	var sectionID int64
	err := q.QueryRow(ctx, `
		SELECT sc.id, lv.code, lv.name_fr, sc.code, sc.name_fr, lv.retired_at IS NOT NULL OR sc.retired_at IS NOT NULL
		  FROM education_levels lv JOIN academic_sections sc ON sc.level_id = lv.id
		 WHERE lv.code = $1 AND sc.code = $2`, levelCode, sectionCode).Scan(
		&sectionID, &p.LevelCode, &p.LevelName, &p.Code, &p.Name, &p.Retired)
	if errors.Is(err, pgx.ErrNoRows) {
		return SectionPage{}, 0, ErrNotFound
	}
	if err != nil {
		return SectionPage{}, 0, fmt.Errorf("section: %w", err)
	}
	rows, err := q.Query(ctx, `
		SELECT id, code, label_fr, state FROM curriculum_editions
		 WHERE section_id = $1 AND state <> 'draft'
		 ORDER BY current_since DESC, id DESC
		 LIMIT $2`, sectionID, maxListed)
	if err != nil {
		return SectionPage{}, 0, fmt.Errorf("editions: %w", err)
	}
	editions, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Edition, error) {
		var e Edition
		err := r.Scan(&e.ID, &e.Code, &e.LabelFr, &e.State)
		return e, err
	})
	if err != nil {
		return SectionPage{}, 0, fmt.Errorf("editions: %w", err)
	}
	for i := range editions {
		e := editions[i]
		if e.Current() {
			p.Current = &e
		} else {
			p.Earlier = append(p.Earlier, e)
		}
		if e.Code == editionCode {
			p.Edition = &e
		}
	}
	switch {
	case editionCode == "":
		p.Edition = p.Current
	case p.Edition == nil:
		return SectionPage{}, 0, ErrNotFound
	}
	return p, sectionID, nil
}

// SubjectPage is a subject of a section's edition: its published courses
// placed in that edition, its published lessons placed there (by the
// assignment's chapter order) and its lessons not yet placed in any edition.
type SubjectPage struct {
	Section  SectionPage
	Code     string
	Names    catalogue.Names
	Courses  []PublicCourseSummary
	Chapters []PublicChapter
	Unplaced []PublicLesson
}

// Label is the subject's name in the French interface.
func (p SubjectPage) Label() catalogue.Label { return p.Names.Label(p.Code, "fr") }

// PublicCourseSummary is a readable course in a list.
type PublicCourseSummary struct {
	PublicID string
	Title    string
	Summary  string
	Language string
}

// PublicChapter is an official chapter of an assignment with the published
// lessons placed in it.
type PublicChapter struct {
	Code    string
	Names   catalogue.Names
	Lessons []PublicLesson
}

// Label is the chapter's name in the French interface.
func (c PublicChapter) Label() catalogue.Label { return c.Names.Label(c.Code, "fr") }

// Subject loads a subject's page in the edition named by editionCode (""
// for the current one). A subject the edition does not assign, a section
// without the edition and malformed codes are ErrNotFound. The page is read
// in one repeatable-read snapshot.
func (s *Store) Subject(ctx context.Context, levelCode, sectionCode, subjectCode, editionCode string) (SubjectPage, error) {
	if !codePattern.MatchString(subjectCode) {
		return SubjectPage{}, ErrNotFound
	}
	tx, err := s.db.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return SubjectPage{}, fmt.Errorf("subject page: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	sec, _, err := s.section(ctx, tx, levelCode, sectionCode, editionCode)
	if err != nil {
		return SubjectPage{}, err
	}
	if sec.Edition == nil {
		return SubjectPage{}, ErrNotFound
	}
	p := SubjectPage{Section: sec}
	var assignmentID, subjectID int64
	err = tx.QueryRow(ctx, `
		SELECT a.id, sj.id, sj.code, sj.name_fr, sj.name_ar
		  FROM curriculum_assignments a JOIN subjects sj ON sj.id = a.subject_id
		 WHERE a.edition_id = $1 AND sj.code = $2`, sec.Edition.ID, subjectCode).Scan(
		&assignmentID, &subjectID, &p.Code, &p.Names.Fr, &p.Names.Ar)
	if errors.Is(err, pgx.ErrNoRows) {
		return SubjectPage{}, ErrNotFound
	}
	if err != nil {
		return SubjectPage{}, fmt.Errorf("subject page: %w", err)
	}

	rows, err := tx.Query(ctx, `
		SELECT pc.public_id::text, pc.title, pc.summary, pc.language
		  FROM reader_published_courses pc
		 WHERE EXISTS (SELECT 1 FROM course_revision_assignments cra
		                WHERE cra.course_revision_id = pc.revision_id AND cra.assignment_id = $1)
		 ORDER BY reader_fold(pc.title), pc.public_id
		 LIMIT $2`, assignmentID, maxListed)
	if err != nil {
		return SubjectPage{}, fmt.Errorf("subject courses: %w", err)
	}
	if p.Courses, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (PublicCourseSummary, error) {
		var c PublicCourseSummary
		err := r.Scan(&c.PublicID, &c.Title, &c.Summary, &c.Language)
		return c, err
	}); err != nil {
		return SubjectPage{}, fmt.Errorf("subject courses: %w", err)
	}

	rows, err = tx.Query(ctx, `
		SELECT ch.code, ch.name_fr, ch.name_ar, p.public_id::text, p.title, p.language
		  FROM curriculum_assignment_chapters cac
		  JOIN chapters ch ON ch.id = cac.chapter_id
		  JOIN reader_published_lessons p ON p.chapter_id = cac.chapter_id
		 WHERE cac.assignment_id = $1
		 ORDER BY cac.position, reader_fold(p.title), p.public_id
		 LIMIT $2`, assignmentID, maxListed)
	if err != nil {
		return SubjectPage{}, fmt.Errorf("subject lessons: %w", err)
	}
	type chapterLesson struct {
		ch PublicChapter
		l  PublicLesson
	}
	placed, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (chapterLesson, error) {
		var x chapterLesson
		err := r.Scan(&x.ch.Code, &x.ch.Names.Fr, &x.ch.Names.Ar, &x.l.PublicID, &x.l.Title, &x.l.Language)
		return x, err
	})
	if err != nil {
		return SubjectPage{}, fmt.Errorf("subject lessons: %w", err)
	}
	for _, x := range placed {
		if n := len(p.Chapters); n == 0 || p.Chapters[n-1].Code != x.ch.Code {
			p.Chapters = append(p.Chapters, x.ch)
		}
		p.Chapters[len(p.Chapters)-1].Lessons = append(p.Chapters[len(p.Chapters)-1].Lessons, x.l)
	}

	rows, err = tx.Query(ctx, `
		SELECT p.public_id::text, p.title, p.language
		  FROM reader_published_lessons p
		 WHERE p.subject_id = $1 AND `+unplacedCondition+`
		 ORDER BY reader_fold(p.title), p.public_id
		 LIMIT $2`, subjectID, maxListed)
	if err != nil {
		return SubjectPage{}, fmt.Errorf("unplaced lessons: %w", err)
	}
	if p.Unplaced, err = pgx.CollectRows(rows, scanPublicLesson); err != nil {
		return SubjectPage{}, fmt.Errorf("unplaced lessons: %w", err)
	}
	return p, tx.Commit(ctx)
}

func scanPublicLesson(r pgx.CollectableRow) (PublicLesson, error) {
	var l PublicLesson
	err := r.Scan(&l.PublicID, &l.Title, &l.Language)
	return l, err
}

// CourseOverview is the public overview of a readable course: the
// published revision's reviewed metadata, placement and outline.
type CourseOverview struct {
	PublicID      string
	Title         string
	Summary       string
	Objectives    string
	Prerequisites string
	Language      string
	HasCover      bool
	CoverAlt      string
	SubjectCode   string
	SubjectNames  catalogue.Names
	AuthorName    string
	ReviewerName  string // the approving course reviewer; empty if none recorded
	Placements    []Placement
	Modules       []OutlineModule
}

// SubjectLabel is the subject's name in the French interface.
func (c CourseOverview) SubjectLabel() catalogue.Label {
	return c.SubjectNames.Label(c.SubjectCode, "fr")
}

// LanguageName is the course language in the interface.
func (c CourseOverview) LanguageName() string { return languageName(c.Language) }

func languageName(lang string) string {
	switch lang {
	case "fr":
		return "Français"
	case "ar":
		return "Arabe"
	}
	return lang
}

// Placement is one curriculum assignment of the published revision.
type Placement struct {
	LevelCode    string
	LevelName    string
	SectionCode  string
	SectionName  string
	EditionCode  string
	EditionLabel string
	EditionState string
}

// StateLabel is the edition's state in the interface.
func (p Placement) StateLabel() string { return editionStateLabel(p.EditionState) }

// Current reports whether the placement's edition is current.
func (p Placement) Current() bool { return p.EditionState == "current" }

// OutlineModule is a module of the published revision with its steps.
type OutlineModule struct {
	Key   string
	Title string
	Steps []OutlineStep
}

// OutlineStep is a step's course-authored title and whether its pinned
// lesson revision is readable now. The lesson's own title is never loaded.
type OutlineStep struct {
	Title    string
	Readable bool
}

// Course loads the public overview of a readable course
// (reader_published_courses), in one repeatable-read snapshot. A course that
// is not readable (unknown, never published, suspended, archived, its
// published revision withdrawn) is ErrNotFound: there is no fallback to an
// earlier revision.
func (s *Store) Course(ctx context.Context, publicID string) (CourseOverview, error) {
	if !ValidPublicID(publicID) {
		return CourseOverview{}, ErrNotFound
	}
	tx, err := s.db.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return CourseOverview{}, fmt.Errorf("course overview: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var c CourseOverview
	var revisionID int64
	var reviewer *string
	err = tx.QueryRow(ctx, `
		SELECT pc.revision_id, pc.public_id::text, pc.title, pc.summary, pc.objectives, pc.prerequisites, pc.language,
		       pc.cover_media_id IS NOT NULL, coalesce(pc.cover_alt, ''),
		       sj.code, sj.name_fr, sj.name_ar, au.display_name,
		       (SELECT ru.display_name FROM course_review_decisions d JOIN users ru ON ru.id = d.reviewer_id
		         WHERE d.revision_id = pc.revision_id AND d.action = 'approve')
		  FROM reader_published_courses pc
		  JOIN subjects sj ON sj.id = pc.subject_id
		  JOIN course_revisions r ON r.id = pc.revision_id
		  JOIN users au ON au.id = r.author_id
		 WHERE pc.public_id = $1::uuid`, publicID).Scan(
		&revisionID, &c.PublicID, &c.Title, &c.Summary, &c.Objectives, &c.Prerequisites, &c.Language,
		&c.HasCover, &c.CoverAlt, &c.SubjectCode, &c.SubjectNames.Fr, &c.SubjectNames.Ar, &c.AuthorName, &reviewer)
	if errors.Is(err, pgx.ErrNoRows) {
		return CourseOverview{}, ErrNotFound
	}
	if err != nil {
		return CourseOverview{}, fmt.Errorf("course overview: %w", err)
	}
	if reviewer != nil {
		c.ReviewerName = *reviewer
	}

	rows, err := tx.Query(ctx, `
		SELECT lv.code, lv.name_fr, sc.code, sc.name_fr, e.code, e.label_fr, e.state
		  FROM course_revision_assignments cra
		  JOIN curriculum_assignments a ON a.id = cra.assignment_id
		  JOIN curriculum_editions e ON e.id = a.edition_id
		  JOIN academic_sections sc ON sc.id = e.section_id
		  JOIN education_levels lv ON lv.id = sc.level_id
		 WHERE cra.course_revision_id = $1
		 ORDER BY lv.display_order, lv.code, sc.display_order, sc.code, e.current_since DESC, e.id DESC`, revisionID)
	if err != nil {
		return CourseOverview{}, fmt.Errorf("course placement: %w", err)
	}
	if c.Placements, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (Placement, error) {
		var p Placement
		err := r.Scan(&p.LevelCode, &p.LevelName, &p.SectionCode, &p.SectionName, &p.EditionCode, &p.EditionLabel, &p.EditionState)
		return p, err
	}); err != nil {
		return CourseOverview{}, fmt.Errorf("course placement: %w", err)
	}

	rows, err = tx.Query(ctx, `
		SELECT m.module_key::text, m.title FROM course_modules m
		 WHERE m.course_revision_id = $1 ORDER BY m.position`, revisionID)
	if err != nil {
		return CourseOverview{}, fmt.Errorf("course modules: %w", err)
	}
	if c.Modules, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (OutlineModule, error) {
		var m OutlineModule
		err := r.Scan(&m.Key, &m.Title)
		return m, err
	}); err != nil {
		return CourseOverview{}, fmt.Errorf("course modules: %w", err)
	}
	index := map[string]int{}
	for i, m := range c.Modules {
		index[m.Key] = i
	}
	// Course-authored step titles and readability only: lesson_title and
	// content are never selected.
	rows, err = tx.Query(ctx, `
		SELECT s.module_key::text, s.title, s.readable FROM reader_course_steps s
		 WHERE s.course_revision_id = $1 ORDER BY s.module_position, s.position`, revisionID)
	if err != nil {
		return CourseOverview{}, fmt.Errorf("course steps: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var key string
		var st OutlineStep
		if err := rows.Scan(&key, &st.Title, &st.Readable); err != nil {
			return CourseOverview{}, fmt.Errorf("course steps: %w", err)
		}
		if i, ok := index[key]; ok {
			c.Modules[i].Steps = append(c.Modules[i].Steps, st)
		}
	}
	if err := rows.Err(); err != nil {
		return CourseOverview{}, fmt.Errorf("course steps: %w", err)
	}
	return c, tx.Commit(ctx)
}

// CourseCover returns the storage key of the cover of a readable course's
// published revision. Anything else is ErrNotFound.
func (s *Store) CourseCover(ctx context.Context, publicID string) (string, error) {
	if !ValidPublicID(publicID) {
		return "", ErrNotFound
	}
	var key string
	err := s.db.QueryRow(ctx, `
		SELECT a.storage_key
		  FROM reader_published_courses pc
		  JOIN media_assets a ON a.id = pc.cover_media_id
		 WHERE pc.public_id = $1::uuid`, publicID).Scan(&key)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("course cover: %w", err)
	}
	return key, nil
}
