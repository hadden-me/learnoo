// Package reports stores student error reports on published lessons and
// their audited resolution (LRN-015, migrations/0009_error_reports.sql).
//
// A signed-in reader reports an error on the published revision they were
// reading; reviewers of the lesson's subject and administrators list, read,
// resolve and dismiss reports. The database keeps the rules even when the
// application is bypassed: a report names the lesson's current published
// revision at insert time, its content never changes, and its status leaves
// "open" once, only together with an append-only resolution row (actor,
// action, note, time) of the same transaction. Reports never change lesson
// content.
//
// Authorization happens inside each transaction with the grant and account
// rows share-locked (internal/permissions). Callers who may not see a report
// get ErrNotFound, exactly as for a missing ID.
package reports

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

// Status of a report, as stored.
type Status string

const (
	StatusOpen      Status = "open"
	StatusResolved  Status = "resolved"
	StatusDismissed Status = "dismissed"
)

// Resolution actions, as stored.
const (
	ActionResolve = "resolve"
	ActionDismiss = "dismiss"
)

const (
	// MaxMessage bounds a report's text, in characters (database check too).
	MaxMessage = 2000
	// MaxNote bounds a resolution note, in characters (database check too).
	MaxNote = 2000
	// MaxList bounds how many reports List returns.
	MaxList = 200
)

var (
	// ErrNotFound means no report the caller may see has this ID, or (for
	// Create) no published lesson has this public ID.
	ErrNotFound = errors.New("not found")
	// ErrDenied is permissions.ErrDenied: the caller is neither an
	// administrator nor a reviewer.
	ErrDenied = permissions.ErrDenied
	// ErrRevisionChanged means the lesson's published revision is no longer
	// the one the reader displayed. Nothing was stored.
	ErrRevisionChanged = errors.New("the published revision changed since it was displayed")
	// ErrTextRequired means the report text or resolution note is blank.
	ErrTextRequired = errors.New("text is required")
	// ErrInvalidText means the text is too long, not valid UTF-8, or holds
	// control (other than line feeds), bidirectional formatting or line and
	// paragraph separator characters.
	ErrInvalidText = errors.New("invalid text")
	// ErrNotOpen means the report was already resolved or dismissed.
	ErrNotOpen = errors.New("report is not open")
	// ErrInvalidStatus means an unknown status filter.
	ErrInvalidStatus = errors.New("invalid status")
	// ErrRejected reports a change refused by a database guard. The Store's
	// own checks normally return a more specific error first.
	ErrRejected = errors.New("change rejected by a database guard")
)

// NormalizeText prepares a report text or resolution note: line breaks
// (CR LF and lone CR, as browsers submit textarea values) become LF,
// surrounding whitespace is trimmed, and the result must hold 1 to max
// characters of valid UTF-8 with no control character other than LF and no
// bidirectional formatting (unicode.Bidi_Control) or U+2028/U+2029
// characters, the rule applied to display names
// (invitations.NormalizeDisplayName) with line feeds allowed. It returns
// ErrTextRequired for blank text and ErrInvalidText otherwise.
func NormalizeText(s string, max int) (string, error) {
	if !utf8.ValidString(s) {
		return "", ErrInvalidText
	}
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	s = strings.TrimSpace(s)
	if s == "" {
		return "", ErrTextRequired
	}
	if utf8.RuneCountInString(s) > max {
		return "", ErrInvalidText
	}
	for _, r := range s {
		if (unicode.IsControl(r) && r != '\n') || unicode.Is(unicode.Bidi_Control, r) || r == ' ' || r == ' ' {
			return "", ErrInvalidText
		}
	}
	return s, nil
}

// publicIDPattern is the lower-case UUID form the reader links to.
var publicIDPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// Store records and resolves error reports.
type Store struct{ db *pgxpool.Pool }

// NewStore builds a Store.
func NewStore(db *pgxpool.Pool) *Store { return &Store{db: db} }

// Resolution is the closing of a report.
type Resolution struct {
	Action     string
	ActorID    int64
	Note       string
	OccurredAt time.Time
}

// Report is an error report as reviewers see it. The reporting account is
// stored but not returned (minimal exposure of student data).
type Report struct {
	ID             int64
	LessonID       int64
	LessonPublicID string
	SubjectID      int64
	RevisionID     int64
	RevisionNumber int
	RevisionTitle  string
	// CourseRevisionID, CoursePublicID and CourseRevisionNumber are set for
	// a report filed in course context: the published course revision the
	// step was displayed in.
	CourseRevisionID     *int64
	CoursePublicID       string
	CourseRevisionNumber int
	// Current is true while the reported revision is still displayed where
	// it was reported: the lesson's published, readable revision, or, in
	// course context, a readable step of the course's published revision.
	Current    bool
	Message    string
	Status     Status
	CreatedAt  time.Time
	Resolution *Resolution
}

// Receipt is what the reader learns after filing a report.
type Receipt struct {
	ID         int64
	RevisionID int64
	CreatedAt  time.Time
}

// Create files a report by reporter on the lesson with publicID, for the
// published revision the reader displayed (its lesson-scoped number). The
// lesson must currently be readable (reader_published_lessons) and its
// published revision must still be revisionNumber; otherwise ErrNotFound or
// ErrRevisionChanged is returned and nothing is stored. The lesson row is
// share-locked first, so the published revision cannot change before the
// report commits; the database guard repeats the check.
func (s *Store) Create(ctx context.Context, reporter int64, publicID string, revisionNumber int, message string) (Receipt, error) {
	message, err := NormalizeText(message, MaxMessage)
	if err != nil {
		return Receipt{}, err
	}
	if reporter == 0 {
		return Receipt{}, ErrDenied
	}
	if !publicIDPattern.MatchString(publicID) {
		return Receipt{}, ErrNotFound
	}
	var rc Receipt
	err = pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		var lessonID int64
		err := tx.QueryRow(ctx, `SELECT id FROM lessons WHERE public_id = $1::uuid FOR SHARE`, publicID).Scan(&lessonID)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		var revisionID int64
		var number int
		err = tx.QueryRow(ctx, `
			SELECT p.revision_id, r.revision_number
			  FROM reader_published_lessons p JOIN lesson_revisions r ON r.id = p.revision_id
			 WHERE p.lesson_id = $1`, lessonID).Scan(&revisionID, &number)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if number != revisionNumber {
			return ErrRevisionChanged
		}
		rc.RevisionID = revisionID
		err = tx.QueryRow(ctx, `
			INSERT INTO error_reports (lesson_id, revision_id, reporter_id, message)
			VALUES ($1, $2, $3, $4) RETURNING id, created_at`, lessonID, revisionID, reporter, message).Scan(&rc.ID, &rc.CreatedAt)
		return mapErr(err)
	})
	if err != nil {
		return Receipt{}, err
	}
	return rc, nil
}

// CreateInCourse files a report by reporter on the lesson with
// lessonPublicID as displayed in the course with coursePublicID (LMS-B08):
// courseRevisionNumber is the course's published revision and
// revisionNumber the lesson revision its step pins, both as displayed. The
// course must be readable (reader_published_courses), the lesson a readable
// step of its published revision (reader_course_steps), and both numbers
// must still match; otherwise ErrNotFound (course or step not readable) or
// ErrRevisionChanged (a newer course revision, or another pinned revision)
// is returned and nothing is stored. Locks follow contract §2: the lesson
// row, then the course row (FOR SHARE), so neither can change before the
// report commits; the database guard repeats the check. The report names
// the pinned revision, which may differ from the lesson's standalone
// published revision.
func (s *Store) CreateInCourse(ctx context.Context, reporter int64, coursePublicID string, courseRevisionNumber int,
	lessonPublicID string, revisionNumber int, message string) (Receipt, error) {
	message, err := NormalizeText(message, MaxMessage)
	if err != nil {
		return Receipt{}, err
	}
	if reporter == 0 {
		return Receipt{}, ErrDenied
	}
	if !publicIDPattern.MatchString(coursePublicID) || !publicIDPattern.MatchString(lessonPublicID) {
		return Receipt{}, ErrNotFound
	}
	var rc Receipt
	err = pgx.BeginTxFunc(ctx, s.db, pgx.TxOptions{IsoLevel: pgx.ReadCommitted}, func(tx pgx.Tx) error {
		var lessonID int64
		err := tx.QueryRow(ctx, `SELECT id FROM lessons WHERE public_id = $1::uuid FOR SHARE`, lessonPublicID).Scan(&lessonID)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		var courseID int64
		err = tx.QueryRow(ctx, `SELECT id FROM courses WHERE public_id = $1::uuid FOR SHARE`, coursePublicID).Scan(&courseID)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		var courseRevisionID int64
		var courseNumber int
		err = tx.QueryRow(ctx, `SELECT revision_id, revision_number FROM reader_published_courses WHERE course_id = $1`,
			courseID).Scan(&courseRevisionID, &courseNumber)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if courseNumber != courseRevisionNumber {
			return ErrRevisionChanged
		}
		var revisionID int64
		var number int
		var readable bool
		err = tx.QueryRow(ctx, `
			SELECT st.readable, st.lesson_revision_id, r.revision_number
			  FROM reader_course_steps st JOIN lesson_revisions r ON r.id = st.lesson_revision_id
			 WHERE st.course_revision_id = $1 AND st.lesson_id = $2`, courseRevisionID, lessonID).Scan(&readable, &revisionID, &number)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		// An unreadable step is reported as not found: its content is not
		// shown anywhere.
		if !readable {
			return ErrNotFound
		}
		if number != revisionNumber {
			return ErrRevisionChanged
		}
		rc.RevisionID = revisionID
		err = tx.QueryRow(ctx, `
			INSERT INTO error_reports (lesson_id, revision_id, reporter_id, message, course_revision_id)
			VALUES ($1, $2, $3, $4, $5) RETURNING id, created_at`, lessonID, revisionID, reporter, message, courseRevisionID).Scan(&rc.ID, &rc.CreatedAt)
		return mapErr(err)
	})
	if err != nil {
		return Receipt{}, err
	}
	return rc, nil
}

// Filter narrows List. Zero values mean no restriction.
type Filter struct {
	Status    Status
	SubjectID int64
}

// List returns reports the actor may see, newest first, at most MaxList:
// every report for an active administrator, otherwise those of the
// subjects the actor actively reviews. A SubjectID the actor does not
// review (and is not an administrator) returns ErrNotFound. An actor who is
// neither gets ErrDenied.
func (s *Store) List(ctx context.Context, actor int64, f Filter) ([]Report, error) {
	switch f.Status {
	case "", StatusOpen, StatusResolved, StatusDismissed:
	default:
		return nil, ErrInvalidStatus
	}
	var out []Report
	err := pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		admin, err := isAdmin(ctx, tx, actor)
		if err != nil {
			return err
		}
		if !admin {
			if f.SubjectID != 0 {
				if err := permissions.RequireReviewerTx(ctx, tx, actor, f.SubjectID); err != nil {
					return hideDenial(err)
				}
			} else {
				var reviews bool
				if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM reviewer_assignments a JOIN users u ON u.id = a.user_id
					WHERE a.user_id = $1 AND u.disabled_at IS NULL)`, actor).Scan(&reviews); err != nil {
					return err
				}
				if !reviews {
					return ErrDenied
				}
			}
		}
		rows, err := tx.Query(ctx, `
			SELECT `+reportColumns+`
			  FROM `+reportJoins+`
			 WHERE ($2 OR EXISTS (SELECT 1 FROM reviewer_assignments a JOIN users u ON u.id = a.user_id
			                       WHERE a.user_id = $1 AND a.subject_id = l.subject_id AND u.disabled_at IS NULL))
			   AND ($3 = '' OR e.status = $3)
			   AND ($4 = 0 OR l.subject_id = $4)
			 ORDER BY e.created_at DESC, e.id DESC
			 LIMIT $5`, actor, admin, string(f.Status), f.SubjectID, MaxList)
		if err != nil {
			return err
		}
		out, err = pgx.CollectRows(rows, scanReport)
		return err
	})
	if err != nil {
		return nil, err
	}
	if out == nil {
		out = []Report{}
	}
	return out, nil
}

// Get returns one report to an active administrator or an active reviewer
// of its lesson's subject; anyone else gets ErrNotFound.
func (s *Store) Get(ctx context.Context, actor, reportID int64) (Report, error) {
	var rep Report
	err := pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		var err error
		rep, err = loadReport(ctx, tx, reportID, false)
		if err != nil {
			return err
		}
		return authorize(ctx, tx, actor, rep.SubjectID)
	})
	if err != nil {
		return Report{}, err
	}
	return rep, nil
}

// Resolve closes an open report as actor with action ActionResolve or
// ActionDismiss and a required note, recording the resolution in the same
// transaction. The report row is locked first, so of concurrent
// resolutions exactly one applies; the others get ErrNotOpen. Callers who
// may not see the report get ErrNotFound.
func (s *Store) Resolve(ctx context.Context, actor, reportID int64, action, note string) (Report, error) {
	if action != ActionResolve && action != ActionDismiss {
		return Report{}, fmt.Errorf("unknown resolution action %q", action)
	}
	note, err := NormalizeText(note, MaxNote)
	if err != nil {
		return Report{}, err
	}
	var rep Report
	err = pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		var err error
		rep, err = loadReport(ctx, tx, reportID, true)
		if err != nil {
			return err
		}
		// Authorization first, so callers without access learn nothing
		// about the report's status.
		if err := authorize(ctx, tx, actor, rep.SubjectID); err != nil {
			return err
		}
		if rep.Status != StatusOpen {
			return ErrNotOpen
		}
		res := Resolution{Action: action, ActorID: actor, Note: note}
		err = tx.QueryRow(ctx, `
			INSERT INTO error_report_resolutions (report_id, actor_id, action, note)
			VALUES ($1, $2, $3, $4) RETURNING occurred_at`, reportID, actor, action, note).Scan(&res.OccurredAt)
		if err != nil {
			return mapErr(err)
		}
		next := StatusResolved
		if action == ActionDismiss {
			next = StatusDismissed
		}
		tag, err := tx.Exec(ctx, `UPDATE error_reports SET status = $2 WHERE id = $1 AND status = 'open'`, reportID, string(next))
		if err != nil {
			return mapErr(err)
		}
		if tag.RowsAffected() != 1 {
			return ErrNotOpen
		}
		rep.Status, rep.Resolution = next, &res
		return nil
	})
	if err != nil {
		return Report{}, err
	}
	return rep, nil
}

const reportColumns = `e.id, e.lesson_id, l.public_id::text, l.subject_id, e.revision_id, r.revision_number, r.title,
	       e.course_revision_id, coalesce(c.public_id::text, ''), coalesce(cr.revision_number, 0),
	       CASE WHEN e.course_revision_id IS NULL
	            THEN EXISTS (SELECT 1 FROM reader_published_lessons p WHERE p.lesson_id = e.lesson_id AND p.revision_id = e.revision_id)
	            ELSE EXISTS (SELECT 1 FROM reader_course_steps st
	                          WHERE st.course_revision_id = e.course_revision_id AND st.lesson_id = e.lesson_id
	                            AND st.lesson_revision_id = e.revision_id AND st.readable) END,
	       e.message, e.status, e.created_at,
	       x.action, x.actor_id, x.note, x.occurred_at`

const reportJoins = `error_reports e
	  JOIN lessons l ON l.id = e.lesson_id
	  JOIN lesson_revisions r ON r.id = e.revision_id
	  LEFT JOIN course_revisions cr ON cr.id = e.course_revision_id
	  LEFT JOIN courses c ON c.id = cr.course_id
	  LEFT JOIN error_report_resolutions x ON x.report_id = e.id`

func scanReport(row pgx.CollectableRow) (Report, error) {
	var rep Report
	var status string
	var action, note *string
	var actorID *int64
	var at *time.Time
	err := row.Scan(&rep.ID, &rep.LessonID, &rep.LessonPublicID, &rep.SubjectID, &rep.RevisionID, &rep.RevisionNumber,
		&rep.RevisionTitle, &rep.CourseRevisionID, &rep.CoursePublicID, &rep.CourseRevisionNumber, &rep.Current, &rep.Message, &status, &rep.CreatedAt, &action, &actorID, &note, &at)
	if err != nil {
		return Report{}, err
	}
	rep.Status = Status(status)
	if action != nil {
		rep.Resolution = &Resolution{Action: *action, ActorID: *actorID, Note: *note, OccurredAt: *at}
	}
	return rep, nil
}

// loadReport reads one report, locking its row FOR UPDATE when forUpdate.
func loadReport(ctx context.Context, tx pgx.Tx, id int64, forUpdate bool) (Report, error) {
	q := `SELECT ` + reportColumns + ` FROM ` + reportJoins + ` WHERE e.id = $1`
	if forUpdate {
		q += ` FOR UPDATE OF e`
	}
	rows, err := tx.Query(ctx, q, id)
	if err != nil {
		return Report{}, err
	}
	rep, err := pgx.CollectExactlyOneRow(rows, scanReport)
	if errors.Is(err, pgx.ErrNoRows) {
		return Report{}, ErrNotFound
	}
	return rep, err
}

// isAdmin reports whether actor is an active administrator, share-locking
// the grant and account rows. A failed check is an error, not a denial.
func isAdmin(ctx context.Context, tx pgx.Tx, actor int64) (bool, error) {
	err := permissions.RequireTx(ctx, tx, actor, permissions.Administrator)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, permissions.ErrDenied) && !deniedWithCause(err):
		return false, nil
	default:
		return false, err
	}
}

// authorize allows an active administrator or an active reviewer of
// subject; any other caller gets ErrNotFound.
func authorize(ctx context.Context, tx pgx.Tx, actor, subject int64) error {
	admin, err := isAdmin(ctx, tx, actor)
	if err != nil || admin {
		return err
	}
	return hideDenial(permissions.RequireReviewerTx(ctx, tx, actor, subject))
}

// hideDenial turns a plain denial into ErrNotFound and keeps failures.
func hideDenial(err error) error {
	if err != nil && errors.Is(err, permissions.ErrDenied) && !deniedWithCause(err) {
		return ErrNotFound
	}
	return err
}

// deniedWithCause reports an ErrDenied joined with the database error that
// prevented the check: a failure, not a denial.
func deniedWithCause(err error) bool {
	var joined interface{ Unwrap() []error }
	if errors.As(err, &joined) {
		for _, e := range joined.Unwrap() {
			if !errors.Is(e, permissions.ErrDenied) {
				return true
			}
		}
	}
	return false
}

// mapErr turns guard violations into ErrRejected and leaves everything
// else (including deadlocks and serialization failures) unchanged.
func mapErr(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "LRN01" {
		return fmt.Errorf("%w: %s", ErrRejected, pgErr.Message)
	}
	return err
}
