package main

import (
	"context"
	"strings"
	"testing"

	"github.com/hadden-me/learnoo/internal/permissions"
)

// Catalogue display names and order (LRN-014, issue #86). Fixture names are
// labelled TEST; no curriculum data.

// catalogueState is every name and order in the catalogue plus the number
// of audit rows, to show refused commands change nothing.
func (c *cli) catalogueState() string {
	c.t.Helper()
	var s string
	if err := c.pool.QueryRow(context.Background(), `
		SELECT coalesce(string_agg(x, '; ' ORDER BY x), '') || ' audit=' || (SELECT count(*) FROM catalogue_audit)
		  FROM (SELECT format('%s %s %s %s', code, name_fr, name_ar, sort_order) AS x FROM subjects
		        UNION ALL
		        SELECT format('%s/%s %s %s %s', s.code, ch.code, ch.name_fr, ch.name_ar, ch.sort_order)
		          FROM chapters ch JOIN subjects s ON s.id = ch.subject_id) e`).Scan(&s); err != nil {
		c.t.Fatal(err)
	}
	return s
}

func TestCatalogueNamesAndOrderAreAudited(t *testing.T) {
	c := newCLI(t)
	c.ok("", "add-subject", "-code", "test-math")
	c.ok("", "add-subject", "-code", "test-physics")
	c.ok("", "add-chapter", "-subject", "test-math", "-code", "ch-01")
	c.ok("", "add-chapter", "-subject", "test-math", "-code", "ch-02")

	// Before any name: codes only, order 0, by code.
	wantContains(t, c.ok("", "show-catalogue"), `subject test-math (id `)
	if out := c.ok("", "show-catalogue"); !strings.Contains(out, `order 0): fr -, ar -`) ||
		strings.Index(out, "test-math") > strings.Index(out, "test-physics") {
		t.Fatalf("show-catalogue before names:\n%s", out)
	}

	wantContains(t, c.ok("", "set-subject-name", "-code", "test-math", "-fr", "  TEST Mathématiques  ", "-ar", "TEST رياضيات"),
		`renamed subject test-math: fr "TEST Mathématiques", ar "TEST رياضيات"`)
	// Only -ar: the French name is kept.
	wantContains(t, c.ok("", "set-subject-name", "-code", "test-math", "-ar", "TEST الرياضيات"),
		`renamed subject test-math: fr "TEST Mathématiques", ar "TEST الرياضيات"`)
	wantContains(t, c.ok("", "set-subject-name", "-code", "test-math", "-ar", "TEST الرياضيات"), "no change: subject test-math already has these names")
	wantContains(t, c.ok("", "set-chapter-name", "-subject", "test-math", "-code", "ch-02", "-fr", "TEST Nombres complexes"),
		`renamed chapter ch-02 of subject test-math: fr "TEST Nombres complexes", ar -`)
	wantContains(t, c.ok("", "set-order", "-subject", "test-physics", "-order", "1"), "subject test-physics now has order 1")
	wantContains(t, c.ok("", "set-order", "-subject", "test-math", "-order", "2"), "subject test-math now has order 2")
	wantContains(t, c.ok("", "set-order", "-subject", "test-math", "-chapter", "ch-01", "-order", "20"), "chapter ch-01 of subject test-math now has order 20")
	wantContains(t, c.ok("", "set-order", "-subject", "test-math", "-chapter", "ch-02", "-order", "10"), "chapter ch-02 of subject test-math now has order 10")
	wantContains(t, c.ok("", "set-order", "-subject", "test-math", "-chapter", "ch-02", "-order", "10"), "no change: chapter ch-02 of subject test-math already has order 10")

	out := c.ok("", "show-catalogue")
	want := []string{
		`subject test-physics (id `, `order 1): fr -, ar -`,
		`subject test-math (id `, `order 2): fr "TEST Mathématiques", ar "TEST الرياضيات"`,
		`  chapter ch-02 (id `, `order 10): fr "TEST Nombres complexes", ar -`,
		`  chapter ch-01 (id `, `order 20): fr -, ar -`,
	}
	pos := 0
	for _, w := range want {
		i := strings.Index(out[pos:], w)
		if i < 0 {
			t.Fatalf("show-catalogue lacks %q after position %d:\n%s", w, pos, out)
		}
		pos += i + len(w)
	}
	if one := c.ok("", "show-catalogue", "-subject", "test-physics"); strings.Contains(one, "test-math") {
		t.Fatalf("show-catalogue -subject: %s", one)
	}

	label := permissions.LocalOperatorLabel()
	rows, err := c.pool.Query(context.Background(), `
		SELECT a.operator_label, a.action, s.code, coalesce(ch.code, ''),
		       format('%s|%s|%s|%s|%s|%s', a.old_name_fr, a.new_name_fr, a.old_name_ar, a.new_name_ar, a.old_sort_order, a.new_sort_order)
		  FROM catalogue_audit a JOIN subjects s ON s.id = a.subject_id LEFT JOIN chapters ch ON ch.id = a.chapter_id
		 WHERE a.action NOT LIKE 'add_%'
		 ORDER BY a.id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var l, action, subject, chapter, values string
		if err := rows.Scan(&l, &action, &subject, &chapter, &values); err != nil {
			t.Fatal(err)
		}
		if l != label {
			t.Errorf("audit %s: label %q, want %q", action, l, label)
		}
		got = append(got, action+" "+subject+"/"+chapter+" "+values)
	}
	wantAudit := []string{
		"rename_subject test-math/ |TEST Mathématiques||TEST رياضيات||",
		"rename_subject test-math/ TEST Mathématiques|TEST Mathématiques|TEST رياضيات|TEST الرياضيات||",
		"rename_chapter test-math/ch-02 |TEST Nombres complexes||||",
		"reorder_subject test-physics/ ||||0|1",
		"reorder_subject test-math/ ||||0|2",
		"reorder_chapter test-math/ch-01 ||||0|20",
		"reorder_chapter test-math/ch-02 ||||0|10",
	}
	if strings.Join(got, "\n") != strings.Join(wantAudit, "\n") {
		t.Fatalf("audit:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(wantAudit, "\n"))
	}
}

func TestCatalogueNameCommandsRefuseInvalidInput(t *testing.T) {
	c := newCLI(t)
	c.ok("", "add-subject", "-code", "test-math")
	c.ok("", "add-chapter", "-subject", "test-math", "-code", "ch-01")
	c.ok("", "set-subject-name", "-code", "test-math", "-fr", "TEST Mathématiques")
	before := c.catalogueState()
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"set-subject-name", "-code", "test-math"}, "at least one of -fr and -ar"},
		{[]string{"set-subject-name", "-fr", "TEST x"}, "needs -code"},
		{[]string{"set-subject-name", "-code", "test-math", "-fr", "TEST x", "extra"}, "takes no other arguments"},
		{[]string{"set-subject-name", "-code", "Test-Math", "-fr", "TEST x"}, "invalid code"},
		{[]string{"set-subject-name", "-code", "unknown", "-fr", "TEST x"}, `unknown subject code "unknown"`},
		{[]string{"set-subject-name", "-code", "test-math", "-fr", ""}, "invalid name"},
		{[]string{"set-subject-name", "-code", "test-math", "-fr", "   "}, "invalid name"},
		{[]string{"set-subject-name", "-code", "test-math", "-fr", strings.Repeat("é", 201)}, "invalid name"},
		{[]string{"set-subject-name", "-code", "test-math", "-fr", "TEST\nforged"}, "invalid name"},
		{[]string{"set-subject-name", "-code", "test-math", "-fr", "TEST\x00x"}, "invalid name"},
		{[]string{"set-subject-name", "-code", "test-math", "-ar", "TEST \u202Eعكس"}, "invalid name"},
		{[]string{"set-subject-name", "-code", "test-math", "-ar", "TEST \u200Fx"}, "invalid name"},
		{[]string{"set-subject-name", "-code", "test-math", "-ar", "TEST \u061Cx"}, "invalid name"},
		{[]string{"set-subject-name", "-code", "test-math", "-ar", "TEST \u2028x"}, "invalid name"},
		{[]string{"set-subject-name", "-code", "test-math", "-fr", "TEST ok", "-ar", "TEST \xffx"}, "invalid name"},
		{[]string{"set-subject-name", "-code", "test-math", "-name", "x"}, "flag provided but not defined"},
		{[]string{"set-chapter-name", "-subject", "test-math", "-fr", "TEST x"}, "needs -subject, -code"},
		{[]string{"set-chapter-name", "-subject", "test-math", "-code", "ch-01"}, "at least one of -fr and -ar"},
		{[]string{"set-chapter-name", "-subject", "unknown", "-code", "ch-01", "-fr", "TEST x"}, `unknown subject code "unknown"`},
		{[]string{"set-chapter-name", "-subject", "test-math", "-code", "ch-99", "-fr", "TEST x"}, `unknown chapter code "ch-99"`},
		{[]string{"set-chapter-name", "-subject", "test-math", "-code", "CH", "-fr", "TEST x"}, "invalid code"},
		{[]string{"set-chapter-name", "-subject", "test-math", "-code", "ch-01", "-ar", "\t"}, "invalid name"},
		{[]string{"set-order", "-subject", "test-math"}, "needs -subject and -order"},
		{[]string{"set-order", "-order", "1"}, "needs -subject and -order"},
		{[]string{"set-order", "-subject", "test-math", "-order", "-1"}, "invalid order"},
		{[]string{"set-order", "-subject", "test-math", "-order", "10000"}, "invalid order"},
		{[]string{"set-order", "-subject", "test-math", "-order", "x"}, "invalid value"},
		{[]string{"set-order", "-subject", "test-math", "-order", "1", "extra"}, "takes no other arguments"},
		{[]string{"set-order", "-subject", "unknown", "-order", "1"}, `unknown subject code "unknown"`},
		{[]string{"set-order", "-subject", "test-math", "-chapter", "ch-99", "-order", "1"}, `unknown chapter code "ch-99"`},
		{[]string{"set-order", "-subject", "test-math", "-chapter", "Ch", "-order", "1"}, "invalid code"},
		{[]string{"show-catalogue", "-subject", "unknown"}, `unknown subject code "unknown"`},
		{[]string{"show-catalogue", "extra"}, "takes only -subject"},
	} {
		_, err := c.run("", tc.args...)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%v: err %v, want %q", tc.args, err, tc.want)
		}
	}
	if after := c.catalogueState(); after != before {
		t.Fatalf("refused commands changed the catalogue:\nbefore %s\nafter  %s", before, after)
	}
	// The limits are inclusive.
	c.ok("", "set-subject-name", "-code", "test-math", "-fr", strings.Repeat("é", 200))
	c.ok("", "set-order", "-subject", "test-math", "-order", "9999")
	c.ok("", "set-order", "-subject", "test-math", "-order", "0")
}
