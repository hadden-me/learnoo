// Package reader serves the authenticated, server-rendered student reader
// (LRN-008): subject/chapter/lesson navigation, published lesson pages
// rendered by internal/rendering, bookmarks and title search, plus the
// sign-in and invitation pages. It reads only what the
// reader_published_lessons view (migrations/0008) exposes.
package reader

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/hadden-me/learnoo/internal/catalogue"
	"github.com/hadden-me/learnoo/internal/rendering"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	// MaxBookmarks bounds one user's bookmarks (and the dashboard list).
	MaxBookmarks = 500
	// MaxSearchRunes bounds a search query; MinSearchRunes avoids listing
	// everything for one character.
	MaxSearchRunes = 100
	MinSearchRunes = 2
	// MaxSearchResults bounds one search response.
	MaxSearchResults = 50
	// maxListed bounds navigation lists defensively.
	maxListed = 1000
)

var (
	// ErrNotFound means no published lesson (or subject) matches: unknown,
	// malformed, never published, unpublished or archived alike.
	ErrNotFound = errors.New("not found")
	// ErrInvalidQuery means the search query is too short or too long,
	// before or after folding.
	ErrInvalidQuery = errors.New("invalid search query")
	// ErrTooManyBookmarks means the user reached MaxBookmarks.
	ErrTooManyBookmarks = errors.New("too many bookmarks")
)

var publicIDPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// ValidPublicID reports whether s is a lower-case UUID, the only form the
// reader links to.
func ValidPublicID(s string) bool { return publicIDPattern.MatchString(s) }

var subjectCodePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// Store reads published lessons and manages bookmarks.
type Store struct{ db *pgxpool.Pool }

// NewStore builds a Store.
func NewStore(db *pgxpool.Pool) *Store { return &Store{db: db} }

// Subject is a subject with at least one published lesson.
type Subject struct {
	ID          int64
	Code        string
	Names       catalogue.Names
	LessonCount int
}

// Label is the subject's name in navigation: French (the reader's
// language), else Arabic, else the code.
func (s Subject) Label() catalogue.Label { return s.Names.Label(s.Code, "fr") }

// LessonSummary is a published lesson in a list.
type LessonSummary struct {
	PublicID     string
	Title        string
	Language     string
	SubjectCode  string
	SubjectNames catalogue.Names
	ChapterCode  string // empty when the published revision has no chapter
	ChapterNames catalogue.Names
}

// SubjectLabel is the subject's name next to the lesson: in the lesson's
// language when set, else the other language, else the code.
func (l LessonSummary) SubjectLabel() catalogue.Label {
	return l.SubjectNames.Label(l.SubjectCode, l.Language)
}

// ChapterLabel is SubjectLabel for the lesson's chapter (empty Text when the
// lesson has no chapter). Catalogue names are display metadata read at
// request time, never part of the reviewed snapshot.
func (l LessonSummary) ChapterLabel() catalogue.Label {
	if l.ChapterCode == "" {
		return catalogue.Label{}
	}
	return l.ChapterNames.Label(l.ChapterCode, l.Language)
}

// Chapter groups a subject's published lessons by their published
// revision's chapter. Code is empty for lessons without a chapter.
type Chapter struct {
	Code    string
	Names   catalogue.Names
	Lessons []LessonSummary
}

// Label is the chapter's heading: French, else Arabic, else the code.
func (c Chapter) Label() catalogue.Label { return c.Names.Label(c.Code, "fr") }

// Lesson is a published lesson ready to render.
type Lesson struct {
	LessonSummary
	LessonID   int64
	RevisionID int64
	// RevisionNumber is the published revision's lesson-scoped number; the
	// error report form sends it back so a report names the revision shown.
	RevisionNumber int
	Objectives     string
	Document       rendering.Document
	AuthorName     string
	ReviewerName   string // the approving reviewer; empty if none recorded
	Bookmarked     bool
}

// Bookmark is a saved lesson. Available is false when the lesson is no
// longer published; Title and codes are then empty.
type Bookmark struct {
	PublicID  string
	Available bool
	LessonSummary
}

// Subjects lists subjects that have published lessons, by the catalogue's
// display order, then code.
func (s *Store) Subjects(ctx context.Context) ([]Subject, error) {
	rows, err := s.db.Query(ctx, `
		SELECT sj.id, sj.code, sj.name_fr, sj.name_ar, count(*)
		  FROM reader_published_lessons p JOIN subjects sj ON sj.id = p.subject_id
		 GROUP BY sj.id
		 ORDER BY sj.sort_order, sj.code
		 LIMIT $1`, maxListed)
	if err != nil {
		return nil, fmt.Errorf("list subjects: %w", err)
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Subject, error) {
		var sj Subject
		err := r.Scan(&sj.ID, &sj.Code, &sj.Names.Fr, &sj.Names.Ar, &sj.LessonCount)
		return sj, err
	})
}

const summaryColumns = `p.public_id::text, p.title, p.language, sj.code, sj.name_fr, sj.name_ar,
	coalesce(ch.code, ''), ch.name_fr, ch.name_ar`

const summaryJoins = `
	  JOIN subjects sj ON sj.id = p.subject_id
	  LEFT JOIN chapters ch ON ch.id = p.chapter_id`

func scanSummary(r pgx.CollectableRow) (LessonSummary, error) {
	var l LessonSummary
	err := r.Scan(&l.PublicID, &l.Title, &l.Language, &l.SubjectCode, &l.SubjectNames.Fr, &l.SubjectNames.Ar,
		&l.ChapterCode, &l.ChapterNames.Fr, &l.ChapterNames.Ar)
	return l, err
}

// SubjectChapters returns a subject and its published lessons grouped by
// chapter (chapters by the catalogue's display order, then code; lessons
// without a chapter last; lessons by folded title). Unknown subjects and
// subjects without published lessons are ErrNotFound.
func (s *Store) SubjectChapters(ctx context.Context, code string) (Subject, []Chapter, error) {
	if !subjectCodePattern.MatchString(code) {
		return Subject{}, nil, ErrNotFound
	}
	rows, err := s.db.Query(ctx, `
		SELECT `+summaryColumns+`, sj.id
		  FROM reader_published_lessons p`+summaryJoins+`
		 WHERE sj.code = $1
		 ORDER BY ch.id IS NULL, ch.sort_order, ch.code, reader_fold(p.title), p.public_id
		 LIMIT $2`, code, maxListed)
	if err != nil {
		return Subject{}, nil, fmt.Errorf("list lessons: %w", err)
	}
	var subject Subject
	lessons, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (LessonSummary, error) {
		var l LessonSummary
		err := r.Scan(&l.PublicID, &l.Title, &l.Language, &l.SubjectCode, &l.SubjectNames.Fr, &l.SubjectNames.Ar,
			&l.ChapterCode, &l.ChapterNames.Fr, &l.ChapterNames.Ar, &subject.ID)
		return l, err
	})
	if err != nil {
		return Subject{}, nil, fmt.Errorf("list lessons: %w", err)
	}
	if len(lessons) == 0 {
		return Subject{}, nil, ErrNotFound
	}
	subject.Code, subject.Names, subject.LessonCount = lessons[0].SubjectCode, lessons[0].SubjectNames, len(lessons)
	var chapters []Chapter
	for _, l := range lessons {
		if n := len(chapters); n == 0 || chapters[n-1].Code != l.ChapterCode {
			chapters = append(chapters, Chapter{Code: l.ChapterCode, Names: l.ChapterNames})
		}
		chapters[len(chapters)-1].Lessons = append(chapters[len(chapters)-1].Lessons, l)
	}
	return subject, chapters, nil
}

// Lesson loads a published lesson by public ID with its credit and whether
// userID bookmarked it. Anything not currently published is ErrNotFound.
func (s *Store) Lesson(ctx context.Context, userID int64, publicID string) (Lesson, error) {
	if !ValidPublicID(publicID) {
		return Lesson{}, ErrNotFound
	}
	var l Lesson
	var content []byte
	var reviewer *string
	err := s.db.QueryRow(ctx, `
		SELECT `+summaryColumns+`, p.lesson_id, p.revision_id,
		       (SELECT lr.revision_number FROM lesson_revisions lr WHERE lr.id = p.revision_id),
		       p.objectives, p.content::text,
		       au.display_name,
		       (SELECT ru.display_name FROM review_decisions d JOIN users ru ON ru.id = d.reviewer_id
		         WHERE d.revision_id = p.revision_id AND d.action = 'approve'),
		       EXISTS (SELECT 1 FROM bookmarks b WHERE b.user_id = $2 AND b.lesson_id = p.lesson_id)
		  FROM reader_published_lessons p`+summaryJoins+`
		  JOIN users au ON au.id = p.author_id
		 WHERE p.public_id = $1::uuid`, publicID, userID).Scan(
		&l.PublicID, &l.Title, &l.Language, &l.SubjectCode, &l.SubjectNames.Fr, &l.SubjectNames.Ar,
		&l.ChapterCode, &l.ChapterNames.Fr, &l.ChapterNames.Ar,
		&l.LessonID, &l.RevisionID, &l.RevisionNumber, &l.Objectives, &content, &l.AuthorName, &reviewer, &l.Bookmarked)
	if errors.Is(err, pgx.ErrNoRows) {
		return Lesson{}, ErrNotFound
	}
	if err != nil {
		return Lesson{}, fmt.Errorf("load lesson: %w", err)
	}
	if reviewer != nil {
		l.ReviewerName = *reviewer
	}
	// The stored snapshot is canonical schema v1 (internal/lessons); jsonb
	// output adds spacing, so compact it before the size-limited decoder.
	var compact bytes.Buffer
	if err := json.Compact(&compact, content); err != nil {
		return Lesson{}, fmt.Errorf("lesson %s content: %w", publicID, err)
	}
	doc, err := rendering.Decode(&compact)
	if err != nil {
		return Lesson{}, fmt.Errorf("lesson %s content: %w", publicID, err)
	}
	l.Document = doc
	return l, nil
}

// LessonMedia returns the storage key of the uploaded asset mediaID if the
// lesson's currently published revision (reader_published_lessons: not
// archived, approved, not withdrawn) both lists it in its frozen media
// references (revision_media) and embeds it in its content as an image
// "media:<id>". Anything else is ErrNotFound. Reading needs only a session,
// as for the lesson page.
func (s *Store) LessonMedia(ctx context.Context, publicID string, mediaID int64) (string, error) {
	if !ValidPublicID(publicID) || mediaID <= 0 {
		return "", ErrNotFound
	}
	var key string
	err := s.db.QueryRow(ctx, `
		SELECT a.storage_key
		  FROM reader_published_lessons p
		  JOIN revision_media rm ON rm.revision_id = p.revision_id
		  JOIN media_assets a ON a.id = rm.media_asset_id
		 WHERE p.public_id = $1::uuid AND a.id = $2
		   AND jsonb_path_exists(p.content, 'strict $.** ? (@.type == "image" && @.src == $src)',
		                         jsonb_build_object('src', 'media:' || $2::bigint::text))`,
		publicID, mediaID).Scan(&key)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("lesson media: %w", err)
	}
	return key, nil
}

// NormalizeQuery trims a search query, turns whitespace (tabs, newlines)
// into single spaces and drops other control characters. It
// returns ok=false for queries shorter than MinSearchRunes or longer than
// MaxSearchRunes.
func NormalizeQuery(q string) (string, bool) {
	if !utf8.ValidString(q) {
		return "", false
	}
	q = strings.Map(func(r rune) rune {
		switch {
		case unicode.IsSpace(r):
			return ' '
		case unicode.IsControl(r):
			return -1
		}
		return r
	}, q)
	q = strings.Join(strings.Fields(q), " ")
	n := utf8.RuneCountInString(q)
	return q, n >= MinSearchRunes && n <= MaxSearchRunes
}

// Search returns published lessons whose title contains the query,
// ignoring case and diacritics (reader_fold, migrations/0008), at most
// MaxSearchResults. The query is a bound parameter used with strpos, so it
// is never interpreted as SQL or as a pattern. A query that is invalid for
// NormalizeQuery, or whose folded form is shorter than MinSearchRunes (for
// example only Arabic harakat, tatweel or combining accents, which fold to
// nothing and would otherwise match every title), returns ErrInvalidQuery.
func (s *Store) Search(ctx context.Context, query string) ([]LessonSummary, error) {
	q, ok := NormalizeQuery(query)
	if !ok {
		return nil, ErrInvalidQuery
	}
	var folded string
	if err := s.db.QueryRow(ctx, `SELECT reader_fold($1)`, q).Scan(&folded); err != nil {
		return nil, fmt.Errorf("search: %w", err)
	}
	if utf8.RuneCountInString(strings.TrimSpace(folded)) < MinSearchRunes {
		return nil, ErrInvalidQuery
	}
	// The length condition repeats the check in SQL, so an empty folded
	// query can never reach strpos (which matches '' everywhere).
	rows, err := s.db.Query(ctx, `
		SELECT `+summaryColumns+`
		  FROM reader_published_lessons p`+summaryJoins+`
		 WHERE char_length(btrim(reader_fold($1))) >= $3
		   AND strpos(reader_fold(p.title), reader_fold($1)) > 0
		 ORDER BY reader_fold(p.title), p.public_id
		 LIMIT $2`, q, MaxSearchResults, MinSearchRunes)
	if err != nil {
		return nil, fmt.Errorf("search: %w", err)
	}
	return pgx.CollectRows(rows, scanSummary)
}

// Bookmarks lists userID's bookmarks, newest first, including lessons that
// are no longer published (Available false).
func (s *Store) Bookmarks(ctx context.Context, userID int64) ([]Bookmark, error) {
	rows, err := s.db.Query(ctx, `
		SELECT l.public_id::text, p.lesson_id IS NOT NULL,
		       coalesce(p.title, ''), coalesce(p.language, ''), coalesce(sj.code, ''), sj.name_fr, sj.name_ar,
		       coalesce(ch.code, ''), ch.name_fr, ch.name_ar
		  FROM bookmarks b
		  JOIN lessons l ON l.id = b.lesson_id
		  LEFT JOIN reader_published_lessons p ON p.lesson_id = b.lesson_id
		  LEFT JOIN subjects sj ON sj.id = p.subject_id
		  LEFT JOIN chapters ch ON ch.id = p.chapter_id
		 WHERE b.user_id = $1
		 ORDER BY b.created_at DESC, l.public_id
		 LIMIT $2`, userID, MaxBookmarks)
	if err != nil {
		return nil, fmt.Errorf("list bookmarks: %w", err)
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Bookmark, error) {
		var b Bookmark
		err := r.Scan(&b.PublicID, &b.Available, &b.Title, &b.Language, &b.SubjectCode, &b.SubjectNames.Fr, &b.SubjectNames.Ar,
			&b.ChapterCode, &b.ChapterNames.Fr, &b.ChapterNames.Ar)
		b.LessonSummary.PublicID = b.PublicID
		return b, err
	})
}

// AddBookmark bookmarks a currently published lesson. Adding an existing
// bookmark is not an error. Unpublished or unknown lessons are ErrNotFound.
// The per-user limit is checked in the same statement; concurrent adds by
// one user can exceed it by a few rows, which is harmless.
func (s *Store) AddBookmark(ctx context.Context, userID int64, publicID string) error {
	if !ValidPublicID(publicID) {
		return ErrNotFound
	}
	var lessonID *int64
	var count int
	err := s.db.QueryRow(ctx, `
		SELECT (SELECT lesson_id FROM reader_published_lessons WHERE public_id = $2::uuid),
		       (SELECT count(*) FROM bookmarks WHERE user_id = $1)`, userID, publicID).Scan(&lessonID, &count)
	if err != nil {
		return fmt.Errorf("add bookmark: %w", err)
	}
	if lessonID == nil {
		return ErrNotFound
	}
	if count >= MaxBookmarks {
		return ErrTooManyBookmarks
	}
	if _, err := s.db.Exec(ctx, `
		INSERT INTO bookmarks (user_id, lesson_id) VALUES ($1, $2)
		ON CONFLICT (user_id, lesson_id) DO NOTHING`, userID, *lessonID); err != nil {
		return fmt.Errorf("add bookmark: %w", err)
	}
	return nil
}

// RemoveBookmark deletes userID's bookmark of the lesson, published or not.
// Removing a missing bookmark is not an error.
func (s *Store) RemoveBookmark(ctx context.Context, userID int64, publicID string) error {
	if !ValidPublicID(publicID) {
		return ErrNotFound
	}
	if _, err := s.db.Exec(ctx, `
		DELETE FROM bookmarks b USING lessons l
		 WHERE b.lesson_id = l.id AND b.user_id = $1 AND l.public_id = $2::uuid`, userID, publicID); err != nil {
		return fmt.Errorf("remove bookmark: %w", err)
	}
	return nil
}
