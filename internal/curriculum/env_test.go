package curriculum_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/hadden-me/learnoo/internal/auth"
	"github.com/hadden-me/learnoo/internal/curriculum"
	"github.com/hadden-me/learnoo/internal/database/dbtest"
	"github.com/hadden-me/learnoo/internal/permissions"
	"github.com/hadden-me/learnoo/internal/users"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

var testArgon2 = auth.Argon2Params{MemoryKiB: 1024, Iterations: 1, Lanes: 1, SaltLen: 16, KeyLen: 32}

var testOperator = func() permissions.Actor {
	a, err := permissions.OperatorAs("test-operator@test-host")
	if err != nil {
		panic(err)
	}
	return a
}()

type env struct {
	t        *testing.T
	ctx      context.Context
	db       *pgxpool.Pool
	store    *curriculum.Store
	perms    *permissions.Store
	admin    permissions.Actor
	adminID  int64
	authorID int64
	math     int64
	physics  int64
}

func newEnv(t *testing.T) *env {
	t.Helper()
	db := dbtest.Migrated(t)
	e := &env{t: t, ctx: context.Background(), db: db, store: curriculum.NewStore(db), perms: permissions.NewStore(db)}
	e.adminID = e.user("admin", permissions.Administrator)
	e.admin = permissions.UserActor(e.adminID)
	e.authorID = e.user("author", permissions.Author, permissions.Publisher)
	e.math = dbtest.AddSubject(t, db, "test-math")
	e.physics = dbtest.AddSubject(t, db, "test-physics")
	return e
}

func (e *env) user(name string, perms ...permissions.Permission) int64 {
	e.t.Helper()
	hash, err := auth.HashPassword("simulated-password-1", testArgon2)
	if err != nil {
		e.t.Fatal(err)
	}
	u, err := users.Create(e.ctx, e.db, name+"@example.test", "TEST "+name, hash)
	if err != nil {
		e.t.Fatal(err)
	}
	for _, p := range perms {
		if _, err := e.perms.Grant(e.ctx, testOperator, u.ID, p); err != nil {
			e.t.Fatal(err)
		}
	}
	return u.ID
}

func (e *env) chapter(subject int64, code string) int64 {
	e.t.Helper()
	return dbtest.AddChapter(e.t, e.db, subject, code)
}

func (e *env) level(code string) curriculum.Level {
	e.t.Helper()
	l, err := e.store.CreateLevel(e.ctx, e.admin, curriculum.Entry{Code: code, NameFR: "TEST Niveau " + code, NameAR: "اختبار " + code})
	if err != nil {
		e.t.Fatal(err)
	}
	return l
}

func (e *env) section(level int64, code string) curriculum.Section {
	e.t.Helper()
	s, err := e.store.CreateSection(e.ctx, e.admin, level, curriculum.Entry{Code: code, NameFR: "TEST Section " + code, NameAR: "شعبة " + code})
	if err != nil {
		e.t.Fatal(err)
	}
	return s
}

func (e *env) edition(section int64, code string) curriculum.Edition {
	e.t.Helper()
	ed, err := e.store.CreateEdition(e.ctx, e.admin, section, curriculum.NewEdition{Code: code, LabelFR: "TEST Édition " + code, LabelAR: "طبعة " + code})
	if err != nil {
		e.t.Fatal(err)
	}
	return ed
}

// hierarchy creates a level, a section and a draft edition.
func (e *env) hierarchy() (curriculum.Level, curriculum.Section, curriculum.Edition) {
	e.t.Helper()
	l := e.level("test-bac")
	s := e.section(l.ID, "test-maths")
	return l, s, e.edition(s.ID, "test-2026")
}

func (e *env) exec(sql string, args ...any) error {
	_, err := e.db.Exec(e.ctx, sql, args...)
	return err
}

func (e *env) count(sql string, args ...any) int {
	e.t.Helper()
	var n int
	if err := e.db.QueryRow(e.ctx, sql, args...).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

func wantErr(t *testing.T, what string, err, want error) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Errorf("%s: err %v, want %v", what, err, want)
	}
}

// wantGuard asserts an LRN01 error whose message contains msg.
func wantGuard(t *testing.T, what string, err error, msg string) {
	t.Helper()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "LRN01" || !strings.Contains(pgErr.Message, msg) {
		t.Errorf("%s: want LRN01 %q, got %v", what, msg, err)
	}
}

func wantSQLState(t *testing.T, what string, err error, code string) {
	t.Helper()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != code {
		t.Errorf("%s: want SQLSTATE %s, got %v", what, code, err)
	}
}

// leastPrivilegeRole creates a non-owner role like the deployed runtime
// role (the pattern of internal/reports/guards_test.go): USAGE on the test
// schema, DML on its tables and sequences, EXECUTE on its functions (the
// default for PUBLIC) and TEMP. asRole runs statements in one committed
// transaction as that role on a new connection; "S." in a statement is
// replaced by the quoted test schema.
func (e *env) leastPrivilegeRole() (asRole func(stmts ...string) error) {
	asRoleAt := e.leastPrivilegeRoleAt()
	return func(stmts ...string) error { return asRoleAt(pgx.ReadCommitted, stmts...) }
}

// leastPrivilegeRoleAt is leastPrivilegeRole with the transaction's
// isolation level as a parameter.
func (e *env) leastPrivilegeRoleAt() (asRole func(iso pgx.TxIsoLevel, stmts ...string) error) {
	t := e.t
	t.Helper()
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatal(err)
	}
	role := pgx.Identifier{"learnoo_test_app_" + hex.EncodeToString(b[:])}.Sanitize()
	var schema, dbName string
	if err := e.db.QueryRow(e.ctx, `SELECT current_schema(), current_database()`).Scan(&schema, &dbName); err != nil {
		t.Fatal(err)
	}
	must := func(sql string) {
		t.Helper()
		if err := e.exec(sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	must(`CREATE ROLE ` + role + ` NOLOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT`)
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = e.db.Exec(ctx, `DROP OWNED BY `+role)
		if _, err := e.db.Exec(ctx, `DROP ROLE `+role); err != nil {
			t.Errorf("drop test role: %v", err)
		}
	})
	qs := pgx.Identifier{schema}.Sanitize()
	must(`GRANT USAGE ON SCHEMA ` + qs + ` TO ` + role)
	must(`GRANT SELECT, INSERT, UPDATE, DELETE, TRUNCATE ON ALL TABLES IN SCHEMA ` + qs + ` TO ` + role)
	must(`GRANT USAGE ON ALL SEQUENCES IN SCHEMA ` + qs + ` TO ` + role)
	must(`GRANT TEMP ON DATABASE ` + pgx.Identifier{dbName}.Sanitize() + ` TO ` + role)
	return func(iso pgx.TxIsoLevel, stmts ...string) error {
		conn, err := pgx.ConnectConfig(e.ctx, e.db.Config().ConnConfig.Copy())
		if err != nil {
			return err
		}
		defer func() { _ = conn.Close(context.Background()) }()
		tx, err := conn.BeginTx(e.ctx, pgx.TxOptions{IsoLevel: iso})
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback(e.ctx) }()
		if _, err := tx.Exec(e.ctx, `SET LOCAL ROLE `+role); err != nil {
			return err
		}
		for _, s := range stmts {
			if _, err := tx.Exec(e.ctx, strings.ReplaceAll(s, "S.", qs+".")); err != nil {
				return err
			}
		}
		return tx.Commit(e.ctx)
	}
}

// auditSQL is the audit row a direct SQL change needs; before and after are
// SQL expressions (JSON literals such as `'{"retired": false}'`, or NULL).
func auditSQL(target string, targetID int64, edition *int64, action, before, after string) string {
	ed := "NULL"
	if edition != nil {
		ed = fmt.Sprint(*edition)
	}
	return fmt.Sprintf(`INSERT INTO S.curriculum_audit (operator_label, target, target_id, edition_id, action, before, after)
		VALUES ('test-operator@test-host', '%s', %d, %s, '%s', %s, %s)`, target, targetID, ed, action, before, after)
}

// JSON literals of the audit value shapes.
const (
	notRetired = `'{"retired": false}'`
	isRetired  = `'{"retired": true}'`
)

func stateJSON(st string) string { return `'{"state": "` + st + `"}'` }

func chaptersJSON(ids ...int64) string {
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = fmt.Sprint(id)
	}
	return `'{"chapterIds": [` + strings.Join(parts, ", ") + `]}'`
}
