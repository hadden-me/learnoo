package reports_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/hadden-me/learnoo/internal/auth"
	"github.com/hadden-me/learnoo/internal/database/dbtest"
	"github.com/hadden-me/learnoo/internal/lessons"
	"github.com/hadden-me/learnoo/internal/permissions"
	"github.com/hadden-me/learnoo/internal/reports"
	"github.com/hadden-me/learnoo/internal/review"
	"github.com/hadden-me/learnoo/internal/users"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Simulated users, subjects and unreviewed TEST documents only.

var testArgon2 = auth.Argon2Params{MemoryKiB: 1024, Iterations: 1, Lanes: 1, SaltLen: 16, KeyLen: 32}

type env struct {
	t         *testing.T
	ctx       context.Context
	db        *pgxpool.Pool
	perms     *permissions.Store
	lessons   *lessons.Store
	review    *review.Store
	reports   *reports.Store
	subject   int64
	other     int64 // a second subject
	author    int64
	reviewer  int64 // reviews subject
	publisher int64
	admin     int64 // administrator without reviewer assignments
	student   int64
	seq       int
}

// testOperator is the local operator actor the tests grant and revoke with.
var testOperator = func() permissions.Actor {
	a, err := permissions.OperatorAs("test")
	if err != nil {
		panic(err)
	}
	return a
}()

func newEnv(t *testing.T) *env {
	t.Helper()
	return newEnvOn(t, dbtest.Migrated(t))
}

// newEnvOn is newEnv on an already migrated pool.
func newEnvOn(t *testing.T, db *pgxpool.Pool) *env {
	t.Helper()
	e := &env{t: t, ctx: context.Background(), db: db, perms: permissions.NewStore(db),
		lessons: lessons.NewStore(db), review: review.NewStore(db), reports: reports.NewStore(db)}
	e.subject = e.newSubject("test-math")
	e.other = e.newSubject("test-physics")
	e.author = e.user("author", permissions.Author)
	e.reviewer = e.user("reviewer")
	e.assign(e.reviewer, e.subject)
	e.publisher = e.user("publisher", permissions.Publisher)
	e.admin = e.user("admin", permissions.Administrator)
	e.student = e.user("student")
	return e
}

func (e *env) newSubject(code string) int64 {
	e.t.Helper()
	var id int64
	if err := e.db.QueryRow(e.ctx, dbtest.SubjectSQL, code, dbtest.TestOperatorLabel).Scan(&id); err != nil {
		e.t.Fatal(err)
	}
	return id
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

func (e *env) assign(user, subject int64) {
	e.t.Helper()
	if _, err := e.perms.GrantReviewer(e.ctx, testOperator, user, subject); err != nil {
		e.t.Fatal(err)
	}
}

func doc(title string) []byte {
	b, err := json.Marshal(map[string]any{
		"schemaVersion": 1, "title": title, "language": "fr", "status": "unreviewed",
		"objective": "Objectif de test.",
		"blocks": []any{map[string]any{"type": "paragraph", "children": []any{
			map[string]any{"type": "text", "text": "Texte de test : " + title}}}},
	})
	if err != nil {
		panic(err)
	}
	return b
}

type lesson struct {
	id, revision int64
	number       int
	publicID     string
}

// published creates (or, with l.id set, revises) a lesson in subject and
// publishes the new revision.
func (e *env) published(l lesson, subject int64, title string) lesson {
	e.t.Helper()
	reviewer := e.reviewer
	if subject != e.subject {
		e.seq++
		reviewer = e.user(fmt.Sprintf("publication-reviewer-%d", e.seq))
		e.assign(reviewer, subject)
	}
	var rev lessons.Revision
	var err error
	if l.id == 0 {
		rev, err = e.lessons.CreateLesson(e.ctx, e.author, lessons.NewLesson{SubjectID: subject, Language: "fr",
			Fields: lessons.Fields{Content: doc(title)}})
	} else {
		rev, err = e.lessons.CreateDraft(e.ctx, e.author, l.id, lessons.Fields{Content: doc(title)})
	}
	if err != nil {
		e.t.Fatal(err)
	}
	if rev, err = e.lessons.Submit(e.ctx, e.author, rev.ID, rev.Version); err != nil {
		e.t.Fatal(err)
	}
	if _, err = e.review.Approve(e.ctx, reviewer, rev.ID, ""); err != nil {
		e.t.Fatal(err)
	}
	if _, err = e.review.Publish(e.ctx, e.publisher, rev.ID); err != nil {
		e.t.Fatal(err)
	}
	out := lesson{id: rev.LessonID, revision: rev.ID, number: rev.Number}
	if err := e.db.QueryRow(e.ctx, `SELECT public_id::text FROM lessons WHERE id = $1`, rev.LessonID).Scan(&out.publicID); err != nil {
		e.t.Fatal(err)
	}
	return out
}

func (e *env) report(reporter int64, l lesson, text string) int64 {
	e.t.Helper()
	rc, err := e.reports.Create(e.ctx, reporter, l.publicID, l.number, text)
	if err != nil {
		e.t.Fatal(err)
	}
	if rc.RevisionID != l.revision {
		e.t.Fatalf("report stored revision %d, displayed %d", rc.RevisionID, l.revision)
	}
	return rc.ID
}

func (e *env) count(sql string, args ...any) int {
	e.t.Helper()
	var n int
	if err := e.db.QueryRow(e.ctx, sql, args...).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

func (e *env) exec(sql string, args ...any) error {
	_, err := e.db.Exec(e.ctx, sql, args...)
	return err
}

// inTx runs stmts in one transaction and commits it.
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

// reportsState is every report and resolution, as text.
func (e *env) reportsState() string {
	e.t.Helper()
	var s string
	if err := e.db.QueryRow(e.ctx, `SELECT coalesce((SELECT jsonb_agg(r ORDER BY id)::text FROM error_reports r), '-') || '|' ||
		coalesce((SELECT jsonb_agg(x ORDER BY id)::text FROM error_report_resolutions x), '-')`).Scan(&s); err != nil {
		e.t.Fatal(err)
	}
	return s
}

// contentState is every lesson and revision, as text.
func (e *env) contentState() string {
	e.t.Helper()
	var s string
	if err := e.db.QueryRow(e.ctx, `SELECT (SELECT jsonb_agg(l ORDER BY id)::text FROM lessons l) || '|' ||
		(SELECT jsonb_agg(r ORDER BY id)::text FROM lesson_revisions r) || '|' ||
		(SELECT count(*) FROM publication_audit) || '|' || (SELECT count(*) FROM revision_withdrawals)`).Scan(&s); err != nil {
		e.t.Fatal(err)
	}
	return s
}

func wantErr(t *testing.T, what string, err, want error) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("%s: want %v, got %v", what, want, err)
	}
}

func wantSQLState(t *testing.T, what string, err error, code string) {
	t.Helper()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != code {
		t.Errorf("%s: want SQLSTATE %s, got %v", what, code, err)
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

func TestNormalizeText(t *testing.T) {
	for in, want := range map[string]string{
		"  Une erreur.  ":                "Une erreur.",
		"Ligne 1\r\nLigne 2\rLigne 3\n":  "Ligne 1\nLigne 2\nLigne 3",
		"خطأ في المعادلة ٢":              "خطأ في المعادلة ٢",
		"x² + 1 = 0 n’a pas de solution": "x² + 1 = 0 n’a pas de solution",
	} {
		got, err := reports.NormalizeText(in, reports.MaxMessage)
		if err != nil || got != want {
			t.Errorf("NormalizeText(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for in, want := range map[string]error{
		"":               reports.ErrTextRequired,
		" \r\n\t ":       reports.ErrTextRequired,
		"tab\there":      reports.ErrInvalidText,
		"nul\x00":        reports.ErrInvalidText,
		"escape\x1b[31m": reports.ErrInvalidText,
		"c1\u0085x":      reports.ErrInvalidText,
		"rlo‮evil":       reports.ErrInvalidText,
		"isolate⁧x":      reports.ErrInvalidText,
		"alm؜":           reports.ErrInvalidText,
		"lrm‎":           reports.ErrInvalidText,
		"line sep":       reports.ErrInvalidText,
		"para sep":       reports.ErrInvalidText,
		"bad\xff":        reports.ErrInvalidText,
		strings.Repeat("é", reports.MaxMessage+1): reports.ErrInvalidText,
	} {
		if _, err := reports.NormalizeText(in, reports.MaxMessage); !errors.Is(err, want) {
			t.Errorf("NormalizeText(%q): want %v, got %v", in, want, err)
		}
	}
	if got, err := reports.NormalizeText(strings.Repeat("é", reports.MaxMessage), reports.MaxMessage); err != nil || len([]rune(got)) != reports.MaxMessage {
		t.Errorf("exactly the limit: %v", err)
	}
}

// A report names the published revision that was displayed, and only that
// one; it changes no lesson content.
func TestCreateStoresTheDisplayedRevision(t *testing.T) {
	e := newEnv(t)
	first := e.published(lesson{}, e.subject, "TEST Première version")
	before := e.contentState()
	id := e.report(e.student, first, "  Le signe de la ligne 2 est faux.\r\nMerci.  ")
	var stored struct {
		lesson, revision, reporter int64
		message, status            string
	}
	if err := e.db.QueryRow(e.ctx, `SELECT lesson_id, revision_id, reporter_id, message, status
		FROM error_reports WHERE id = $1`, id).Scan(&stored.lesson, &stored.revision, &stored.reporter, &stored.message, &stored.status); err != nil {
		t.Fatal(err)
	}
	if stored.lesson != first.id || stored.revision != first.revision || stored.reporter != e.student ||
		stored.message != "Le signe de la ligne 2 est faux.\nMerci." || stored.status != "open" {
		t.Fatalf("stored report: %+v", stored)
	}
	if after := e.contentState(); after != before {
		t.Fatalf("filing a report changed lesson content:\n%s\n%s", before, after)
	}

	// A new published revision: the old number is stale, the new one works.
	second := e.published(lesson{id: first.id, publicID: first.publicID}, e.subject, "TEST Seconde version")
	reports0 := e.reportsState()
	_, err := e.reports.Create(e.ctx, e.student, first.publicID, first.number, "Erreur sur l’ancienne version.")
	wantErr(t, "stale displayed revision", err, reports.ErrRevisionChanged)
	_, err = e.reports.Create(e.ctx, e.student, first.publicID, 99, "Numéro inventé.")
	wantErr(t, "unknown revision number", err, reports.ErrRevisionChanged)
	if e.reportsState() != reports0 {
		t.Fatal("refused reports stored something")
	}
	e.report(e.student, second, "Erreur sur la nouvelle version.")

	// Validation happens before anything is looked up or stored.
	for text, want := range map[string]error{"   ": reports.ErrTextRequired, "a‮b": reports.ErrInvalidText,
		strings.Repeat("x", reports.MaxMessage+1): reports.ErrInvalidText} {
		_, err := e.reports.Create(e.ctx, e.student, second.publicID, second.number, text)
		wantErr(t, "invalid text", err, want)
	}
	for _, id := range []string{"", "not-a-uuid", strings.ToUpper(second.publicID), "00000000-0000-0000-0000-000000000000"} {
		_, err := e.reports.Create(e.ctx, e.student, id, second.number, "Texte.")
		wantErr(t, "unknown lesson "+id, err, reports.ErrNotFound)
	}
}

// Lessons that students cannot read cannot be reported.
func TestCreateRefusesUnreadableLessons(t *testing.T) {
	e := newEnv(t)
	ctx := e.ctx

	// Never published: a lesson with only a draft.
	draft, err := e.lessons.CreateLesson(ctx, e.author, lessons.NewLesson{SubjectID: e.subject, Language: "fr",
		Fields: lessons.Fields{Content: doc("TEST Brouillon")}})
	if err != nil {
		t.Fatal(err)
	}
	var draftPublic string
	if err := e.db.QueryRow(ctx, `SELECT public_id::text FROM lessons WHERE id = $1`, draft.LessonID).Scan(&draftPublic); err != nil {
		t.Fatal(err)
	}
	_, err = e.reports.Create(ctx, e.student, draftPublic, draft.Number, "Texte.")
	wantErr(t, "draft lesson", err, reports.ErrNotFound)

	// Unpublished.
	unpub := e.published(lesson{}, e.subject, "TEST Dépubliée")
	if _, err := e.review.Unpublish(ctx, e.admin, unpub.id, "TEST unpublication", unpub.revision); err != nil {
		t.Fatal(err)
	}
	_, err = e.reports.Create(ctx, e.student, unpub.publicID, unpub.number, "Texte.")
	wantErr(t, "unpublished lesson", err, reports.ErrNotFound)

	// Withdrawn (which also unpublishes).
	wd := e.published(lesson{}, e.subject, "TEST Retirée")
	current := wd.revision
	if _, err := e.review.Withdraw(ctx, e.admin, wd.revision, "TEST withdrawal", &current); err != nil {
		t.Fatal(err)
	}
	_, err = e.reports.Create(ctx, e.student, wd.publicID, wd.number, "Texte.")
	wantErr(t, "withdrawn revision", err, reports.ErrNotFound)

	// Archived (no code path yet; set directly).
	arch := e.published(lesson{}, e.subject, "TEST Archivée")
	if err := e.exec(`UPDATE lessons SET archived_at = now() WHERE id = $1`, arch.id); err != nil {
		t.Fatal(err)
	}
	_, err = e.reports.Create(ctx, e.student, arch.publicID, arch.number, "Texte.")
	wantErr(t, "archived lesson", err, reports.ErrNotFound)

	// A disabled account (the session layer refuses it first; the guard
	// holds without it).
	ok := e.published(lesson{}, e.subject, "TEST Publiée")
	if err := e.exec(`UPDATE users SET disabled_at = now() WHERE id = $1`, e.student); err != nil {
		t.Fatal(err)
	}
	_, err = e.reports.Create(ctx, e.student, ok.publicID, ok.number, "Texte.")
	wantErr(t, "disabled reporter", err, reports.ErrRejected)
	if n := e.count(`SELECT count(*) FROM error_reports`); n != 0 {
		t.Fatalf("%d reports stored", n)
	}
}

// Reviewers see their subjects only; administrators see everything;
// students and other reviewers cannot tell a report exists.
func TestVisibilityIsScopedBySubject(t *testing.T) {
	e := newEnv(t)
	math := e.published(lesson{}, e.subject, "TEST Mathématiques")
	phys := e.published(lesson{}, e.other, "TEST Physique")
	mathReport := e.report(e.student, math, "Erreur en mathématiques.")
	physReport := e.report(e.student, phys, "Erreur en physique.")
	physReviewer := e.user("physics-reviewer")
	e.assign(physReviewer, e.other)
	// An administrator who also reviews a subject still sees everything.
	adminReviewer := e.user("admin-reviewer", permissions.Administrator)
	e.assign(adminReviewer, e.subject)

	ids := func(actor int64, f reports.Filter) []int64 {
		t.Helper()
		list, err := e.reports.List(e.ctx, actor, f)
		if err != nil {
			t.Fatal(err)
		}
		var out []int64
		for _, r := range list {
			out = append(out, r.ID)
		}
		return out
	}
	eq := func(what string, got []int64, want ...int64) {
		t.Helper()
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("%s: got %v, want %v", what, got, want)
		}
	}
	eq("math reviewer", ids(e.reviewer, reports.Filter{}), mathReport)
	eq("physics reviewer", ids(physReviewer, reports.Filter{}), physReport)
	eq("admin", ids(e.admin, reports.Filter{}), physReport, mathReport)
	eq("admin who reviews", ids(adminReviewer, reports.Filter{}), physReport, mathReport)
	eq("admin, one subject", ids(e.admin, reports.Filter{SubjectID: e.other}), physReport)
	eq("reviewer, own subject", ids(e.reviewer, reports.Filter{SubjectID: e.subject}), mathReport)
	eq("status filter", ids(e.admin, reports.Filter{Status: reports.StatusResolved}))
	_, err := e.reports.List(e.ctx, e.reviewer, reports.Filter{SubjectID: e.other})
	wantErr(t, "reviewer, other subject", err, reports.ErrNotFound)
	_, err = e.reports.List(e.ctx, e.admin, reports.Filter{Status: "closed"})
	wantErr(t, "unknown status", err, reports.ErrInvalidStatus)
	for name, actor := range map[string]int64{"student": e.student, "author": e.author, "publisher": e.publisher} {
		_, err := e.reports.List(e.ctx, actor, reports.Filter{})
		wantErr(t, name+" list", err, reports.ErrDenied)
		_, err = e.reports.Get(e.ctx, actor, mathReport)
		wantErr(t, name+" get", err, reports.ErrNotFound)
	}

	got, err := e.reports.Get(e.ctx, e.reviewer, mathReport)
	if err != nil {
		t.Fatal(err)
	}
	if got.LessonID != math.id || got.LessonPublicID != math.publicID || got.RevisionID != math.revision ||
		got.RevisionNumber != math.number || got.RevisionTitle != "TEST Mathématiques" || !got.Current ||
		got.Message != "Erreur en mathématiques." || got.Status != reports.StatusOpen || got.Resolution != nil || got.SubjectID != e.subject {
		t.Fatalf("report: %+v", got)
	}
	_, err = e.reports.Get(e.ctx, e.reviewer, physReport)
	wantErr(t, "other subject", err, reports.ErrNotFound)
	_, err = e.reports.Get(e.ctx, e.reviewer, physReport+1000)
	wantErr(t, "missing report", err, reports.ErrNotFound)
	if _, err := e.reports.Get(e.ctx, e.admin, physReport); err != nil {
		t.Fatalf("admin get: %v", err)
	}

	// A new publication makes the report's revision historical.
	e.published(lesson{id: math.id}, e.subject, "TEST Mathématiques corrigées")
	if got, err = e.reports.Get(e.ctx, e.reviewer, mathReport); err != nil || got.Current || got.RevisionID != math.revision {
		t.Fatalf("after republication: %+v %v", got, err)
	}

	// Losing the assignment, or the account, hides the reports.
	if _, err := e.perms.RevokeReviewer(e.ctx, testOperator, physReviewer, e.other); err != nil {
		t.Fatal(err)
	}
	_, err = e.reports.Get(e.ctx, physReviewer, physReport)
	wantErr(t, "revoked reviewer", err, reports.ErrNotFound)
	if err := e.exec(`UPDATE users SET disabled_at = now() WHERE id = $1`, e.admin); err != nil {
		t.Fatal(err)
	}
	_, err = e.reports.Get(e.ctx, e.admin, physReport)
	wantErr(t, "disabled admin", err, reports.ErrNotFound)
}

func TestResolutionRequiresANoteAndIsFinal(t *testing.T) {
	e := newEnv(t)
	math := e.published(lesson{}, e.subject, "TEST Résolution")
	phys := e.published(lesson{}, e.other, "TEST Autre matière")
	id := e.report(e.student, math, "Erreur.")
	physID := e.report(e.student, phys, "Erreur.")
	before := e.reportsState()
	content := e.contentState()

	_, err := e.reports.Resolve(e.ctx, e.reviewer, id, reports.ActionResolve, " \n ")
	wantErr(t, "blank note", err, reports.ErrTextRequired)
	_, err = e.reports.Resolve(e.ctx, e.reviewer, id, reports.ActionDismiss, strings.Repeat("n", reports.MaxNote+1))
	wantErr(t, "long note", err, reports.ErrInvalidText)
	_, err = e.reports.Resolve(e.ctx, e.reviewer, id, reports.ActionResolve, "bidi‮")
	wantErr(t, "bidi note", err, reports.ErrInvalidText)
	_, err = e.reports.Resolve(e.ctx, e.reviewer, physID, reports.ActionResolve, "Note.")
	wantErr(t, "other subject", err, reports.ErrNotFound)
	_, err = e.reports.Resolve(e.ctx, e.student, id, reports.ActionResolve, "Note.")
	wantErr(t, "student", err, reports.ErrNotFound)
	_, err = e.reports.Resolve(e.ctx, e.author, id, reports.ActionDismiss, "Note.")
	wantErr(t, "author", err, reports.ErrNotFound)
	if _, err := e.reports.Resolve(e.ctx, e.reviewer, id, "delete", "Note."); err == nil {
		t.Fatal("unknown action accepted")
	}
	if e.reportsState() != before {
		t.Fatal("refused resolutions changed reports")
	}

	got, err := e.reports.Resolve(e.ctx, e.reviewer, id, reports.ActionResolve, "  Corrigé dans la révision suivante.  ")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != reports.StatusResolved || got.Resolution == nil || got.Resolution.ActorID != e.reviewer ||
		got.Resolution.Action != reports.ActionResolve || got.Resolution.Note != "Corrigé dans la révision suivante." {
		t.Fatalf("resolved: %+v %+v", got, got.Resolution)
	}
	again, err := e.reports.Get(e.ctx, e.admin, id)
	if err != nil || again.Resolution == nil || !again.Resolution.OccurredAt.Equal(got.Resolution.OccurredAt) || again.Status != reports.StatusResolved {
		t.Fatalf("reread: %+v %v", again, err)
	}
	_, err = e.reports.Resolve(e.ctx, e.reviewer, id, reports.ActionDismiss, "Autre note.")
	wantErr(t, "second resolution", err, reports.ErrNotOpen)
	_, err = e.reports.Resolve(e.ctx, e.admin, id, reports.ActionResolve, "Autre note.")
	wantErr(t, "admin after resolution", err, reports.ErrNotOpen)
	// Authorization precedes the status check: a closed report of another
	// subject is not found, not "not open".
	physReviewer := e.user("physics-reviewer")
	e.assign(physReviewer, e.other)
	_, err = e.reports.Resolve(e.ctx, physReviewer, id, reports.ActionDismiss, "Note.")
	wantErr(t, "closed report, other subject", err, reports.ErrNotFound)

	// An administrator without any reviewer assignment dismisses the other
	// report through the Store.
	if n := e.count(`SELECT count(*) FROM reviewer_assignments WHERE user_id = $1`, e.admin); n != 0 {
		t.Fatalf("admin has %d assignments", n)
	}
	if got, err = e.reports.Resolve(e.ctx, e.admin, physID, reports.ActionDismiss, "Pas une erreur."); err != nil || got.Status != reports.StatusDismissed {
		t.Fatalf("admin dismiss: %+v %v", got, err)
	}
	if n := e.count(`SELECT count(*) FROM error_report_resolutions`); n != 2 {
		t.Fatalf("%d resolutions", n)
	}
	if after := e.contentState(); after != content {
		t.Fatalf("resolving changed lesson content:\n%s\n%s", content, after)
	}
}

// Of concurrent resolutions and dismissals of one report, exactly one
// applies and is audited.
func TestConcurrentResolutionsExactlyOne(t *testing.T) {
	e := newEnv(t)
	l := e.published(lesson{}, e.subject, "TEST Concurrence")
	second := e.user("second-reviewer")
	e.assign(second, e.subject)
	actors := []int64{e.reviewer, second, e.admin}
	for i := 0; i < 15; i++ {
		id := e.report(e.student, l, fmt.Sprintf("Erreur %d.", i))
		const n = 6
		start := make(chan struct{})
		errs := make(chan error, n)
		var wg sync.WaitGroup
		for w := 0; w < n; w++ {
			action := reports.ActionResolve
			if w%2 == 1 {
				action = reports.ActionDismiss
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				_, err := e.reports.Resolve(e.ctx, actors[w%len(actors)], id, action, fmt.Sprintf("Note %d.", w))
				errs <- err
			}()
		}
		close(start)
		wg.Wait()
		close(errs)
		ok := 0
		for err := range errs {
			switch {
			case err == nil:
				ok++
			case !errors.Is(err, reports.ErrNotOpen):
				t.Fatalf("iteration %d: %v", i, err)
			}
		}
		if ok != 1 {
			t.Fatalf("iteration %d: %d resolutions applied", i, ok)
		}
		var status, action string
		if err := e.db.QueryRow(e.ctx, `SELECT e.status, x.action FROM error_reports e JOIN error_report_resolutions x ON x.report_id = e.id
			WHERE e.id = $1`, id).Scan(&status, &action); err != nil {
			t.Fatal(err)
		}
		if (action == "resolve") != (status == "resolved") || e.count(`SELECT count(*) FROM error_report_resolutions WHERE report_id = $1`, id) != 1 {
			t.Fatalf("iteration %d: status %s, action %s", i, status, action)
		}
	}
}

// Filing a report racing a new publication either names the old revision
// (committed before the publication) or is refused; it never names a
// revision that is not the published one at its commit.
func TestReportRacingPublication(t *testing.T) {
	e := newEnv(t)
	for i := 0; i < 8; i++ {
		l := e.published(lesson{}, e.subject, fmt.Sprintf("TEST Course %d", i))
		draft, err := e.lessons.CreateDraft(e.ctx, e.author, l.id, lessons.Fields{Content: doc("TEST Suite")})
		if err != nil {
			t.Fatal(err)
		}
		if draft, err = e.lessons.Submit(e.ctx, e.author, draft.ID, draft.Version); err != nil {
			t.Fatal(err)
		}
		if _, err = e.review.Approve(e.ctx, e.reviewer, draft.ID, ""); err != nil {
			t.Fatal(err)
		}
		start := make(chan struct{})
		var wg sync.WaitGroup
		var reportErr, publishErr error
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			_, reportErr = e.reports.Create(e.ctx, e.student, l.publicID, l.number, "Erreur.")
		}()
		go func() { defer wg.Done(); <-start; _, publishErr = e.review.Publish(e.ctx, e.publisher, draft.ID) }()
		close(start)
		wg.Wait()
		if publishErr != nil {
			t.Fatal(publishErr)
		}
		if reportErr != nil && !errors.Is(reportErr, reports.ErrRevisionChanged) {
			t.Fatalf("iteration %d: %v", i, reportErr)
		}
		if reportErr == nil {
			if n := e.count(`SELECT count(*) FROM error_reports WHERE lesson_id = $1 AND revision_id = $2`, l.id, l.revision); n != 1 {
				t.Fatalf("iteration %d: report not stored against the displayed revision", i)
			}
		}
	}
}
