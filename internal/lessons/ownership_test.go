package lessons_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/hadden-me/learnoo/internal/lessons"
	"github.com/hadden-me/learnoo/internal/permissions"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Lesson ownership (maintainer decision of 2026-11-25, issue #6): the
// lesson's creator owns it; only the owner, while holding author, starts a
// draft of it or adds a translation of it. Administrators get no exception.

// ownedWithChangesRequested returns a revision of a new lesson of owner,
// sent back with changes requested, so the lesson has no active candidate
// and accepts a successor draft from its owner.
func (f fixture) ownedWithChangesRequested(owner int64, title string) lessons.Revision {
	f.t.Helper()
	rev, err := f.store.CreateLesson(f.ctx, owner, lessons.NewLesson{
		SubjectID: f.subject, Language: "fr", Fields: lessons.Fields{Content: doc(title, "fr")},
	})
	if err != nil {
		f.t.Fatal(err)
	}
	if rev, err = f.store.Submit(f.ctx, owner, rev.ID, rev.Version); err != nil {
		f.t.Fatal(err)
	}
	// No review decisions exist in this slice; set the status directly.
	f.mustExec(`UPDATE lesson_revisions SET status = 'changes_requested' WHERE id = $1`, rev.ID)
	return rev
}

// administrator returns a new account holding only administrator.
func (f fixture) administrator(name string) int64 {
	f.t.Helper()
	id := f.user(name)
	f.grant(id, permissions.Administrator)
	return id
}

func (f fixture) grant(user int64, ps ...permissions.Permission) {
	f.t.Helper()
	for _, p := range ps {
		if _, err := f.perms.Grant(f.ctx, operator, user, p); err != nil {
			f.t.Fatal(err)
		}
	}
}

func TestOnlyTheOwnerStartsDrafts(t *testing.T) {
	f := newFixture(t)
	admin := f.administrator("admin")
	rev := f.ownedWithChangesRequested(f.author, "Owned")

	other := f.newAuthor("other")
	adminAuthor := f.user("admin-author")
	f.grant(adminAuthor, permissions.Administrator, permissions.Author)
	publisherAuthor := f.user("publisher-author")
	f.grant(publisherAuthor, permissions.Publisher, permissions.Author)
	reviewerAuthor := f.newAuthor("reviewer-author")
	if _, err := f.perms.GrantReviewer(f.ctx, operator, reviewerAuthor, f.subject); err != nil {
		t.Fatal(err)
	}
	fields := lessons.Fields{Content: doc("Not the owner", "fr")}
	// Authors who do not own the lesson, whatever else they hold.
	for name, actor := range map[string]int64{
		"another author":          other,
		"administrator (author)":  adminAuthor,
		"publisher (author)":      publisherAuthor,
		"subject reviewer/author": reviewerAuthor,
	} {
		_, err := f.store.CreateDraft(f.ctx, actor, rev.LessonID, fields)
		if !errors.Is(err, lessons.ErrNotOwner) || !errors.Is(err, lessons.ErrDenied) {
			t.Errorf("%s: want ErrNotOwner (an ErrDenied), got %v", name, err)
		}
	}
	// Without author, the permission check refuses first.
	_, err := f.store.CreateDraft(f.ctx, admin, rev.LessonID, fields)
	if !errors.Is(err, lessons.ErrDenied) || errors.Is(err, lessons.ErrNotOwner) {
		t.Errorf("administrator without author: want ErrDenied, got %v", err)
	}
	// The ownership check comes before the lesson's state: a non-owner
	// learns nothing about an archived lesson or an active candidate.
	if n := f.count(`SELECT count(*) FROM lesson_revisions WHERE lesson_id = $1`, rev.LessonID); n != 1 {
		t.Fatalf("refused drafts stored revisions: %d", n)
	}

	// The owner without author, and the disabled owner, are refused.
	revoked := f.newAuthor("revoked-owner")
	revokedRev := f.ownedWithChangesRequested(revoked, "Revoked owner")
	if _, err := f.perms.Revoke(f.ctx, operator, revoked, permissions.Author); err != nil {
		t.Fatal(err)
	}
	_, err = f.store.CreateDraft(f.ctx, revoked, revokedRev.LessonID, fields)
	if !errors.Is(err, lessons.ErrDenied) || errors.Is(err, lessons.ErrNotOwner) {
		t.Errorf("owner without author: want ErrDenied, got %v", err)
	}
	disabled := f.newAuthor("disabled-owner")
	disabledRev := f.ownedWithChangesRequested(disabled, "Disabled owner")
	f.mustExec(`UPDATE users SET disabled_at = now() WHERE id = $1`, disabled)
	_, err = f.store.CreateDraft(f.ctx, disabled, disabledRev.LessonID, fields)
	if !errors.Is(err, lessons.ErrDenied) || errors.Is(err, lessons.ErrNotOwner) {
		t.Errorf("disabled owner: want ErrDenied, got %v", err)
	}
	// Nobody else may continue those lessons either.
	for _, lesson := range []int64{revokedRev.LessonID, disabledRev.LessonID} {
		if _, err := f.store.CreateDraft(f.ctx, other, lesson, fields); !errors.Is(err, lessons.ErrNotOwner) {
			t.Errorf("another author on lesson %d: %v", lesson, err)
		}
	}
	if n := f.count(`SELECT count(*) FROM lesson_revisions WHERE lesson_id IN ($1, $2)`, revokedRev.LessonID, disabledRev.LessonID); n != 2 {
		t.Fatalf("refused drafts stored revisions: %d", n)
	}

	// The owner holding author may.
	draft, err := f.store.CreateDraft(f.ctx, f.author, rev.LessonID, lessons.Fields{Content: doc("Successor", "fr")})
	if err != nil {
		t.Fatal(err)
	}
	if draft.AuthorID != f.author || draft.Number != 2 {
		t.Fatalf("successor: %+v", draft)
	}
	// Archival and active candidates are still reported to the owner only.
	_, err = f.store.CreateDraft(f.ctx, other, rev.LessonID, fields)
	wantErr(t, err, lessons.ErrNotOwner)
	_, err = f.store.CreateDraft(f.ctx, f.author, rev.LessonID, fields)
	wantErr(t, err, lessons.ErrActiveCandidateExists)

	// An archived lesson: the non-owner still gets ErrNotOwner, never
	// ErrArchived; only the owner learns it is archived.
	archived := f.ownedWithChangesRequested(f.author, "Archived")
	f.mustExec(`UPDATE lessons SET archived_at = now() WHERE id = $1`, archived.LessonID)
	for name, actor := range map[string]int64{"another author": other, "administrator (author)": adminAuthor} {
		_, err := f.store.CreateDraft(f.ctx, actor, archived.LessonID, fields)
		if !errors.Is(err, lessons.ErrNotOwner) || errors.Is(err, lessons.ErrArchived) {
			t.Errorf("%s on an archived lesson: want ErrNotOwner, got %v", name, err)
		}
	}
	_, err = f.store.CreateDraft(f.ctx, f.author, archived.LessonID, fields)
	wantErr(t, err, lessons.ErrArchived)
}

func TestOnlyTheOwnerTranslates(t *testing.T) {
	f := newFixture(t)
	admin := f.administrator("admin")
	src := f.lesson("Source")
	other := f.newAuthor("other")
	adminAuthor := f.user("admin-author")
	f.grant(adminAuthor, permissions.Administrator, permissions.Author)
	translate := func(actor, source int64) (lessons.Revision, error) {
		return f.store.CreateLesson(f.ctx, actor, lessons.NewLesson{
			SubjectID: f.subject, Language: "ar", TranslationOf: source, Fields: lessons.Fields{Content: doc("ترجمة", "ar")},
		})
	}
	for name, actor := range map[string]int64{"another author": other, "administrator (author)": adminAuthor} {
		if _, err := translate(actor, src.LessonID); !errors.Is(err, lessons.ErrNotOwner) {
			t.Errorf("%s: want ErrNotOwner, got %v", name, err)
		}
	}
	if _, err := translate(admin, src.LessonID); !errors.Is(err, lessons.ErrDenied) || errors.Is(err, lessons.ErrNotOwner) {
		t.Errorf("administrator without author: want ErrDenied, got %v", err)
	}
	if n := f.count(`SELECT count(*) FROM lessons`); n != 1 {
		t.Fatalf("refused translations stored lessons: %d", n)
	}

	tr, err := translate(f.author, src.LessonID)
	if err != nil {
		t.Fatal(err)
	}
	if tr.AuthorID != f.author {
		t.Fatalf("translation: %+v", tr)
	}
	l, err := f.store.Lesson(f.ctx, tr.LessonID)
	if err != nil || l.CreatedBy != f.author {
		t.Fatalf("translation lesson: %+v %v", l, err)
	}
	// The translation is the owner's too; the source's state is not
	// revealed to others (archived, or the language already taken).
	if _, err := translate(other, tr.LessonID); !errors.Is(err, lessons.ErrNotOwner) {
		t.Errorf("translation of the translation by another author: %v", err)
	}
	f.mustExec(`UPDATE lessons SET archived_at = now() WHERE id = $1`, src.LessonID)
	if _, err := translate(other, src.LessonID); !errors.Is(err, lessons.ErrNotOwner) {
		t.Errorf("archived source, another author: %v", err)
	}
}

// The database keeps ownership consistent for direct SQL of any role: a
// revision is authored by the lesson's owner, and a translation group's
// lessons share one owner. (It cannot check who is acting; the store does.)
func TestOwnershipGuards(t *testing.T) {
	f := newFixture(t)
	admin := f.administrator("admin")
	rev := f.ownedWithChangesRequested(f.author, "Guarded")
	other := f.newAuthor("other")
	insertRevision := func(author int64) error {
		return f.exec(`
			INSERT INTO lesson_revisions (lesson_id, revision_number, author_id, title, objectives,
				content, schema_version, language, subject_id, created_at, updated_at)
			SELECT lesson_id, 2, $2, title, objectives, content, schema_version, language, subject_id, now(), now()
			  FROM lesson_revisions WHERE id = $1`, rev.ID, author)
	}
	wantGuard(t, "revision by another author", insertRevision(other), "authored by the lesson's owner")
	wantGuard(t, "revision by an administrator", insertRevision(admin), "authored by the lesson's owner")
	insertLesson := func(created int64, language string) error {
		return f.exec(`
			INSERT INTO lessons (translation_group_id, language, subject_id, created_at, created_by)
			SELECT translation_group_id, $2, subject_id, now(), $3 FROM lessons WHERE id = $1`, rev.LessonID, language, created)
	}
	wantGuard(t, "translation by another author", insertLesson(other, "ar"), "only the owner of a translation group")
	wantGuard(t, "translation by an administrator", insertLesson(admin, "ar"), "only the owner of a translation group")
	// A group with no lesson accepts a first lesson only from the
	// transaction that created it.
	var empty string
	if err := f.db.QueryRow(f.ctx, `INSERT INTO translation_groups (subject_id) VALUES ($1) RETURNING id::text`, f.subject).Scan(&empty); err != nil {
		t.Fatal(err)
	}
	wantGuard(t, "first lesson of an older group", f.exec(`
		INSERT INTO lessons (translation_group_id, language, subject_id, created_at, created_by)
		VALUES ($1::uuid, 'fr', $2, now(), $3)`, empty, f.subject, other), "first lesson")
	wantGuard(t, "lesson of an unknown group", f.exec(`
		INSERT INTO lessons (translation_group_id, language, subject_id, created_at, created_by)
		VALUES (gen_random_uuid(), 'fr', $1, now(), $2)`, f.subject, other), "first lesson")
	if n := f.count(`SELECT count(*) FROM lessons`); n != 1 {
		t.Fatalf("refused inserts stored lessons: %d", n)
	}
	if n := f.count(`SELECT count(*) FROM lesson_revisions`); n != 1 {
		t.Fatalf("refused inserts stored revisions: %d", n)
	}

	// The owner may do both directly.
	if err := insertRevision(f.author); err != nil {
		t.Fatalf("owner's revision: %v", err)
	}
	if err := insertLesson(f.author, "ar"); err != nil {
		t.Fatalf("owner's translation: %v", err)
	}
}

// A REPEATABLE READ writer whose snapshot predates another author's new
// lesson cannot add a translation to its group: the group and its first
// lesson are invisible to it, so the guard finds no owner and refuses.
func TestOwnershipGuardRefusesStaleSnapshot(t *testing.T) {
	f := newFixture(t)
	other := f.newAuthor("other")
	for _, iso := range []pgx.TxIsoLevel{pgx.RepeatableRead, pgx.Serializable} {
		t.Run(string(iso), func(t *testing.T) {
			tx, err := f.db.BeginTx(f.ctx, pgx.TxOptions{IsoLevel: iso})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback(f.ctx) }()
			if _, err := tx.Exec(f.ctx, `SELECT count(*) FROM lessons`); err != nil { // takes the snapshot
				t.Fatal(err)
			}
			src := f.lesson("Created after the snapshot " + string(iso))
			var group string
			if err := f.db.QueryRow(f.ctx, `SELECT translation_group_id::text FROM lessons WHERE id = $1`, src.LessonID).Scan(&group); err != nil {
				t.Fatal(err)
			}
			_, err = tx.Exec(f.ctx, `
				INSERT INTO lessons (translation_group_id, language, subject_id, created_at, created_by)
				VALUES ($1::uuid, 'ar', $2, now(), $3)`, group, f.subject, other)
			var pgErr *pgconn.PgError
			if !errors.As(err, &pgErr) || (pgErr.Code != "LRN01" && pgErr.Code != "40001") {
				t.Fatalf("stale insert: want LRN01 or 40001, got %v", err)
			}
			_ = tx.Rollback(f.ctx)
			if n := f.count(`SELECT count(*) FROM lessons WHERE translation_group_id = $1::uuid`, group); n != 1 {
				t.Fatalf("group has %d lessons", n)
			}
		})
	}
}

// The new guards cannot be fooled by temporary tables of the calling role
// that shadow lessons or translation_groups.
func TestOwnershipGuardsIgnoreTemporaryShadowTables(t *testing.T) {
	f := newFixture(t)
	rev := f.ownedWithChangesRequested(f.author, "Shadowed owner")
	other := f.newAuthor("other")
	var group string
	if err := f.db.QueryRow(f.ctx, `SELECT translation_group_id::text FROM lessons WHERE id = $1`, rev.LessonID).Scan(&group); err != nil {
		t.Fatal(err)
	}
	asRole, qs := f.leastPrivilegeRole()

	// A shadow lessons naming the other author as owner.
	wantGuard(t, "revision via shadow owner", asRole(
		`CREATE TEMP TABLE lessons (id bigint, created_by bigint, published_revision_id bigint, translation_group_id uuid) ON COMMIT DROP`,
		fmt.Sprintf(`INSERT INTO pg_temp.lessons VALUES (%d, %d, NULL, '%s')`, rev.LessonID, other, group),
		fmt.Sprintf(`INSERT INTO %[1]s.lesson_revisions (lesson_id, revision_number, author_id, title, objectives,
				content, schema_version, language, subject_id, created_at, updated_at)
			SELECT lesson_id, 2, %[2]d, title, objectives, content, schema_version, language, subject_id, now(), now()
			  FROM %[1]s.lesson_revisions WHERE id = %[3]d`, qs, other, rev.ID),
	), "authored by the lesson's owner")

	// Shadow tables presenting the group as new and empty (a temporary row's
	// xmin is the current transaction).
	wantGuard(t, "translation via shadow group", asRole(
		`CREATE TEMP TABLE lessons (translation_group_id uuid, created_by bigint) ON COMMIT DROP`,
		`CREATE TEMP TABLE translation_groups (id uuid, subject_id bigint) ON COMMIT DROP`,
		fmt.Sprintf(`INSERT INTO pg_temp.translation_groups VALUES ('%s', %d)`, group, f.subject),
		fmt.Sprintf(`INSERT INTO %s.lessons (translation_group_id, language, subject_id, created_at, created_by)
			VALUES ('%s', 'ar', %d, now(), %d)`, qs, group, f.subject, other),
	), "only the owner of a translation group")

	// Only translation_groups shadowed, at READ COMMITTED: a temporary
	// row's xmin is the current transaction, so an unqualified read would
	// pass a group that has no lesson as "created by this transaction".
	// First a committed, empty group; then a group still being created by
	// another open transaction (invisible to the role, so the guard refuses
	// before the foreign key would wait for it).
	var empty string
	if err := f.db.QueryRow(f.ctx, `INSERT INTO translation_groups (subject_id) VALUES ($1) RETURNING id::text`, f.subject).Scan(&empty); err != nil {
		t.Fatal(err)
	}
	open, err := f.db.Begin(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = open.Rollback(f.ctx) }()
	var pending string
	if err := open.QueryRow(f.ctx, `INSERT INTO translation_groups (subject_id) VALUES ($1) RETURNING id::text`, f.subject).Scan(&pending); err != nil {
		t.Fatal(err)
	}
	for name, g := range map[string]string{"committed empty group": empty, "group of an open transaction": pending} {
		wantGuard(t, name+" via shadow translation_groups", asRole(
			`SET LOCAL TRANSACTION ISOLATION LEVEL READ COMMITTED`,
			`SET LOCAL statement_timeout = '5s'`,
			`CREATE TEMP TABLE translation_groups (id uuid, subject_id bigint) ON COMMIT DROP`,
			fmt.Sprintf(`INSERT INTO pg_temp.translation_groups VALUES ('%s', %d)`, g, f.subject),
			fmt.Sprintf(`INSERT INTO %s.lessons (translation_group_id, language, subject_id, created_at, created_by)
				VALUES ('%s', 'fr', %d, now(), %d)`, qs, g, f.subject, other),
		), "first lesson")
	}
	if err := open.Rollback(f.ctx); err != nil {
		t.Fatal(err)
	}

	if n := f.count(`SELECT count(*) FROM lesson_revisions WHERE lesson_id = $1`, rev.LessonID); n != 1 {
		t.Errorf("shadowed inserts stored revisions: %d", n)
	}
	if n := f.count(`SELECT count(*) FROM lessons`); n != 1 {
		t.Errorf("shadowed inserts stored lessons: %d", n)
	}
	// Without a shadow, the same role may act for the owner.
	if err := asRole(fmt.Sprintf(`INSERT INTO %s.lessons (translation_group_id, language, subject_id, created_at, created_by)
			VALUES ('%s', 'ar', %d, now(), %d)`, qs, group, f.subject, f.author)); err != nil {
		t.Fatalf("owner's translation as the role: %v", err)
	}
}
