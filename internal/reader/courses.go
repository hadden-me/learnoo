package reader

// Course reading (LMS-B08, docs/LMS_CONTRACT.md §9): the outline of a
// readable course, its steps at their pinned lesson revisions, course-context
// media, course bookmarks and course search. Every read goes through the
// views of migrations/0015: reader_published_courses (pointer set, not
// archived, published revision approved and not withdrawn) and
// reader_course_steps (readable = the pinned revision is eligible now; the
// lesson revision's title and content are joined only for a readable step).
// Nothing here substitutes another revision for an unreadable step, and an
// unreadable step exposes only its course-authored title.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/hadden-me/learnoo/internal/catalogue"
	"github.com/hadden-me/learnoo/internal/rendering"
	"github.com/jackc/pgx/v5"
)

// maxHomeCourses bounds the course lists of the student home.
const maxHomeCourses = 24

// CourseSummary is a readable course in a list.
type CourseSummary struct {
	PublicID string
	Title    string
	Summary  string
	Language string
	// Placements are the curriculum editions the published revision is
	// placed in, labelled; earlier (superseded or retired) editions say so.
	Placements []Placement
}

// Placement is one curriculum placement of a published course revision.
type Placement struct {
	Level   string
	Section string
	Edition string
	// Earlier is true for a superseded or retired edition.
	Earlier bool
}

// OutlineStep is one step of a course outline. Readable is false while the
// pinned lesson revision is not eligible (withdrawn, or its lesson
// unpublished or archived); only the course-authored Title is then known.
type OutlineStep struct {
	Index          int // 0-based position in the whole course
	Title          string
	LessonPublicID string
	Readable       bool
	Current        bool
}

// Number is the step's 1-based position in the course.
func (s OutlineStep) Number() int { return s.Index + 1 }

// OutlineModule groups the steps of a module, in order.
type OutlineModule struct {
	Title string
	Steps []OutlineStep
}

// Course is a readable course with its outline.
type Course struct {
	CourseSummary
	ID             int64
	RevisionID     int64
	RevisionNumber int
	Objectives     string
	Prerequisites  string
	SubjectCode    string
	SubjectNames   catalogue.Names
	OwnerName      string
	ReviewerName   string
	Bookmarked     bool
	Modules        []OutlineModule
	StepCount      int
	// Unavailable counts the steps that are not readable now.
	Unavailable int
}

// SubjectLabel is the course subject's name, in the course language when set.
func (c Course) SubjectLabel() catalogue.Label {
	return c.SubjectNames.Label(c.SubjectCode, c.Language)
}

// Steps returns every step of the outline in order.
func (c Course) Steps() []OutlineStep {
	var out []OutlineStep
	for _, m := range c.Modules {
		out = append(out, m.Steps...)
	}
	return out
}

// CourseStep is one step of a readable course, read at its pinned revision.
type CourseStep struct {
	Course Course
	Step   OutlineStep
	Module string
	// Previous and Next are the neighbouring steps, readable or not
	// (navigation never skips an unavailable step); nil at either end.
	Previous, Next *OutlineStep
	// The fields below are set only when Step.Readable.
	LessonID       int64
	RevisionID     int64
	RevisionNumber int
	Objectives     string
	Document       rendering.Document
	AuthorName     string
	ReviewerName   string
	// StandaloneNumber is the lesson's current standalone published
	// revision number; Differs is true when it is not the pinned one.
	StandaloneNumber int
	Differs          bool
}

// CourseBookmark is a saved course. Available is false while the course is
// not readable (suspended, archived); Title is then empty.
type CourseBookmark struct {
	PublicID  string
	Available bool
	Title     string
	Language  string
}

// courseHeader reads a readable course by public ID.
func (s *Store) courseHeader(ctx context.Context, q pgx.Tx, userID int64, publicID string) (Course, error) {
	var c Course
	var reviewer *string
	err := q.QueryRow(ctx, `
		SELECT pc.course_id, pc.public_id::text, pc.revision_id, pc.revision_number, pc.title, pc.summary,
		       pc.objectives, pc.prerequisites, pc.language, sj.code, sj.name_fr, sj.name_ar, ou.display_name,
		       (SELECT ru.display_name FROM course_review_decisions d JOIN users ru ON ru.id = d.reviewer_id
		         WHERE d.revision_id = pc.revision_id AND d.action = 'approve'),
		       EXISTS (SELECT 1 FROM course_bookmarks b WHERE b.user_id = $2 AND b.course_id = pc.course_id)
		  FROM reader_published_courses pc
		  JOIN subjects sj ON sj.id = pc.subject_id
		  JOIN course_revisions cr ON cr.id = pc.revision_id
		  JOIN users ou ON ou.id = cr.author_id
		 WHERE pc.public_id = $1::uuid`, publicID, userID).Scan(
		&c.ID, &c.PublicID, &c.RevisionID, &c.RevisionNumber, &c.Title, &c.Summary, &c.Objectives, &c.Prerequisites,
		&c.Language, &c.SubjectCode, &c.SubjectNames.Fr, &c.SubjectNames.Ar, &c.OwnerName, &reviewer, &c.Bookmarked)
	if errors.Is(err, pgx.ErrNoRows) {
		return Course{}, ErrNotFound
	}
	if err != nil {
		return Course{}, fmt.Errorf("load course: %w", err)
	}
	if reviewer != nil {
		c.ReviewerName = *reviewer
	}
	placements, err := coursePlacements(ctx, q, []int64{c.RevisionID})
	if err != nil {
		return Course{}, err
	}
	c.Placements = placements[c.RevisionID]
	return c, nil
}

// coursePlacements labels the placements of published course revisions.
func coursePlacements(ctx context.Context, q interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}, revisions []int64) (map[int64][]Placement, error) {
	out := map[int64][]Placement{}
	if len(revisions) == 0 {
		return out, nil
	}
	rows, err := q.Query(ctx, `
		SELECT cra.course_revision_id, lv.name_fr, sec.name_fr, e.label_fr, e.state <> 'current'
		  FROM course_revision_assignments cra
		  JOIN curriculum_assignments a ON a.id = cra.assignment_id
		  JOIN curriculum_editions e ON e.id = a.edition_id
		  JOIN academic_sections sec ON sec.id = e.section_id
		  JOIN education_levels lv ON lv.id = sec.level_id
		 WHERE cra.course_revision_id = ANY ($1)
		 ORDER BY cra.course_revision_id, lv.display_order, lv.code, sec.display_order, sec.code, e.current_since DESC, e.id`, revisions)
	if err != nil {
		return nil, fmt.Errorf("course placements: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var rev int64
		var p Placement
		if err := rows.Scan(&rev, &p.Level, &p.Section, &p.Edition, &p.Earlier); err != nil {
			return nil, fmt.Errorf("course placements: %w", err)
		}
		out[rev] = append(out[rev], p)
	}
	return out, rows.Err()
}

// stepRow is one row of reader_course_steps; the lesson fields are set only
// for the requested, readable step.
type stepRow struct {
	OutlineStep
	moduleKey      string
	module         string
	lessonID       int64
	revisionID     int64
	revisionNumber *int
	objectives     *string
	content        []byte
	author         *string
	reviewer       *string
	standalone     *int
}

// courseSteps reads every step of the course's published revision in one
// statement, so the outline, the readable flags and the requested step's
// content come from one snapshot. target is the lesson public ID whose
// content to read ("" for none). No row means the course stopped being
// readable after its header was read.
func courseSteps(ctx context.Context, q pgx.Tx, revisionID int64, target string) ([]stepRow, error) {
	var out []stepRow
	i := 0
	rows, err := q.Query(ctx, `
		SELECT st.module_key::text, st.module_title, st.title, st.lesson_public_id::text, st.readable, st.lesson_id, st.lesson_revision_id,
		       lr.revision_number, lr.objectives, CASE WHEN lr.id IS NOT NULL THEN st.content::text END,
		       au.display_name,
		       (SELECT ru.display_name FROM review_decisions d JOIN users ru ON ru.id = d.reviewer_id
		         WHERE lr.id IS NOT NULL AND d.revision_id = lr.id AND d.action = 'approve'),
		       (SELECT r.revision_number FROM reader_published_lessons p JOIN lesson_revisions r ON r.id = p.revision_id
		         WHERE lr.id IS NOT NULL AND p.lesson_id = st.lesson_id)
		  FROM reader_course_steps st
		  LEFT JOIN lesson_revisions lr
		         ON st.readable AND st.lesson_public_id::text = $2 AND lr.id = st.lesson_revision_id
		  LEFT JOIN users au ON au.id = lr.author_id
		 WHERE st.course_revision_id = $1
		 ORDER BY st.module_position, st.position`, revisionID, target)
	if err != nil {
		return nil, fmt.Errorf("course steps: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var r stepRow
		var content *string
		if err := rows.Scan(&r.moduleKey, &r.module, &r.Title, &r.LessonPublicID, &r.Readable, &r.lessonID, &r.revisionID,
			&r.revisionNumber, &r.objectives, &content, &r.author, &r.reviewer, &r.standalone); err != nil {
			return nil, fmt.Errorf("course steps: %w", err)
		}
		if content != nil {
			r.content = []byte(*content)
		}
		r.Index = i
		i++
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("course steps: %w", err)
	}
	return out, nil
}

// readTx runs fn in a read-only READ COMMITTED transaction.
func (s *Store) readTx(ctx context.Context, fn func(tx pgx.Tx) error) error {
	return pgx.BeginTxFunc(ctx, s.db, pgx.TxOptions{AccessMode: pgx.ReadOnly}, fn)
}

func buildOutline(c *Course, rows []stepRow, target string) {
	c.Modules = nil
	c.StepCount, c.Unavailable = len(rows), 0
	for i := range rows {
		rows[i].Current = target != "" && rows[i].LessonPublicID == target
		if !rows[i].Readable {
			c.Unavailable++
		}
		if i == 0 || rows[i-1].moduleKey != rows[i].moduleKey {
			c.Modules = append(c.Modules, OutlineModule{Title: rows[i].module})
		}
		m := &c.Modules[len(c.Modules)-1]
		m.Steps = append(m.Steps, rows[i].OutlineStep)
	}
}

// Course loads a readable course and its outline for userID. A course that
// is not readable (unknown, never published, suspended, archived, or its
// published revision withdrawn) is ErrNotFound.
func (s *Store) Course(ctx context.Context, userID int64, publicID string) (Course, error) {
	if !ValidPublicID(publicID) {
		return Course{}, ErrNotFound
	}
	var c Course
	err := s.readTx(ctx, func(tx pgx.Tx) error {
		var err error
		if c, err = s.courseHeader(ctx, tx, userID, publicID); err != nil {
			return err
		}
		rows, err := courseSteps(ctx, tx, c.RevisionID, "")
		if err != nil {
			return err
		}
		if len(rows) == 0 {
			return ErrNotFound
		}
		buildOutline(&c, rows, "")
		return nil
	})
	return c, err
}

// CourseStep loads the step of a readable course that pins the lesson with
// lessonPublicID. The step's lesson content is read only when the step is
// readable; otherwise only its course-authored title is returned, with the
// outline and neighbours. A course that is not readable, or a lesson that is
// not a step of its published revision, is ErrNotFound.
func (s *Store) CourseStep(ctx context.Context, userID int64, coursePublicID, lessonPublicID string) (CourseStep, error) {
	if !ValidPublicID(coursePublicID) || !ValidPublicID(lessonPublicID) {
		return CourseStep{}, ErrNotFound
	}
	var cs CourseStep
	err := s.readTx(ctx, func(tx pgx.Tx) error {
		c, err := s.courseHeader(ctx, tx, userID, coursePublicID)
		if err != nil {
			return err
		}
		rows, err := courseSteps(ctx, tx, c.RevisionID, lessonPublicID)
		if err != nil {
			return err
		}
		buildOutline(&c, rows, lessonPublicID)
		at := -1
		for i := range rows {
			if rows[i].Current {
				at = i
			}
		}
		if at < 0 {
			return ErrNotFound
		}
		r := rows[at]
		cs = CourseStep{Course: c, Step: r.OutlineStep, Module: r.module}
		if at > 0 {
			p := rows[at-1].OutlineStep
			cs.Previous = &p
		}
		if at+1 < len(rows) {
			n := rows[at+1].OutlineStep
			cs.Next = &n
		}
		if !r.Readable {
			return nil
		}
		if r.content == nil || r.revisionNumber == nil {
			// readable rows always carry their content; defensive.
			return fmt.Errorf("course step %s: readable without content", lessonPublicID)
		}
		cs.LessonID, cs.RevisionID, cs.RevisionNumber = r.lessonID, r.revisionID, *r.revisionNumber
		if r.objectives != nil {
			cs.Objectives = *r.objectives
		}
		if r.author != nil {
			cs.AuthorName = *r.author
		}
		if r.reviewer != nil {
			cs.ReviewerName = *r.reviewer
		}
		if r.standalone != nil {
			cs.StandaloneNumber = *r.standalone
		}
		cs.Differs = cs.StandaloneNumber != cs.RevisionNumber
		var compact bytes.Buffer
		if err := json.Compact(&compact, r.content); err != nil {
			return fmt.Errorf("course step %s content: %w", lessonPublicID, err)
		}
		doc, err := rendering.Decode(&compact)
		if err != nil {
			return fmt.Errorf("course step %s content: %w", lessonPublicID, err)
		}
		cs.Document = doc
		return nil
	})
	return cs, err
}

// CourseMedia returns the storage key of the uploaded asset mediaID if the
// step of the readable course that pins the lesson is readable and its
// pinned revision both lists the asset in its frozen media references
// (revision_media) and embeds it in its content as an image
// "media:<id>" (the embed check of LessonMedia). Anything else is
// ErrNotFound. One statement: readability and the embed check come from
// one snapshot.
func (s *Store) CourseMedia(ctx context.Context, coursePublicID, lessonPublicID string, mediaID int64) (string, error) {
	if !ValidPublicID(coursePublicID) || !ValidPublicID(lessonPublicID) || mediaID <= 0 {
		return "", ErrNotFound
	}
	var key string
	err := s.db.QueryRow(ctx, `
		SELECT a.storage_key
		  FROM reader_course_steps st
		  JOIN revision_media rm ON rm.revision_id = st.lesson_revision_id
		  JOIN media_assets a ON a.id = rm.media_asset_id
		 WHERE st.course_public_id = $1::uuid AND st.lesson_public_id = $2::uuid AND st.readable AND a.id = $3
		   AND jsonb_path_exists(st.content, 'strict $.** ? (@.type == "image" && @.src == $src)',
		                         jsonb_build_object('src', 'media:' || $3::bigint::text))`,
		coursePublicID, lessonPublicID, mediaID).Scan(&key)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("course media: %w", err)
	}
	return key, nil
}

// CourseBookmarks lists userID's course bookmarks, newest first, including
// courses that are no longer readable (Available false).
func (s *Store) CourseBookmarks(ctx context.Context, userID int64) ([]CourseBookmark, error) {
	rows, err := s.db.Query(ctx, `
		SELECT c.public_id::text, pc.course_id IS NOT NULL, coalesce(pc.title, ''), coalesce(pc.language, '')
		  FROM course_bookmarks b
		  JOIN courses c ON c.id = b.course_id
		  LEFT JOIN reader_published_courses pc ON pc.course_id = b.course_id
		 WHERE b.user_id = $1
		 ORDER BY b.created_at DESC, c.public_id
		 LIMIT $2`, userID, MaxBookmarks)
	if err != nil {
		return nil, fmt.Errorf("list course bookmarks: %w", err)
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (CourseBookmark, error) {
		var b CourseBookmark
		err := r.Scan(&b.PublicID, &b.Available, &b.Title, &b.Language)
		return b, err
	})
}

// AddCourseBookmark bookmarks a readable course. Adding an existing
// bookmark is not an error; a course that is not readable is ErrNotFound.
// The per-user limit (MaxBookmarks courses) is checked in the same
// statement, as for lessons.
func (s *Store) AddCourseBookmark(ctx context.Context, userID int64, publicID string) error {
	if !ValidPublicID(publicID) {
		return ErrNotFound
	}
	var courseID *int64
	var count int
	err := s.db.QueryRow(ctx, `
		SELECT (SELECT course_id FROM reader_published_courses WHERE public_id = $2::uuid),
		       (SELECT count(*) FROM course_bookmarks WHERE user_id = $1)`, userID, publicID).Scan(&courseID, &count)
	if err != nil {
		return fmt.Errorf("add course bookmark: %w", err)
	}
	if courseID == nil {
		return ErrNotFound
	}
	if count >= MaxBookmarks {
		return ErrTooManyBookmarks
	}
	if _, err := s.db.Exec(ctx, `
		INSERT INTO course_bookmarks (user_id, course_id) VALUES ($1, $2)
		ON CONFLICT (user_id, course_id) DO NOTHING`, userID, *courseID); err != nil {
		return fmt.Errorf("add course bookmark: %w", err)
	}
	return nil
}

// RemoveCourseBookmark deletes userID's bookmark of the course, readable or
// not. Removing a missing bookmark is not an error.
func (s *Store) RemoveCourseBookmark(ctx context.Context, userID int64, publicID string) error {
	if !ValidPublicID(publicID) {
		return ErrNotFound
	}
	if _, err := s.db.Exec(ctx, `
		DELETE FROM course_bookmarks b USING courses c
		 WHERE b.course_id = c.id AND b.user_id = $1 AND c.public_id = $2::uuid`, userID, publicID); err != nil {
		return fmt.Errorf("remove course bookmark: %w", err)
	}
	return nil
}

// summaries scans course summary rows (public_id, title, summary,
// language, revision_id) and labels their placements.
func (s *Store) summaries(ctx context.Context, rows pgx.Rows) ([]CourseSummary, error) {
	var revs []int64
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (CourseSummary, error) {
		var c CourseSummary
		var rev int64
		err := r.Scan(&c.PublicID, &c.Title, &c.Summary, &c.Language, &rev)
		revs = append(revs, rev)
		return c, err
	})
	if err != nil {
		return nil, fmt.Errorf("list courses: %w", err)
	}
	placements, err := coursePlacements(ctx, s.db, revs)
	if err != nil {
		return nil, err
	}
	for i := range out {
		out[i].Placements = placements[revs[i]]
	}
	return out, nil
}

// SearchCourses returns readable courses whose title, summary, objectives,
// prerequisites or the title of a readable step contains the query,
// ignoring case and diacritics like Search, at most MaxSearchResults. Step
// titles of unreadable steps (course-authored or lesson) never match.
func (s *Store) SearchCourses(ctx context.Context, query string) ([]CourseSummary, error) {
	q, ok := NormalizeQuery(query)
	if !ok {
		return nil, ErrInvalidQuery
	}
	var folded string
	if err := s.db.QueryRow(ctx, `SELECT reader_fold($1)`, q).Scan(&folded); err != nil {
		return nil, fmt.Errorf("search courses: %w", err)
	}
	if utf8.RuneCountInString(strings.TrimSpace(folded)) < MinSearchRunes {
		return nil, ErrInvalidQuery
	}
	rows, err := s.db.Query(ctx, `
		SELECT pc.public_id::text, pc.title, pc.summary, pc.language, pc.revision_id
		  FROM reader_published_courses pc
		 WHERE char_length(btrim(reader_fold($1))) >= $3
		   AND (strpos(reader_fold(pc.title || ' ' || pc.summary || ' ' || pc.objectives || ' ' || pc.prerequisites), reader_fold($1)) > 0
		        OR EXISTS (SELECT 1 FROM reader_course_steps st
		                    WHERE st.course_revision_id = pc.revision_id AND st.readable
		                      AND (strpos(reader_fold(st.title), reader_fold($1)) > 0
		                           OR strpos(reader_fold(st.lesson_title), reader_fold($1)) > 0)))
		 ORDER BY reader_fold(pc.title), pc.public_id
		 LIMIT $2`, q, MaxSearchResults, MinSearchRunes)
	if err != nil {
		return nil, fmt.Errorf("search courses: %w", err)
	}
	return s.summaries(ctx, rows)
}
