// Package invitations issues and accepts single-use, expiring invitations
// that create Student accounts (LRN-008). A Student holds no permissions.
// There is no public self-registration: invitations are issued by a local
// operator tool (cmd/server invite-student, Issue) or, since LMS-B10, by an
// active administrator through the administration API (IssueBy).
//
// Only the SHA-256 of a token is stored; the token itself is shown once to
// the operator. Issuing and accepting are recorded in the append-only
// student_invitation_audit table by database triggers (migrations/0008).
package invitations

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/hadden-me/learnoo/internal/permissions"
	"github.com/hadden-me/learnoo/internal/users"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	// DefaultTTL is how long an invitation stays valid unless the operator
	// chooses otherwise.
	DefaultTTL = 72 * time.Hour
	// MinTTL and MaxTTL bound the operator's choice (MaxTTL matches the
	// database constraint).
	MinTTL = time.Hour
	MaxTTL = 30 * 24 * time.Hour
	// MinPasswordLen and MaxPasswordBytes match create-user and the
	// password hashing limit.
	MinPasswordLen   = 12
	MaxPasswordBytes = 1024
	// MaxDisplayName matches the users table constraint.
	MaxDisplayName = 100
	// MaxOperatorLabel matches the database constraint.
	MaxOperatorLabel = 200
)

const tokenBytes = 32

var (
	// ErrInvalid covers unknown, malformed, expired and already used
	// tokens alike, so the response never says which.
	ErrInvalid = errors.New("invitation invalid, expired or already used")
	// ErrEmailTaken means an account already exists for the invited email.
	ErrEmailTaken = errors.New("an account already exists for this email")
	// ErrInvalidTTL is returned for a lifetime outside MinTTL..MaxTTL.
	ErrInvalidTTL = fmt.Errorf("invitation lifetime must be between %s and %s", MinTTL, MaxTTL)
	// ErrInvalidLabel is returned for an empty or overlong label, or one with
	// control, format (invisible or bidirectional) or line/paragraph
	// separator characters.
	ErrInvalidLabel = errors.New("operator label must be 1-200 characters without control, invisible or bidirectional characters")
	// ErrInvalidName is returned for an unacceptable display name.
	ErrInvalidName = errors.New("display name must be 1-100 characters, without control or bidirectional formatting characters")
	// ErrInvalidPassword is returned for a password outside the length rules.
	ErrInvalidPassword = fmt.Errorf("password must be at least %d characters and at most %d bytes", MinPasswordLen, MaxPasswordBytes)
)

// Invitation is a stored invitation without its token.
type Invitation struct {
	ID    int64
	Email string
	// Exactly one of OperatorLabel (operator tools) and IssuedBy (an
	// administrator) names the issuer.
	OperatorLabel string
	IssuedBy      *int64
	CreatedAt     time.Time
	ExpiresAt     time.Time
}

// Store issues and accepts invitations.
type Store struct {
	db  *pgxpool.Pool
	now func() time.Time
}

// NewStore builds a Store. now (used for issuing) may be nil for the wall
// clock; acceptance always uses the database's transaction time.
func NewStore(db *pgxpool.Pool, now func() time.Time) *Store {
	if now == nil {
		now = time.Now
	}
	return &Store{db: db, now: now}
}

func normalizeLabel(label string) (string, error) {
	l := strings.TrimSpace(label)
	if l == "" || utf8.RuneCountInString(l) > MaxOperatorLabel || !utf8.ValidString(l) {
		return "", ErrInvalidLabel
	}
	// Control, format (invisible, bidirectional) and line/paragraph
	// separator characters could make the audit label display as another
	// identity; the database constraint refuses the same set.
	for _, r := range l {
		if unicode.In(r, unicode.Cc, unicode.Cf, unicode.Zl, unicode.Zp) {
			return "", ErrInvalidLabel
		}
	}
	return l, nil
}

// NormalizeDisplayName trims a display name and rejects empty or overlong
// names, invalid UTF-8, control characters and bidirectional formatting
// characters (which could reorder surrounding text where names are shown,
// for example in author and reviewer credit).
func NormalizeDisplayName(name string) (string, error) {
	if !utf8.ValidString(name) {
		return "", ErrInvalidName
	}
	n := strings.TrimSpace(name)
	if c := utf8.RuneCountInString(n); c < 1 || c > MaxDisplayName {
		return "", ErrInvalidName
	}
	for _, r := range n {
		if unicode.IsControl(r) || unicode.Is(unicode.Bidi_Control, r) || r == ' ' || r == ' ' {
			return "", ErrInvalidName
		}
	}
	return n, nil
}

// CheckPassword applies the account password rules.
func CheckPassword(password string) error {
	if !utf8.ValidString(password) || utf8.RuneCountInString(password) < MinPasswordLen || len(password) > MaxPasswordBytes {
		return ErrInvalidPassword
	}
	return nil
}

// Issue creates an invitation for email valid for ttl, recorded with the
// operator label, and returns the one-time token. The token is not stored
// and cannot be recovered; issue a new invitation if it is lost. An email
// that already has an account is refused.
func (s *Store) Issue(ctx context.Context, email, operatorLabel string, ttl time.Duration) (string, Invitation, error) {
	label, err := normalizeLabel(operatorLabel)
	if err != nil {
		return "", Invitation{}, err
	}
	return s.issue(ctx, email, &label, nil, ttl)
}

// IssueBy is Issue for an administrator (LMS-B10): adminID must hold
// administrator on an active account, rechecked first inside the
// transaction (permissions.RequireTx; permissions.ErrDenied otherwise), and
// is recorded as the invitation's issuer and in its audit row
// (migrations/0016) instead of an operator label.
func (s *Store) IssueBy(ctx context.Context, adminID int64, email string, ttl time.Duration) (string, Invitation, error) {
	return s.issue(ctx, email, nil, &adminID, ttl)
}

func (s *Store) issue(ctx context.Context, email string, label *string, adminID *int64, ttl time.Duration) (string, Invitation, error) {
	e, err := users.NormalizeEmail(email)
	if err != nil {
		return "", Invitation{}, err
	}
	if ttl < MinTTL || ttl > MaxTTL {
		return "", Invitation{}, ErrInvalidTTL
	}
	token := newToken()
	now := s.now().UTC().Truncate(time.Microsecond)
	inv := Invitation{Email: e, IssuedBy: adminID, CreatedAt: now, ExpiresAt: now.Add(ttl)}
	if label != nil {
		inv.OperatorLabel = *label
	}
	err = pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		if adminID != nil {
			if err := permissions.RequireTx(ctx, tx, *adminID, permissions.Administrator); err != nil {
				return err
			}
		}
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM users WHERE email = $1)`, e).Scan(&exists); err != nil {
			return err
		}
		if exists {
			return ErrEmailTaken
		}
		return tx.QueryRow(ctx, `
			INSERT INTO student_invitations (token_hash, email, operator_label, issued_by, created_at, expires_at)
			VALUES ($1, $2, $3, $4, $5, $6) RETURNING id`,
			hashToken(token), e, label, adminID, inv.CreatedAt, inv.ExpiresAt).Scan(&inv.ID)
	})
	if err != nil {
		if errors.Is(err, ErrEmailTaken) || errors.Is(err, permissions.ErrDenied) {
			return "", Invitation{}, err
		}
		return "", Invitation{}, fmt.Errorf("issue invitation: %w", err)
	}
	return token, inv, nil
}

// Pending returns the invited email for a token that is still usable:
// known, unexpired (database time) and not accepted. Otherwise ErrInvalid.
func (s *Store) Pending(ctx context.Context, token string) (string, error) {
	if !validToken(token) {
		return "", ErrInvalid
	}
	var email string
	err := s.db.QueryRow(ctx, `
		SELECT email FROM student_invitations
		 WHERE token_hash = $1 AND accepted_at IS NULL AND expires_at > now()`, hashToken(token)).Scan(&email)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrInvalid
	}
	if err != nil {
		return "", fmt.Errorf("load invitation: %w", err)
	}
	return email, nil
}

// Accept creates the Student account for a usable token with displayName
// and an already-computed password hash, and marks the invitation used, in
// one transaction. The invitation row is locked, so of concurrent
// acceptances exactly one succeeds; the others get ErrInvalid. The account
// receives no permissions.
func (s *Store) Accept(ctx context.Context, token, displayName, passwordHash string) (users.User, error) {
	if !validToken(token) {
		return users.User{}, ErrInvalid
	}
	name, err := NormalizeDisplayName(displayName)
	if err != nil {
		return users.User{}, err
	}
	var u users.User
	err = pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		var id int64
		var email string
		err := tx.QueryRow(ctx, `
			SELECT id, email FROM student_invitations
			 WHERE token_hash = $1 AND accepted_at IS NULL AND expires_at > now()
			 FOR UPDATE`, hashToken(token)).Scan(&id, &email)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrInvalid
		}
		if err != nil {
			return err
		}
		// FOR UPDATE rechecks the WHERE clause against the committed row
		// after waiting, so a concurrent acceptance that committed first
		// yields no row here.
		u = users.User{Email: email, DisplayName: name}
		err = tx.QueryRow(ctx,
			`INSERT INTO users (email, display_name, password_hash) VALUES ($1, $2, $3) RETURNING id, created_at`,
			email, name, passwordHash).Scan(&u.ID, &u.CreatedAt)
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return ErrEmailTaken
		}
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE student_invitations SET accepted_at = now(), accepted_user_id = $2 WHERE id = $1`, id, u.ID)
		return err
	})
	if err != nil {
		if errors.Is(err, ErrInvalid) || errors.Is(err, ErrEmailTaken) {
			return users.User{}, err
		}
		return users.User{}, fmt.Errorf("accept invitation: %w", err)
	}
	return u, nil
}

func newToken() string {
	b := make([]byte, tokenBytes)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand failed: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func validToken(token string) bool {
	if len(token) != base64.RawURLEncoding.EncodedLen(tokenBytes) {
		return false
	}
	_, err := base64.RawURLEncoding.DecodeString(token)
	return err == nil
}

func hashToken(token string) []byte {
	h := sha256.Sum256([]byte("learnoo-student-invitation\x00" + token))
	return h[:]
}
