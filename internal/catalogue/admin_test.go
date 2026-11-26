package catalogue_test

import (
	"context"
	"errors"
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/hadden-me/learnoo/internal/catalogue"
	"github.com/hadden-me/learnoo/internal/database"
	"github.com/hadden-me/learnoo/internal/database/dbtest"
	"github.com/hadden-me/learnoo/internal/permissions"
	"github.com/hadden-me/learnoo/internal/users"
	"github.com/hadden-me/learnoo/migrations"
)

// LMS-B10: active administrators change display names and order through the
// same store as the operator commands, audited with their actor_id
// (migrations/0016). Simulated TEST users and entries only.

func (e *env) account(name string, perms ...permissions.Permission) int64 {
	e.t.Helper()
	u, err := users.Create(e.ctx, e.db, name+"@example.test", name, "$argon2id$test-not-a-hash")
	if err != nil {
		e.t.Fatal(err)
	}
	store := permissions.NewStore(e.db)
	for _, p := range perms {
		if _, err := store.Grant(e.ctx, operator, u.ID, p); err != nil {
			e.t.Fatal(err)
		}
	}
	return u.ID
}

func TestAdministratorChangesNamesAndOrderAudited(t *testing.T) {
	e := newEnv(t, dbtest.Migrated(t))
	math := dbtest.AddSubject(t, e.db, "test-math")
	ch := dbtest.AddChapter(t, e.db, math, "ch-01")
	admin := e.account("test-admin", permissions.Administrator)
	author := e.account("test-author", permissions.Author)
	disabledAdmin := e.account("test-admin-off", permissions.Administrator)
	if _, err := e.db.Exec(e.ctx, `UPDATE users SET disabled_at = now() WHERE id = $1`, disabledAdmin); err != nil {
		t.Fatal(err)
	}
	as := permissions.UserActor(admin)

	sj, changed, err := e.store.SetSubjectNames(e.ctx, as, "test-math", ptr("TEST Mathématiques"), ptr("TEST رياضيات"))
	if err != nil || !changed || *sj.Names.Fr != "TEST Mathématiques" {
		t.Fatalf("rename subject: %+v %v %v", sj, changed, err)
	}
	if _, changed, err := e.store.SetSubjectNames(e.ctx, as, "test-math", ptr("TEST Mathématiques"), nil); err != nil || changed {
		t.Fatalf("repeat: %v %v", changed, err)
	}
	if _, _, changed, err := e.store.SetChapterNames(e.ctx, as, "test-math", "ch-01", nil, ptr("TEST فصل")); err != nil || !changed {
		t.Fatalf("rename chapter: %v %v", changed, err)
	}
	if _, changed, err := e.store.SetSubjectOrder(e.ctx, as, "test-math", 7); err != nil || !changed {
		t.Fatalf("reorder subject: %v %v", changed, err)
	}
	if _, _, changed, err := e.store.SetChapterOrder(e.ctx, as, "test-math", "ch-01", 3); err != nil || !changed {
		t.Fatalf("reorder chapter: %v %v", changed, err)
	}
	var rows, withActor int
	if err := e.db.QueryRow(e.ctx, `SELECT count(*), count(*) FILTER (WHERE actor_id = $1 AND operator_label IS NULL)
		FROM catalogue_audit WHERE action IN ('rename_subject', 'rename_chapter', 'reorder_subject', 'reorder_chapter')`, admin).
		Scan(&rows, &withActor); err != nil {
		t.Fatal(err)
	}
	if rows != 4 || withActor != 4 {
		t.Fatalf("audit rows %d, with the administrator as actor %d; want 4 and 4", rows, withActor)
	}

	// Anyone else is denied before anything is locked or written.
	before := e.state()
	for name, actor := range map[string]permissions.Actor{
		"author": permissions.UserActor(author), "disabled administrator": permissions.UserActor(disabledAdmin), "unknown": permissions.UserActor(999999),
	} {
		if _, _, err := e.store.SetSubjectNames(e.ctx, actor, "test-math", ptr("TEST x"), nil); !errors.Is(err, permissions.ErrDenied) {
			t.Errorf("%s rename: %v", name, err)
		}
		if _, _, _, err := e.store.SetChapterOrder(e.ctx, actor, "test-math", "ch-01", 9); !errors.Is(err, permissions.ErrDenied) {
			t.Errorf("%s reorder: %v", name, err)
		}
	}
	if after := e.state(); after != before {
		t.Errorf("denied changes wrote something:\n%s\n%s", before, after)
	}
	// Adding entries stays an operator command.
	if _, _, _, err := e.store.AddChapter(e.ctx, as, "test-math", "ch-02"); !errors.Is(err, catalogue.ErrOperatorOnly) {
		t.Errorf("administrator adding a chapter: %v", err)
	}

	// The database refuses an actor_id on additions, and rows naming both
	// or neither of actor_id and operator_label.
	wantCode(t, "actor on add_subject", e.inTx(`WITH s AS (INSERT INTO subjects (code) VALUES ('test-new') RETURNING id)
		INSERT INTO catalogue_audit (actor_id, action, subject_id) SELECT `+itoa(admin)+`, 'add_subject', id FROM s`),
		"23514", "catalogue_audit_actor_scope")
	wantCode(t, "both actors", e.inTx(`INSERT INTO catalogue_audit (actor_id, operator_label, action, subject_id, old_sort_order, new_sort_order)
		VALUES (`+itoa(admin)+`, 'x@y', 'reorder_subject', `+itoa(math)+`, 7, 8)`), "23514", "catalogue_audit_actor")
	wantCode(t, "no actor", e.inTx(`INSERT INTO catalogue_audit (action, subject_id, chapter_id, old_sort_order, new_sort_order)
		VALUES ('reorder_chapter', `+itoa(math)+`, `+itoa(ch)+`, 3, 4)`), "23514", "catalogue_audit_actor")
}

func TestTreeListsSubjectsWithChapters(t *testing.T) {
	e := newEnv(t, dbtest.Migrated(t))
	math := dbtest.AddSubject(t, e.db, "test-math")
	dbtest.AddSubject(t, e.db, "test-empty")
	dbtest.AddChapter(t, e.db, math, "ch-02")
	dbtest.AddChapter(t, e.db, math, "ch-01")
	if _, _, _, err := e.store.SetChapterOrder(e.ctx, operator, "test-math", "ch-01", 5); err != nil {
		t.Fatal(err)
	}
	tree, err := e.store.Tree(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(tree) != 2 || tree[0].Code != "test-empty" || len(tree[0].Chapters) != 0 || tree[0].Chapters == nil ||
		tree[1].Code != "test-math" || len(tree[1].Chapters) != 2 || tree[1].Chapters[0].Code != "ch-02" {
		t.Fatalf("tree %+v", tree)
	}
}

func itoa(n int64) string {
	const digits = "0123456789"
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{digits[n%10]}, b...)
		n /= 10
	}
	return string(b)
}

// Upgrading to 0016 keeps every existing operator row: catalogue audit rows
// and invitations (with their audit) keep their operator label and get no
// actor_id.
func TestUpgradeTo0016KeepsOperatorRows(t *testing.T) {
	db := dbtest.EmptySchema(t)
	ctx := context.Background()
	before := fstest.MapFS{}
	entries, err := fs.ReadDir(migrations.FS, ".")
	if err != nil {
		t.Fatal(err)
	}
	for _, en := range entries {
		if strings.HasSuffix(en.Name(), ".sql") && en.Name() < "0016" {
			body, err := fs.ReadFile(migrations.FS, en.Name())
			if err != nil {
				t.Fatal(err)
			}
			before[en.Name()] = &fstest.MapFile{Data: body}
		}
	}
	if _, err := database.Migrate(ctx, db, before); err != nil {
		t.Fatal(err)
	}
	e := newEnv(t, db)
	math := dbtest.AddSubject(t, db, "test-math")
	if err := e.inTx(`INSERT INTO catalogue_audit (operator_label, action, subject_id, old_sort_order, new_sort_order)
		VALUES ('x@y', 'reorder_subject', `+itoa(math)+`, 0, 3)`, `UPDATE subjects SET sort_order = 3 WHERE id = `+itoa(math)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `INSERT INTO student_invitations (token_hash, email, operator_label, created_at, expires_at)
		VALUES (sha256('t'), 'old@example.test', 'x@y', now(), now() + interval '1 hour')`); err != nil {
		t.Fatal(err)
	}
	applied, err := database.Migrate(ctx, db, migrations.FS)
	if err != nil || len(applied) != 1 || !strings.HasPrefix(applied[0], "0016_") {
		t.Fatalf("applied %v: %v", applied, err)
	}
	var catalogueRows, invitationRows, auditRows int
	if err := db.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM catalogue_audit WHERE actor_id IS NULL AND operator_label IS NOT NULL),
		(SELECT count(*) FROM student_invitations WHERE issued_by IS NULL AND operator_label = 'x@y'),
		(SELECT count(*) FROM student_invitation_audit WHERE actor_id IS NULL AND operator_label = 'x@y')`).
		Scan(&catalogueRows, &invitationRows, &auditRows); err != nil {
		t.Fatal(err)
	}
	if catalogueRows != 2 || invitationRows != 1 || auditRows != 1 {
		t.Fatalf("after upgrade: %d catalogue, %d invitation, %d invitation audit operator rows", catalogueRows, invitationRows, auditRows)
	}
}
