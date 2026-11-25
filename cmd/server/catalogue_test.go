package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/hadden-me/learnoo/internal/permissions"
	"github.com/jackc/pgx/v5/pgconn"
)

func wantContains(t *testing.T, out, want string) {
	t.Helper()
	if !strings.Contains(out, want) {
		t.Fatalf("output %q lacks %q", out, want)
	}
}

func TestAddSubjectAndChapterAreAudited(t *testing.T) {
	c := newCLI(t)
	wantContains(t, c.ok("", "add-subject", "-code", "test-math"), "added subject test-math")
	wantContains(t, c.ok("", "add-chapter", "-subject", "test-math", "-code", "ch-01"), "added chapter ch-01 to subject test-math")
	// Repeats change nothing and add no audit row.
	wantContains(t, c.ok("", "add-subject", "-code", "test-math"), "no change: subject test-math already exists")
	wantContains(t, c.ok("", "add-chapter", "-subject", "test-math", "-code", "ch-01"), "no change: chapter ch-01 already exists")
	// The same chapter code in another subject is a different chapter.
	c.ok("", "add-subject", "-code", "test-physics")
	c.ok("", "add-chapter", "-subject", "test-physics", "-code", "ch-01")

	if n := c.count(`SELECT count(*) FROM subjects`); n != 2 {
		t.Fatalf("subjects = %d, want 2", n)
	}
	if n := c.count(`SELECT count(*) FROM chapters`); n != 2 {
		t.Fatalf("chapters = %d, want 2", n)
	}
	label := permissions.LocalOperatorLabel()
	rows, err := c.pool.Query(context.Background(), `
		SELECT a.operator_label, a.action, s.code, coalesce(ch.code, ''), ch.subject_id IS NULL OR ch.subject_id = a.subject_id
		  FROM catalogue_audit a JOIN subjects s ON s.id = a.subject_id LEFT JOIN chapters ch ON ch.id = a.chapter_id
		 ORDER BY a.id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var l, action, subject, chapter string
		var consistent bool
		if err := rows.Scan(&l, &action, &subject, &chapter, &consistent); err != nil {
			t.Fatal(err)
		}
		if l != label || !consistent {
			t.Errorf("audit row %s %s/%s: label %q (want %q), consistent %v", action, subject, chapter, l, label, consistent)
		}
		got = append(got, action+" "+subject+"/"+chapter)
	}
	want := []string{"add_subject test-math/", "add_chapter test-math/ch-01", "add_subject test-physics/", "add_chapter test-physics/ch-01"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("audit = %v, want %v", got, want)
	}

	// The audit is append-only for the application's role.
	for _, sql := range []string{`UPDATE catalogue_audit SET action = 'add_subject'`, `DELETE FROM catalogue_audit`, `TRUNCATE catalogue_audit`} {
		_, err := c.pool.Exec(context.Background(), sql)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "LRN01" {
			t.Errorf("%s: err %v, want LRN01", sql, err)
		}
	}
	// Subjects and chapters cannot be added without their audit row of the
	// same transaction (checked at commit).
	ctx := context.Background()
	wantLRN01 := func(what string, err error) {
		t.Helper()
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "LRN01" {
			t.Errorf("%s: err %v, want LRN01", what, err)
		}
	}
	before := c.snapshot()
	_, err = c.pool.Exec(ctx, `INSERT INTO subjects (code) VALUES ('test-unaudited')`)
	wantLRN01("unaudited subject", err)
	_, err = c.pool.Exec(ctx, `INSERT INTO chapters (subject_id, code) SELECT id, 'unaudited' FROM subjects WHERE code = 'test-math'`)
	wantLRN01("unaudited chapter", err)
	// An audit row naming another entry does not count.
	_, err = c.pool.Exec(ctx, `WITH s AS (INSERT INTO subjects (code) VALUES ('test-wrong-audit') RETURNING id)
		INSERT INTO catalogue_audit (operator_label, action, subject_id) SELECT 'x@y', 'add_subject', id FROM subjects WHERE code = 'test-math'`)
	wantLRN01("audit of another subject", err)
	_, err = c.pool.Exec(ctx, `WITH ch AS (INSERT INTO chapters (subject_id, code) SELECT id, 'wrong-audit' FROM subjects WHERE code = 'test-math' RETURNING id, subject_id)
		INSERT INTO catalogue_audit (operator_label, action, subject_id) SELECT 'x@y', 'add_subject', subject_id FROM ch`)
	wantLRN01("chapter with a subject audit row", err)
	// A row claiming another transaction is refused.
	_, err = c.pool.Exec(ctx, `INSERT INTO catalogue_audit (operator_label, action, subject_id, xact_id)
		SELECT 'x@y', 'add_subject', id, '1'::xid8 FROM subjects LIMIT 1`)
	wantLRN01("forged transaction", err)
	// Identities never change: no rename or move.
	for name, sql := range map[string]string{
		"rename subject":   `UPDATE subjects SET code = 'test-renamed' WHERE code = 'test-math'`,
		"backdate subject": `UPDATE subjects SET created_at = created_at - interval '1 day' WHERE code = 'test-math'`,
		"rename chapter":   `UPDATE chapters SET code = 'renamed' WHERE code = 'ch-01'`,
		"move chapter":     `UPDATE chapters SET subject_id = (SELECT id FROM subjects WHERE code = 'test-physics') WHERE subject_id = (SELECT id FROM subjects WHERE code = 'test-math')`,
	} {
		_, err := c.pool.Exec(ctx, sql)
		wantLRN01(name, err)
	}
	if n := c.count(`SELECT count(*) FROM subjects WHERE code IN ('test-math', 'test-physics')`); n != 2 {
		t.Fatalf("subjects changed: %d", n)
	}
	if after := c.snapshot(); after != before {
		t.Fatalf("refused inserts changed the database:\n%s\n%s", before, after)
	}

	// A backdated row is refused.
	_, err = c.pool.Exec(context.Background(), `INSERT INTO catalogue_audit (occurred_at, operator_label, action, subject_id)
		SELECT now() - interval '1 day', 'x@y', 'add_subject', id FROM subjects LIMIT 1`)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "LRN01" {
		t.Errorf("backdated audit row: err %v, want LRN01", err)
	}
}

func TestAddSubjectAndChapterRefuseInvalidInput(t *testing.T) {
	c := newCLI(t)
	c.ok("", "add-subject", "-code", "test-math")
	before := c.snapshot()
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"add-subject"}, "needs -code"},
		{[]string{"add-subject", "-code", "x", "extra"}, "needs -code"},
		{[]string{"add-subject", "-code", "Maths"}, "invalid code"},
		{[]string{"add-subject", "-code", "-maths"}, "invalid code"},
		{[]string{"add-subject", "-code", "math s"}, "invalid code"},
		{[]string{"add-subject", "-code", "mathé"}, "invalid code"},
		{[]string{"add-subject", "-code", strings.Repeat("a", 64)}, "invalid code"},
		{[]string{"add-subject", "-name", "x"}, "flag provided but not defined"},
		{[]string{"add-chapter", "-code", "ch-01"}, "needs -subject and -code"},
		{[]string{"add-chapter", "-subject", "test-math"}, "needs -subject and -code"},
		{[]string{"add-chapter", "-subject", "test-math", "-code", "CH"}, "invalid code"},
		{[]string{"add-chapter", "-subject", "Test", "-code", "ch"}, "invalid code"},
		{[]string{"add-chapter", "-subject", "unknown", "-code", "ch-01"}, `unknown subject code "unknown"`},
	} {
		_, err := c.run("", tc.args...)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%v: err %v, want %q", tc.args, err, tc.want)
		}
	}
	if after := c.snapshot(); after != before {
		t.Fatalf("refused commands changed the database:\nbefore %s\nafter  %s", before, after)
	}
	// A 63-character code is the longest accepted.
	c.ok("", "add-subject", "-code", strings.Repeat("a", 63))
}
