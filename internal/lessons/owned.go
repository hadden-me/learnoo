package lessons

import (
	"context"
	"fmt"
	"time"

	"github.com/hadden-me/learnoo/internal/permissions"
	"github.com/hadden-me/learnoo/internal/rendering"
	"github.com/jackc/pgx/v5"
)

// MaxOwnedListed bounds one Owned listing: the most recently updated lessons
// are returned first.
const MaxOwnedListed = 500

// OwnedLesson is a lesson in its owner's listing.
type OwnedLesson struct {
	ID                  int64
	PublicID            string
	TranslationGroupID  string
	Language            string
	SubjectID           int64
	SubjectCode         string
	PublishedRevisionID *int64
	Archived            bool
	CreatedAt           time.Time
	// FeedbackRevisionID names the revision whose review requested changes
	// when the author has that feedback to act on: the lesson's most recent
	// review decision is request_changes and no later revision has been
	// submitted. It stays set while the successor draft is edited. Nil
	// otherwise.
	FeedbackRevisionID *int64
	// Latest is the lesson's highest-numbered revision, or nil when its only
	// draft was discarded.
	Latest *LatestRevision
}

// LatestRevision summarizes a lesson's latest revision.
type LatestRevision struct {
	ID          int64
	Number      int
	Status      Status
	Title       string
	ChapterID   *int64
	ChapterCode *string
	Version     int
	UpdatedAt   time.Time
}

// Owned lists the lessons actor created (lessons.created_by), most recently
// updated first (by the latest revision, or the lesson's creation for a
// lesson without revisions), at most MaxOwnedListed. actor must hold author,
// checked in the same transaction; administrators get no exception and see
// only their own lessons.
func (s *Store) Owned(ctx context.Context, actor int64) ([]OwnedLesson, error) {
	var out []OwnedLesson
	err := pgx.BeginTxFunc(ctx, s.db, pgx.TxOptions{IsoLevel: pgx.RepeatableRead}, func(tx pgx.Tx) error {
		if err := permissions.RequireTx(ctx, tx, actor, permissions.Author); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `
			SELECT l.id, l.public_id::text, l.translation_group_id::text, l.language, l.subject_id, sj.code,
			       l.published_revision_id, l.archived_at IS NOT NULL, l.created_at,
			       r.id, r.revision_number, r.status, r.title, r.chapter_id, ch.code, r.version, r.updated_at,
			       CASE WHEN fd.action = 'request_changes'
			             AND NOT EXISTS (SELECT 1 FROM lesson_revisions later
			                              WHERE later.lesson_id = l.id
			                                AND later.revision_number > fd.revision_number
			                                AND later.submitted_at IS NOT NULL)
			            THEN fd.id END
			  FROM lessons l
			  JOIN subjects sj ON sj.id = l.subject_id
			  LEFT JOIN LATERAL (
			        SELECT id, revision_number, status, title, chapter_id, version, updated_at
			          FROM lesson_revisions
			         WHERE lesson_id = l.id
			         ORDER BY revision_number DESC
			         LIMIT 1) r ON true
			  LEFT JOIN chapters ch ON ch.id = r.chapter_id
			  -- The lesson's most recent review decision.
			  LEFT JOIN LATERAL (
			        SELECT dr.id, dr.revision_number, d.action
			          FROM lesson_revisions dr JOIN review_decisions d ON d.revision_id = dr.id
			         WHERE dr.lesson_id = l.id
			         ORDER BY d.decided_at DESC, dr.revision_number DESC
			         LIMIT 1) fd ON true
			 WHERE l.created_by = $1
			 ORDER BY coalesce(r.updated_at, l.created_at) DESC, l.id DESC
			 LIMIT $2`, actor, MaxOwnedListed)
		if err != nil {
			return fmt.Errorf("list owned lessons: %w", err)
		}
		out, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (OwnedLesson, error) {
			var l OwnedLesson
			var id *int64
			var number, version *int
			var status, title *string
			var updated *time.Time
			var r LatestRevision
			err := row.Scan(&l.ID, &l.PublicID, &l.TranslationGroupID, &l.Language, &l.SubjectID, &l.SubjectCode,
				&l.PublishedRevisionID, &l.Archived, &l.CreatedAt,
				&id, &number, &status, &title, &r.ChapterID, &r.ChapterCode, &version, &updated, &l.FeedbackRevisionID)
			if err != nil {
				return l, err
			}
			if id != nil {
				r.ID, r.Number, r.Status, r.Title, r.Version, r.UpdatedAt = *id, *number, Status(*status), *title, *version, *updated
				l.Latest = &r
			}
			return l, nil
		})
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ValidateContent checks a schema v1 document exactly as a draft save does
// (valid UTF-8, the canonical encoding, the schema v1 validator and its size
// limit) and returns the decoded document. Errors wrap ErrInvalidContent.
// Nothing is stored; the Studio preview uses it.
func ValidateContent(content []byte) (rendering.Document, error) {
	doc, _, err := canonicalize(content)
	return doc, err
}
