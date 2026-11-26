package reader

// The student home (/learn, LMS-B08, docs/LMS_PRODUCT_SPEC.md §7): an
// optional level/section preference, subject shortcuts, available courses,
// bookmarks and recently published material. Lists read only the reader
// views (reader_published_lessons, reader_published_courses); the
// preference narrows them to the current curriculum edition of the chosen
// section (or of every section of the chosen level).

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// maxRecent bounds the "recently published" list.
const maxRecent = 10

// ErrInvalidPreference means the chosen level or section does not exist,
// is retired, or the section is not in the level.
var ErrInvalidPreference = errors.New("invalid preference")

// Preference is a student's optional level and section. Zero LevelID
// means none.
type Preference struct {
	LevelID   int64
	SectionID int64 // 0: every section of the level
	Level     string
	LevelAr   string
	Section   string
	SectionAr string
}

// Set reports whether a preference is stored and still usable.
func (p Preference) Set() bool { return p.LevelID != 0 }

// SectionOption is a selectable section of a level.
type SectionOption struct {
	ID     int64
	Name   string
	NameAr string
}

// LevelOption is a selectable level with its selectable sections.
type LevelOption struct {
	ID       int64
	Name     string
	NameAr   string
	Sections []SectionOption
}

// RecentItem is a recently published lesson or course.
type RecentItem struct {
	Course   bool
	PublicID string
	Title    string
	Language string
	At       time.Time
}

// Preference returns userID's preference, or the zero Preference when none
// is stored or its level or section has been retired since.
func (s *Store) Preference(ctx context.Context, userID int64) (Preference, error) {
	var p Preference
	var sec *int64
	var secName, secNameAr *string
	err := s.db.QueryRow(ctx, `
		SELECT lv.id, lv.name_fr, lv.name_ar, sec.id, sec.name_fr, sec.name_ar
		  FROM student_preferences sp
		  JOIN education_levels lv ON lv.id = sp.level_id AND lv.retired_at IS NULL
		  LEFT JOIN academic_sections sec ON sec.id = sp.section_id
		 WHERE sp.user_id = $1 AND (sp.section_id IS NULL OR sec.retired_at IS NULL)`, userID).Scan(&p.LevelID, &p.Level, &p.LevelAr, &sec, &secName, &secNameAr)
	if errors.Is(err, pgx.ErrNoRows) {
		return Preference{}, nil
	}
	if err != nil {
		return Preference{}, fmt.Errorf("load preference: %w", err)
	}
	if sec != nil {
		p.SectionID, p.Section, p.SectionAr = *sec, *secName, *secNameAr
	}
	return p, nil
}

// LevelOptions lists the levels and sections a student may choose: not
// retired, in the catalogue's display order.
func (s *Store) LevelOptions(ctx context.Context) ([]LevelOption, error) {
	rows, err := s.db.Query(ctx, `
		SELECT lv.id, lv.name_fr, lv.name_ar, sec.id, sec.name_fr, sec.name_ar
		  FROM education_levels lv
		  LEFT JOIN academic_sections sec ON sec.level_id = lv.id AND sec.retired_at IS NULL
		 WHERE lv.retired_at IS NULL
		 ORDER BY lv.display_order, lv.code, sec.display_order, sec.code
		 LIMIT $1`, maxListed)
	if err != nil {
		return nil, fmt.Errorf("level options: %w", err)
	}
	defer rows.Close()
	var out []LevelOption
	for rows.Next() {
		var id int64
		var name, nameAr string
		var secID *int64
		var secName, secNameAr *string
		if err := rows.Scan(&id, &name, &nameAr, &secID, &secName, &secNameAr); err != nil {
			return nil, fmt.Errorf("level options: %w", err)
		}
		if n := len(out); n == 0 || out[n-1].ID != id {
			out = append(out, LevelOption{ID: id, Name: name, NameAr: nameAr})
		}
		if secID != nil {
			l := &out[len(out)-1]
			l.Sections = append(l.Sections, SectionOption{ID: *secID, Name: *secName, NameAr: *secNameAr})
		}
	}
	return out, rows.Err()
}

// SetPreference stores userID's level and optional section (sectionID 0:
// the whole level). Both must exist and not be retired, and the section
// must belong to the level (also a database constraint).
func (s *Store) SetPreference(ctx context.Context, userID, levelID, sectionID int64) error {
	var sec *int64
	if sectionID != 0 {
		sec = &sectionID
	}
	tag, err := s.db.Exec(ctx, `
		INSERT INTO student_preferences (user_id, level_id, section_id, updated_at)
		SELECT $1, lv.id, $3::bigint, now()
		  FROM education_levels lv
		 WHERE lv.id = $2 AND lv.retired_at IS NULL
		   AND ($3::bigint IS NULL OR EXISTS (SELECT 1 FROM academic_sections sec
		                                       WHERE sec.id = $3 AND sec.level_id = lv.id AND sec.retired_at IS NULL))
		ON CONFLICT (user_id) DO UPDATE SET level_id = EXCLUDED.level_id, section_id = EXCLUDED.section_id, updated_at = now()`,
		userID, levelID, sec)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23503" {
		// A concurrent retirement cannot delete rows; a foreign key failure
		// means an unknown ID.
		return ErrInvalidPreference
	}
	if err != nil {
		return fmt.Errorf("save preference: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return ErrInvalidPreference
	}
	return nil
}

// ClearPreference removes userID's preference ("browse everything").
func (s *Store) ClearPreference(ctx context.Context, userID int64) error {
	if _, err := s.db.Exec(ctx, `DELETE FROM student_preferences WHERE user_id = $1`, userID); err != nil {
		return fmt.Errorf("clear preference: %w", err)
	}
	return nil
}

func prefArgs(p Preference) (lv, sec *int64) {
	if !p.Set() {
		return nil, nil
	}
	l := p.LevelID
	lv = &l
	if p.SectionID != 0 {
		s := p.SectionID
		sec = &s
	}
	return lv, sec
}

// HomeCourses lists readable courses, newest publication first, at most
// maxHomeCourses: with a preference, those placed in a current edition of
// the preferred section or level; without one, every readable course.
func (s *Store) HomeCourses(ctx context.Context, p Preference) ([]CourseSummary, error) {
	lv, sec := prefArgs(p)
	rows, err := s.db.Query(ctx, `
		SELECT pc.public_id::text, pc.title, pc.summary, pc.language, pc.revision_id
		  FROM reader_published_courses pc
		 WHERE ($1::bigint IS NULL OR EXISTS (
		        SELECT 1 FROM course_revision_assignments cra
		          JOIN curriculum_assignments a ON a.id = cra.assignment_id
		          JOIN curriculum_editions e ON e.id = a.edition_id AND e.state = 'current'
		          JOIN academic_sections sec ON sec.id = e.section_id
		         WHERE cra.course_revision_id = pc.revision_id
		           AND sec.level_id = $1 AND ($2::bigint IS NULL OR sec.id = $2)))
		 ORDER BY (SELECT max(occurred_at) FROM course_publication_audit au
		            WHERE au.course_id = pc.course_id AND au.revision_id = pc.revision_id) DESC NULLS LAST,
		          pc.public_id
		 LIMIT $3`, lv, sec, maxHomeCourses)
	if err != nil {
		return nil, fmt.Errorf("home courses: %w", err)
	}
	return s.summaries(ctx, rows)
}

// HomeSubjects lists subjects with published lessons, like Subjects; with a
// preference, only those assigned in a current edition of the preferred
// section or level.
func (s *Store) HomeSubjects(ctx context.Context, p Preference) ([]Subject, error) {
	lv, sec := prefArgs(p)
	rows, err := s.db.Query(ctx, `
		SELECT sj.id, sj.code, sj.name_fr, sj.name_ar, count(*)
		  FROM reader_published_lessons pl JOIN subjects sj ON sj.id = pl.subject_id
		 WHERE ($1::bigint IS NULL OR EXISTS (
		        SELECT 1 FROM curriculum_assignments a
		          JOIN curriculum_editions e ON e.id = a.edition_id AND e.state = 'current'
		          JOIN academic_sections sec ON sec.id = e.section_id
		         WHERE a.subject_id = sj.id AND sec.level_id = $1 AND ($2::bigint IS NULL OR sec.id = $2)))
		 GROUP BY sj.id
		 ORDER BY sj.sort_order, sj.code
		 LIMIT $3`, lv, sec, maxListed)
	if err != nil {
		return nil, fmt.Errorf("home subjects: %w", err)
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Subject, error) {
		var sj Subject
		err := r.Scan(&sj.ID, &sj.Code, &sj.Names.Fr, &sj.Names.Ar, &sj.LessonCount)
		return sj, err
	})
}

// Recent lists the most recently published readable lessons and courses
// (their current published revision's latest publish or restore row),
// newest first, at most maxRecent.
func (s *Store) Recent(ctx context.Context) ([]RecentItem, error) {
	rows, err := s.db.Query(ctx, `
		(SELECT false, p.public_id::text, p.title, p.language,
		        (SELECT max(a.occurred_at) FROM publication_audit a
		          WHERE a.lesson_id = p.lesson_id AND a.revision_id = p.revision_id AND a.action IN ('publish', 'restore')) AS at
		   FROM reader_published_lessons p)
		UNION ALL
		(SELECT true, pc.public_id::text, pc.title, pc.language,
		        (SELECT max(a.occurred_at) FROM course_publication_audit a
		          WHERE a.course_id = pc.course_id AND a.revision_id = pc.revision_id) AS at
		   FROM reader_published_courses pc)
		 ORDER BY at DESC NULLS LAST, 2
		 LIMIT $1`, maxRecent)
	if err != nil {
		return nil, fmt.Errorf("recent: %w", err)
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (RecentItem, error) {
		var it RecentItem
		var at *time.Time
		err := r.Scan(&it.Course, &it.PublicID, &it.Title, &it.Language, &at)
		if at != nil {
			it.At = *at
		}
		return it, err
	})
}
