// Package lessons stores lesson identities and their revision snapshots.
//
// The database enforces the editorial invariants (migrations/0004_lessons.sql):
// one active candidate per lesson (a draft, a revision in review, or an
// approved revision never published), immutable submitted snapshots
// and media references, immutable media assets, and consistent foreign keys.
// This package adds the checks that need the caller: the author permission,
// lesson ownership (only a lesson's creator, while holding author, starts a
// draft of it or translates it; the database guards the same rule),
// optimistic version checks and schema v1 validation through
// internal/rendering before anything is stored.
//
// Review decisions and publication are in internal/review (LRN-007);
// restoration and withdrawal are LRN-009. Revisions carry their review
// decision when one exists.
package lessons

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/hadden-me/learnoo/internal/permissions"
	"github.com/hadden-me/learnoo/internal/rendering"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Status is a revision's workflow state. Publication is the lesson's
// published revision pointer, not a status.
type Status string

const (
	StatusDraft            Status = "draft"
	StatusInReview         Status = "in_review"
	StatusChangesRequested Status = "changes_requested"
	StatusApproved         Status = "approved"
)

// Limits checked before storing.
const (
	MaxPrerequisites = 2000 // characters
	MaxMediaPerDraft = 100
)

var (
	// ErrDenied is permissions.ErrDenied: missing author permission,
	// anonymous caller, or another author's draft.
	ErrDenied = permissions.ErrDenied
	// ErrNotOwner means the caller is not the lesson's owner (its creator,
	// lessons.created_by): only the owner may start a draft of it or add a
	// translation of it. Administrators get no exception. It wraps ErrDenied;
	// the HTTP slice should answer it like a missing lesson.
	ErrNotOwner = fmt.Errorf("%w: not the lesson's owner", permissions.ErrDenied)
	// ErrNotFound means the lesson or revision does not exist.
	ErrNotFound = errors.New("lesson or revision not found")
	// ErrNotDraft means the revision was submitted and can no longer change.
	ErrNotDraft = errors.New("revision is not an editable draft")
	// ErrVersionConflict means the draft changed since the caller loaded it.
	ErrVersionConflict = errors.New("draft was changed since it was loaded")
	// ErrActiveCandidateExists means the lesson already has an active
	// candidate: a draft, a revision in review, or an approved revision that
	// has not been published (or withdrawn) yet.
	ErrActiveCandidateExists = errors.New("lesson already has a draft, in-review or approved-unpublished candidate")
	// ErrInvalidContent wraps a schema v1 validation or field-limit failure.
	ErrInvalidContent = errors.New("invalid lesson content")
	// ErrLanguageMismatch means the document language differs from the lesson's.
	ErrLanguageMismatch = errors.New("document language does not match the lesson")
	// ErrUnknownSubject means the subject does not exist.
	ErrUnknownSubject = errors.New("unknown subject")
	// ErrUnknownChapter means the chapter does not exist in the lesson's subject.
	ErrUnknownChapter = errors.New("unknown chapter for this subject")
	// ErrRetiredSubject means the subject is retired (migrations/0013): no
	// new lesson can be created in it. Drafts of existing lessons keep it.
	// The database guards decide both retirement rules; mapErr maps them.
	ErrRetiredSubject = errors.New("subject is retired")
	// ErrRetiredChapter means a draft sets a retired chapter that is not the
	// chapter of its base revision.
	ErrRetiredChapter = errors.New("chapter is retired")
	// ErrUnknownMedia means a referenced media asset does not exist.
	ErrUnknownMedia = errors.New("unknown media asset")
	// ErrUnattachedMedia means the content embeds an uploaded image
	// ("media:<id>") that is not among the revision's media references. It
	// wraps ErrInvalidContent.
	ErrUnattachedMedia = fmt.Errorf("%w: an embedded image is not among the revision's media references", ErrInvalidContent)
	// ErrArchived means the lesson is archived and accepts no new drafts.
	ErrArchived = errors.New("lesson is archived")
	// ErrTranslationExists means the translation group already has a lesson
	// in that language.
	ErrTranslationExists = errors.New("translation group already has a lesson in this language")
	// ErrTranslationSubject means a translation names a different subject
	// from its source lesson; every lesson of a group shares one subject.
	ErrTranslationSubject = errors.New("a translation must have the subject of its source lesson")
	// ErrImmutable reports a change rejected by a database guard. The Store's
	// own checks normally return a more specific error first.
	ErrImmutable = errors.New("change rejected: snapshot or asset is immutable")
)

// Fields are the author-editable parts of a draft snapshot. Title,
// objectives and language are read from the document itself.
type Fields struct {
	// Content is a schema v1 lesson document (docs/fixtures/README.md).
	// Exercises and worked solutions are embedded in it.
	Content       []byte
	Prerequisites string
	ChapterID     *int64
	MediaIDs      []int64
}

// NewLesson describes a lesson and its first draft.
type NewLesson struct {
	SubjectID int64
	Language  string
	// TranslationOf optionally names an existing lesson whose translation
	// group the new lesson joins. Zero starts a new group.
	TranslationOf int64
	Fields
}

// Lesson is the stable identity of a lesson.
type Lesson struct {
	ID                  int64
	PublicID            string
	TranslationGroupID  string
	Language            string
	SubjectID           int64
	PublishedRevisionID *int64
	ArchivedAt          *time.Time
	CreatedAt           time.Time
	CreatedBy           int64
}

// Revision is one stored snapshot.
type Revision struct {
	ID             int64
	LessonID       int64
	Number         int
	Status         Status
	AuthorID       int64
	BaseRevisionID *int64
	Title          string
	Objectives     string
	Prerequisites  string
	Content        []byte
	SchemaVersion  int
	Language       string
	SubjectID      int64
	ChapterID      *int64
	Version        int
	CreatedAt      time.Time
	UpdatedAt      time.Time
	SubmittedAt    *time.Time
	MediaIDs       []int64
	// Decision is the review decision that took the revision out of review,
	// or nil while it is a draft or in review.
	Decision *Decision
}

// Decision is a recorded review decision (migrations/0006).
type Decision struct {
	// Action is "approve" or "request_changes".
	Action     string
	ReviewerID int64
	// Comment is empty for an approval without comment.
	Comment   string
	DecidedAt time.Time
}

// ApprovedBy returns the approving reviewer, if the revision was approved.
func (r Revision) ApprovedBy() *int64 {
	if r.Decision == nil || r.Decision.Action != "approve" {
		return nil
	}
	id := r.Decision.ReviewerID
	return &id
}

// Store creates, saves, submits and discards lesson drafts. It validates
// content but does not bound how much it reads: callers must cap input size
// (the HTTP layer limits request bodies). Every timestamp it writes
// (created_at, updated_at, submitted_at) is the database transaction time,
// as the audit tables record, so the times of one transaction agree.
type Store struct {
	db *pgxpool.Pool
}

// NewStore builds a Store.
func NewStore(db *pgxpool.Pool) *Store {
	return &Store{db: db}
}

type snapshot struct {
	content []byte
	doc     rendering.Document
	prereq  string
	chapter *int64
	media   []int64
}

// decodeCanonical validates a stored or submitted document independently of
// its formatting: whitespace is removed before the schema v1 decoder (and its
// size limit) sees it, so PostgreSQL's jsonb output, which adds spacing and
// reorders keys, validates exactly like the bytes that were saved.
func decodeCanonical(data []byte) (rendering.Document, error) {
	var compact bytes.Buffer
	if err := json.Compact(&compact, data); err != nil {
		return rendering.Document{}, fmt.Errorf("%w: invalid JSON: %v", ErrInvalidContent, err)
	}
	doc, err := rendering.Decode(&compact)
	if err != nil {
		return rendering.Document{}, fmt.Errorf("%w: %v", ErrInvalidContent, err)
	}
	return doc, nil
}

// canonicalize validates data and returns the document with its canonical
// encoding: the decoded document re-encoded compactly, which itself must
// validate. Only canonical content is stored, so the stored keys and values
// are exactly what the decoder saw (encoding/json matches field names
// case-insensitively, last one winning; the canonical form has no such
// duplicates).
func canonicalize(data []byte) (rendering.Document, []byte, error) {
	if !utf8.Valid(data) {
		return rendering.Document{}, nil, fmt.Errorf("%w: content is not valid UTF-8", ErrInvalidContent)
	}
	doc, err := decodeCanonical(data)
	if err != nil {
		return rendering.Document{}, nil, err
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(doc); err != nil {
		return rendering.Document{}, nil, fmt.Errorf("%w: %v", ErrInvalidContent, err)
	}
	content := bytes.TrimSuffix(buf.Bytes(), []byte("\n"))
	if _, err := decodeCanonical(content); err != nil {
		return rendering.Document{}, nil, err
	}
	return doc, content, nil
}

// prepare validates f and returns what is stored.
func prepare(f Fields) (snapshot, error) {
	doc, content, err := canonicalize(f.Content)
	if err != nil {
		return snapshot{}, err
	}
	if !utf8.ValidString(f.Prerequisites) || utf8.RuneCountInString(f.Prerequisites) > MaxPrerequisites {
		return snapshot{}, fmt.Errorf("%w: prerequisites must be valid text of at most %d characters", ErrInvalidContent, MaxPrerequisites)
	}
	media := slices.Clone(f.MediaIDs)
	slices.Sort(media)
	media = slices.Compact(media)
	if len(media) > MaxMediaPerDraft {
		return snapshot{}, fmt.Errorf("%w: at most %d media references", ErrInvalidContent, MaxMediaPerDraft)
	}
	if len(media) > 0 && media[0] <= 0 {
		return snapshot{}, ErrUnknownMedia
	}
	// Every embedded image must be attached: the caller's mediaIds are
	// checked for read access (HTTP slice) and frozen with the snapshot at
	// submission, so the content can only show assets its author may read.
	// Attached assets need not be embedded (removing the last reference
	// keeps the attachment until the author removes it).
	for _, id := range rendering.MediaIDs(doc) {
		if _, found := slices.BinarySearch(media, id); !found {
			return snapshot{}, ErrUnattachedMedia
		}
	}
	return snapshot{content: content, doc: doc, prereq: f.Prerequisites, chapter: f.ChapterID, media: media}, nil
}

// CreateLesson creates a lesson and its first draft, authored by actor,
// who becomes the lesson's owner. A translation (nl.TranslationOf) joins the
// source lesson's translation group and requires actor to own the source:
// otherwise ErrNotOwner, checked before anything else about the source.
func (s *Store) CreateLesson(ctx context.Context, actor int64, nl NewLesson) (Revision, error) {
	var rev Revision
	err := pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		if err := permissions.RequireTx(ctx, tx, actor, permissions.Author); err != nil {
			return err
		}
		snap, err := prepare(nl.Fields)
		if err != nil {
			return err
		}
		if snap.doc.Language != nl.Language {
			return ErrLanguageMismatch
		}
		var group string
		if nl.TranslationOf != 0 {
			// The share lock keeps the source from being archived meanwhile.
			var subject, owner int64
			var archived *time.Time
			err := tx.QueryRow(ctx, `
				SELECT translation_group_id::text, subject_id, archived_at, created_by
				  FROM lessons WHERE id = $1 FOR SHARE`, nl.TranslationOf).Scan(&group, &subject, &archived, &owner)
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			if err != nil {
				return err
			}
			if owner != actor {
				return ErrNotOwner
			}
			if archived != nil {
				return ErrArchived
			}
			if subject != nl.SubjectID {
				return ErrTranslationSubject
			}
		} else {
			err := tx.QueryRow(ctx, `INSERT INTO translation_groups (subject_id) VALUES ($1) RETURNING id::text`, nl.SubjectID).Scan(&group)
			if err != nil {
				return mapErr(err)
			}
		}
		var lessonID int64
		err = tx.QueryRow(ctx, `
			INSERT INTO lessons (translation_group_id, language, subject_id, created_at, created_by)
			VALUES ($1::uuid, $2, $3, now(), $4)
			RETURNING id`, group, nl.Language, nl.SubjectID, actor).Scan(&lessonID)
		if err != nil {
			return mapErr(err)
		}
		rev, err = insertRevision(ctx, tx, lessonID, 1, nil, nl.SubjectID, actor, snap)
		return err
	})
	return rev, err
}

// CreateDraft starts a new draft of an existing lesson, authored by actor:
// the successor of a changes-requested revision, or a new draft while the
// published revision stays untouched. Only the lesson's owner may (actor
// must have created it and hold author); anyone else gets ErrNotOwner,
// checked right after the lesson is found and before its state. Its base is the lesson's published
// revision, read under a lock in the same transaction. At most one active
// candidate exists per lesson: a draft, an in-review revision, or an approved
// revision not yet published (nor, from LRN-009, withdrawn). While one
// exists, CreateDraft returns ErrActiveCandidateExists.
func (s *Store) CreateDraft(ctx context.Context, actor, lessonID int64, f Fields) (Revision, error) {
	var rev Revision
	err := pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		if err := permissions.RequireTx(ctx, tx, actor, permissions.Author); err != nil {
			return err
		}
		snap, err := prepare(f)
		if err != nil {
			return err
		}
		var language string
		var subject, owner int64
		var published *int64
		var archived *time.Time
		var last int
		// The row lock serializes draft creation per lesson and keeps the
		// published pointer stable until the draft records it as its base.
		err = tx.QueryRow(ctx, `
			SELECT language, subject_id, published_revision_id, archived_at, last_revision_number, created_by
			  FROM lessons WHERE id = $1 FOR UPDATE`, lessonID).Scan(&language, &subject, &published, &archived, &last, &owner)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		// created_by never changes, so this needs no further lock; the
		// author permission was share-locked by RequireTx above.
		if owner != actor {
			return ErrNotOwner
		}
		if archived != nil {
			return ErrArchived
		}
		if snap.doc.Language != language {
			return ErrLanguageMismatch
		}
		// Same predicate as the lesson_revisions_one_active_candidate index,
		// which rejects the insert (mapped to the same error) regardless.
		// Publication changes candidate_closed_at only under the lesson row
		// lock held here, so this check cannot race it.
		var active bool
		err = tx.QueryRow(ctx, `
			SELECT EXISTS (SELECT 1 FROM lesson_revisions
			                WHERE lesson_id = $1
			                  AND (status IN ('draft', 'in_review')
			                       OR (status = 'approved' AND candidate_closed_at IS NULL)))`, lessonID).Scan(&active)
		if err != nil {
			return err
		}
		if active {
			return ErrActiveCandidateExists
		}
		// Numbers are never reused, even after a discarded draft.
		rev, err = insertRevision(ctx, tx, lessonID, last+1, published, subject, actor, snap)
		return err
	})
	return rev, err
}

func insertRevision(ctx context.Context, tx pgx.Tx, lessonID int64, number int, base *int64, subject, author int64, snap snapshot) (Revision, error) {
	var id int64
	err := tx.QueryRow(ctx, `
		INSERT INTO lesson_revisions (lesson_id, revision_number, author_id, base_revision_id,
			title, objectives, prerequisites, content, schema_version, language, subject_id,
			chapter_id, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, now(), now())
		RETURNING id`,
		lessonID, number, author, base, snap.doc.Title, snap.doc.Objective, snap.prereq,
		snap.content, snap.doc.SchemaVersion, snap.doc.Language, subject, snap.chapter).Scan(&id)
	if err != nil {
		return Revision{}, mapErr(err)
	}
	if err := insertMedia(ctx, tx, id, snap.media); err != nil {
		return Revision{}, err
	}
	return loadRevision(ctx, tx, id)
}

func insertMedia(ctx context.Context, tx pgx.Tx, revisionID int64, media []int64) error {
	if len(media) == 0 {
		return nil
	}
	_, err := tx.Exec(ctx, `INSERT INTO revision_media (revision_id, media_asset_id)
		SELECT $1, unnest($2::bigint[])`, revisionID, media)
	return mapErr(err)
}

type draftState struct {
	language string
	archived bool
}

// lockOwnDraft checks the caller may change the revision and locks it.
//
// Lock order is lesson first, then revision, as in CreateDraft and in the
// published-pointer trigger (UPDATE lessons, then FOR SHARE on the
// revision), so a save cannot deadlock with a concurrent publication. The
// lesson is share-locked so it cannot be archived meanwhile; a revision's
// lesson_id never changes, so reading it unlocked first is safe.
//
// Not found and denied are distinguishable here. The HTTP slice must decide
// whether to merge them so other authors cannot probe for revision IDs.
func lockOwnDraft(ctx context.Context, tx pgx.Tx, actor, revisionID int64, expectedVersion int) (draftState, error) {
	var st draftState
	var lessonID int64
	err := tx.QueryRow(ctx, `SELECT lesson_id FROM lesson_revisions WHERE id = $1`, revisionID).Scan(&lessonID)
	if errors.Is(err, pgx.ErrNoRows) {
		return st, ErrNotFound
	}
	if err != nil {
		return st, err
	}
	if err := tx.QueryRow(ctx, `SELECT archived_at IS NOT NULL FROM lessons WHERE id = $1 FOR SHARE`, lessonID).Scan(&st.archived); err != nil {
		return st, err
	}
	var author int64
	var status Status
	var version int
	err = tx.QueryRow(ctx, `
		SELECT author_id, status, version, language
		  FROM lesson_revisions WHERE id = $1 FOR UPDATE`, revisionID).Scan(&author, &status, &version, &st.language)
	switch {
	case errors.Is(err, pgx.ErrNoRows): // discarded concurrently
		return st, ErrNotFound
	case err != nil:
		return st, err
	case author != actor:
		return st, ErrDenied
	case status != StatusDraft:
		return st, ErrNotDraft
	case version != expectedVersion:
		return st, ErrVersionConflict
	}
	return st, nil
}

// SaveDraft replaces the content, metadata and media references of the
// actor's own draft, provided it is still at expectedVersion. A stale save
// returns ErrVersionConflict and changes nothing. The returned revision
// carries the new version.
func (s *Store) SaveDraft(ctx context.Context, actor, revisionID int64, expectedVersion int, f Fields) (Revision, error) {
	var rev Revision
	err := pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		if err := permissions.RequireTx(ctx, tx, actor, permissions.Author); err != nil {
			return err
		}
		snap, err := prepare(f)
		if err != nil {
			return err
		}
		st, err := lockOwnDraft(ctx, tx, actor, revisionID, expectedVersion)
		if err != nil {
			return err
		}
		if st.archived {
			return ErrArchived
		}
		if snap.doc.Language != st.language {
			return ErrLanguageMismatch
		}
		tag, err := tx.Exec(ctx, `
			UPDATE lesson_revisions
			   SET title = $4, objectives = $5, prerequisites = $6, content = $7,
			       schema_version = $8, chapter_id = $9, version = version + 1, updated_at = now()
			 WHERE id = $1 AND version = $2 AND author_id = $3 AND status = 'draft'`,
			revisionID, expectedVersion, actor, snap.doc.Title, snap.doc.Objective, snap.prereq,
			snap.content, snap.doc.SchemaVersion, snap.chapter)
		if err != nil {
			return mapErr(err)
		}
		if tag.RowsAffected() != 1 {
			return ErrVersionConflict
		}
		if _, err := tx.Exec(ctx, `DELETE FROM revision_media WHERE revision_id = $1`, revisionID); err != nil {
			return mapErr(err)
		}
		if err := insertMedia(ctx, tx, revisionID, snap.media); err != nil {
			return err
		}
		rev, err = loadRevision(ctx, tx, revisionID)
		return err
	})
	return rev, err
}

// Submit moves the actor's own draft at expectedVersion into review and
// records the submission in submission_audit (actor, revision, transaction
// time) in the same transaction. From then on its content, metadata and
// media references are frozen by the database; corrections need a
// successor draft. Its submitted_at is the transaction time.
func (s *Store) Submit(ctx context.Context, actor, revisionID int64, expectedVersion int) (Revision, error) {
	var rev Revision
	err := pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		if err := permissions.RequireTx(ctx, tx, actor, permissions.Author); err != nil {
			return err
		}
		st, err := lockOwnDraft(ctx, tx, actor, revisionID, expectedVersion)
		if err != nil {
			return err
		}
		if st.archived {
			return ErrArchived
		}
		// Only valid, canonical schema v1 content enters review.
		if err := CheckStoredContentTx(ctx, tx, revisionID); err != nil {
			return err
		}
		// The audit row first: the database lets a revision leave draft only
		// together with a submission_audit row of the same transaction, and
		// both record the transaction time (migrations/0011).
		if _, err := tx.Exec(ctx, `
			INSERT INTO submission_audit (lesson_id, revision_id, actor_id)
			SELECT lesson_id, id, $2 FROM lesson_revisions WHERE id = $1`, revisionID, actor); err != nil {
			return mapErr(err)
		}
		tag, err := tx.Exec(ctx, `
			UPDATE lesson_revisions
			   SET status = 'in_review', submitted_at = now(), updated_at = now(), version = version + 1
			 WHERE id = $1 AND version = $2 AND author_id = $3 AND status = 'draft'`,
			revisionID, expectedVersion, actor)
		if err != nil {
			return mapErr(err)
		}
		if tag.RowsAffected() != 1 {
			return ErrVersionConflict
		}
		rev, err = loadRevision(ctx, tx, revisionID)
		return err
	})
	return rev, err
}

// CheckStoredContentTx re-checks a stored snapshot against the current
// schema v1 validator inside tx: it returns an ErrInvalidContent error unless
// the content is valid and in canonical form. Equality with the canonical
// re-encoding rejects content altered outside the store with keys the
// decoder reads differently from the stored JSON (case-folded duplicates).
// Every uploaded image the content embeds must be among the revision's
// media references (ErrUnattachedMedia otherwise). Submission and
// restoration (LRN-009) use it.
func CheckStoredContentTx(ctx context.Context, tx pgx.Tx, revisionID int64) error {
	var content []byte
	if err := tx.QueryRow(ctx, `SELECT content FROM lesson_revisions WHERE id = $1`, revisionID).Scan(&content); err != nil {
		return err
	}
	doc, canon, err := canonicalize(content)
	if err != nil {
		return err
	}
	if embedded := rendering.MediaIDs(doc); len(embedded) > 0 {
		var unattached bool
		err := tx.QueryRow(ctx, `
			SELECT EXISTS (SELECT 1 FROM unnest($2::bigint[]) AS e(id)
			                WHERE NOT EXISTS (SELECT 1 FROM revision_media rm
			                                   WHERE rm.revision_id = $1 AND rm.media_asset_id = e.id))`,
			revisionID, embedded).Scan(&unattached)
		if err != nil {
			return err
		}
		if unattached {
			return ErrUnattachedMedia
		}
	}
	var same bool
	if err := tx.QueryRow(ctx, `SELECT content = $2::jsonb FROM lesson_revisions WHERE id = $1`, revisionID, canon).Scan(&same); err != nil {
		return err
	}
	if !same {
		return fmt.Errorf("%w: stored content is not in canonical form", ErrInvalidContent)
	}
	return nil
}

// DiscardDraft deletes the actor's own draft at expectedVersion, with its
// media references, so the lesson can accept a new draft. Submitted
// revisions are never deleted.
func (s *Store) DiscardDraft(ctx context.Context, actor, revisionID int64, expectedVersion int) error {
	return pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		if err := permissions.RequireTx(ctx, tx, actor, permissions.Author); err != nil {
			return err
		}
		if _, err := lockOwnDraft(ctx, tx, actor, revisionID, expectedVersion); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `
			DELETE FROM lesson_revisions
			 WHERE id = $1 AND version = $2 AND author_id = $3 AND status = 'draft'`,
			revisionID, expectedVersion, actor)
		if err != nil {
			return mapErr(err)
		}
		if tag.RowsAffected() != 1 {
			return ErrVersionConflict
		}
		return nil
	})
}

// Revision loads a stored revision. It performs no authorization: callers
// exposing it (the HTTP slice) must check access first.
func (s *Store) Revision(ctx context.Context, id int64) (Revision, error) {
	var rev Revision
	err := pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		var err error
		rev, err = loadRevision(ctx, tx, id)
		return err
	})
	return rev, err
}

// RevisionTx loads a stored revision inside tx. It performs no
// authorization and takes no locks.
func RevisionTx(ctx context.Context, tx pgx.Tx, id int64) (Revision, error) {
	return loadRevision(ctx, tx, id)
}

// Lesson loads a lesson identity. It performs no authorization.
func (s *Store) Lesson(ctx context.Context, id int64) (Lesson, error) {
	var l Lesson
	err := s.db.QueryRow(ctx, `
		SELECT id, public_id::text, translation_group_id::text, language, subject_id,
		       published_revision_id, archived_at, created_at, created_by
		  FROM lessons WHERE id = $1`, id).Scan(&l.ID, &l.PublicID, &l.TranslationGroupID, &l.Language,
		&l.SubjectID, &l.PublishedRevisionID, &l.ArchivedAt, &l.CreatedAt, &l.CreatedBy)
	if errors.Is(err, pgx.ErrNoRows) {
		return Lesson{}, ErrNotFound
	}
	return l, err
}

func loadRevision(ctx context.Context, tx pgx.Tx, id int64) (Revision, error) {
	var r Revision
	var status string
	var action, comment *string
	var reviewer *int64
	var decided *time.Time
	err := tx.QueryRow(ctx, `
		SELECT r.id, r.lesson_id, r.revision_number, r.status, r.author_id, r.base_revision_id, r.title,
		       r.objectives, r.prerequisites, r.content, r.schema_version, r.language, r.subject_id,
		       r.chapter_id, r.version, r.created_at, r.updated_at, r.submitted_at,
		       ARRAY(SELECT media_asset_id FROM revision_media WHERE revision_id = r.id ORDER BY 1),
		       d.action, d.reviewer_id, d.comment, d.decided_at
		  FROM lesson_revisions r LEFT JOIN review_decisions d ON d.revision_id = r.id
		 WHERE r.id = $1`, id).Scan(&r.ID, &r.LessonID, &r.Number, &status,
		&r.AuthorID, &r.BaseRevisionID, &r.Title, &r.Objectives, &r.Prerequisites, &r.Content,
		&r.SchemaVersion, &r.Language, &r.SubjectID, &r.ChapterID, &r.Version, &r.CreatedAt,
		&r.UpdatedAt, &r.SubmittedAt, &r.MediaIDs, &action, &reviewer, &comment, &decided)
	if errors.Is(err, pgx.ErrNoRows) {
		return Revision{}, ErrNotFound
	}
	if err != nil {
		return Revision{}, fmt.Errorf("load revision: %w", err)
	}
	r.Status = Status(status)
	if action != nil && reviewer != nil && decided != nil {
		r.Decision = &Decision{Action: *action, ReviewerID: *reviewer, DecidedAt: *decided}
		if comment != nil {
			r.Decision.Comment = *comment
		}
	}
	return r, nil
}

// mapErr turns constraint and guard violations into typed errors.
func mapErr(err error) error {
	var pgErr *pgconn.PgError
	if err == nil || !errors.As(err, &pgErr) {
		return err
	}
	switch pgErr.Code {
	case "23503": // foreign_key_violation
		switch pgErr.ConstraintName {
		case "lessons_subject_id_fkey", "translation_groups_subject_id_fkey":
			return ErrUnknownSubject
		case "lesson_revisions_chapter_fkey":
			return ErrUnknownChapter
		case "revision_media_media_asset_id_fkey":
			return ErrUnknownMedia
		}
	case "23505": // unique_violation
		switch pgErr.ConstraintName {
		case "lesson_revisions_one_active_candidate":
			return ErrActiveCandidateExists
		case "lessons_translation_language_key":
			return ErrTranslationExists
		}
	case "22021", "22P05": // NUL or invalid bytes in text; \u0000 in jsonb
		return fmt.Errorf("%w: %s", ErrInvalidContent, pgErr.Message)
	case "LRN01":
		switch {
		case strings.Contains(pgErr.Message, "no lesson can be added to it"):
			return errors.Join(ErrRetiredSubject, err)
		case strings.Contains(pgErr.Message, "a draft keeps it only from its base revision"):
			return errors.Join(ErrRetiredChapter, err)
		}
		return fmt.Errorf("%w: %s", ErrImmutable, pgErr.Message)
	}
	return err
}
