// Package catalogue reads and extends the minimal curriculum catalogue:
// subjects (migrations/0002) and the chapters inside them (0004), identified
// by codes only. Display names, ordering and seed data are LRN-014; until
// then lists are ordered by code and nothing is seeded.
//
// Only the operator tools add entries (add-subject, add-chapter), each in one
// transaction with an append-only catalogue_audit row naming the operator
// label (migrations/0010). Nothing here renames or deletes an entry.
package catalogue

import (
	"context"
	"errors"
	"fmt"
	"regexp"

	"github.com/hadden-me/learnoo/internal/permissions"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// MaxListed bounds one list response defensively. The pilot catalogue is
// far smaller.
const MaxListed = 1000

// codePattern is the subjects_code_format and chapters_code_format CHECK.
var codePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// ValidCode reports whether code is a valid subject or chapter code: 1 to 63
// lower-case ASCII letters, digits and hyphens, not starting with a hyphen.
func ValidCode(code string) bool { return codePattern.MatchString(code) }

var (
	// ErrInvalidCode means a code does not match ValidCode.
	ErrInvalidCode = errors.New("invalid code: use 1 to 63 lower-case letters, digits and hyphens, starting with a letter or digit")
	// ErrNotFound means the subject does not exist.
	ErrNotFound = errors.New("subject not found")
	// ErrOperatorOnly means a non-operator actor tried to add an entry.
	ErrOperatorOnly = errors.New("only operator tools add catalogue entries")
)

// Subject is a catalogue subject.
type Subject struct {
	ID   int64
	Code string
}

// Chapter is a chapter inside a subject.
type Chapter struct {
	ID   int64
	Code string
}

// Store reads and extends the catalogue.
type Store struct{ db *pgxpool.Pool }

// NewStore builds a Store.
func NewStore(db *pgxpool.Pool) *Store { return &Store{db: db} }

// Subjects lists every subject by code, at most MaxListed. It performs no
// authorization.
func (s *Store) Subjects(ctx context.Context) ([]Subject, error) {
	rows, err := s.db.Query(ctx, `SELECT id, code FROM subjects ORDER BY code, id LIMIT $1`, MaxListed)
	if err != nil {
		return nil, fmt.Errorf("list subjects: %w", err)
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Subject, error) {
		var sj Subject
		err := r.Scan(&sj.ID, &sj.Code)
		return sj, err
	})
}

// Chapters returns the subject with this code and its chapters by code, at
// most MaxListed. An unknown or malformed code is ErrNotFound. It performs
// no authorization.
func (s *Store) Chapters(ctx context.Context, subjectCode string) (Subject, []Chapter, error) {
	if !ValidCode(subjectCode) {
		return Subject{}, nil, ErrNotFound
	}
	var subject Subject
	var chapters []Chapter
	err := pgx.BeginTxFunc(ctx, s.db, pgx.TxOptions{AccessMode: pgx.ReadOnly, IsoLevel: pgx.RepeatableRead}, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `SELECT id, code FROM subjects WHERE code = $1`, subjectCode).Scan(&subject.ID, &subject.Code)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("load subject: %w", err)
		}
		rows, err := tx.Query(ctx, `SELECT id, code FROM chapters WHERE subject_id = $1 ORDER BY code, id LIMIT $2`, subject.ID, MaxListed)
		if err != nil {
			return fmt.Errorf("list chapters: %w", err)
		}
		chapters, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (Chapter, error) {
			var c Chapter
			err := r.Scan(&c.ID, &c.Code)
			return c, err
		})
		return err
	})
	if err != nil {
		return Subject{}, nil, err
	}
	return subject, chapters, nil
}

// operatorLabel returns the audit label of an operator actor.
func operatorLabel(actor permissions.Actor) (string, error) {
	label := actor.AuditLabel()
	if label == nil {
		return "", ErrOperatorOnly
	}
	return *label, nil
}

// AddSubject adds a subject with this code and audits it, in one
// transaction. created is false, and nothing is written, when the code
// already exists.
func (s *Store) AddSubject(ctx context.Context, actor permissions.Actor, code string) (sj Subject, created bool, err error) {
	if !ValidCode(code) {
		return Subject{}, false, ErrInvalidCode
	}
	label, err := operatorLabel(actor)
	if err != nil {
		return Subject{}, false, err
	}
	err = pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `INSERT INTO subjects (code) VALUES ($1) ON CONFLICT (code) DO NOTHING RETURNING id, code`,
			code).Scan(&sj.ID, &sj.Code)
		if errors.Is(err, pgx.ErrNoRows) {
			// Already present: report it unchanged.
			return tx.QueryRow(ctx, `SELECT id, code FROM subjects WHERE code = $1`, code).Scan(&sj.ID, &sj.Code)
		}
		if err != nil {
			return fmt.Errorf("add subject: %w", err)
		}
		created = true
		_, err = tx.Exec(ctx, `INSERT INTO catalogue_audit (operator_label, action, subject_id) VALUES ($1, 'add_subject', $2)`,
			label, sj.ID)
		return err
	})
	if err != nil {
		return Subject{}, false, err
	}
	return sj, created, nil
}

// AddChapter adds a chapter with this code to the subject and audits it, in
// one transaction. created is false, and nothing is written, when the
// subject already has a chapter with this code. An unknown subject is
// ErrNotFound.
func (s *Store) AddChapter(ctx context.Context, actor permissions.Actor, subjectCode, code string) (sj Subject, ch Chapter, created bool, err error) {
	if !ValidCode(subjectCode) || !ValidCode(code) {
		return Subject{}, Chapter{}, false, ErrInvalidCode
	}
	label, err := operatorLabel(actor)
	if err != nil {
		return Subject{}, Chapter{}, false, err
	}
	err = pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		// The share lock keeps the subject from being deleted meanwhile.
		err := tx.QueryRow(ctx, `SELECT id, code FROM subjects WHERE code = $1 FOR SHARE`, subjectCode).Scan(&sj.ID, &sj.Code)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("load subject: %w", err)
		}
		err = tx.QueryRow(ctx, `
			INSERT INTO chapters (subject_id, code) VALUES ($1, $2)
			ON CONFLICT ON CONSTRAINT chapters_subject_code_key DO NOTHING
			RETURNING id, code`, sj.ID, code).Scan(&ch.ID, &ch.Code)
		if errors.Is(err, pgx.ErrNoRows) {
			return tx.QueryRow(ctx, `SELECT id, code FROM chapters WHERE subject_id = $1 AND code = $2`,
				sj.ID, code).Scan(&ch.ID, &ch.Code)
		}
		if err != nil {
			return fmt.Errorf("add chapter: %w", err)
		}
		created = true
		_, err = tx.Exec(ctx, `INSERT INTO catalogue_audit (operator_label, action, subject_id, chapter_id) VALUES ($1, 'add_chapter', $2, $3)`,
			label, sj.ID, ch.ID)
		return err
	})
	if err != nil {
		return Subject{}, Chapter{}, false, err
	}
	return sj, ch, created, nil
}
