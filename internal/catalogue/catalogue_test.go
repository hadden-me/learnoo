package catalogue_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/hadden-me/learnoo/internal/catalogue"
	"github.com/hadden-me/learnoo/internal/database"
	"github.com/hadden-me/learnoo/internal/database/dbtest"
	"github.com/hadden-me/learnoo/internal/permissions"
	"github.com/hadden-me/learnoo/migrations"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Display names and order (migrations/0012, LRN-014 issue #86). Every name
// here is a TEST fixture, not curriculum data.

var operator, _ = permissions.OperatorAs(dbtest.TestOperatorLabel)

func ptr(s string) *string { return &s }

func TestLabelFallsBackToTheOtherLanguageThenTheCode(t *testing.T) {
	fr, ar := ptr("TEST Mathématiques"), ptr("TEST رياضيات")
	for _, tc := range []struct {
		names catalogue.Names
		lang  string
		want  catalogue.Label
	}{
		{catalogue.Names{Fr: fr, Ar: ar}, "fr", catalogue.Label{Text: *fr, Lang: "fr"}},
		{catalogue.Names{Fr: fr, Ar: ar}, "ar", catalogue.Label{Text: *ar, Lang: "ar"}},
		{catalogue.Names{Ar: ar}, "fr", catalogue.Label{Text: *ar, Lang: "ar"}},
		{catalogue.Names{Fr: fr}, "ar", catalogue.Label{Text: *fr, Lang: "fr"}},
		{catalogue.Names{}, "ar", catalogue.Label{Text: "test-math"}},
		{catalogue.Names{}, "fr", catalogue.Label{Text: "test-math"}},
	} {
		if got := tc.names.Label("test-math", tc.lang); got != tc.want {
			t.Errorf("%+v in %s: %+v, want %+v", tc.names, tc.lang, got, tc.want)
		}
	}
}

func TestNormalizeName(t *testing.T) {
	for in, want := range map[string]string{
		"  TEST Mathématiques \t": "TEST Mathématiques",
		"TEST رياضيات":            "TEST رياضيات",
		"TEST \u00A0x":            "TEST \u00A0x",
		strings.Repeat("ع", 200):  strings.Repeat("ع", 200),
	} {
		if got, err := catalogue.NormalizeName(in); err != nil || got != want {
			t.Errorf("%q: %q %v, want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"", " \n ", strings.Repeat("a", 201), "a\nb", "a\x7fb", "a\u0085b", "a\u00ADb", "a\u200Bb",
		"a\u200Eb", "a\u202Eb", "a\u2066b", "a\u061Cb", "a\u2028b", "a\u2029b", "a\uFEFFb", "a\U000E0041b", "a\xffb"} {
		if _, err := catalogue.NormalizeName(in); !errors.Is(err, catalogue.ErrInvalidName) {
			t.Errorf("%q: err %v, want ErrInvalidName", in, err)
		}
	}
}

type env struct {
	t     *testing.T
	ctx   context.Context
	db    *pgxpool.Pool
	store *catalogue.Store
}

func newEnv(t *testing.T, db *pgxpool.Pool) *env {
	return &env{t: t, ctx: context.Background(), db: db, store: catalogue.NewStore(db)}
}

// inTx runs statements in one transaction and commits, so deferred guards run.
func (e *env) inTx(stmts ...string) error {
	tx, err := e.db.Begin(e.ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(e.ctx) }()
	for _, s := range stmts {
		if _, err := tx.Exec(e.ctx, s); err != nil {
			return err
		}
	}
	return tx.Commit(e.ctx)
}

func (e *env) id(sql string) int64 {
	e.t.Helper()
	var id int64
	if err := e.db.QueryRow(e.ctx, sql).Scan(&id); err != nil {
		e.t.Fatalf("%s: %v", sql, err)
	}
	return id
}

// state is every name and order and the audit size.
func (e *env) state() string {
	e.t.Helper()
	var s string
	if err := e.db.QueryRow(e.ctx, `
		SELECT coalesce(string_agg(x, '; ' ORDER BY x COLLATE "C"), '') || ' audit=' || (SELECT count(*) FROM catalogue_audit)
		  FROM (SELECT format('%s %s %s %s %s', id, code, name_fr, name_ar, sort_order) AS x FROM subjects
		        UNION ALL
		        SELECT format('%s %s/%s %s %s %s', id, subject_id, code, name_fr, name_ar, sort_order) FROM chapters) e`).Scan(&s); err != nil {
		e.t.Fatal(err)
	}
	return s
}

func wantCode(t *testing.T, what string, err error, code, msg string) {
	t.Helper()
	var pg *pgconn.PgError
	if !errors.As(err, &pg) || pg.Code != code || !strings.Contains(pg.Message, msg) {
		t.Errorf("%s: err %v, want SQLSTATE %s containing %q", what, err, code, msg)
	}
}

// before0012 is the embedded migrations older than 0012, as an
// installation upgraded from 0010 (or 0011) has them.
func before0012(t *testing.T) fs.FS {
	t.Helper()
	entries, err := fs.ReadDir(migrations.FS, ".")
	if err != nil {
		t.Fatal(err)
	}
	out := fstest.MapFS{}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".sql") || e.Name() >= "0012" {
			continue
		}
		b, err := fs.ReadFile(migrations.FS, e.Name())
		if err != nil {
			t.Fatal(err)
		}
		out[e.Name()] = &fstest.MapFile{Data: b}
	}
	if _, ok := out["0010_catalogue_audit.sql"]; !ok {
		t.Fatal("0010 missing")
	}
	return out
}

func TestUpgradeKeepsExistingEntries(t *testing.T) {
	db := dbtest.EmptySchema(t)
	ctx := context.Background()
	if _, err := database.Migrate(ctx, db, before0012(t)); err != nil {
		t.Fatal(err)
	}
	e := newEnv(t, db)
	zoo := dbtest.AddSubject(t, db, "test-zoology")
	math := dbtest.AddSubject(t, db, "test-math")
	chB := dbtest.AddChapter(t, db, math, "ch-b")
	chA := dbtest.AddChapter(t, db, math, "ch-a")
	var auditBefore string
	if err := db.QueryRow(ctx, `SELECT string_agg(format('%s %s %s %s %s %s', id, occurred_at, operator_label, action, subject_id, chapter_id), '; ' ORDER BY id) FROM catalogue_audit`).Scan(&auditBefore); err != nil {
		t.Fatal(err)
	}

	applied, err := database.Migrate(ctx, db, migrations.FS)
	if err != nil {
		t.Fatal(err)
	}
	if len(applied) == 0 || !strings.HasPrefix(applied[len(applied)-1], "0012_") {
		t.Fatalf("applied %v", applied)
	}
	if err := database.Check(ctx, db, migrations.FS); err != nil {
		t.Fatal(err)
	}
	var auditAfter string
	if err := db.QueryRow(ctx, `SELECT string_agg(format('%s %s %s %s %s %s', id, occurred_at, operator_label, action, subject_id, chapter_id), '; ' ORDER BY id) FROM catalogue_audit`).Scan(&auditAfter); err != nil {
		t.Fatal(err)
	}
	if auditAfter != auditBefore {
		t.Fatalf("audit changed:\n%s\n%s", auditBefore, auditAfter)
	}
	entries := []string{fmt.Sprintf("%d test-math   0", math), fmt.Sprintf("%d test-zoology   0", zoo),
		fmt.Sprintf("%d %d/ch-a   0", chA, math), fmt.Sprintf("%d %d/ch-b   0", chB, math)}
	sort.Strings(entries)
	want := strings.Join(entries, "; ") + " audit=4"
	if got := e.state(); got != want {
		t.Fatalf("after upgrade:\n got %s\nwant %s", got, want)
	}
	// Existing entries list by code (order 0) and can be named, audited.
	subjects, err := e.store.Subjects(ctx)
	if err != nil || len(subjects) != 2 || subjects[0].Code != "test-math" || subjects[0].Names.Fr != nil {
		t.Fatalf("subjects %+v %v", subjects, err)
	}
	if _, changed, err := e.store.SetSubjectNames(ctx, operator, "test-zoology", ptr("TEST Zoologie"), nil); err != nil || !changed {
		t.Fatalf("rename: %v %v", changed, err)
	}
	if _, _, changed, err := e.store.SetChapterOrder(ctx, operator, "test-math", "ch-b", 1); err != nil || !changed {
		t.Fatalf("reorder: %v %v", changed, err)
	}
	if _, chapters, err := e.store.Chapters(ctx, "test-math"); err != nil || chapters[0].Code != "ch-a" || chapters[1].SortOrder != 1 {
		t.Fatalf("chapters %+v %v", chapters, err)
	}
}

func TestStoreOrdersAndNamesEntries(t *testing.T) {
	e := newEnv(t, dbtest.Migrated(t))
	ctx := e.ctx
	for _, code := range []string{"test-c", "test-a", "test-b"} {
		if _, _, err := e.store.AddSubject(ctx, operator, code); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := e.store.SetSubjectOrder(ctx, operator, "test-c", 1); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.store.SetSubjectOrder(ctx, operator, "test-b", 2); err != nil {
		t.Fatal(err)
	}
	sj, changed, err := e.store.SetSubjectNames(ctx, operator, "test-b", ptr(" TEST Biologie "), ptr("TEST أحياء"))
	if err != nil || !changed || *sj.Names.Fr != "TEST Biologie" || *sj.Names.Ar != "TEST أحياء" || sj.SortOrder != 2 {
		t.Fatalf("rename: %+v %v %v", sj, changed, err)
	}
	subjects, err := e.store.Subjects(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var codes []string
	for _, s := range subjects {
		codes = append(codes, fmt.Sprintf("%s:%d", s.Code, s.SortOrder))
	}
	// Order first (test-a keeps 0), then code.
	if got := strings.Join(codes, ","); got != "test-a:0,test-c:1,test-b:2" {
		t.Fatalf("order %s", got)
	}
	// Errors.
	if _, _, err := e.store.SetSubjectNames(ctx, operator, "test-x", ptr("TEST x"), nil); !errors.Is(err, catalogue.ErrNotFound) {
		t.Errorf("unknown subject: %v", err)
	}
	if _, _, _, err := e.store.SetChapterNames(ctx, operator, "test-a", "ch-x", ptr("TEST x"), nil); !errors.Is(err, catalogue.ErrChapterNotFound) {
		t.Errorf("unknown chapter: %v", err)
	}
	if _, _, err := e.store.SetSubjectNames(ctx, operator, "test-a", nil, nil); !errors.Is(err, catalogue.ErrInvalidName) {
		t.Errorf("no names: %v", err)
	}
	if _, _, err := e.store.SetSubjectOrder(ctx, operator, "test-a", 10000); !errors.Is(err, catalogue.ErrInvalidSortOrder) {
		t.Errorf("order: %v", err)
	}
	if _, _, err := e.store.SetSubjectNames(ctx, permissions.UserActor(1), "test-a", ptr("TEST x"), nil); !errors.Is(err, catalogue.ErrOperatorOnly) {
		t.Errorf("user actor: %v", err)
	}
}

// The guards of 0012, exercised with direct SQL that bypasses the Store.
func TestNameAndOrderChangesNeedTheirAudit(t *testing.T) {
	e := newEnv(t, dbtest.Migrated(t))
	math := dbtest.AddSubject(t, e.db, "test-math")
	other := dbtest.AddSubject(t, e.db, "test-physics")
	ch := dbtest.AddChapter(t, e.db, math, "ch-01")
	if _, _, err := e.store.SetSubjectNames(e.ctx, operator, "test-math", ptr("TEST Mathématiques"), nil); err != nil {
		t.Fatal(err)
	}
	audit := func(action string, chapter int64, values string) string {
		cols, vals := "operator_label, action, subject_id", fmt.Sprintf("'x@y', '%s', %d", action, math)
		if chapter != 0 {
			cols, vals = cols+", chapter_id", vals+fmt.Sprintf(", %d", chapter)
		}
		if strings.HasPrefix(action, "rename") {
			cols += ", old_name_fr, old_name_ar, new_name_fr, new_name_ar"
		} else {
			cols += ", old_sort_order, new_sort_order"
		}
		return fmt.Sprintf(`INSERT INTO catalogue_audit (%s) VALUES (%s, %s)`, cols, vals, values)
	}
	renameMath := func(fr string) string {
		return fmt.Sprintf(`UPDATE subjects SET name_fr = %s WHERE id = %d`, fr, math)
	}
	before := e.state()

	// Unaudited changes, by the table owner here (the deployed runtime role
	// is covered by internal/server's deploy test).
	wantCode(t, "unaudited subject rename", e.inTx(renameMath(`'TEST Autre'`)), "LRN01", "renamed without")
	wantCode(t, "unaudited name removal", e.inTx(renameMath(`NULL`)), "LRN01", "renamed without")
	wantCode(t, "unaudited Arabic name", e.inTx(fmt.Sprintf(`UPDATE subjects SET name_ar = 'TEST رياضيات' WHERE id = %d`, math)), "LRN01", "renamed without")
	wantCode(t, "unaudited subject reorder", e.inTx(fmt.Sprintf(`UPDATE subjects SET sort_order = 3 WHERE id = %d`, math)), "LRN01", "reordered without")
	wantCode(t, "unaudited chapter rename", e.inTx(fmt.Sprintf(`UPDATE chapters SET name_fr = 'TEST Chapitre' WHERE id = %d`, ch)), "LRN01", "renamed without")
	wantCode(t, "unaudited chapter reorder", e.inTx(fmt.Sprintf(`UPDATE chapters SET sort_order = 1 WHERE id = %d`, ch)), "LRN01", "reordered without")
	wantCode(t, "bulk reorder", e.inTx(`UPDATE subjects SET sort_order = sort_order + 1`), "LRN01", "reordered without")
	// An audit row of another kind, entry or transaction does not count.
	wantCode(t, "reorder row for a rename", e.inTx(audit("reorder_subject", 0, "0, 5"), renameMath(`'TEST Autre'`)), "LRN01", "renamed without")
	wantCode(t, "subject row for a chapter", e.inTx(audit("rename_subject", 0, "'TEST Mathématiques', NULL, 'TEST Chapitre', NULL"),
		fmt.Sprintf(`UPDATE chapters SET name_fr = 'TEST Chapitre' WHERE id = %d`, ch)), "LRN01", "renamed without")
	wantCode(t, "other subject's row", e.inTx(
		fmt.Sprintf(`INSERT INTO catalogue_audit (operator_label, action, subject_id, old_name_fr, old_name_ar, new_name_fr, new_name_ar)
			VALUES ('x@y', 'rename_subject', %d, NULL, NULL, 'TEST Autre', NULL)`, other),
		renameMath(`'TEST Autre'`)), "LRN01", "renamed without")
	// The audit must record exactly the change made (rule 2).
	wantCode(t, "different new name", e.inTx(audit("rename_subject", 0, "'TEST Mathématiques', NULL, 'TEST Audité', NULL"), renameMath(`'TEST Autre'`)),
		"LRN01", "renamed without")
	wantCode(t, "Arabic name changed too", e.inTx(audit("rename_subject", 0, "'TEST Mathématiques', NULL, 'TEST Autre', NULL"),
		fmt.Sprintf(`UPDATE subjects SET name_fr = 'TEST Autre', name_ar = 'TEST رياضيات' WHERE id = %d`, math)), "LRN01", "renamed without")
	wantCode(t, "different new order", e.inTx(audit("reorder_subject", 0, "0, 5"), fmt.Sprintf(`UPDATE subjects SET sort_order = 6 WHERE id = %d`, math)),
		"LRN01", "reordered without")
	// Old values must be the current ones (rule 1).
	wantCode(t, "wrong old name", e.inTx(audit("rename_subject", 0, "'TEST Faux', NULL, 'TEST Autre', NULL")), "LRN01", "current names")
	wantCode(t, "wrong old order", e.inTx(audit("reorder_subject", 0, "4, 5")), "LRN01", "current order")
	wantCode(t, "wrong old chapter order", e.inTx(audit("reorder_chapter", ch, "4, 5")), "LRN01", "current order")
	// Every audited change must happen (rule 3).
	wantCode(t, "audit without rename", e.inTx(audit("rename_subject", 0, "'TEST Mathématiques', NULL, 'TEST Autre', NULL")), "LRN01", "not applied")
	wantCode(t, "audit without reorder", e.inTx(audit("reorder_chapter", ch, "0, 5")), "LRN01", "not applied")
	wantCode(t, "rename undone", e.inTx(
		audit("rename_subject", 0, "'TEST Mathématiques', NULL, 'TEST Autre', NULL"), renameMath(`'TEST Autre'`),
		// Back without its own audit row.
		renameMath(`'TEST Mathématiques'`)), "LRN01", "renamed without")
	wantCode(t, "a row reused for a second change", e.inTx(
		audit("rename_subject", 0, "'TEST Mathématiques', NULL, 'TEST Autre', NULL"), renameMath(`'TEST Autre'`),
		audit("rename_subject", 0, "'TEST Autre', NULL, 'TEST Mathématiques', NULL"), renameMath(`'TEST Mathématiques'`),
		renameMath(`'TEST Autre'`)), "LRN01", "renamed without")
	wantCode(t, "an unapplied row followed by another", e.inTx(
		audit("rename_subject", 0, "'TEST Mathématiques', NULL, 'TEST Autre', NULL"),
		audit("rename_subject", 0, "'TEST Mathématiques', NULL, 'TEST Troisième', NULL"), renameMath(`'TEST Troisième'`)), "LRN01", "not applied")
	// Rows that record no change, or invalid names, are refused by the CHECK.
	wantCode(t, "rename without change", e.inTx(audit("rename_subject", 0, "'TEST Mathématiques', NULL, 'TEST Mathématiques', NULL")), "23514", "catalogue_audit_values")
	wantCode(t, "reorder without change", e.inTx(audit("reorder_subject", 0, "0, 0")), "23514", "catalogue_audit_values")
	wantCode(t, "addition with values", e.inTx(fmt.Sprintf(`INSERT INTO catalogue_audit (operator_label, action, subject_id, new_sort_order) VALUES ('x@y', 'add_subject', %d, 1)`, math)),
		"23514", "catalogue_audit_values")
	wantCode(t, "chapter rename without a chapter", e.inTx(audit("rename_chapter", 0, "'TEST Mathématiques', NULL, 'TEST Autre', NULL")), "23514", "catalogue_audit_scope")
	for name, bad := range map[string]string{"control": `E'TEST\nforged'`, "bidi": `E'TEST \u202Ex'`, "untrimmed": `' TEST'`, "empty": `''`,
		"too long": `repeat('a', 201)`, "arabic letter mark": `E'TEST \u061Cx'`} {
		wantCode(t, "audit name "+name, e.inTx(audit("rename_subject", 0, "'TEST Mathématiques', NULL, "+bad+", NULL"), renameMath(bad)),
			"23514", "catalogue_audit_names")
	}
	wantCode(t, "order out of range", e.inTx(audit("reorder_subject", 0, "0, 10000"), fmt.Sprintf(`UPDATE subjects SET sort_order = 10000 WHERE id = %d`, math)),
		"23514", "catalogue_audit_sort_order")
	// New entries start without names and order.
	wantCode(t, "subject added with a name", e.inTx(`WITH s AS (INSERT INTO subjects (code, name_fr) VALUES ('test-named', 'TEST Nommée') RETURNING id)
		INSERT INTO catalogue_audit (operator_label, action, subject_id) SELECT 'x@y', 'add_subject', id FROM s`), "LRN01", "no names and order 0")
	wantCode(t, "chapter added with an order", e.inTx(fmt.Sprintf(`WITH c AS (INSERT INTO chapters (subject_id, code, sort_order) VALUES (%d, 'ch-x', 3) RETURNING id, subject_id)
		INSERT INTO catalogue_audit (operator_label, action, subject_id, chapter_id) SELECT 'x@y', 'add_chapter', subject_id, id FROM c`, math)), "LRN01", "no names and order 0")
	// Identity stays immutable, even with a rename audit row.
	wantCode(t, "code change with an audit row", e.inTx(audit("rename_subject", 0, "'TEST Mathématiques', NULL, 'TEST Autre', NULL"),
		fmt.Sprintf(`UPDATE subjects SET code = 'test-other', name_fr = 'TEST Autre' WHERE id = %d`, math)), "LRN01", "subject identity is immutable")
	wantCode(t, "chapter code change", e.inTx(audit("rename_chapter", ch, "NULL, NULL, 'TEST Autre', NULL"),
		fmt.Sprintf(`UPDATE chapters SET code = 'ch-02', name_fr = 'TEST Autre' WHERE id = %d`, ch)), "LRN01", "chapter identity is immutable")
	wantCode(t, "chapter move", e.inTx(fmt.Sprintf(`UPDATE chapters SET subject_id = %d WHERE id = %d`, other, ch)), "LRN01", "chapter identity is immutable")
	// The audit stays append-only.
	wantCode(t, "edit audit", e.inTx(`UPDATE catalogue_audit SET new_name_fr = 'TEST forgé' WHERE action = 'rename_subject'`), "LRN01", "append-only")
	wantCode(t, "delete audit", e.inTx(`DELETE FROM catalogue_audit WHERE action = 'rename_subject'`), "LRN01", "append-only")
	if after := e.state(); after != before {
		t.Fatalf("refused statements changed the catalogue:\n%s\n%s", before, after)
	}

	// Accepted direct SQL: chained audited changes in one transaction,
	// including removing a name, and a no-op update without a row.
	if err := e.inTx(
		audit("rename_subject", 0, "'TEST Mathématiques', NULL, 'TEST Autre', 'TEST رياضيات'"),
		fmt.Sprintf(`UPDATE subjects SET name_fr = 'TEST Autre', name_ar = 'TEST رياضيات' WHERE id = %d`, math),
		audit("rename_subject", 0, "'TEST Autre', 'TEST رياضيات', NULL, 'TEST رياضيات'"), renameMath(`NULL`),
		audit("reorder_chapter", ch, "0, 7"), fmt.Sprintf(`UPDATE chapters SET sort_order = 7 WHERE id = %d`, ch),
		fmt.Sprintf(`UPDATE subjects SET code = code WHERE id = %d`, math),
	); err != nil {
		t.Fatal(err)
	}
	var fr *string
	var ar string
	var order int
	if err := e.db.QueryRow(e.ctx, `SELECT s.name_fr, s.name_ar, c.sort_order FROM subjects s JOIN chapters c ON c.subject_id = s.id WHERE s.id = $1`, math).
		Scan(&fr, &ar, &order); err != nil || fr != nil || ar != "TEST رياضيات" || order != 7 {
		t.Fatalf("after chained changes: %v %q %d %v", fr, ar, order, err)
	}
}

// leastPrivilegeRole creates a non-owner role with DML on the test schema
// and TEMP (the pattern of internal/reports/guards_test.go). asRole runs
// statements in one committed transaction as that role on a new
// connection, so no cached plan hides a shadowing regression.
func (e *env) leastPrivilegeRole() func(stmts ...string) error {
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
		if _, err := e.db.Exec(e.ctx, sql); err != nil {
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
	must(`GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA ` + qs + ` TO ` + role)
	must(`GRANT USAGE ON ALL SEQUENCES IN SCHEMA ` + qs + ` TO ` + role)
	must(`GRANT TEMP ON DATABASE ` + pgx.Identifier{dbName}.Sanitize() + ` TO ` + role)
	return func(stmts ...string) error {
		conn, err := pgx.ConnectConfig(e.ctx, e.db.Config().ConnConfig.Copy())
		if err != nil {
			return err
		}
		defer func() { _ = conn.Close(context.Background()) }()
		tx, err := conn.Begin(e.ctx)
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

// The 0012 guards read catalogue_audit, subjects and chapters. A temporary
// table of the calling role with any of those names must not fool them
// (pg_temp is otherwise searched first for relations).
func TestNameGuardsIgnoreTemporaryShadowTables(t *testing.T) {
	e := newEnv(t, dbtest.Migrated(t))
	math := dbtest.AddSubject(t, e.db, "test-math")
	ch := dbtest.AddChapter(t, e.db, math, "ch-01")
	asRole := e.leastPrivilegeRole()
	before := e.state()

	// The runtime-like role is bound by the guards without shadows too.
	wantCode(t, "role: unaudited rename", asRole(fmt.Sprintf(`UPDATE S.subjects SET name_fr = 'TEST Autre' WHERE id = %d`, math)), "LRN01", "renamed without")

	// Rule 2: a forged audit row in a shadow catalogue_audit.
	wantCode(t, "rename via shadow audit", asRole(
		`CREATE TEMP TABLE catalogue_audit (id bigint, action text, subject_id bigint, chapter_id bigint, xact_id xid8,
			old_name_fr text, old_name_ar text, new_name_fr text, new_name_ar text, old_sort_order integer, new_sort_order integer)`,
		fmt.Sprintf(`INSERT INTO pg_temp.catalogue_audit VALUES (1, 'rename_subject', %d, NULL, pg_current_xact_id(), NULL, NULL, 'TEST Autre', NULL, NULL, NULL)`, math),
		fmt.Sprintf(`UPDATE S.subjects SET name_fr = 'TEST Autre' WHERE id = %d`, math),
	), "LRN01", "renamed without")
	wantCode(t, "reorder via shadow audit", asRole(
		`CREATE TEMP TABLE catalogue_audit (id bigint, action text, subject_id bigint, chapter_id bigint, xact_id xid8,
			old_name_fr text, old_name_ar text, new_name_fr text, new_name_ar text, old_sort_order integer, new_sort_order integer)`,
		fmt.Sprintf(`INSERT INTO pg_temp.catalogue_audit VALUES (1, 'reorder_chapter', %d, %d, pg_current_xact_id(), NULL, NULL, NULL, NULL, 0, 4)`, math, ch),
		fmt.Sprintf(`UPDATE S.chapters SET sort_order = 4 WHERE id = %d`, ch),
	), "LRN01", "reordered without")
	// Rule 1: wrong old values hidden by a shadow subjects or chapters table.
	wantCode(t, "old name via shadow subjects", asRole(
		`CREATE TEMP TABLE subjects (id bigint, name_fr text, name_ar text, sort_order integer)`,
		fmt.Sprintf(`INSERT INTO pg_temp.subjects VALUES (%d, 'TEST Faux', NULL, 0)`, math),
		fmt.Sprintf(`INSERT INTO S.catalogue_audit (operator_label, action, subject_id, old_name_fr, old_name_ar, new_name_fr, new_name_ar)
			VALUES ('x@y', 'rename_subject', %d, 'TEST Faux', NULL, 'TEST Autre', NULL)`, math),
	), "LRN01", "current names")
	wantCode(t, "old order via shadow chapters", asRole(
		`CREATE TEMP TABLE chapters (id bigint, subject_id bigint, name_fr text, name_ar text, sort_order integer)`,
		fmt.Sprintf(`INSERT INTO pg_temp.chapters VALUES (%d, %d, NULL, NULL, 9)`, ch, math),
		fmt.Sprintf(`INSERT INTO S.catalogue_audit (operator_label, action, subject_id, chapter_id, old_sort_order, new_sort_order)
			VALUES ('x@y', 'reorder_chapter', %d, %d, 9, 4)`, math, ch),
	), "LRN01", "current order")
	// Rule 3: an unapplied change hidden by shadow tables at commit.
	wantCode(t, "unapplied rename via shadow subjects", asRole(
		fmt.Sprintf(`INSERT INTO S.catalogue_audit (operator_label, action, subject_id, old_name_fr, old_name_ar, new_name_fr, new_name_ar)
			VALUES ('x@y', 'rename_subject', %d, NULL, NULL, 'TEST Autre', NULL)`, math),
		`CREATE TEMP TABLE subjects (id bigint, name_fr text, name_ar text, sort_order integer)`,
		fmt.Sprintf(`INSERT INTO pg_temp.subjects VALUES (%d, 'TEST Autre', NULL, 0)`, math),
	), "LRN01", "not applied")
	wantCode(t, "unapplied reorder via shadow chapters", asRole(
		fmt.Sprintf(`INSERT INTO S.catalogue_audit (operator_label, action, subject_id, chapter_id, old_sort_order, new_sort_order)
			VALUES ('x@y', 'reorder_chapter', %d, %d, 0, 4)`, math, ch),
		`CREATE TEMP TABLE chapters (id bigint, subject_id bigint, name_fr text, name_ar text, sort_order integer)`,
		fmt.Sprintf(`INSERT INTO pg_temp.chapters VALUES (%d, %d, NULL, NULL, 4)`, ch, math),
	), "LRN01", "not applied")
	wantCode(t, "unapplied rename via shadow audit continuation", asRole(
		fmt.Sprintf(`INSERT INTO S.catalogue_audit (operator_label, action, subject_id, old_name_fr, old_name_ar, new_name_fr, new_name_ar)
			VALUES ('x@y', 'rename_subject', %d, NULL, NULL, 'TEST Autre', NULL)`, math),
		`CREATE TEMP TABLE catalogue_audit (id bigint, action text, subject_id bigint, chapter_id bigint, xact_id xid8,
			old_name_fr text, old_name_ar text, new_name_fr text, new_name_ar text, old_sort_order integer, new_sort_order integer)`,
		fmt.Sprintf(`INSERT INTO pg_temp.catalogue_audit VALUES (9223372036854775807, 'rename_subject', %d, NULL, pg_current_xact_id(), 'TEST Autre', NULL, NULL, NULL, NULL, NULL)`, math),
	), "LRN01", "not applied")
	if after := e.state(); after != before {
		t.Fatalf("refused statements changed the catalogue:\n%s\n%s", before, after)
	}

	// The same role succeeds with a correct audited change.
	if err := asRole(
		fmt.Sprintf(`INSERT INTO S.catalogue_audit (operator_label, action, subject_id, old_name_fr, old_name_ar, new_name_fr, new_name_ar)
			VALUES ('x@y', 'rename_subject', %d, NULL, NULL, 'TEST Mathématiques', 'TEST رياضيات')`, math),
		fmt.Sprintf(`UPDATE S.subjects SET name_fr = 'TEST Mathématiques', name_ar = 'TEST رياضيات' WHERE id = %d`, math),
	); err != nil {
		t.Fatal(err)
	}
}
