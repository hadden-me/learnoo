// Package courses stores teacher-owned courses and their draft revisions
// (LMS-B03, migrations/0014_courses.sql, docs/LMS_CONTRACT.md §4 and §5): a
// course identity (owner, subject, language), its revisions with overview
// metadata and cover, their curriculum placements, ordered modules and
// ordered steps pinning lesson revisions.
//
// This slice covers the owner's drafts: create a course with its first
// draft, list owned courses, start a draft, read, save and discard a draft,
// delete a never-submitted course, search attachable lessons and list a
// lesson's eligible revisions. Submission, review, publication and the rest
// of the lifecycle are LMS-B04.
//
// Every write runs in one READ COMMITTED transaction that first rechecks the
// author permission (permissions.RequireTx, share-locking the grant and
// account rows), then takes its locks in the contract §2 order: curriculum
// editions (ascending ID), lessons (ascending ID) and the pinned lesson
// revisions (ascending ID), the course, then the course revision. Another
// author's course or revision is reported as ErrNotFound, exactly like a
// missing one, so IDs cannot be probed. The database guards enforce the
// draft-only, ownership, cover and candidate rules for every writer; this
// package adds the checks that need the caller (permission, ownership,
// optimistic versions) and the attachment and placement rules.
package courses

import (
	"context"
	"crypto/rand"
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

// Field limits (contract §4), in characters.
const (
	MaxTitle         = 200
	MaxSummary       = 1000
	MaxObjectives    = 2000
	MaxPrerequisites = 2000
	MaxCoverAlt      = 300
	MaxModules       = 50
	MaxSteps         = 200
	// MaxAssignments bounds one revision's placements defensively.
	MaxAssignments = 50
	// OwnedPage is the page size of Owned.
	OwnedPage = 100
	// AttachablePage is the page size of AttachableLessons.
	AttachablePage = 50
	// MaxQueryRunes bounds an attachable-lesson search query.
	MaxQueryRunes = 100
)

var (
	// ErrDenied is permissions.ErrDenied: missing author permission.
	ErrDenied = permissions.ErrDenied
	// ErrNotFound means the course, revision or lesson does not exist or
	// belongs to someone else.
	ErrNotFound = errors.New("not found")
	// ErrNotDraft means the revision was submitted and can no longer change.
	ErrNotDraft = errors.New("course revision is not an editable draft")
	// ErrVersionConflict means the draft changed since the caller loaded it.
	ErrVersionConflict = errors.New("draft was changed since it was loaded")
	// ErrActiveCandidateExists means the course already has a draft, a
	// revision in review or an approved revision awaiting publication.
	ErrActiveCandidateExists = errors.New("course already has an active candidate")
	// ErrHasHistory means a revision of the course was submitted (or the
	// course was published), so the course is never deleted.
	ErrHasHistory = errors.New("course has submitted history")
	// ErrArchived means the course is archived and accepts no new drafts.
	ErrArchived = errors.New("course is archived")
	// ErrInvalidInput wraps a field-limit or format failure.
	ErrInvalidInput = errors.New("invalid input")
	// ErrInvalidStep wraps a step that cannot be attached.
	ErrInvalidStep = errors.New("invalid step")
	// ErrPlacementInvalid wraps a placement the curriculum rules refuse.
	ErrPlacementInvalid = errors.New("placement invalid")
	// ErrUnknownSubject means the subject does not exist.
	ErrUnknownSubject = errors.New("unknown subject")
	// ErrUnknownMedia means the cover is not an image the caller uploaded.
	ErrUnknownMedia = errors.New("unknown media asset")
	// ErrRetry means the course changed between the unlocked read that
	// chose the locks and the locked re-read; nothing was written.
	ErrRetry = errors.New("course changed concurrently; retry")
	// ErrImmutable reports a change rejected by a database guard that the
	// store's own checks did not catch first.
	ErrImmutable = errors.New("change rejected by a database guard")
)

// Status is a course revision's workflow state.
type Status string

const (
	StatusDraft            Status = "draft"
	StatusInReview         Status = "in_review"
	StatusChangesRequested Status = "changes_requested"
	StatusApproved         Status = "approved"
)

// Course is a course identity.
type Course struct {
	ID                  int64
	PublicID            string
	OwnerID             int64
	SubjectID           int64
	Language            string
	PublishedRevisionID *int64
	ArchivedAt          *time.Time
	CreatedAt           time.Time
	// Suspended: not archived, no published revision, and the last pointer
	// change was a suspension (LMS-B04).
	Suspended bool
}

// suspendedSQL computes Course.Suspended for the courses row c.
const suspendedSQL = `(c.published_revision_id IS NULL AND c.archived_at IS NULL AND coalesce((
	SELECT a.action = 'suspend' FROM course_publication_audit a WHERE a.course_id = c.id ORDER BY a.id DESC LIMIT 1), false))`

// Revision is a stored course revision with its children.
type Revision struct {
	ID                int64
	CourseID          int64
	Number            int
	Status            Status
	Kind              string
	AuthorID          int64
	BaseRevisionID    *int64
	CompareRevisionID *int64
	SubjectID         int64
	Language          string
	Title             string
	Summary           string
	Objectives        string
	Prerequisites     string
	CoverMediaID      *int64
	CoverAlt          *string
	Version           int
	CreatedAt         time.Time
	UpdatedAt         time.Time
	SubmittedAt       *time.Time
	// WithdrawnAt is set once an administrator withdrew the revision.
	WithdrawnAt *time.Time
	// Decision is the review decision, nil until decided.
	Decision *Decision
	// Blocked is set, for a revision in review that no active reviewer of
	// its subject may decide, with the disqualifications.
	Blocked     *Blocked
	Assignments []Placement
	Modules     []Module
	// DroppedSteps lists, only on the result of CreateDraft, the steps of
	// the copied revision that were left out because another author's
	// lesson is no longer attachable at its pinned revision.
	DroppedSteps []DroppedStep
}

// DroppedStep is a step CreateDraft could not copy.
type DroppedStep struct {
	ModuleKey  string
	Position   int
	Title      string
	LessonID   int64
	RevisionID int64
}

// Placement is one curriculum assignment a revision selects, with the
// state of its edition: Selectable is curriculum_assignment_selectable now;
// Retired means its edition, section or level is retired (flagged on save,
// refused at submission).
type Placement struct {
	AssignmentID int64
	EditionID    int64
	EditionState string
	Selectable   bool
	Retired      bool
}

// Module is an ordered module of a revision.
type Module struct {
	Key       string
	Position  int
	Title     string
	ChapterID *int64
	Steps     []Step
}

// Step pins one lesson revision. Only the course-authored title is
// returned, never the lesson revision's own title. OwnLesson means the
// lesson's owner is the course's owner; Eligible is
// course_step_revision_eligible now.
type Step struct {
	Position       int
	Title          string
	LessonID       int64
	RevisionID     int64
	RevisionNumber int
	OwnLesson      bool
	Eligible       bool
}

// Draft is the full content of a draft save: every field is replaced.
type Draft struct {
	Title         string
	Summary       string
	Objectives    string
	Prerequisites string
	CoverMediaID  *int64
	CoverAlt      *string
	AssignmentIDs []int64
	Modules       []ModuleInput
}

// ModuleInput is one module of a save, in order. An empty Key creates a new
// module key; an existing key keeps the module's identity across revisions.
type ModuleInput struct {
	Key       string
	Title     string
	ChapterID *int64
	Steps     []StepInput
}

// StepInput is one step of a module, in order.
type StepInput struct {
	LessonID   int64
	RevisionID int64
	Title      string
}

// Store reads and writes courses.
type Store struct{ db *pgxpool.Pool }

// NewStore builds a Store.
func NewStore(db *pgxpool.Pool) *Store { return &Store{db: db} }

// ---- validation ----

// NormalizeText applies the course text rule (course_valid_text): valid
// UTF-8, CR and CRLF become LF, surrounding white space is trimmed, at most
// max characters, no control characters (line feeds only when multiline),
// bidirectional formatting characters or line/paragraph separators. It is
// the error-report rule of internal/reports without the non-empty
// requirement.
func NormalizeText(s string, max int, multiline bool) (string, bool) {
	if !utf8.ValidString(s) {
		return "", false
	}
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	s = strings.TrimSpace(s)
	if utf8.RuneCountInString(s) > max {
		return "", false
	}
	for _, r := range s {
		if r == '\n' {
			if !multiline {
				return "", false
			}
			continue
		}
		if unicode.IsControl(r) || unicode.Is(unicode.Bidi_Control, r) || r == ' ' || r == ' ' {
			return "", false
		}
	}
	return s, true
}

var keyPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

func newKey() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{ErrInvalidInput}, args...)...)
}

func invalidStep(format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{ErrInvalidStep}, args...)...)
}

func placementInvalid(format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{ErrPlacementInvalid}, args...)...)
}

// prepare validates and normalizes d, assigning keys to new modules.
func prepare(d Draft) (Draft, error) {
	var ok bool
	fields := []struct {
		name      string
		v         *string
		max       int
		multiline bool
	}{
		{"title", &d.Title, MaxTitle, false},
		{"summary", &d.Summary, MaxSummary, true},
		{"objectives", &d.Objectives, MaxObjectives, true},
		{"prerequisites", &d.Prerequisites, MaxPrerequisites, true},
	}
	for _, f := range fields {
		if *f.v, ok = NormalizeText(*f.v, f.max, f.multiline); !ok {
			return d, invalid("%s must be plain text of at most %d characters", f.name, f.max)
		}
	}
	if d.CoverAlt != nil {
		alt, ok := NormalizeText(*d.CoverAlt, MaxCoverAlt, false)
		if !ok {
			return d, invalid("coverAlt must be one line of plain text of at most %d characters", MaxCoverAlt)
		}
		d.CoverAlt = &alt
		if alt == "" {
			d.CoverAlt = nil
		}
	}
	switch {
	case d.CoverMediaID != nil && d.CoverAlt == nil:
		return d, invalid("a cover image needs coverAlt")
	case d.CoverMediaID == nil && d.CoverAlt != nil:
		return d, invalid("coverAlt is set without a cover image")
	case d.CoverMediaID != nil && *d.CoverMediaID <= 0:
		return d, ErrUnknownMedia
	}
	if len(d.AssignmentIDs) > MaxAssignments {
		return d, placementInvalid("at most %d curriculum assignments", MaxAssignments)
	}
	seenA := map[int64]bool{}
	for _, id := range d.AssignmentIDs {
		if id <= 0 || seenA[id] {
			return d, placementInvalid("assignment %d is unknown or listed twice", id)
		}
		seenA[id] = true
	}
	if len(d.Modules) > MaxModules {
		return d, invalid("at most %d modules", MaxModules)
	}
	modules := make([]ModuleInput, len(d.Modules))
	seenK := map[string]bool{}
	seenL := map[int64]bool{}
	steps := 0
	for i, m := range d.Modules {
		if m.Key == "" {
			m.Key = newKey()
		} else if !keyPattern.MatchString(m.Key) {
			return d, invalid("module %d: key must be a lower-case UUID", i+1)
		}
		if seenK[m.Key] {
			return d, invalid("module %d: key is used twice", i+1)
		}
		seenK[m.Key] = true
		if m.Title, ok = NormalizeText(m.Title, MaxTitle, false); !ok || m.Title == "" {
			return d, invalid("module %d: title must be one line of 1 to %d characters", i+1, MaxTitle)
		}
		if m.ChapterID != nil && *m.ChapterID <= 0 {
			return d, placementInvalid("module %d: unknown chapter", i+1)
		}
		ss := make([]StepInput, len(m.Steps))
		for j, s := range m.Steps {
			steps++
			if steps > MaxSteps {
				return d, invalid("at most %d steps", MaxSteps)
			}
			if s.LessonID <= 0 || s.RevisionID <= 0 {
				return d, invalidStep("module %d, step %d: unknown lesson or revision", i+1, j+1)
			}
			if seenL[s.LessonID] {
				return d, invalidStep("lesson %d is attached twice", s.LessonID)
			}
			seenL[s.LessonID] = true
			if s.Title, ok = NormalizeText(s.Title, MaxTitle, false); !ok || s.Title == "" {
				return d, invalid("module %d, step %d: title must be one line of 1 to %d characters", i+1, j+1, MaxTitle)
			}
			ss[j] = s
		}
		m.Steps = ss
		modules[i] = m
	}
	d.Modules = modules
	return d, nil
}

// ---- transactions and errors ----

func (s *Store) write(ctx context.Context, actor int64, fn func(tx pgx.Tx) error) error {
	err := pgx.BeginTxFunc(ctx, s.db, pgx.TxOptions{IsoLevel: pgx.ReadCommitted}, func(tx pgx.Tx) error {
		if err := permissions.RequireTx(ctx, tx, actor, permissions.Author); err != nil {
			return err
		}
		return fn(tx)
	})
	return mapErr(err)
}

func (s *Store) read(ctx context.Context, actor int64, fn func(tx pgx.Tx) error) error {
	err := pgx.BeginTxFunc(ctx, s.db, pgx.TxOptions{IsoLevel: pgx.RepeatableRead}, func(tx pgx.Tx) error {
		if err := permissions.RequireTx(ctx, tx, actor, permissions.Author); err != nil {
			return err
		}
		return fn(tx)
	})
	return mapErr(err)
}

// mapErr turns constraint and guard violations into package errors. The
// store checks every rule first under its locks; these mappings cover
// races with direct writers.
func mapErr(err error) error {
	var pgErr *pgconn.PgError
	if err == nil || !errors.As(err, &pgErr) {
		return err
	}
	switch pgErr.Code {
	case "23505":
		switch pgErr.ConstraintName {
		case "course_revisions_one_active_candidate":
			return errors.Join(ErrActiveCandidateExists, err)
		case "course_steps_pkey":
			return errors.Join(ErrInvalidStep, err)
		}
	case "23503":
		switch pgErr.ConstraintName {
		case "course_steps_lesson_revision_fkey", "course_steps_lesson_fkey":
			return errors.Join(ErrInvalidStep, err)
		case "course_revisions_cover_media_id_fkey":
			return errors.Join(ErrUnknownMedia, err)
		case "courses_subject_id_fkey":
			return errors.Join(ErrUnknownSubject, err)
		case "course_modules_chapter_fkey", "course_revision_assignments_assignment_fkey":
			return errors.Join(ErrPlacementInvalid, err)
		}
	case "23514", "22021", "22P05":
		return errors.Join(ErrInvalidInput, err)
	case "LRN01":
		if strings.Contains(pgErr.Message, "is not eligible") {
			return errors.Join(ErrInvalidStep, err)
		}
		if strings.Contains(pgErr.Message, "changed concurrently") {
			return errors.Join(ErrRetry, err)
		}
		return errors.Join(ErrImmutable, err)
	}
	return err
}

// ownedCourseOf reads, without locks, the course of a revision and checks
// the caller owns it. It only chooses which rows to lock; every rule is
// rechecked under the locks. owner_id changes only through B04's transfer.
func ownedCourseOf(ctx context.Context, tx pgx.Tx, actor, revisionID int64) (int64, error) {
	var course, owner int64
	err := tx.QueryRow(ctx, `
		SELECT r.course_id, c.owner_id FROM course_revisions r JOIN courses c ON c.id = r.course_id
		 WHERE r.id = $1`, revisionID).Scan(&course, &owner)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && owner != actor) {
		return 0, ErrNotFound
	}
	return course, err
}

func ownedCourse(ctx context.Context, tx pgx.Tx, actor, courseID int64) error {
	var owner int64
	err := tx.QueryRow(ctx, `SELECT owner_id FROM courses WHERE id = $1`, courseID).Scan(&owner)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && owner != actor) {
		return ErrNotFound
	}
	return err
}

type lockedCourse struct {
	subject   int64
	language  string
	published *int64
	archived  bool
	last      int
}

// lockCourse locks the course (mode FOR SHARE or FOR UPDATE) and rechecks
// the owner.
func lockCourse(ctx context.Context, tx pgx.Tx, actor, courseID int64, mode string) (lockedCourse, error) {
	var c lockedCourse
	var owner int64
	err := tx.QueryRow(ctx, `
		SELECT owner_id, subject_id, language, published_revision_id, archived_at IS NOT NULL, last_revision_number
		  FROM courses WHERE id = $1 `+mode, courseID).Scan(&owner, &c.subject, &c.language, &c.published, &c.archived, &c.last)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && owner != actor) {
		return c, ErrNotFound
	}
	return c, err
}

// lockDependencies takes, in contract §2 order and all FOR SHARE in
// ascending ID: the editions of the given assignments, then (for a save,
// which newly selects them) the sections, levels and subjects of those
// assignments and the given module chapters, so a concurrent retirement
// waits for the save or is seen by it, then the lessons and the pinned
// lesson revisions.
func lockDependencies(ctx context.Context, tx pgx.Tx, assignments, chapters, lessonIDs, revisionIDs []int64) error {
	type lock struct {
		ids []int64
		sql string
	}
	for _, l := range []lock{
		{assignments, `SELECT id FROM curriculum_editions
			 WHERE id IN (SELECT edition_id FROM curriculum_assignments WHERE id = ANY ($1))
			 ORDER BY id FOR SHARE`},
		{assignments, `SELECT id FROM academic_sections
			 WHERE id IN (SELECT e.section_id FROM curriculum_assignments a JOIN curriculum_editions e ON e.id = a.edition_id
			               WHERE a.id = ANY ($1))
			 ORDER BY id FOR SHARE`},
		{assignments, `SELECT id FROM education_levels
			 WHERE id IN (SELECT s.level_id FROM curriculum_assignments a JOIN curriculum_editions e ON e.id = a.edition_id
			                JOIN academic_sections s ON s.id = e.section_id WHERE a.id = ANY ($1))
			 ORDER BY id FOR SHARE`},
		{assignments, `SELECT id FROM subjects
			 WHERE id IN (SELECT subject_id FROM curriculum_assignments WHERE id = ANY ($1))
			 ORDER BY id FOR SHARE`},
		{chapters, `SELECT id FROM chapters WHERE id = ANY ($1) ORDER BY id FOR SHARE`},
		{lessonIDs, `SELECT id FROM lessons WHERE id = ANY ($1) ORDER BY id FOR SHARE`},
		{revisionIDs, `SELECT id FROM lesson_revisions WHERE id = ANY ($1) ORDER BY id FOR SHARE`},
	} {
		if len(l.ids) == 0 {
			continue
		}
		if _, err := tx.Exec(ctx, l.sql, l.ids); err != nil {
			return err
		}
	}
	return nil
}

// ---- courses ----

// CreateCourse creates a course of subjectID in language, owned by actor,
// and its first, empty draft.
func (s *Store) CreateCourse(ctx context.Context, actor, subjectID int64, language string) (Course, Revision, error) {
	var c Course
	var rev Revision
	if language != "fr" && language != "ar" {
		return c, rev, invalid("language must be fr or ar")
	}
	err := s.write(ctx, actor, func(tx pgx.Tx) error {
		var retired bool
		err := tx.QueryRow(ctx, `SELECT retired_at IS NOT NULL FROM subjects WHERE id = $1 FOR SHARE`, subjectID).Scan(&retired)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrUnknownSubject
		}
		if err != nil {
			return err
		}
		if retired {
			return placementInvalid("the subject is retired")
		}
		var id int64
		if err := tx.QueryRow(ctx, `
			INSERT INTO courses (owner_id, subject_id, language, created_at, created_by)
			VALUES ($1, $2, $3, now(), $1) RETURNING id`, actor, subjectID, language).Scan(&id); err != nil {
			return err
		}
		var revID int64
		if err := tx.QueryRow(ctx, `
			INSERT INTO course_revisions (course_id, subject_id, language, revision_number, author_id, created_at, updated_at)
			VALUES ($1, $2, $3, 1, $4, now(), now()) RETURNING id`, id, subjectID, language, actor).Scan(&revID); err != nil {
			return err
		}
		if c, err = loadCourse(ctx, tx, id); err != nil {
			return err
		}
		rev, err = loadRevision(ctx, tx, revID)
		return err
	})
	return c, rev, err
}

func loadCourse(ctx context.Context, tx pgx.Tx, id int64) (Course, error) {
	var c Course
	err := tx.QueryRow(ctx, `
		SELECT c.id, c.public_id::text, c.owner_id, c.subject_id, c.language, c.published_revision_id, c.archived_at, c.created_at,
		       `+suspendedSQL+`
		  FROM courses c WHERE c.id = $1`, id).Scan(&c.ID, &c.PublicID, &c.OwnerID, &c.SubjectID, &c.Language,
		&c.PublishedRevisionID, &c.ArchivedAt, &c.CreatedAt, &c.Suspended)
	if errors.Is(err, pgx.ErrNoRows) {
		return c, ErrNotFound
	}
	return c, err
}

// OwnedCourse is a course in its owner's listing with its latest revision.
type OwnedCourse struct {
	Course
	Latest *RevisionSummary
}

// RevisionSummary summarizes a revision.
type RevisionSummary struct {
	ID        int64
	Number    int
	Status    Status
	Title     string
	Version   int
	UpdatedAt time.Time
	// Withdrawn by an administrator (LMS-B04).
	Withdrawn bool
}

// Owned pages through the courses actor owns, newest first (by course
// ID): before is the last course ID of the previous page (0 for the
// first). It returns at most OwnedPage courses and the cursor of the next
// page, or nil. Administrators get no exception.
func (s *Store) Owned(ctx context.Context, actor, before int64) ([]OwnedCourse, *int64, error) {
	var out []OwnedCourse
	err := s.read(ctx, actor, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT c.id, c.public_id::text, c.owner_id, c.subject_id, c.language, c.published_revision_id, c.archived_at, c.created_at,
			       `+suspendedSQL+`, r.id, r.revision_number, r.status, r.title, r.version, r.updated_at, r.withdrawn_at IS NOT NULL
			  FROM courses c
			  LEFT JOIN LATERAL (SELECT id, revision_number, status, title, version, updated_at, withdrawn_at FROM course_revisions
			                      WHERE course_id = c.id ORDER BY revision_number DESC LIMIT 1) r ON true
			 WHERE c.owner_id = $1 AND ($2 = 0 OR c.id < $2)
			 ORDER BY c.id DESC
			 LIMIT $3`, actor, before, OwnedPage+1)
		if err != nil {
			return err
		}
		out, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (OwnedCourse, error) {
			var o OwnedCourse
			var id *int64
			var number, version *int
			var status, title *string
			var updated *time.Time
			var withdrawn *bool
			err := row.Scan(&o.ID, &o.PublicID, &o.OwnerID, &o.SubjectID, &o.Language, &o.PublishedRevisionID, &o.ArchivedAt,
				&o.CreatedAt, &o.Suspended, &id, &number, &status, &title, &version, &updated, &withdrawn)
			if err == nil && id != nil {
				o.Latest = &RevisionSummary{ID: *id, Number: *number, Status: Status(*status), Title: *title, Version: *version, UpdatedAt: *updated, Withdrawn: *withdrawn}
			}
			return o, err
		})
		return err
	})
	if err != nil {
		return nil, nil, err
	}
	var next *int64
	if len(out) > OwnedPage {
		out = out[:OwnedPage]
		id := out[len(out)-1].ID
		next = &id
	}
	return out, next, nil
}

// DeleteCourse permanently deletes actor's course with its draft revisions,
// placements, modules and steps, only while none of its revisions was ever
// submitted and it was never published (ErrHasHistory otherwise). Lessons
// and media are never touched.
func (s *Store) DeleteCourse(ctx context.Context, actor, courseID int64) error {
	return s.write(ctx, actor, func(tx pgx.Tx) error {
		c, err := lockCourse(ctx, tx, actor, courseID, "FOR UPDATE")
		if err != nil {
			return err
		}
		var history bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM course_revisions WHERE course_id = $1 AND status <> 'draft')
			OR EXISTS (SELECT 1 FROM course_lifecycle_audit WHERE course_id = $1)`,
			courseID).Scan(&history); err != nil {
			return err
		}
		if history || c.published != nil {
			return ErrHasHistory
		}
		_, err = tx.Exec(ctx, `DELETE FROM courses WHERE id = $1`, courseID)
		return err
	})
}

// ---- revisions ----

// CreateDraft starts a new draft of actor's course. It copies the course's
// latest revision (overview, cover if actor uploaded it, placements, modules
// and the steps still attachable, the others listed in DroppedSteps) or,
// without one, starts empty. Its base is the published pointer and its
// comparison course_comparison_revision (the pointer, or while suspended or
// archived the last published or restored revision not withdrawn). While the course has an active candidate it returns
// ErrActiveCandidateExists. The copy is not validated: a step or placement
// that is no longer allowed is reported by the next save.
func (s *Store) CreateDraft(ctx context.Context, actor, courseID int64) (Revision, error) {
	var rev Revision
	err := s.write(ctx, actor, func(tx pgx.Tx) error {
		if err := ownedCourse(ctx, tx, actor, courseID); err != nil {
			return err
		}
		source, err := latestRevision(ctx, tx, courseID)
		if err != nil {
			return err
		}
		var assignments, lessonIDs, revisionIDs []int64
		if source != nil {
			if err := tx.QueryRow(ctx, `
				SELECT ARRAY(SELECT assignment_id FROM course_revision_assignments WHERE course_revision_id = $1),
				       ARRAY(SELECT lesson_id FROM course_steps WHERE course_revision_id = $1),
				       ARRAY(SELECT lesson_revision_id FROM course_steps WHERE course_revision_id = $1)`,
				*source).Scan(&assignments, &lessonIDs, &revisionIDs); err != nil {
				return err
			}
		}
		if err := lockDependencies(ctx, tx, assignments, nil, lessonIDs, revisionIDs); err != nil {
			return err
		}
		// FOR UPDATE serializes draft creation per course, as CreateDraft
		// does for lessons, and keeps the pointer stable until the draft
		// records it as its base.
		c, err := lockCourse(ctx, tx, actor, courseID, "FOR UPDATE")
		if err != nil {
			return err
		}
		if c.archived {
			return ErrArchived
		}
		var active bool
		if err := tx.QueryRow(ctx, `
			SELECT EXISTS (SELECT 1 FROM course_revisions
			                WHERE course_id = $1 AND withdrawn_at IS NULL
			                  AND (status IN ('draft', 'in_review') OR (status = 'approved' AND candidate_closed_at IS NULL)))`,
			courseID).Scan(&active); err != nil {
			return err
		}
		if active {
			return ErrActiveCandidateExists
		}
		again, err := latestRevision(ctx, tx, courseID)
		if err != nil {
			return err
		}
		if !sameID(source, again) {
			return ErrRetry
		}
		var id int64
		if err := tx.QueryRow(ctx, `
			INSERT INTO course_revisions (course_id, subject_id, language, revision_number, author_id,
				base_revision_id, compare_revision_id, title, summary, objectives, prerequisites,
				cover_media_id, cover_alt, created_at, updated_at)
			SELECT $1, $2, $3, $4, $5, $6, course_comparison_revision($1),
			       coalesce(src.title, ''), coalesce(src.summary, ''), coalesce(src.objectives, ''), coalesce(src.prerequisites, ''),
			       CASE WHEN up.user_id IS NOT NULL THEN src.cover_media_id END,
			       CASE WHEN up.user_id IS NOT NULL THEN src.cover_alt END,
			       now(), now()
			  FROM (SELECT 1) one
			  LEFT JOIN course_revisions src ON src.id = $7
			  LEFT JOIN media_uploads up ON up.user_id = $5 AND up.media_asset_id = src.cover_media_id
			RETURNING id`,
			courseID, c.subject, c.language, c.last+1, actor, c.published, source).Scan(&id); err != nil {
			return err
		}
		if source != nil {
			for _, q := range []string{
				`INSERT INTO course_revision_assignments (course_revision_id, subject_id, assignment_id)
				 SELECT $1, subject_id, assignment_id FROM course_revision_assignments WHERE course_revision_id = $2`,
				`INSERT INTO course_modules (course_revision_id, subject_id, language, module_key, position, title, chapter_id)
				 SELECT $1, subject_id, language, module_key, position, title, chapter_id FROM course_modules WHERE course_revision_id = $2`,
				// Only attachable steps are copied (the step guard refuses
				// the others): the owner's own lessons, and another
				// author's lesson only at a revision still eligible.
				`INSERT INTO course_steps (course_revision_id, module_key, position, title, lesson_id, subject_id, language, lesson_revision_id)
				 SELECT $1, s.module_key, s.position, s.title, s.lesson_id, s.subject_id, s.language, s.lesson_revision_id
				   FROM course_steps s JOIN lessons l ON l.id = s.lesson_id
				  WHERE s.course_revision_id = $2
				    AND (l.created_by = $3 OR course_step_revision_eligible(s.lesson_id, s.lesson_revision_id))`,
			} {
				args := []any{id, *source}
				if strings.Contains(q, "course_steps") {
					args = append(args, actor)
				}
				if _, err := tx.Exec(ctx, q, args...); err != nil {
					return err
				}
			}
		}
		if rev, err = loadRevision(ctx, tx, id); err != nil {
			return err
		}
		if source != nil {
			rows, err := tx.Query(ctx, `
				SELECT s.module_key::text, s.position, s.title, s.lesson_id, s.lesson_revision_id
				  FROM course_steps s
				 WHERE s.course_revision_id = $1
				   AND NOT EXISTS (SELECT 1 FROM course_steps n WHERE n.course_revision_id = $2 AND n.lesson_id = s.lesson_id)
				 ORDER BY s.module_key, s.position`, *source, id)
			if err != nil {
				return err
			}
			rev.DroppedSteps, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (DroppedStep, error) {
				var d DroppedStep
				err := row.Scan(&d.ModuleKey, &d.Position, &d.Title, &d.LessonID, &d.RevisionID)
				return d, err
			})
			return err
		}
		return nil
	})
	return rev, err
}

func latestRevision(ctx context.Context, tx pgx.Tx, courseID int64) (*int64, error) {
	var id int64
	err := tx.QueryRow(ctx, `SELECT id FROM course_revisions WHERE course_id = $1 ORDER BY revision_number DESC LIMIT 1`, courseID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &id, nil
}

func sameID(a, b *int64) bool {
	return (a == nil) == (b == nil) && (a == nil || *a == *b)
}

// Revision returns a revision of a course actor owns.
func (s *Store) Revision(ctx context.Context, actor, revisionID int64) (Revision, error) {
	var rev Revision
	err := s.read(ctx, actor, func(tx pgx.Tx) error {
		if _, err := ownedCourseOf(ctx, tx, actor, revisionID); err != nil {
			return err
		}
		var err error
		rev, err = loadRevision(ctx, tx, revisionID)
		return err
	})
	return rev, err
}

// lockDraft locks actor's draft revision FOR UPDATE (after its course, FOR
// SHARE) and checks its state and version.
func lockDraft(ctx context.Context, tx pgx.Tx, actor, courseID, revisionID int64, expectedVersion int) (lockedCourse, error) {
	c, err := lockCourse(ctx, tx, actor, courseID, "FOR SHARE")
	if err != nil {
		return c, err
	}
	var status Status
	var version int
	err = tx.QueryRow(ctx, `SELECT status, version FROM course_revisions WHERE id = $1 AND course_id = $2 FOR UPDATE`,
		revisionID, courseID).Scan(&status, &version)
	switch {
	case errors.Is(err, pgx.ErrNoRows): // discarded concurrently
		return c, ErrNotFound
	case err != nil:
		return c, err
	case status != StatusDraft:
		return c, ErrNotDraft
	case version != expectedVersion:
		return c, ErrVersionConflict
	}
	return c, nil
}

// SaveDraft replaces the whole content of actor's draft at expectedVersion:
// overview, cover, placements, modules and steps. A stale save returns
// ErrVersionConflict and changes nothing.
//
// Steps: the lesson must have the course's subject and language; the
// owner's own lesson may pin any of its revisions, another author's lesson
// only an eligible revision (course_step_revision_eligible) of a lesson that
// is currently published; a lesson appears once (ErrInvalidStep).
// Placements: assignments of the course's subject; the saved revision must
// pass course_revision_placement_problems in save mode, and every module
// chapter must be in one of its assignments (ErrPlacementInvalid). The
// cover must be an image actor uploaded (ErrUnknownMedia).
func (s *Store) SaveDraft(ctx context.Context, actor, revisionID int64, expectedVersion int, d Draft) (Revision, error) {
	d, err := prepare(d)
	if err != nil {
		return Revision{}, err
	}
	var lessonIDs, revisionIDs, chapterIDs []int64
	for _, m := range d.Modules {
		if m.ChapterID != nil {
			chapterIDs = append(chapterIDs, *m.ChapterID)
		}
		for _, st := range m.Steps {
			lessonIDs = append(lessonIDs, st.LessonID)
			revisionIDs = append(revisionIDs, st.RevisionID)
		}
	}
	var rev Revision
	err = s.write(ctx, actor, func(tx pgx.Tx) error {
		courseID, err := ownedCourseOf(ctx, tx, actor, revisionID)
		if err != nil {
			return err
		}
		if err := lockDependencies(ctx, tx, d.AssignmentIDs, chapterIDs, lessonIDs, revisionIDs); err != nil {
			return err
		}
		c, err := lockDraft(ctx, tx, actor, courseID, revisionID, expectedVersion)
		if err != nil {
			return err
		}
		if err := checkPlacements(ctx, tx, c, d.AssignmentIDs, chapterIDs); err != nil {
			return err
		}
		if err := checkSteps(ctx, tx, actor, c, d.Modules); err != nil {
			return err
		}
		// The children are replaced first, so that the kind the server
		// derives (course_revision_derived_kind, LMS-B04) reads them.
		if err := replaceChildren(ctx, tx, revisionID, d); err != nil {
			return err
		}
		if d.CoverMediaID != nil {
			var uploaded bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM media_uploads WHERE user_id = $1 AND media_asset_id = $2)`,
				actor, *d.CoverMediaID).Scan(&uploaded); err != nil {
				return err
			}
			if !uploaded {
				return ErrUnknownMedia
			}
		}
		tag, err := tx.Exec(ctx, `
			UPDATE course_revisions u
			   SET title = $3, summary = $4, objectives = $5, prerequisites = $6, cover_media_id = $7, cover_alt = $8,
			       version = version + 1, updated_at = now(),
			       kind = CASE WHEN course_revision_structure_amends(u.id)
			                    AND EXISTS (SELECT 1 FROM course_revisions c WHERE c.id = u.compare_revision_id
			                                   AND (c.title, c.summary, c.objectives, c.prerequisites, c.cover_media_id, c.cover_alt)
			                                       IS NOT DISTINCT FROM ($3, $4, $5, $6, $7::bigint, $8::text))
			                   THEN 'amendment' ELSE 'ordinary' END
			 WHERE u.id = $1 AND u.version = $2 AND u.status = 'draft'`,
			revisionID, expectedVersion, d.Title, d.Summary, d.Objectives, d.Prerequisites, d.CoverMediaID, d.CoverAlt)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return ErrVersionConflict
		}
		if err := placementProblems(ctx, tx, revisionID, "save"); err != nil {
			return err
		}
		rev, err = loadRevision(ctx, tx, revisionID)
		return err
	})
	return rev, err
}

func checkPlacements(ctx context.Context, tx pgx.Tx, c lockedCourse, assignments, chapters []int64) error {
	if len(assignments) > 0 {
		var bad *int64
		err := tx.QueryRow(ctx, `
			SELECT want.id FROM unnest($1::bigint[]) AS want(id)
			 WHERE NOT EXISTS (SELECT 1 FROM curriculum_assignments a JOIN curriculum_editions e ON e.id = a.edition_id
			                    WHERE a.id = want.id AND a.subject_id = $2 AND e.state <> 'draft')
			 LIMIT 1`, assignments, c.subject).Scan(&bad)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if bad != nil {
			return placementInvalid("assignment %d does not exist for the course's subject, or its edition is still a draft", *bad)
		}
	}
	if len(chapters) > 0 {
		var bad *int64
		err := tx.QueryRow(ctx, `
			SELECT want.id FROM unnest($1::bigint[]) AS want(id)
			 WHERE NOT EXISTS (SELECT 1 FROM chapters ch WHERE ch.id = want.id AND ch.subject_id = $2)
			 LIMIT 1`, chapters, c.subject).Scan(&bad)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if bad != nil {
			return placementInvalid("chapter %d does not exist in the course's subject", *bad)
		}
	}
	return nil
}

func checkSteps(ctx context.Context, tx pgx.Tx, actor int64, c lockedCourse, modules []ModuleInput) error {
	var lessonIDs, revisionIDs []int64
	for _, m := range modules {
		for _, st := range m.Steps {
			lessonIDs = append(lessonIDs, st.LessonID)
			revisionIDs = append(revisionIDs, st.RevisionID)
		}
	}
	if len(lessonIDs) == 0 {
		return nil
	}
	rows, err := tx.Query(ctx, `
		SELECT s.lesson_id, s.revision_id, l.id IS NOT NULL, l.subject_id = $3 AND l.language = $4,
		       l.created_by = $5, EXISTS (SELECT 1 FROM lesson_revisions r WHERE r.id = s.revision_id AND r.lesson_id = s.lesson_id),
		       course_step_revision_eligible(s.lesson_id, s.revision_id)
		  FROM unnest($1::bigint[], $2::bigint[]) WITH ORDINALITY AS s(lesson_id, revision_id, n)
		  LEFT JOIN lessons l ON l.id = s.lesson_id
		 ORDER BY s.n`, lessonIDs, revisionIDs, c.subject, c.language, actor)
	if err != nil {
		return err
	}
	type check struct {
		lesson, revision  int64
		exists            bool
		compatible, own   *bool
		belongs, eligible bool
	}
	checks, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (check, error) {
		var ch check
		err := row.Scan(&ch.lesson, &ch.revision, &ch.exists, &ch.compatible, &ch.own, &ch.belongs, &ch.eligible)
		return ch, err
	})
	if err != nil {
		return err
	}
	for _, ch := range checks {
		switch {
		// Another author's lesson that is not currently published, or a
		// revision of it that is not eligible, is reported like a missing
		// lesson: only published lessons of others are visible.
		case !ch.exists || (!*ch.own && !ch.eligible):
			return invalidStep("lesson %d does not exist, or revision %d of it cannot be attached (another author's lesson needs an eligible revision: approved, published, not withdrawn, with the lesson published)", ch.lesson, ch.revision)
		case !*ch.compatible:
			return invalidStep("lesson %d is not in the course's subject and language", ch.lesson)
		case !ch.belongs:
			return invalidStep("revision %d is not a revision of lesson %d", ch.revision, ch.lesson)
		}
	}
	return nil
}

func replaceChildren(ctx context.Context, tx pgx.Tx, revisionID int64, d Draft) error {
	for _, q := range []string{
		`DELETE FROM course_steps WHERE course_revision_id = $1`,
		`DELETE FROM course_modules WHERE course_revision_id = $1`,
		`DELETE FROM course_revision_assignments WHERE course_revision_id = $1`,
	} {
		if _, err := tx.Exec(ctx, q, revisionID); err != nil {
			return err
		}
	}
	if len(d.AssignmentIDs) > 0 {
		if _, err := tx.Exec(ctx, `
			INSERT INTO course_revision_assignments (course_revision_id, subject_id, assignment_id)
			SELECT r.id, r.subject_id, a FROM course_revisions r, unnest($2::bigint[]) a WHERE r.id = $1`,
			revisionID, d.AssignmentIDs); err != nil {
			return err
		}
	}
	if len(d.Modules) == 0 {
		return nil
	}
	var keys, titles, stepKeys, stepTitles []string
	var positions, stepPositions []int32
	var chapters []*int64
	var stepLessons, stepRevisions []int64
	for i, m := range d.Modules {
		keys = append(keys, m.Key)
		titles = append(titles, m.Title)
		positions = append(positions, int32(i+1))
		chapters = append(chapters, m.ChapterID)
		for j, st := range m.Steps {
			stepKeys = append(stepKeys, m.Key)
			stepTitles = append(stepTitles, st.Title)
			stepPositions = append(stepPositions, int32(j+1))
			stepLessons = append(stepLessons, st.LessonID)
			stepRevisions = append(stepRevisions, st.RevisionID)
		}
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO course_modules (course_revision_id, subject_id, language, module_key, position, title, chapter_id)
		SELECT r.id, r.subject_id, r.language, m.k::uuid, m.p, m.t, m.c
		  FROM course_revisions r, unnest($2::text[], $3::int[], $4::text[], $5::bigint[]) AS m(k, p, t, c)
		 WHERE r.id = $1`, revisionID, keys, positions, titles, chapters); err != nil {
		return err
	}
	if len(stepKeys) > 0 {
		if _, err := tx.Exec(ctx, `
			INSERT INTO course_steps (course_revision_id, module_key, position, title, lesson_id, subject_id, language, lesson_revision_id)
			SELECT r.id, s.k::uuid, s.p, s.t, s.l, r.subject_id, r.language, s.v
			  FROM course_revisions r, unnest($2::text[], $3::int[], $4::text[], $5::bigint[], $6::bigint[]) AS s(k, p, t, l, v)
			 WHERE r.id = $1`, revisionID, stepKeys, stepPositions, stepTitles, stepLessons, stepRevisions); err != nil {
			return err
		}
	}
	return nil
}

// placementProblems runs course_revision_placement_problems and turns the
// first problem into ErrPlacementInvalid.
func placementProblems(ctx context.Context, tx pgx.Tx, revisionID int64, mode string) error {
	var problem string
	var assignment, chapter *int64
	err := tx.QueryRow(ctx, `
		SELECT problem, assignment, chapter FROM course_revision_placement_problems($1, $2)
		 ORDER BY problem, assignment, chapter LIMIT 1`, revisionID, mode).Scan(&problem, &assignment, &chapter)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	switch problem {
	case "assignment_not_selectable":
		return placementInvalid("assignment %d cannot be newly selected: its edition is not current, or its section, level or subject is retired", *assignment)
	case "chapter_outside_assignments":
		return placementInvalid("chapter %d is in none of the revision's curriculum assignments", *chapter)
	case "chapter_retired":
		return placementInvalid("chapter %d is retired", *chapter)
	case "assignment_retired":
		return placementInvalid("assignment %d is in a retired edition, section or level", *assignment)
	case "no_assignment":
		return placementInvalid("at least one curriculum assignment is required")
	}
	return placementInvalid("%s", strings.ReplaceAll(problem, "_", " "))
}

// DiscardDraft deletes actor's draft at expectedVersion with its children,
// so the course can accept a new draft. The revision number is not reused.
func (s *Store) DiscardDraft(ctx context.Context, actor, revisionID int64, expectedVersion int) error {
	return s.write(ctx, actor, func(tx pgx.Tx) error {
		courseID, err := ownedCourseOf(ctx, tx, actor, revisionID)
		if err != nil {
			return err
		}
		if _, err := lockDraft(ctx, tx, actor, courseID, revisionID, expectedVersion); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `DELETE FROM course_revisions WHERE id = $1 AND version = $2 AND status = 'draft'`, revisionID, expectedVersion)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return ErrVersionConflict
		}
		return nil
	})
}

func loadRevision(ctx context.Context, tx pgx.Tx, id int64) (Revision, error) {
	var r Revision
	var status string
	var owner int64
	err := tx.QueryRow(ctx, `
		SELECT r.id, r.course_id, r.revision_number, r.status, r.kind, r.author_id, r.base_revision_id, r.compare_revision_id,
		       r.subject_id, r.language, r.title, r.summary, r.objectives, r.prerequisites, r.cover_media_id, r.cover_alt,
		       r.version, r.created_at, r.updated_at, r.submitted_at, r.withdrawn_at, c.owner_id
		  FROM course_revisions r JOIN courses c ON c.id = r.course_id WHERE r.id = $1`, id).Scan(
		&r.ID, &r.CourseID, &r.Number, &status, &r.Kind, &r.AuthorID, &r.BaseRevisionID, &r.CompareRevisionID,
		&r.SubjectID, &r.Language, &r.Title, &r.Summary, &r.Objectives, &r.Prerequisites, &r.CoverMediaID, &r.CoverAlt,
		&r.Version, &r.CreatedAt, &r.UpdatedAt, &r.SubmittedAt, &r.WithdrawnAt, &owner)
	if errors.Is(err, pgx.ErrNoRows) {
		return r, ErrNotFound
	}
	if err != nil {
		return r, fmt.Errorf("load course revision: %w", err)
	}
	r.Status = Status(status)
	if r.Decision, err = loadDecision(ctx, tx, id); err != nil {
		return r, err
	}
	if r.Status == StatusInReview && r.WithdrawnAt == nil {
		if r.Blocked, err = reviewBlocked(ctx, tx, id, r.SubjectID); err != nil {
			return r, err
		}
	}
	rows, err := tx.Query(ctx, `
		SELECT a.id, e.id, e.state, curriculum_assignment_selectable(a.id),
		       e.state = 'retired' OR s.retired_at IS NOT NULL OR l.retired_at IS NOT NULL
		  FROM course_revision_assignments cra
		  JOIN curriculum_assignments a ON a.id = cra.assignment_id
		  JOIN curriculum_editions e ON e.id = a.edition_id
		  JOIN academic_sections s ON s.id = e.section_id
		  JOIN education_levels l ON l.id = s.level_id
		 WHERE cra.course_revision_id = $1
		 ORDER BY a.id`, id)
	if err != nil {
		return r, err
	}
	if r.Assignments, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (Placement, error) {
		var p Placement
		err := row.Scan(&p.AssignmentID, &p.EditionID, &p.EditionState, &p.Selectable, &p.Retired)
		return p, err
	}); err != nil {
		return r, err
	}
	rows, err = tx.Query(ctx, `
		SELECT module_key::text, position, title, chapter_id FROM course_modules
		 WHERE course_revision_id = $1 ORDER BY position`, id)
	if err != nil {
		return r, err
	}
	if r.Modules, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (Module, error) {
		var m Module
		err := row.Scan(&m.Key, &m.Position, &m.Title, &m.ChapterID)
		return m, err
	}); err != nil {
		return r, err
	}
	rows, err = tx.Query(ctx, `
		SELECT s.module_key::text, s.position, s.title, s.lesson_id, s.lesson_revision_id, lr.revision_number,
		       l.created_by = $2, course_step_revision_eligible(s.lesson_id, s.lesson_revision_id)
		  FROM course_steps s
		  JOIN lessons l ON l.id = s.lesson_id
		  JOIN lesson_revisions lr ON lr.id = s.lesson_revision_id
		 WHERE s.course_revision_id = $1
		 ORDER BY s.module_key, s.position`, id, owner)
	if err != nil {
		return r, err
	}
	type keyed struct {
		key string
		Step
	}
	steps, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (keyed, error) {
		var k keyed
		err := row.Scan(&k.key, &k.Position, &k.Title, &k.LessonID, &k.RevisionID, &k.RevisionNumber, &k.OwnLesson, &k.Eligible)
		return k, err
	})
	if err != nil {
		return r, err
	}
	for i := range r.Modules {
		r.Modules[i].Steps = []Step{}
		for _, k := range steps {
			if k.key == r.Modules[i].Key {
				r.Modules[i].Steps = append(r.Modules[i].Steps, k.Step)
			}
		}
	}
	return r, nil
}

// ---- attachable lessons ----

// AttachableLesson is a lesson the owner of a course may attach to it.
// Title is the latest revision's title for the owner's own lesson and the
// published revision's title for another author's.
type AttachableLesson struct {
	LessonID            int64
	PublicID            string
	Own                 bool
	Title               string
	PublishedRevisionID *int64
	// Latest is the owner's own lesson's latest revision (nil otherwise).
	Latest *RevisionSummary
}

// AttachableLessons pages through the lessons actor may attach to their
// course: the same subject and language, and either actor's own lesson or
// another author's lesson that is currently published and not archived.
// q (optional) matches the title ignoring case and diacritics (reader_fold);
// after is the last lesson ID of the previous page (0 for the first). It
// returns at most AttachablePage lessons, by lesson ID, and the cursor of
// the next page, or nil.
func (s *Store) AttachableLessons(ctx context.Context, actor, courseID int64, q string, after int64) ([]AttachableLesson, *int64, error) {
	q = strings.Join(strings.Fields(q), " ")
	if !utf8.ValidString(q) || utf8.RuneCountInString(q) > MaxQueryRunes ||
		strings.IndexFunc(q, func(r rune) bool { return unicode.IsControl(r) || unicode.Is(unicode.Bidi_Control, r) }) >= 0 {
		return nil, nil, invalid("q must be plain text of at most %d characters", MaxQueryRunes)
	}
	var out []AttachableLesson
	err := s.read(ctx, actor, func(tx pgx.Tx) error {
		if err := ownedCourse(ctx, tx, actor, courseID); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `
			SELECT l.id, l.public_id::text, l.created_by = c.owner_id,
			       coalesce(CASE WHEN l.created_by = c.owner_id THEN lr.title ELSE pr.title END, ''),
			       l.published_revision_id,
			       lr.id, lr.revision_number, lr.status, lr.version, lr.updated_at
			  FROM courses c
			  JOIN lessons l ON l.subject_id = c.subject_id AND l.language = c.language
			  LEFT JOIN lesson_revisions pr ON pr.id = l.published_revision_id
			  LEFT JOIN LATERAL (SELECT id, revision_number, status, title, version, updated_at FROM lesson_revisions
			                      WHERE lesson_id = l.id AND l.created_by = c.owner_id
			                      ORDER BY revision_number DESC LIMIT 1) lr ON true
			 WHERE c.id = $1 AND l.id > $2
			   AND (l.created_by = c.owner_id OR (l.published_revision_id IS NOT NULL AND l.archived_at IS NULL))
			   AND ($3 = '' OR strpos(reader_fold(coalesce(CASE WHEN l.created_by = c.owner_id THEN lr.title ELSE pr.title END, '')),
			                          reader_fold($3)) > 0)
			 ORDER BY l.id
			 LIMIT $4`, courseID, after, q, AttachablePage+1)
		if err != nil {
			return err
		}
		out, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (AttachableLesson, error) {
			var a AttachableLesson
			var id *int64
			var number, version *int
			var status *string
			var updated *time.Time
			err := row.Scan(&a.LessonID, &a.PublicID, &a.Own, &a.Title, &a.PublishedRevisionID, &id, &number, &status, &version, &updated)
			if err == nil && id != nil {
				a.Latest = &RevisionSummary{ID: *id, Number: *number, Status: Status(*status), Title: a.Title, Version: *version, UpdatedAt: *updated}
			}
			return a, err
		})
		return err
	})
	if err != nil {
		return nil, nil, err
	}
	var next *int64
	if len(out) > AttachablePage {
		out = out[:AttachablePage]
		id := out[len(out)-1].LessonID
		next = &id
	}
	return out, next, nil
}

// LessonRevision is a revision a course step may pin.
type LessonRevision struct {
	ID       int64
	Number   int
	Status   Status
	Title    string
	Eligible bool
}

// EligibleRevisions lists the revisions of lessonID that actor may pin, by
// number descending: every revision of actor's own lesson (Eligible tells
// which may be submitted), or the eligible revisions of another author's
// lesson that is currently published and not archived. Any other lesson is
// ErrNotFound.
func (s *Store) EligibleRevisions(ctx context.Context, actor, lessonID int64) ([]LessonRevision, error) {
	var out []LessonRevision
	err := s.read(ctx, actor, func(tx pgx.Tx) error {
		var own, published bool
		err := tx.QueryRow(ctx, `SELECT created_by = $2, published_revision_id IS NOT NULL AND archived_at IS NULL FROM lessons WHERE id = $1`,
			lessonID, actor).Scan(&own, &published)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && !own && !published) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `
			SELECT id, revision_number, status, title, eligible FROM (
			    SELECT id, revision_number, status, title, course_step_revision_eligible(lesson_id, id) AS eligible
			      FROM lesson_revisions WHERE lesson_id = $1) r
			 WHERE $2 OR eligible
			 ORDER BY revision_number DESC`, lessonID, own)
		if err != nil {
			return err
		}
		out, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (LessonRevision, error) {
			var lr LessonRevision
			var status string
			err := row.Scan(&lr.ID, &lr.Number, &status, &lr.Title, &lr.Eligible)
			lr.Status = Status(status)
			return lr, err
		})
		return err
	})
	if err == nil && out == nil {
		out = []LessonRevision{}
	}
	return out, err
}
