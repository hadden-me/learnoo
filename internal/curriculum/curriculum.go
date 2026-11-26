// Package curriculum stores the curriculum hierarchy of LMS-B02
// (migrations/0013_curriculum.sql, docs/LMS_CONTRACT.md §3): education
// levels, the academic sections inside them, the editions of each section,
// the subjects an edition assigns and each assignment's ordered official
// chapters, plus retirement of subjects and chapters.
//
// Every change runs in one transaction that first rechecks the actor
// (permissions.Authorize: an operator, or a user who is an active
// administrator, share-locking the grant and account rows), then locks
// editions (ascending ID) before any other row, and writes its
// curriculum_audit row in the same transaction. Levels, sections and
// editions carry a version: every write names the version it expects and a
// mismatch is ErrVersionConflict, changing nothing.
//
// Assignments and chapter lists change only while their edition is a draft
// (ErrEditionNotDraft); the database guards enforce the same rule for every
// writer. Nothing here deletes a level, section, edition, subject or chapter.
package curriculum

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/hadden-me/learnoo/internal/permissions"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Limits.
const (
	MaxNameRunes    = 200
	MaxDisplayOrder = 9999
	// MaxChapters bounds one assignment's chapter list.
	MaxChapters = 500
	// MaxListed bounds one list response defensively.
	MaxListed = 1000
)

var (
	// ErrNotFound means the level, section, edition or assignment does not
	// exist (or the assignment is not in the named edition).
	ErrNotFound = errors.New("not found")
	// ErrInvalidInput means a code, name or order is malformed.
	ErrInvalidInput = errors.New("invalid input")
	// ErrCodeExists means the code is already used in its scope.
	ErrCodeExists = errors.New("code already exists")
	// ErrVersionConflict means the entry changed since it was loaded.
	ErrVersionConflict = errors.New("version conflict")
	// ErrEditionNotDraft means the edition left draft, so its assignments
	// and chapter lists are immutable (and it cannot be made current again).
	ErrEditionNotDraft = errors.New("edition is not a draft")
	// ErrStateConflict means the state transition is not allowed from the
	// edition's current state, or a concurrent make-current won.
	ErrStateConflict = errors.New("edition state conflict")
	// ErrAlreadyRetired means the entry is already retired.
	ErrAlreadyRetired = errors.New("already retired")
	// ErrParentRetired means the parent level or section is retired, so
	// nothing new can be placed under it.
	ErrParentRetired = errors.New("parent is retired")
	// ErrUnknownSubject means the subject does not exist.
	ErrUnknownSubject = errors.New("unknown subject")
	// ErrAssignmentExists means the edition already assigns the subject.
	ErrAssignmentExists = errors.New("subject already assigned")
	// ErrPlacementInvalid means a selection the curriculum rules refuse: a
	// retired subject or chapter, a chapter of another subject or that does
	// not exist, or a duplicate chapter.
	ErrPlacementInvalid = errors.New("placement invalid")
)

var codePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// ValidCode reports whether code matches the code format of every catalogue
// table: 1 to 63 lower-case ASCII letters, digits and hyphens, not starting
// with a hyphen.
func ValidCode(code string) bool { return codePattern.MatchString(code) }

// ValidName reports whether name passes curriculum_valid_name: 1 to 200
// characters, no surrounding white space, no control, format or
// line/paragraph separator characters. Arabic text is allowed.
func ValidName(name string) bool {
	if name == "" || !utf8.ValidString(name) || utf8.RuneCountInString(name) > MaxNameRunes || name != strings.TrimSpace(name) {
		return false
	}
	return strings.IndexFunc(name, func(r rune) bool { return unicode.In(r, unicode.Cc, unicode.Cf, unicode.Zl, unicode.Zp) }) < 0
}

// EditionState is the lifecycle state of an edition.
type EditionState string

const (
	Draft      EditionState = "draft"
	Current    EditionState = "current"
	Superseded EditionState = "superseded"
	Retired    EditionState = "retired"
)

// Level is an education level with its sections.
type Level struct {
	ID           int64
	Code         string
	NameFR       string
	NameAR       string
	DisplayOrder int
	RetiredAt    *time.Time
	Version      int
	CreatedAt    time.Time
	Sections     []Section
}

// Section is an academic section inside a level.
type Section struct {
	ID           int64
	LevelID      int64
	Code         string
	NameFR       string
	NameAR       string
	DisplayOrder int
	RetiredAt    *time.Time
	Version      int
	CreatedAt    time.Time
}

// Edition is a curriculum edition of a section. Assignments is filled by
// Edition and the writes that return an edition, not by Editions.
type Edition struct {
	ID           int64
	SectionID    int64
	Code         string
	LabelFR      string
	LabelAR      string
	State        EditionState
	CurrentSince *time.Time
	SupersededAt *time.Time
	RetiredAt    *time.Time
	Version      int
	CreatedAt    time.Time
	Assignments  []Assignment
}

// Assignment is a subject as taught in an edition, with its ordered chapters.
type Assignment struct {
	ID          int64
	EditionID   int64
	SubjectID   int64
	SubjectCode string
	// Selectable is curriculum_assignment_selectable: the edition is current
	// and neither section, level nor subject is retired.
	Selectable bool
	Chapters   []AssignedChapter
}

// AssignedChapter is one official chapter in an assignment's list.
type AssignedChapter struct {
	ChapterID int64
	Code      string
	Position  int
	Retired   bool
}

// Entry holds the fields of a new level or section.
type Entry struct {
	Code         string
	NameFR       string
	NameAR       string
	DisplayOrder int
}

// Display holds the mutable display fields of a level or section.
type Display struct {
	NameFR       string
	NameAR       string
	DisplayOrder int
}

// NewEdition holds the fields of a new draft edition.
type NewEdition struct {
	Code    string
	LabelFR string
	LabelAR string
}

// Store reads and changes the curriculum.
type Store struct{ db *pgxpool.Pool }

// NewStore builds a Store.
func NewStore(db *pgxpool.Pool) *Store { return &Store{db: db} }

func validEntry(code string, d Display) error {
	if !ValidCode(code) {
		return fmt.Errorf("%w: code must be 1 to 63 lower-case letters, digits and hyphens, starting with a letter or digit", ErrInvalidInput)
	}
	return validDisplay(d)
}

func validDisplay(d Display) error {
	if !ValidName(d.NameFR) || !ValidName(d.NameAR) {
		return fmt.Errorf("%w: names must be 1 to 200 characters without surrounding spaces or control characters", ErrInvalidInput)
	}
	if d.DisplayOrder < 0 || d.DisplayOrder > MaxDisplayOrder {
		return fmt.Errorf("%w: display order must be 0 to %d", ErrInvalidInput, MaxDisplayOrder)
	}
	return nil
}

func validLabels(fr, ar string) error {
	if !ValidName(fr) || !ValidName(ar) {
		return fmt.Errorf("%w: labels must be 1 to 200 characters without surrounding spaces or control characters", ErrInvalidInput)
	}
	return nil
}

// write runs fn in a READ COMMITTED transaction after rechecking the actor.
func (s *Store) write(ctx context.Context, actor permissions.Actor, fn func(tx pgx.Tx) error) error {
	err := pgx.BeginTxFunc(ctx, s.db, pgx.TxOptions{IsoLevel: pgx.ReadCommitted}, func(tx pgx.Tx) error {
		if err := permissions.Authorize(ctx, tx, actor); err != nil {
			return err
		}
		return fn(tx)
	})
	return mapError(err)
}

// mapError turns constraint violations the store cannot pre-check under its
// locks into package errors.
func mapError(err error) error {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return err
	}
	switch {
	case pgErr.Code == "23505" && pgErr.ConstraintName == "curriculum_editions_one_current":
		return errors.Join(ErrStateConflict, err)
	case pgErr.Code == "23505" && pgErr.ConstraintName == "curriculum_assignments_edition_subject_key":
		return errors.Join(ErrAssignmentExists, err)
	case pgErr.Code == "23505" && strings.HasSuffix(pgErr.ConstraintName, "code_key"):
		return errors.Join(ErrCodeExists, err)
	case pgErr.Code == "23514":
		return errors.Join(ErrInvalidInput, err)
	}
	return err
}

// audit writes the curriculum_audit row of a change.
func audit(ctx context.Context, tx pgx.Tx, actor permissions.Actor, target string, targetID int64, editionID *int64, action string, before, after map[string]any) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO curriculum_audit (actor_id, operator_label, target, target_id, edition_id, action, before, after)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		actor.AuditID(), actor.AuditLabel(), target, targetID, editionID, action, jsonOrNil(before), jsonOrNil(after))
	if err != nil {
		return fmt.Errorf("audit %s %s: %w", action, target, err)
	}
	return nil
}

func jsonOrNil(m map[string]any) any {
	if m == nil {
		return nil
	}
	return m
}

// ---- reads ----

const levelColumns = `id, code, name_fr, name_ar, display_order, retired_at, version, created_at`
const sectionColumns = `id, level_id, code, name_fr, name_ar, display_order, retired_at, version, created_at`
const editionColumns = `id, section_id, code, label_fr, label_ar, state, current_since, superseded_at, retired_at, version, created_at`

func scanLevel(r pgx.Row) (Level, error) {
	var l Level
	err := r.Scan(&l.ID, &l.Code, &l.NameFR, &l.NameAR, &l.DisplayOrder, &l.RetiredAt, &l.Version, &l.CreatedAt)
	return l, err
}

func scanSection(r pgx.Row) (Section, error) {
	var s Section
	err := r.Scan(&s.ID, &s.LevelID, &s.Code, &s.NameFR, &s.NameAR, &s.DisplayOrder, &s.RetiredAt, &s.Version, &s.CreatedAt)
	return s, err
}

func scanEdition(r pgx.Row) (Edition, error) {
	var e Edition
	err := r.Scan(&e.ID, &e.SectionID, &e.Code, &e.LabelFR, &e.LabelAR, &e.State, &e.CurrentSince, &e.SupersededAt,
		&e.RetiredAt, &e.Version, &e.CreatedAt)
	return e, err
}

func readOnly(ctx context.Context, db *pgxpool.Pool, fn func(tx pgx.Tx) error) error {
	return pgx.BeginTxFunc(ctx, db, pgx.TxOptions{AccessMode: pgx.ReadOnly, IsoLevel: pgx.RepeatableRead}, fn)
}

// Levels lists every level with its sections, retired ones included, by
// display order then code (byte order, whatever the database locale). It
// performs no authorization.
func (s *Store) Levels(ctx context.Context) ([]Level, error) {
	var levels []Level
	err := readOnly(ctx, s.db, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+levelColumns+` FROM education_levels
			ORDER BY display_order, code COLLATE "C", id LIMIT $1`, MaxListed)
		if err != nil {
			return err
		}
		levels, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (Level, error) { return scanLevel(r) })
		if err != nil {
			return err
		}
		rows, err = tx.Query(ctx, `SELECT `+sectionColumns+` FROM academic_sections
			ORDER BY level_id, display_order, code COLLATE "C", id LIMIT $1`, MaxListed)
		if err != nil {
			return err
		}
		sections, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Section, error) { return scanSection(r) })
		if err != nil {
			return err
		}
		index := make(map[int64]int, len(levels))
		for i := range levels {
			levels[i].Sections = []Section{}
			index[levels[i].ID] = i
		}
		for _, sec := range sections {
			if i, ok := index[sec.LevelID]; ok {
				levels[i].Sections = append(levels[i].Sections, sec)
			}
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("list levels: %w", err)
	}
	return levels, nil
}

// Editions returns the section and its editions (without assignments),
// newest first. An unknown section is ErrNotFound. It performs no
// authorization.
func (s *Store) Editions(ctx context.Context, sectionID int64) (Section, []Edition, error) {
	var sec Section
	var editions []Edition
	err := readOnly(ctx, s.db, func(tx pgx.Tx) error {
		var err error
		sec, err = scanSection(tx.QueryRow(ctx, `SELECT `+sectionColumns+` FROM academic_sections WHERE id = $1`, sectionID))
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT `+editionColumns+` FROM curriculum_editions WHERE section_id = $1
			ORDER BY id DESC LIMIT $2`, sectionID, MaxListed)
		if err != nil {
			return err
		}
		editions, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (Edition, error) { return scanEdition(r) })
		return err
	})
	if err != nil {
		return Section{}, nil, err
	}
	return sec, editions, nil
}

// Edition returns the edition with its assignments and chapter lists. An
// unknown edition is ErrNotFound. It performs no authorization.
func (s *Store) Edition(ctx context.Context, id int64) (Edition, error) {
	var e Edition
	err := readOnly(ctx, s.db, func(tx pgx.Tx) error {
		var err error
		e, err = loadEdition(ctx, tx, id)
		return err
	})
	return e, err
}

func loadEdition(ctx context.Context, tx pgx.Tx, id int64) (Edition, error) {
	e, err := scanEdition(tx.QueryRow(ctx, `SELECT `+editionColumns+` FROM curriculum_editions WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Edition{}, ErrNotFound
	}
	if err != nil {
		return Edition{}, fmt.Errorf("load edition: %w", err)
	}
	rows, err := tx.Query(ctx, `
		SELECT a.id, a.edition_id, a.subject_id, sj.code, curriculum_assignment_selectable(a.id)
		  FROM curriculum_assignments a JOIN subjects sj ON sj.id = a.subject_id
		 WHERE a.edition_id = $1 ORDER BY sj.code COLLATE "C", a.id`, id)
	if err != nil {
		return Edition{}, fmt.Errorf("load assignments: %w", err)
	}
	e.Assignments, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (Assignment, error) {
		var a Assignment
		err := r.Scan(&a.ID, &a.EditionID, &a.SubjectID, &a.SubjectCode, &a.Selectable)
		a.Chapters = []AssignedChapter{}
		return a, err
	})
	if err != nil {
		return Edition{}, fmt.Errorf("load assignments: %w", err)
	}
	rows, err = tx.Query(ctx, `
		SELECT ac.assignment_id, ac.chapter_id, c.code, ac.position, c.retired_at IS NOT NULL
		  FROM curriculum_assignment_chapters ac
		  JOIN curriculum_assignments a ON a.id = ac.assignment_id
		  JOIN chapters c ON c.id = ac.chapter_id
		 WHERE a.edition_id = $1 ORDER BY ac.assignment_id, ac.position`, id)
	if err != nil {
		return Edition{}, fmt.Errorf("load chapters: %w", err)
	}
	index := make(map[int64]int, len(e.Assignments))
	for i, a := range e.Assignments {
		index[a.ID] = i
	}
	var assignmentID int64
	var c AssignedChapter
	_, err = pgx.ForEachRow(rows, []any{&assignmentID, &c.ChapterID, &c.Code, &c.Position, &c.Retired}, func() error {
		if i, ok := index[assignmentID]; ok {
			e.Assignments[i].Chapters = append(e.Assignments[i].Chapters, c)
		}
		return nil
	})
	if err != nil {
		return Edition{}, fmt.Errorf("load chapters: %w", err)
	}
	if e.Assignments == nil {
		e.Assignments = []Assignment{}
	}
	return e, nil
}

// ---- levels and sections ----

// hierarchy describes the two tables that share the level/section logic.
type hierarchy struct {
	table, columns, target string
}

var (
	levelsTable   = hierarchy{"education_levels", levelColumns, "level"}
	sectionsTable = hierarchy{"academic_sections", sectionColumns, "section"}
)

// CreateLevel adds a level.
func (s *Store) CreateLevel(ctx context.Context, actor permissions.Actor, in Entry) (Level, error) {
	if err := validEntry(in.Code, Display{in.NameFR, in.NameAR, in.DisplayOrder}); err != nil {
		return Level{}, err
	}
	var l Level
	err := s.write(ctx, actor, func(tx pgx.Tx) error {
		var err error
		l, err = scanLevel(tx.QueryRow(ctx, `
			INSERT INTO education_levels (code, name_fr, name_ar, display_order) VALUES ($1, $2, $3, $4)
			ON CONFLICT (code) DO NOTHING RETURNING `+levelColumns, in.Code, in.NameFR, in.NameAR, in.DisplayOrder))
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrCodeExists
		}
		if err != nil {
			return err
		}
		l.Sections = []Section{}
		return audit(ctx, tx, actor, "level", l.ID, nil, "create", nil, entryJSON(in))
	})
	return l, err
}

// CreateSection adds a section to a level that is not retired.
func (s *Store) CreateSection(ctx context.Context, actor permissions.Actor, levelID int64, in Entry) (Section, error) {
	if err := validEntry(in.Code, Display{in.NameFR, in.NameAR, in.DisplayOrder}); err != nil {
		return Section{}, err
	}
	var sec Section
	err := s.write(ctx, actor, func(tx pgx.Tx) error {
		// FOR SHARE: a concurrent retirement of the level waits.
		var retired *time.Time
		err := tx.QueryRow(ctx, `SELECT retired_at FROM education_levels WHERE id = $1 FOR SHARE`, levelID).Scan(&retired)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if retired != nil {
			return ErrParentRetired
		}
		sec, err = scanSection(tx.QueryRow(ctx, `
			INSERT INTO academic_sections (level_id, code, name_fr, name_ar, display_order) VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT ON CONSTRAINT academic_sections_level_code_key DO NOTHING RETURNING `+sectionColumns,
			levelID, in.Code, in.NameFR, in.NameAR, in.DisplayOrder))
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrCodeExists
		}
		if err != nil {
			return err
		}
		return audit(ctx, tx, actor, "section", sec.ID, nil, "create", nil, entryJSON(in))
	})
	return sec, err
}

func entryJSON(in Entry) map[string]any {
	return map[string]any{"code": in.Code, "nameFr": in.NameFR, "nameAr": in.NameAR, "displayOrder": in.DisplayOrder}
}

// UpdateLevel changes a level's names and display order. Each changed aspect
// (names, order) gets its own audit row; an unchanged request writes nothing.
func (s *Store) UpdateLevel(ctx context.Context, actor permissions.Actor, id int64, expectedVersion int, d Display) (Level, error) {
	var l Level
	err := s.updateDisplay(ctx, actor, levelsTable, id, expectedVersion, d, func(r pgx.Row) error {
		var err error
		l, err = scanLevel(r)
		return err
	})
	return l, err
}

// UpdateSection changes a section's names and display order, like UpdateLevel.
func (s *Store) UpdateSection(ctx context.Context, actor permissions.Actor, id int64, expectedVersion int, d Display) (Section, error) {
	var sec Section
	err := s.updateDisplay(ctx, actor, sectionsTable, id, expectedVersion, d, func(r pgx.Row) error {
		var err error
		sec, err = scanSection(r)
		return err
	})
	return sec, err
}

func (s *Store) updateDisplay(ctx context.Context, actor permissions.Actor, h hierarchy, id int64, expectedVersion int, d Display, scan func(pgx.Row) error) error {
	if err := validDisplay(d); err != nil {
		return err
	}
	return s.write(ctx, actor, func(tx pgx.Tx) error {
		var cur Display
		var version int
		err := tx.QueryRow(ctx, `SELECT name_fr, name_ar, display_order, version FROM `+h.table+` WHERE id = $1 FOR NO KEY UPDATE`, id).
			Scan(&cur.NameFR, &cur.NameAR, &cur.DisplayOrder, &version)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if version != expectedVersion {
			return ErrVersionConflict
		}
		if cur == d {
			return scan(tx.QueryRow(ctx, `SELECT `+h.columns+` FROM `+h.table+` WHERE id = $1`, id))
		}
		if cur.NameFR != d.NameFR || cur.NameAR != d.NameAR {
			if err := audit(ctx, tx, actor, h.target, id, nil, "rename",
				map[string]any{"nameFr": cur.NameFR, "nameAr": cur.NameAR},
				map[string]any{"nameFr": d.NameFR, "nameAr": d.NameAR}); err != nil {
				return err
			}
		}
		if cur.DisplayOrder != d.DisplayOrder {
			if err := audit(ctx, tx, actor, h.target, id, nil, "reorder",
				map[string]any{"displayOrder": cur.DisplayOrder}, map[string]any{"displayOrder": d.DisplayOrder}); err != nil {
				return err
			}
		}
		return scan(tx.QueryRow(ctx, `UPDATE `+h.table+` SET name_fr = $2, name_ar = $3, display_order = $4, version = version + 1
			WHERE id = $1 RETURNING `+h.columns, id, d.NameFR, d.NameAR, d.DisplayOrder))
	})
}

// RetireLevel retires a level: no new section under it, and none of its
// editions' assignments stays selectable. Retirement is never undone.
func (s *Store) RetireLevel(ctx context.Context, actor permissions.Actor, id int64, expectedVersion int) (Level, error) {
	var l Level
	err := s.retireEntry(ctx, actor, levelsTable, id, expectedVersion, func(r pgx.Row) error {
		var err error
		l, err = scanLevel(r)
		return err
	})
	return l, err
}

// RetireSection retires a section, like RetireLevel.
func (s *Store) RetireSection(ctx context.Context, actor permissions.Actor, id int64, expectedVersion int) (Section, error) {
	var sec Section
	err := s.retireEntry(ctx, actor, sectionsTable, id, expectedVersion, func(r pgx.Row) error {
		var err error
		sec, err = scanSection(r)
		return err
	})
	return sec, err
}

func (s *Store) retireEntry(ctx context.Context, actor permissions.Actor, h hierarchy, id int64, expectedVersion int, scan func(pgx.Row) error) error {
	return s.write(ctx, actor, func(tx pgx.Tx) error {
		var retired *time.Time
		var version int
		err := tx.QueryRow(ctx, `SELECT retired_at, version FROM `+h.table+` WHERE id = $1 FOR NO KEY UPDATE`, id).Scan(&retired, &version)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if version != expectedVersion {
			return ErrVersionConflict
		}
		if retired != nil {
			return ErrAlreadyRetired
		}
		if err := scan(tx.QueryRow(ctx, `UPDATE `+h.table+` SET retired_at = now(), version = version + 1
			WHERE id = $1 RETURNING `+h.columns, id)); err != nil {
			return err
		}
		return audit(ctx, tx, actor, h.target, id, nil, "retire", nil, map[string]any{"retired": true})
	})
}

// ---- subjects and chapters ----

// RetireSubject retires a subject: no edition can newly assign it.
// Existing assignments, lessons and grants keep resolving. Unknown is
// ErrNotFound.
func (s *Store) RetireSubject(ctx context.Context, actor permissions.Actor, subjectID int64) error {
	return s.retireCatalogue(ctx, actor, "subjects", "subject", subjectID)
}

// RetireChapter retires a chapter: no chapter list can newly include it.
func (s *Store) RetireChapter(ctx context.Context, actor permissions.Actor, chapterID int64) error {
	return s.retireCatalogue(ctx, actor, "chapters", "chapter", chapterID)
}

func (s *Store) retireCatalogue(ctx context.Context, actor permissions.Actor, table, target string, id int64) error {
	return s.write(ctx, actor, func(tx pgx.Tx) error {
		var retired *time.Time
		err := tx.QueryRow(ctx, `SELECT retired_at FROM `+table+` WHERE id = $1 FOR NO KEY UPDATE`, id).Scan(&retired)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if retired != nil {
			return ErrAlreadyRetired
		}
		if _, err := tx.Exec(ctx, `UPDATE `+table+` SET retired_at = now() WHERE id = $1`, id); err != nil {
			return err
		}
		return audit(ctx, tx, actor, target, id, nil, "retire", nil, map[string]any{"retired": true})
	})
}

// ---- editions ----

// CreateEdition adds a draft edition to a section that, like its level, is
// not retired.
func (s *Store) CreateEdition(ctx context.Context, actor permissions.Actor, sectionID int64, in NewEdition) (Edition, error) {
	if !ValidCode(in.Code) {
		return Edition{}, fmt.Errorf("%w: code must be 1 to 63 lower-case letters, digits and hyphens, starting with a letter or digit", ErrInvalidInput)
	}
	if err := validLabels(in.LabelFR, in.LabelAR); err != nil {
		return Edition{}, err
	}
	var e Edition
	err := s.write(ctx, actor, func(tx pgx.Tx) error {
		if err := sectionOpen(ctx, tx, sectionID); err != nil {
			return err
		}
		var err error
		e, err = scanEdition(tx.QueryRow(ctx, `
			INSERT INTO curriculum_editions (section_id, code, label_fr, label_ar) VALUES ($1, $2, $3, $4)
			ON CONFLICT ON CONSTRAINT curriculum_editions_section_code_key DO NOTHING RETURNING `+editionColumns,
			sectionID, in.Code, in.LabelFR, in.LabelAR))
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrCodeExists
		}
		if err != nil {
			return err
		}
		e.Assignments = []Assignment{}
		return audit(ctx, tx, actor, "edition", e.ID, &e.ID, "create", nil,
			map[string]any{"sectionId": sectionID, "code": in.Code, "labelFr": in.LabelFR, "labelAr": in.LabelAR})
	})
	return e, err
}

// sectionOpen share-locks the section and its level and returns
// ErrNotFound or ErrParentRetired.
func sectionOpen(ctx context.Context, tx pgx.Tx, sectionID int64) error {
	var sectionRetired, levelRetired *time.Time
	err := tx.QueryRow(ctx, `
		SELECT s.retired_at, l.retired_at FROM academic_sections s JOIN education_levels l ON l.id = s.level_id
		 WHERE s.id = $1 FOR SHARE OF s, l`, sectionID).Scan(&sectionRetired, &levelRetired)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if sectionRetired != nil || levelRetired != nil {
		return ErrParentRetired
	}
	return nil
}

// lockEdition takes the edition FOR UPDATE and checks the expected version.
func lockEdition(ctx context.Context, tx pgx.Tx, id int64, expectedVersion int) (Edition, error) {
	e, err := scanEdition(tx.QueryRow(ctx, `SELECT `+editionColumns+` FROM curriculum_editions WHERE id = $1 FOR UPDATE`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Edition{}, ErrNotFound
	}
	if err != nil {
		return Edition{}, err
	}
	if e.Version != expectedVersion {
		return Edition{}, ErrVersionConflict
	}
	return e, nil
}

// RenameEdition changes an edition's labels (display metadata, in any state).
func (s *Store) RenameEdition(ctx context.Context, actor permissions.Actor, id int64, expectedVersion int, labelFR, labelAR string) (Edition, error) {
	if err := validLabels(labelFR, labelAR); err != nil {
		return Edition{}, err
	}
	var out Edition
	err := s.write(ctx, actor, func(tx pgx.Tx) error {
		e, err := lockEdition(ctx, tx, id, expectedVersion)
		if err != nil {
			return err
		}
		if e.LabelFR != labelFR || e.LabelAR != labelAR {
			if _, err := tx.Exec(ctx, `UPDATE curriculum_editions SET label_fr = $2, label_ar = $3, version = version + 1 WHERE id = $1`,
				id, labelFR, labelAR); err != nil {
				return err
			}
			if err := audit(ctx, tx, actor, "edition", id, &id, "rename",
				map[string]any{"labelFr": e.LabelFR, "labelAr": e.LabelAR},
				map[string]any{"labelFr": labelFR, "labelAr": labelAR}); err != nil {
				return err
			}
		}
		out, err = loadEdition(ctx, tx, id)
		return err
	})
	return out, err
}

// draftEdition locks a draft edition for an assignment edit and bumps its
// version, so concurrent edits of the same edition conflict.
func draftEdition(ctx context.Context, tx pgx.Tx, id int64, expectedVersion int) error {
	e, err := lockEdition(ctx, tx, id, expectedVersion)
	if errors.Is(err, ErrVersionConflict) {
		// A non-draft edition is reported as such even when its version moved.
		var st EditionState
		if qerr := tx.QueryRow(ctx, `SELECT state FROM curriculum_editions WHERE id = $1`, id).Scan(&st); qerr == nil && st != Draft {
			return ErrEditionNotDraft
		}
		return err
	}
	if err != nil {
		return err
	}
	if e.State != Draft {
		return ErrEditionNotDraft
	}
	_, err = tx.Exec(ctx, `UPDATE curriculum_editions SET version = version + 1 WHERE id = $1`, id)
	return err
}

// AddAssignment assigns a subject to a draft edition, with an empty chapter
// list. A retired subject is ErrPlacementInvalid.
func (s *Store) AddAssignment(ctx context.Context, actor permissions.Actor, editionID int64, expectedVersion int, subjectID int64) (Edition, Assignment, error) {
	var out Edition
	var assignmentID int64
	err := s.write(ctx, actor, func(tx pgx.Tx) error {
		if err := draftEdition(ctx, tx, editionID, expectedVersion); err != nil {
			return err
		}
		var retired *time.Time
		err := tx.QueryRow(ctx, `SELECT retired_at FROM subjects WHERE id = $1 FOR SHARE`, subjectID).Scan(&retired)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrUnknownSubject
		}
		if err != nil {
			return err
		}
		if retired != nil {
			return fmt.Errorf("%w: subject %d is retired", ErrPlacementInvalid, subjectID)
		}
		err = tx.QueryRow(ctx, `INSERT INTO curriculum_assignments (edition_id, subject_id) VALUES ($1, $2)
			ON CONFLICT ON CONSTRAINT curriculum_assignments_edition_subject_key DO NOTHING RETURNING id`,
			editionID, subjectID).Scan(&assignmentID)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrAssignmentExists
		}
		if err != nil {
			return err
		}
		if err := audit(ctx, tx, actor, "assignment", assignmentID, &editionID, "add_assignment", nil,
			map[string]any{"subjectId": subjectID}); err != nil {
			return err
		}
		out, err = loadEdition(ctx, tx, editionID)
		return err
	})
	if err != nil {
		return Edition{}, Assignment{}, err
	}
	for _, a := range out.Assignments {
		if a.ID == assignmentID {
			return out, a, nil
		}
	}
	return out, Assignment{}, fmt.Errorf("assignment %d missing after insert", assignmentID)
}

// assignmentOf returns the subject of an assignment of the edition, or
// ErrNotFound.
func assignmentOf(ctx context.Context, tx pgx.Tx, editionID, assignmentID int64) (int64, []int64, error) {
	var subjectID int64
	err := tx.QueryRow(ctx, `SELECT subject_id FROM curriculum_assignments WHERE id = $1 AND edition_id = $2`,
		assignmentID, editionID).Scan(&subjectID)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil, ErrNotFound
	}
	if err != nil {
		return 0, nil, err
	}
	rows, err := tx.Query(ctx, `SELECT chapter_id FROM curriculum_assignment_chapters WHERE assignment_id = $1 ORDER BY position`, assignmentID)
	if err != nil {
		return 0, nil, err
	}
	chapters, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if chapters == nil {
		chapters = []int64{}
	}
	return subjectID, chapters, err
}

// RemoveAssignment removes an assignment, and its chapter list, from a
// draft edition.
func (s *Store) RemoveAssignment(ctx context.Context, actor permissions.Actor, editionID, assignmentID int64, expectedVersion int) (Edition, error) {
	var out Edition
	err := s.write(ctx, actor, func(tx pgx.Tx) error {
		if err := draftEdition(ctx, tx, editionID, expectedVersion); err != nil {
			return err
		}
		subjectID, chapters, err := assignmentOf(ctx, tx, editionID, assignmentID)
		if err != nil {
			return err
		}
		if err := audit(ctx, tx, actor, "assignment", assignmentID, &editionID, "remove_assignment",
			map[string]any{"subjectId": subjectID, "chapterIds": chapters}, nil); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM curriculum_assignments WHERE id = $1`, assignmentID); err != nil {
			return err
		}
		out, err = loadEdition(ctx, tx, editionID)
		return err
	})
	return out, err
}

// SetChapters replaces an assignment's chapter list with chapterIDs, in
// that order (positions 1..n). Every chapter must exist in the assignment's
// subject, not be retired and appear once; otherwise ErrPlacementInvalid.
func (s *Store) SetChapters(ctx context.Context, actor permissions.Actor, editionID, assignmentID int64, expectedVersion int, chapterIDs []int64) (Edition, error) {
	if len(chapterIDs) > MaxChapters {
		return Edition{}, fmt.Errorf("%w: at most %d chapters per assignment", ErrInvalidInput, MaxChapters)
	}
	seen := make(map[int64]bool, len(chapterIDs))
	for _, id := range chapterIDs {
		if seen[id] {
			return Edition{}, fmt.Errorf("%w: chapter %d is listed twice", ErrPlacementInvalid, id)
		}
		seen[id] = true
	}
	var out Edition
	err := s.write(ctx, actor, func(tx pgx.Tx) error {
		if err := draftEdition(ctx, tx, editionID, expectedVersion); err != nil {
			return err
		}
		subjectID, before, err := assignmentOf(ctx, tx, editionID, assignmentID)
		if err != nil {
			return err
		}
		if len(chapterIDs) > 0 {
			rows, err := tx.Query(ctx, `SELECT id, subject_id, retired_at IS NOT NULL FROM chapters WHERE id = ANY ($1) FOR SHARE`, chapterIDs)
			if err != nil {
				return err
			}
			type chapterRow struct {
				subject int64
				retired bool
			}
			found := map[int64]chapterRow{}
			var id, subject int64
			var retired bool
			if _, err := pgx.ForEachRow(rows, []any{&id, &subject, &retired}, func() error {
				found[id] = chapterRow{subject, retired}
				return nil
			}); err != nil {
				return err
			}
			for _, id := range chapterIDs {
				c, ok := found[id]
				switch {
				case !ok || c.subject != subjectID:
					return fmt.Errorf("%w: chapter %d is not a chapter of the assignment's subject", ErrPlacementInvalid, id)
				case c.retired:
					return fmt.Errorf("%w: chapter %d is retired", ErrPlacementInvalid, id)
				}
			}
		}
		if err := audit(ctx, tx, actor, "assignment", assignmentID, &editionID, "set_chapters",
			map[string]any{"chapterIds": before}, map[string]any{"chapterIds": nonNil(chapterIDs)}); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM curriculum_assignment_chapters WHERE assignment_id = $1`, assignmentID); err != nil {
			return err
		}
		if len(chapterIDs) > 0 {
			if _, err := tx.Exec(ctx, `
				INSERT INTO curriculum_assignment_chapters (assignment_id, subject_id, chapter_id, position)
				SELECT $1, $2, c.id, c.n FROM unnest($3::bigint[]) WITH ORDINALITY AS c (id, n)`,
				assignmentID, subjectID, chapterIDs); err != nil {
				return err
			}
		}
		out, err = loadEdition(ctx, tx, editionID)
		return err
	})
	return out, err
}

func nonNil(ids []int64) []int64 {
	if ids == nil {
		return []int64{}
	}
	return ids
}

// MakeCurrent makes a draft edition its section's current edition; the
// previous current edition, if any, becomes superseded in the same
// transaction. The section's current edition and the target are locked FOR
// UPDATE in ascending ID. expectedCurrent names the edition the caller
// expects to supersede (nil: the section has no current edition); any other
// current edition is ErrStateConflict, so of two racing make-current
// requests made from the same view, one wins and the other changes nothing
// (ErrStateConflict, or ErrEditionNotDraft when both targeted the same
// edition). It returns the edition and the ID of the superseded edition, if
// any.
func (s *Store) MakeCurrent(ctx context.Context, actor permissions.Actor, id int64, expectedVersion int, expectedCurrent *int64) (Edition, *int64, error) {
	var out Edition
	var superseded *int64
	err := s.write(ctx, actor, func(tx pgx.Tx) error {
		var sectionID int64
		err := tx.QueryRow(ctx, `SELECT section_id FROM curriculum_editions WHERE id = $1`, id).Scan(&sectionID)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT id, state, version FROM curriculum_editions
			WHERE section_id = $1 AND (state = 'current' OR id = $2) ORDER BY id FOR UPDATE`, sectionID, id)
		if err != nil {
			return err
		}
		var targetState EditionState
		targetVersion := -1
		var previous *int64
		var eid int64
		var st EditionState
		var version int
		if _, err := pgx.ForEachRow(rows, []any{&eid, &st, &version}, func() error {
			switch {
			case eid == id:
				targetState, targetVersion = st, version
			case st == Current:
				p := eid
				previous = &p
			}
			return nil
		}); err != nil {
			return err
		}
		if targetVersion < 0 {
			return ErrNotFound
		}
		if targetState != Draft {
			return ErrEditionNotDraft
		}
		if targetVersion != expectedVersion {
			return ErrVersionConflict
		}
		// A make-current that committed while this one waited may have made
		// an edition current that this transaction has not locked.
		var now *int64
		err = tx.QueryRow(ctx, `SELECT id FROM curriculum_editions WHERE section_id = $1 AND state = 'current'`, sectionID).Scan(&now)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if !sameID(now, previous) || !sameID(previous, expectedCurrent) {
			return ErrStateConflict
		}
		if err := sectionOpen(ctx, tx, sectionID); err != nil {
			return err
		}
		if previous != nil {
			if _, err := tx.Exec(ctx, `UPDATE curriculum_editions SET state = 'superseded', superseded_at = now(), version = version + 1
				WHERE id = $1`, *previous); err != nil {
				return err
			}
			if err := audit(ctx, tx, actor, "edition", *previous, previous, "supersede",
				map[string]any{"state": "current"}, map[string]any{"state": "superseded", "supersededBy": id}); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE curriculum_editions SET state = 'current', current_since = now(), version = version + 1
			WHERE id = $1`, id); err != nil {
			return err
		}
		before := map[string]any{"state": "draft"}
		if previous != nil {
			before["previousCurrentId"] = *previous
		}
		if err := audit(ctx, tx, actor, "edition", id, &id, "make_current", before, map[string]any{"state": "current"}); err != nil {
			return err
		}
		superseded = previous
		out, err = loadEdition(ctx, tx, id)
		return err
	})
	if err != nil {
		return Edition{}, nil, err
	}
	return out, superseded, nil
}

// RetireEdition retires a current or superseded edition (a draft cannot be
// retired: ErrStateConflict). Retiring the current edition leaves the
// section without one.
func (s *Store) RetireEdition(ctx context.Context, actor permissions.Actor, id int64, expectedVersion int) (Edition, error) {
	var out Edition
	err := s.write(ctx, actor, func(tx pgx.Tx) error {
		e, err := lockEdition(ctx, tx, id, expectedVersion)
		if err != nil {
			return err
		}
		switch e.State {
		case Retired:
			return ErrAlreadyRetired
		case Draft:
			return ErrStateConflict
		}
		if _, err := tx.Exec(ctx, `UPDATE curriculum_editions SET state = 'retired', retired_at = now(), version = version + 1
			WHERE id = $1`, id); err != nil {
			return err
		}
		if err := audit(ctx, tx, actor, "edition", id, &id, "retire",
			map[string]any{"state": string(e.State)}, map[string]any{"state": "retired"}); err != nil {
			return err
		}
		out, err = loadEdition(ctx, tx, id)
		return err
	})
	return out, err
}

func sameID(a, b *int64) bool {
	return (a == nil && b == nil) || (a != nil && b != nil && *a == *b)
}
