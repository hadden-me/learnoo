// Package catalogue reads and extends the curriculum catalogue: subjects
// (migrations/0002) and the chapters inside them (0004), identified by
// immutable codes, with optional French and Arabic display names and a
// display order (0012). Lists are ordered by sort order, then code. Nothing
// is seeded: the pilot catalogue waits for teacher-confirmed names (LRN-014).
//
// Only the operator tools add entries (add-subject, add-chapter). Display
// names and order are changed by the operator tools (set-subject-name,
// set-chapter-name, set-order) and, since LMS-B10, by active administrators
// through the administration API, each change in one transaction with an
// append-only catalogue_audit row naming the operator label or the
// administrator (actor_id), and the old and new values of a rename or
// reorder (migrations/0010, 0012, 0016). Nothing here changes a code or
// deletes an entry.
package catalogue

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

// MaxNameRunes bounds a display name (the catalogue_valid_name rule).
const MaxNameRunes = 200

// MaxSortOrder is the largest sort order; the smallest is 0, the default.
const MaxSortOrder = 9999

var (
	// ErrInvalidName means a display name is empty after trimming, too long,
	// not valid UTF-8 or contains a forbidden character.
	ErrInvalidName = errors.New("invalid name: use 1 to 200 characters, without control, format (invisible or bidirectional) or line/paragraph separator characters")
	// ErrInvalidSortOrder means a sort order is outside 0..MaxSortOrder.
	ErrInvalidSortOrder = errors.New("invalid order: use a whole number from 0 to 9999")
	// ErrChapterNotFound means the subject has no chapter with this code.
	ErrChapterNotFound = errors.New("chapter not found")
	// ErrInvalidCode means a code does not match ValidCode.
	ErrInvalidCode = errors.New("invalid code: use 1 to 63 lower-case letters, digits and hyphens, starting with a letter or digit")
	// ErrSubjectRetired means the subject is retired: no chapter can be
	// added to it (migrations/0013).
	ErrSubjectRetired = errors.New("subject is retired; no chapter can be added to it")
	// ErrNotFound means the subject does not exist.
	ErrNotFound = errors.New("subject not found")
	// ErrOperatorOnly means a non-operator actor tried to add a subject or
	// chapter.
	ErrOperatorOnly = errors.New("only operator tools add catalogue entries")
)

// NormalizeName trims surrounding white space from a display name and
// checks the catalogue_valid_name rule (migrations/0012): 1 to MaxNameRunes
// characters, valid UTF-8, no control (Cc), format (Cf) or line/paragraph
// separator (Zl, Zp) characters. Arabic and other scripts are accepted.
func NormalizeName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" || !utf8.ValidString(name) || utf8.RuneCountInString(name) > MaxNameRunes ||
		strings.IndexFunc(name, forbiddenNameRune) >= 0 {
		return "", ErrInvalidName
	}
	return name, nil
}

// forbiddenNameRune is the operator label rule (permissions.OperatorAs). It
// must stay at least as strict as catalogue_valid_name.
func forbiddenNameRune(r rune) bool {
	return unicode.In(r, unicode.Cc, unicode.Cf, unicode.Zl, unicode.Zp)
}

// ValidSortOrder reports whether order is a valid sort order.
func ValidSortOrder(order int) bool { return order >= 0 && order <= MaxSortOrder }

// Names are an entry's display names, nil when not set.
type Names struct {
	Fr *string
	Ar *string
}

// Label is the text shown for a catalogue entry and its language: "fr" or
// "ar" for a display name, "" for the code shown when no name is set.
type Label struct {
	Text string
	Lang string
}

// Label returns the name in lang ("fr" or "ar") when set, otherwise the
// name in the other language, otherwise code.
func (n Names) Label(code, lang string) Label {
	first, second, firstLang, secondLang := n.Fr, n.Ar, "fr", "ar"
	if lang == "ar" {
		first, second, firstLang, secondLang = n.Ar, n.Fr, "ar", "fr"
	}
	switch {
	case first != nil:
		return Label{Text: *first, Lang: firstLang}
	case second != nil:
		return Label{Text: *second, Lang: secondLang}
	}
	return Label{Text: code}
}

// Subject is a catalogue subject.
type Subject struct {
	ID        int64
	Code      string
	Names     Names
	SortOrder int
	// RetiredAt is set once the subject is retired (migrations/0013): it
	// keeps resolving but cannot be newly selected.
	RetiredAt *time.Time
}

// Chapter is a chapter inside a subject.
type Chapter struct {
	ID        int64
	Code      string
	Names     Names
	SortOrder int
	// RetiredAt is set once the chapter is retired (migrations/0013).
	RetiredAt *time.Time
}

const entryColumns = `id, code, name_fr, name_ar, sort_order, retired_at`

func scanSubject(r pgx.Row) (Subject, error) {
	var sj Subject
	err := r.Scan(&sj.ID, &sj.Code, &sj.Names.Fr, &sj.Names.Ar, &sj.SortOrder, &sj.RetiredAt)
	return sj, err
}

func scanChapter(r pgx.Row) (Chapter, error) {
	var c Chapter
	err := r.Scan(&c.ID, &c.Code, &c.Names.Fr, &c.Names.Ar, &c.SortOrder, &c.RetiredAt)
	return c, err
}

// Store reads and extends the catalogue.
type Store struct{ db *pgxpool.Pool }

// NewStore builds a Store.
func NewStore(db *pgxpool.Pool) *Store { return &Store{db: db} }

// Subjects lists every subject by sort order, then code, at most MaxListed.
// It performs no authorization.
func (s *Store) Subjects(ctx context.Context) ([]Subject, error) {
	rows, err := s.db.Query(ctx, `SELECT `+entryColumns+` FROM subjects ORDER BY sort_order, code, id LIMIT $1`, MaxListed)
	if err != nil {
		return nil, fmt.Errorf("list subjects: %w", err)
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Subject, error) { return scanSubject(r) })
}

// Chapters returns the subject with this code and its chapters by sort
// order, then code, at most MaxListed. An unknown or malformed code is ErrNotFound. It performs
// no authorization.
func (s *Store) Chapters(ctx context.Context, subjectCode string) (Subject, []Chapter, error) {
	if !ValidCode(subjectCode) {
		return Subject{}, nil, ErrNotFound
	}
	var subject Subject
	var chapters []Chapter
	err := pgx.BeginTxFunc(ctx, s.db, pgx.TxOptions{AccessMode: pgx.ReadOnly, IsoLevel: pgx.RepeatableRead}, func(tx pgx.Tx) error {
		var err error
		subject, err = scanSubject(tx.QueryRow(ctx, `SELECT `+entryColumns+` FROM subjects WHERE code = $1`, subjectCode))
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("load subject: %w", err)
		}
		rows, err := tx.Query(ctx, `SELECT `+entryColumns+` FROM chapters WHERE subject_id = $1
			ORDER BY sort_order, code, id LIMIT $2`, subject.ID, MaxListed)
		if err != nil {
			return fmt.Errorf("list chapters: %w", err)
		}
		chapters, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (Chapter, error) { return scanChapter(r) })
		return err
	})
	if err != nil {
		return Subject{}, nil, err
	}
	return subject, chapters, nil
}

// SubjectWithChapters is a subject and its chapters.
type SubjectWithChapters struct {
	Subject
	Chapters []Chapter
}

// Tree lists every subject with its chapters, retired entries included,
// each list by sort order, then code, from one snapshot. More than
// MaxListed subjects or chapters in all is an error rather than a truncated
// list. It performs no authorization.
func (s *Store) Tree(ctx context.Context) ([]SubjectWithChapters, error) {
	var out []SubjectWithChapters
	err := pgx.BeginTxFunc(ctx, s.db, pgx.TxOptions{AccessMode: pgx.ReadOnly, IsoLevel: pgx.RepeatableRead}, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+entryColumns+` FROM subjects ORDER BY sort_order, code COLLATE "C", id LIMIT $1`, MaxListed+1)
		if err != nil {
			return fmt.Errorf("list subjects: %w", err)
		}
		subjects, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Subject, error) { return scanSubject(r) })
		if err != nil {
			return fmt.Errorf("list subjects: %w", err)
		}
		if len(subjects) > MaxListed {
			return fmt.Errorf("list subjects: more than %d", MaxListed)
		}
		rows, err = tx.Query(ctx, `SELECT subject_id, `+entryColumns+` FROM chapters ORDER BY sort_order, code COLLATE "C", id LIMIT $1`, MaxListed+1)
		if err != nil {
			return fmt.Errorf("list chapters: %w", err)
		}
		bySubject := map[int64][]Chapter{}
		count := 0
		for rows.Next() {
			var subjectID int64
			var c Chapter
			if err := rows.Scan(&subjectID, &c.ID, &c.Code, &c.Names.Fr, &c.Names.Ar, &c.SortOrder, &c.RetiredAt); err != nil {
				rows.Close()
				return fmt.Errorf("list chapters: %w", err)
			}
			bySubject[subjectID] = append(bySubject[subjectID], c)
			count++
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("list chapters: %w", err)
		}
		if count > MaxListed {
			return fmt.Errorf("list chapters: more than %d", MaxListed)
		}
		out = make([]SubjectWithChapters, 0, len(subjects))
		for _, sj := range subjects {
			chapters := bySubject[sj.ID]
			if chapters == nil {
				chapters = []Chapter{}
			}
			out = append(out, SubjectWithChapters{Subject: sj, Chapters: chapters})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// operatorLabel returns the audit label of an operator actor.
func operatorLabel(actor permissions.Actor) (string, error) {
	label := actor.AuditLabel()
	if label == nil {
		return "", ErrOperatorOnly
	}
	return *label, nil
}

// metadataActor is who changes display metadata: an operator (label) or an
// administrator (actor_id). A user actor is rechecked inside tx, first, so
// the permission and account rows are locked before any catalogue entry
// (docs/LMS_CONTRACT.md §2): a revocation or disablement committed before
// the change denies it.
type metadataActor struct {
	label   *string
	actorID *int64
}

func authorizeMetadata(ctx context.Context, tx pgx.Tx, actor permissions.Actor) (metadataActor, error) {
	if label := actor.AuditLabel(); label != nil {
		return metadataActor{label: label}, nil
	}
	id := actor.AuditID()
	if id == nil {
		return metadataActor{}, permissions.ErrDenied
	}
	if err := permissions.RequireTx(ctx, tx, *id, permissions.Administrator); err != nil {
		return metadataActor{}, err
	}
	return metadataActor{actorID: id}, nil
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
		var err error
		sj, err = scanSubject(tx.QueryRow(ctx, `INSERT INTO subjects (code) VALUES ($1) ON CONFLICT (code) DO NOTHING RETURNING `+entryColumns, code))
		if errors.Is(err, pgx.ErrNoRows) {
			// Already present: report it unchanged.
			sj, err = scanSubject(tx.QueryRow(ctx, `SELECT `+entryColumns+` FROM subjects WHERE code = $1`, code))
			return err
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
		var err error
		sj, err = scanSubject(tx.QueryRow(ctx, `SELECT `+entryColumns+` FROM subjects WHERE code = $1 FOR SHARE`, subjectCode))
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("load subject: %w", err)
		}
		if sj.RetiredAt != nil {
			ch, err = scanChapter(tx.QueryRow(ctx, `SELECT `+entryColumns+` FROM chapters WHERE subject_id = $1 AND code = $2`,
				sj.ID, code))
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrSubjectRetired
			}
			return err
		}
		ch, err = scanChapter(tx.QueryRow(ctx, `
			INSERT INTO chapters (subject_id, code) VALUES ($1, $2)
			ON CONFLICT ON CONSTRAINT chapters_subject_code_key DO NOTHING
			RETURNING `+entryColumns, sj.ID, code))
		if errors.Is(err, pgx.ErrNoRows) {
			ch, err = scanChapter(tx.QueryRow(ctx, `SELECT `+entryColumns+` FROM chapters WHERE subject_id = $1 AND code = $2`,
				sj.ID, code))
			return err
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

// changeNames returns names with fr and ar applied (nil keeps a name), and
// whether that changes anything. fr and ar must already be normalized.
func changeNames(cur Names, fr, ar *string) (Names, bool) {
	next := cur
	if fr != nil {
		next.Fr = fr
	}
	if ar != nil {
		next.Ar = ar
	}
	return next, !sameName(cur.Fr, next.Fr) || !sameName(cur.Ar, next.Ar)
}

func sameName(a, b *string) bool { return (a == nil) == (b == nil) && (a == nil || *a == *b) }

// normalizeNames validates the names to set: at least one, each by
// NormalizeName.
func normalizeNames(fr, ar *string) (*string, *string, error) {
	if fr == nil && ar == nil {
		return nil, nil, ErrInvalidName
	}
	norm := func(p *string) (*string, error) {
		if p == nil {
			return nil, nil
		}
		n, err := NormalizeName(*p)
		return &n, err
	}
	nfr, err := norm(fr)
	if err != nil {
		return nil, nil, err
	}
	nar, err := norm(ar)
	if err != nil {
		return nil, nil, err
	}
	return nfr, nar, nil
}

// lockSubject loads the subject with this code and locks it against
// concurrent changes for the transaction.
func lockSubject(ctx context.Context, tx pgx.Tx, code string) (Subject, error) {
	sj, err := scanSubject(tx.QueryRow(ctx, `SELECT `+entryColumns+` FROM subjects WHERE code = $1 FOR NO KEY UPDATE`, code))
	if errors.Is(err, pgx.ErrNoRows) {
		return Subject{}, ErrNotFound
	}
	if err != nil {
		return Subject{}, fmt.Errorf("load subject: %w", err)
	}
	return sj, nil
}

// lockChapter loads the subject and its chapter with these codes, locking
// the chapter against concurrent changes for the transaction.
func lockChapter(ctx context.Context, tx pgx.Tx, subjectCode, code string) (Subject, Chapter, error) {
	sj, err := scanSubject(tx.QueryRow(ctx, `SELECT `+entryColumns+` FROM subjects WHERE code = $1`, subjectCode))
	if errors.Is(err, pgx.ErrNoRows) {
		return Subject{}, Chapter{}, ErrNotFound
	}
	if err != nil {
		return Subject{}, Chapter{}, fmt.Errorf("load subject: %w", err)
	}
	ch, err := scanChapter(tx.QueryRow(ctx, `SELECT `+entryColumns+` FROM chapters WHERE subject_id = $1 AND code = $2 FOR NO KEY UPDATE`,
		sj.ID, code))
	if errors.Is(err, pgx.ErrNoRows) {
		return Subject{}, Chapter{}, ErrChapterNotFound
	}
	if err != nil {
		return Subject{}, Chapter{}, fmt.Errorf("load chapter: %w", err)
	}
	return sj, ch, nil
}

// SetSubjectNames sets the French (fr) and/or Arabic (ar) display name of
// the subject with this code; a nil name is kept. Names are normalized by
// NormalizeName; at least one is required. The change and its audit row
// (rename_subject, old and new names) are written in one transaction.
// changed is false, and nothing is written, when the names are already
// these. An unknown subject is ErrNotFound.
//
// actor is an operator (audited with its label) or a user who must be an
// active administrator, rechecked first inside the transaction
// (permissions.RequireTx; permissions.ErrDenied otherwise) and audited as
// actor_id. The same holds for SetChapterNames, SetSubjectOrder and
// SetChapterOrder.
func (s *Store) SetSubjectNames(ctx context.Context, actor permissions.Actor, code string, fr, ar *string) (sj Subject, changed bool, err error) {
	if !ValidCode(code) {
		return Subject{}, false, ErrInvalidCode
	}
	fr, ar, err = normalizeNames(fr, ar)
	if err != nil {
		return Subject{}, false, err
	}
	err = pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		who, err := authorizeMetadata(ctx, tx, actor)
		if err != nil {
			return err
		}
		cur, err := lockSubject(ctx, tx, code)
		if err != nil {
			return err
		}
		sj = cur
		next, diff := changeNames(cur.Names, fr, ar)
		if !diff {
			return nil
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO catalogue_audit (operator_label, actor_id, action, subject_id, old_name_fr, old_name_ar, new_name_fr, new_name_ar)
			VALUES ($1, $7, 'rename_subject', $2, $3, $4, $5, $6)`, who.label, cur.ID, cur.Names.Fr, cur.Names.Ar, next.Fr, next.Ar, who.actorID); err != nil {
			return fmt.Errorf("audit rename: %w", err)
		}
		if _, err := tx.Exec(ctx, `UPDATE subjects SET name_fr = $2, name_ar = $3 WHERE id = $1`, cur.ID, next.Fr, next.Ar); err != nil {
			return fmt.Errorf("rename subject: %w", err)
		}
		sj.Names, changed = next, true
		return nil
	})
	if err != nil {
		return Subject{}, false, err
	}
	return sj, changed, nil
}

// SetChapterNames is SetSubjectNames for the chapter with this code in the
// subject (audited as rename_chapter). An unknown subject is ErrNotFound, an
// unknown chapter ErrChapterNotFound.
func (s *Store) SetChapterNames(ctx context.Context, actor permissions.Actor, subjectCode, code string, fr, ar *string) (sj Subject, ch Chapter, changed bool, err error) {
	if !ValidCode(subjectCode) || !ValidCode(code) {
		return Subject{}, Chapter{}, false, ErrInvalidCode
	}
	fr, ar, err = normalizeNames(fr, ar)
	if err != nil {
		return Subject{}, Chapter{}, false, err
	}
	err = pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		who, err := authorizeMetadata(ctx, tx, actor)
		if err != nil {
			return err
		}
		var cur Chapter
		sj, cur, err = lockChapter(ctx, tx, subjectCode, code)
		if err != nil {
			return err
		}
		ch = cur
		next, diff := changeNames(cur.Names, fr, ar)
		if !diff {
			return nil
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO catalogue_audit (operator_label, actor_id, action, subject_id, chapter_id, old_name_fr, old_name_ar, new_name_fr, new_name_ar)
			VALUES ($1, $8, 'rename_chapter', $2, $3, $4, $5, $6, $7)`, who.label, sj.ID, cur.ID, cur.Names.Fr, cur.Names.Ar, next.Fr, next.Ar, who.actorID); err != nil {
			return fmt.Errorf("audit rename: %w", err)
		}
		if _, err := tx.Exec(ctx, `UPDATE chapters SET name_fr = $2, name_ar = $3 WHERE id = $1`, cur.ID, next.Fr, next.Ar); err != nil {
			return fmt.Errorf("rename chapter: %w", err)
		}
		ch.Names, changed = next, true
		return nil
	})
	if err != nil {
		return Subject{}, Chapter{}, false, err
	}
	return sj, ch, changed, nil
}

// SetSubjectOrder sets the sort order of the subject with this code. The
// change and its audit row (reorder_subject, old and new order) are written
// in one transaction; changed is false, and nothing is written, when the
// order is already this. An unknown subject is ErrNotFound.
func (s *Store) SetSubjectOrder(ctx context.Context, actor permissions.Actor, code string, order int) (sj Subject, changed bool, err error) {
	if !ValidCode(code) {
		return Subject{}, false, ErrInvalidCode
	}
	if !ValidSortOrder(order) {
		return Subject{}, false, ErrInvalidSortOrder
	}
	err = pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		who, err := authorizeMetadata(ctx, tx, actor)
		if err != nil {
			return err
		}
		cur, err := lockSubject(ctx, tx, code)
		if err != nil {
			return err
		}
		sj = cur
		if cur.SortOrder == order {
			return nil
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO catalogue_audit (operator_label, actor_id, action, subject_id, old_sort_order, new_sort_order)
			VALUES ($1, $5, 'reorder_subject', $2, $3, $4)`, who.label, cur.ID, cur.SortOrder, order, who.actorID); err != nil {
			return fmt.Errorf("audit reorder: %w", err)
		}
		if _, err := tx.Exec(ctx, `UPDATE subjects SET sort_order = $2 WHERE id = $1`, cur.ID, order); err != nil {
			return fmt.Errorf("reorder subject: %w", err)
		}
		sj.SortOrder, changed = order, true
		return nil
	})
	if err != nil {
		return Subject{}, false, err
	}
	return sj, changed, nil
}

// SetChapterOrder is SetSubjectOrder for the chapter with this code in the
// subject (audited as reorder_chapter). An unknown subject is ErrNotFound,
// an unknown chapter ErrChapterNotFound.
func (s *Store) SetChapterOrder(ctx context.Context, actor permissions.Actor, subjectCode, code string, order int) (sj Subject, ch Chapter, changed bool, err error) {
	if !ValidCode(subjectCode) || !ValidCode(code) {
		return Subject{}, Chapter{}, false, ErrInvalidCode
	}
	if !ValidSortOrder(order) {
		return Subject{}, Chapter{}, false, ErrInvalidSortOrder
	}
	err = pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		who, err := authorizeMetadata(ctx, tx, actor)
		if err != nil {
			return err
		}
		var cur Chapter
		sj, cur, err = lockChapter(ctx, tx, subjectCode, code)
		if err != nil {
			return err
		}
		ch = cur
		if cur.SortOrder == order {
			return nil
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO catalogue_audit (operator_label, actor_id, action, subject_id, chapter_id, old_sort_order, new_sort_order)
			VALUES ($1, $6, 'reorder_chapter', $2, $3, $4, $5)`, who.label, sj.ID, cur.ID, cur.SortOrder, order, who.actorID); err != nil {
			return fmt.Errorf("audit reorder: %w", err)
		}
		if _, err := tx.Exec(ctx, `UPDATE chapters SET sort_order = $2 WHERE id = $1`, cur.ID, order); err != nil {
			return fmt.Errorf("reorder chapter: %w", err)
		}
		ch.SortOrder, changed = order, true
		return nil
	})
	if err != nil {
		return Subject{}, Chapter{}, false, err
	}
	return sj, ch, changed, nil
}
