package lessons_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/hadden-me/learnoo/internal/lessons"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// An approved revision that has never been published is the lesson's active
// candidate (maintainer decision, docs/DECISIONS.md "Lesson persistence").
// These tests simulate review decisions and publication with direct SQL
// (decision or audit row plus the status or pointer change, as the database
// requires since LRN-007); unpublication and restoration (LRN-009) are
// simulated with direct pointer changes.

func (f fixture) approved(title string) lessons.Revision {
	f.t.Helper()
	rev := f.submitted(title)
	f.mustApprove(rev.ID)
	return rev
}

// closedAt returns the revision's candidate_closed_at.
func (f fixture) closedAt(rev int64) *time.Time {
	f.t.Helper()
	var at *time.Time
	if err := f.db.QueryRow(f.ctx, `SELECT candidate_closed_at FROM lesson_revisions WHERE id = $1`, rev).Scan(&at); err != nil {
		f.t.Fatal(err)
	}
	return at
}

// withoutClosedAt is the stored row minus candidate_closed_at.
func (f fixture) withoutClosedAt(rev int64) string {
	f.t.Helper()
	var row string
	if err := f.db.QueryRow(f.ctx, `SELECT (to_jsonb(r) - 'candidate_closed_at')::text FROM lesson_revisions r WHERE id = $1`, rev).Scan(&row); err != nil {
		f.t.Fatal(err)
	}
	return row
}

// insertDirect inserts a draft bypassing the Store, with the given number and
// the lesson's current published revision as base.
func (f fixture) insertDirect(from lessons.Revision, number int) error {
	return f.exec(`
		INSERT INTO lesson_revisions (lesson_id, revision_number, author_id, base_revision_id, title, objectives,
			content, schema_version, language, subject_id, created_at, updated_at)
		SELECT r.lesson_id, $2, r.author_id, l.published_revision_id, r.title, r.objectives, r.content,
		       r.schema_version, r.language, r.subject_id, now(), now()
		  FROM lesson_revisions r JOIN lessons l ON l.id = r.lesson_id WHERE r.id = $1`, from.ID, number)
}

func wantActiveCandidateIndex(t *testing.T, what string, err error) {
	t.Helper()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23505" || pgErr.ConstraintName != "lesson_revisions_one_active_candidate" {
		t.Errorf("%s: want the active-candidate index violation, got %v", what, err)
	}
}

func TestApprovedUnpublishedBlocksDraftsUntilPublished(t *testing.T) {
	f := newFixture(t)
	rev := f.approved("Approved")
	if f.closedAt(rev.ID) != nil {
		t.Fatal("approval closed the candidate")
	}
	_, err := f.store.CreateDraft(f.ctx, f.author, rev.LessonID, lessons.Fields{Content: doc("Blocked", "fr")})
	wantErr(t, err, lessons.ErrActiveCandidateExists)
	// Another author is refused as a non-owner before the lesson's state is
	// examined (TestOnlyTheOwnerStartsDrafts).
	other := f.newAuthor("other")
	_, err = f.store.CreateDraft(f.ctx, other, rev.LessonID, lessons.Fields{Content: doc("Blocked too", "fr")})
	wantErr(t, err, lessons.ErrNotOwner)
	// The index rejects writers that bypass the Store.
	wantActiveCandidateIndex(t, "direct insert", f.insertDirect(rev, 5))
	if n := f.count(`SELECT count(*) FROM lesson_revisions WHERE lesson_id = $1`, rev.LessonID); n != 1 {
		t.Fatalf("blocked drafts stored: %d revisions", n)
	}

	// Publication closes the candidate and changes nothing else.
	before := f.withoutClosedAt(rev.ID)
	if err := f.publish(rev.LessonID, rev.ID); err != nil {
		t.Fatal(err)
	}
	if f.closedAt(rev.ID) == nil {
		t.Fatal("publication did not close the candidate")
	}
	if f.withoutClosedAt(rev.ID) != before {
		t.Fatal("publication changed the snapshot")
	}
	draft, err := f.store.CreateDraft(f.ctx, f.author, rev.LessonID, lessons.Fields{Content: doc("Next edition", "fr")})
	if err != nil {
		t.Fatalf("draft after publication: %v", err)
	}
	if draft.BaseRevisionID == nil || *draft.BaseRevisionID != rev.ID {
		t.Fatalf("base %v, want %d", draft.BaseRevisionID, rev.ID)
	}
	// Unpublishing does not reopen it.
	f.mustExec(`UPDATE lessons SET published_revision_id = NULL WHERE id = $1`, rev.LessonID)
	if f.closedAt(rev.ID) == nil {
		t.Fatal("unpublishing reopened the candidate")
	}
}

func TestRestoringEarlierRevisionKeepsDraftsAllowed(t *testing.T) {
	f := newFixture(t)
	first := f.approved("First edition")
	if err := f.publish(first.LessonID, first.ID); err != nil {
		t.Fatal(err)
	}
	firstClosed := f.closedAt(first.ID)

	second, err := f.store.CreateDraft(f.ctx, f.author, first.LessonID, lessons.Fields{Content: doc("Second edition", "fr")})
	if err != nil {
		t.Fatal(err)
	}
	second, err = f.store.Submit(f.ctx, f.author, second.ID, second.Version)
	if err != nil {
		t.Fatal(err)
	}
	f.mustApprove(second.ID)
	// The newer approval blocks drafts while the first edition stays published.
	_, err = f.store.CreateDraft(f.ctx, f.author, first.LessonID, lessons.Fields{Content: doc("Blocked", "fr")})
	wantErr(t, err, lessons.ErrActiveCandidateExists)

	if err := f.publish(first.LessonID, second.ID); err != nil {
		t.Fatal(err)
	}
	// Restoration re-points to the earlier, already published revision.
	if err := f.publish(first.LessonID, first.ID); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if got := f.closedAt(first.ID); got == nil || !got.Equal(*firstClosed) {
		t.Fatalf("restoration changed the first edition's closing time: %v, was %v", got, firstClosed)
	}
	if f.closedAt(second.ID) == nil {
		t.Fatal("second edition reopened")
	}
	draft, err := f.store.CreateDraft(f.ctx, f.author, first.LessonID, lessons.Fields{Content: doc("Third edition", "fr")})
	if err != nil {
		t.Fatalf("draft after restoration: %v", err)
	}
	if draft.BaseRevisionID == nil || *draft.BaseRevisionID != first.ID {
		t.Fatalf("base %v, want %d", draft.BaseRevisionID, first.ID)
	}
}

func TestCandidateClosedAtRules(t *testing.T) {
	f := newFixture(t)
	closeSQL := `UPDATE lesson_revisions SET candidate_closed_at = now() WHERE id = $1`

	draft := f.lesson("Draft")
	wantSQLState(t, "close a draft", f.exec(closeSQL, draft.ID), "LRN01")
	wantSQLState(t, "close a draft while editing it", f.exec(`UPDATE lesson_revisions SET candidate_closed_at = now(), title = 'x' WHERE id = $1`, draft.ID), "LRN01")
	inReview := f.submitted("In review")
	wantSQLState(t, "close a revision in review", f.exec(closeSQL, inReview.ID), "LRN01")
	wantSQLState(t, "approve and close at once", f.exec(`UPDATE lesson_revisions SET status = 'approved', candidate_closed_at = now() WHERE id = $1`, inReview.ID), "LRN01")
	requested := f.submitted("Changes requested")
	f.mustRequestChanges(requested.ID)
	wantSQLState(t, "close a changes-requested revision", f.exec(closeSQL, requested.ID), "LRN01")
	// A revision cannot be created closed.
	wantSQLState(t, "insert closed", f.exec(`
		INSERT INTO lesson_revisions (lesson_id, revision_number, author_id, title, objectives, content,
			schema_version, language, subject_id, created_at, updated_at, candidate_closed_at)
		SELECT lesson_id, 9, author_id, title, objectives, content, schema_version, language, subject_id, now(), now(), now()
		  FROM lesson_revisions WHERE id = $1`, requested.ID), "23514")

	// A direct UPDATE cannot close a candidate, with any timestamp: that would
	// release the lesson without publication or an audited withdrawal.
	approved := f.approved("Approved")
	before := f.snapshot(approved.ID)
	for name, set := range map[string]string{
		"now":             `candidate_closed_at = now()`,
		"forged past":     `candidate_closed_at = '1970-01-01T00:00:00Z'`,
		"forged future":   `candidate_closed_at = now() + interval '1 day'`,
		"with updated_at": `candidate_closed_at = now(), updated_at = updated_at + interval '1 second'`,
		"with title":      `candidate_closed_at = now(), title = 'Edited'`,
	} {
		wantSQLState(t, "direct close "+name, f.exec(`UPDATE lesson_revisions SET `+set+` WHERE id = $1`, approved.ID), "LRN01")
	}
	if f.snapshot(approved.ID) != before {
		t.Fatal("rejected updates changed the approved revision")
	}
	_, err := f.store.CreateDraft(f.ctx, f.author, approved.LessonID, lessons.Fields{Content: doc("Still blocked", "fr")})
	wantErr(t, err, lessons.ErrActiveCandidateExists)

	// Inside a trigger (the path publication uses now, and LRN-009's audited
	// withdrawal will use), only the transaction time, alone, is accepted.
	f.mustExec(`CREATE TABLE test_nested_updates (stmt text NOT NULL)`)
	f.mustExec(`CREATE FUNCTION test_run_nested() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN EXECUTE NEW.stmt; RETURN NULL; END $$`)
	f.mustExec(`CREATE TRIGGER test_run_nested AFTER INSERT ON test_nested_updates
		FOR EACH ROW EXECUTE FUNCTION test_run_nested()`)
	nested := func(set string, id int64) error {
		return f.exec(`INSERT INTO test_nested_updates VALUES ($1)`,
			fmt.Sprintf(`UPDATE lesson_revisions SET %s WHERE id = %d`, set, id))
	}
	for name, set := range map[string]string{
		"forged past":     `candidate_closed_at = '1970-01-01T00:00:00Z'`,
		"forged future":   `candidate_closed_at = now() + interval '1 day'`,
		"with updated_at": `candidate_closed_at = now(), updated_at = updated_at + interval '1 second'`,
		"with version":    `candidate_closed_at = now(), version = version + 1`,
		"with status":     `candidate_closed_at = now(), status = 'in_review'`,
	} {
		wantSQLState(t, "nested close "+name, nested(set, approved.ID), "LRN01")
	}
	wantSQLState(t, "nested close of a draft", nested(`candidate_closed_at = now()`, draft.ID), "LRN01")
	wantSQLState(t, "nested close in review", nested(`candidate_closed_at = now()`, inReview.ID), "LRN01")
	wantSQLState(t, "nested close changes requested", nested(`candidate_closed_at = now()`, requested.ID), "LRN01")
	wantSQLState(t, "nested approve and close", nested(`status = 'approved', candidate_closed_at = now()`, inReview.ID), "LRN01")
	if f.snapshot(approved.ID) != before {
		t.Fatal("rejected nested updates changed the approved revision")
	}

	// The trigger path itself works for the table owner (the tests' role):
	// closing alone at the transaction time, what LRN-009's audited
	// SECURITY DEFINER withdrawal trigger will do, releases the lesson.
	other := f.approved("Withdrawal path")
	if err := nested(`candidate_closed_at = now()`, other.ID); err != nil {
		t.Fatalf("nested close: %v", err)
	}
	if f.closedAt(other.ID) == nil {
		t.Fatal("nested close did not close the candidate")
	}
	if _, err := f.store.CreateDraft(f.ctx, f.author, other.LessonID, lessons.Fields{Content: doc("After nested close", "fr")}); err != nil {
		t.Fatalf("draft after nested close: %v", err)
	}

	// Publication closes it, at the transaction time.
	var txTime time.Time
	tx, err := f.db.Begin(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(f.ctx) }()
	if err := tx.QueryRow(f.ctx, `SELECT now()`).Scan(&txTime); err != nil {
		t.Fatal(err)
	}
	if err := f.publishIn(tx, approved.LessonID, approved.ID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(f.ctx); err != nil {
		t.Fatal(err)
	}
	closed := f.closedAt(approved.ID)
	if closed == nil || !closed.Equal(txTime) {
		t.Fatalf("closed at %v, want the publication's transaction time %v", closed, txTime)
	}
	for name, set := range map[string]string{
		"clear":       `candidate_closed_at = NULL`,
		"change":      `candidate_closed_at = candidate_closed_at + interval '1 day'`,
		"close twice": `candidate_closed_at = now()`,
	} {
		wantSQLState(t, "direct "+name, f.exec(`UPDATE lesson_revisions SET `+set+` WHERE id = $1`, approved.ID), "LRN01")
		wantSQLState(t, "nested "+name, nested(set, approved.ID), "LRN01")
	}
	if got := f.closedAt(approved.ID); got == nil || !got.Equal(*closed) {
		t.Fatalf("closing time changed: %v, was %v", got, closed)
	}
	if _, err := f.store.CreateDraft(f.ctx, f.author, approved.LessonID, lessons.Fields{Content: doc("After publication", "fr")}); err != nil {
		t.Fatalf("draft after publication: %v", err)
	}
}

func TestPublishingStillRequiresApproval(t *testing.T) {
	f := newFixture(t)
	draft := f.lesson("Draft")
	wantSQLState(t, "publish draft", f.publish(draft.LessonID, draft.ID), "LRN01")
	inReview := f.submitted("In review")
	wantSQLState(t, "publish in review", f.publish(inReview.LessonID, inReview.ID), "LRN01")
	requested := f.submitted("Changes requested")
	f.mustRequestChanges(requested.ID)
	wantSQLState(t, "publish changes requested", f.publish(requested.LessonID, requested.ID), "LRN01")
	// Raw pointer updates without an audit row reach the 0004 approved-only
	// guard first.
	for name, r := range map[string]lessons.Revision{"draft": draft, "in review": inReview, "changes requested": requested} {
		wantGuard(t, "raw pointer to "+name, f.exec(`UPDATE lessons SET published_revision_id = $1 WHERE id = $2`, r.ID, r.LessonID), approvedOnly)
	}
	for _, id := range []int64{draft.ID, inReview.ID, requested.ID} {
		if f.closedAt(id) != nil {
			t.Fatalf("rejected publication closed revision %d", id)
		}
	}
	if n := f.count(`SELECT count(*) FROM lessons WHERE published_revision_id IS NOT NULL`); n != 0 {
		t.Fatalf("%d lessons published", n)
	}
}

// Publication and draft creation both lock the lesson row first, so a draft
// is either rejected (publication not yet committed) or based on the newly
// published revision; it is never created against the stale pointer.
func TestPublishAndCreateDraftAreSerialized(t *testing.T) {
	f := newFixture(t)

	// Publication holds the lesson: the draft waits, then sees it published.
	rev := f.approved("Held publication")
	tx, err := f.db.Begin(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(f.ctx) }()
	if err := f.publishIn(tx, rev.LessonID, rev.ID); err != nil {
		t.Fatal(err)
	}
	type result struct {
		rev lessons.Revision
		err error
	}
	done := make(chan result, 1)
	go func() {
		r, err := f.store.CreateDraft(f.ctx, f.author, rev.LessonID, lessons.Fields{Content: doc("Waiting", "fr")})
		done <- result{r, err}
	}()
	f.waitBlockedBy(tx.Conn().PgConn().PID())
	if err := tx.Commit(f.ctx); err != nil {
		t.Fatal(err)
	}
	r := <-done
	if r.err != nil {
		t.Fatalf("draft after committed publication: %v", r.err)
	}
	if r.rev.BaseRevisionID == nil || *r.rev.BaseRevisionID != rev.ID {
		t.Fatalf("base %v, want %d", r.rev.BaseRevisionID, rev.ID)
	}

	// A rolled-back publication leaves the candidate open.
	rev = f.approved("Rolled back publication")
	tx2, err := f.db.Begin(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx2.Rollback(f.ctx) }()
	if err := f.publishIn(tx2, rev.LessonID, rev.ID); err != nil {
		t.Fatal(err)
	}
	go func() {
		r, err := f.store.CreateDraft(f.ctx, f.author, rev.LessonID, lessons.Fields{Content: doc("Waiting", "fr")})
		done <- result{r, err}
	}()
	f.waitBlockedBy(tx2.Conn().PgConn().PID())
	if err := tx2.Rollback(f.ctx); err != nil {
		t.Fatal(err)
	}
	wantErr(t, (<-done).err, lessons.ErrActiveCandidateExists)
	if f.closedAt(rev.ID) != nil {
		t.Fatal("rolled-back publication closed the candidate")
	}

	// Unordered races: either outcome, never a stale base or a deadlock.
	for i := 0; i < 15; i++ {
		rev := f.approved("Race")
		start := make(chan struct{})
		published := make(chan error, 1)
		go func() {
			<-start
			published <- f.publish(rev.LessonID, rev.ID)
		}()
		go func() {
			<-start
			r, err := f.store.CreateDraft(f.ctx, f.author, rev.LessonID, lessons.Fields{Content: doc("Racing", "fr")})
			done <- result{r, err}
		}()
		close(start)
		if err := <-published; err != nil {
			t.Fatalf("iteration %d: publish: %v", i, err)
		}
		r := <-done
		switch {
		case r.err == nil:
			if r.rev.BaseRevisionID == nil || *r.rev.BaseRevisionID != rev.ID {
				t.Fatalf("iteration %d: draft base %v, want %d", i, r.rev.BaseRevisionID, rev.ID)
			}
		case errors.Is(r.err, lessons.ErrActiveCandidateExists):
			if n := f.count(`SELECT count(*) FROM lesson_revisions WHERE lesson_id = $1`, rev.LessonID); n != 1 {
				t.Fatalf("iteration %d: rejected draft stored", i)
			}
		default:
			t.Fatalf("iteration %d: create draft: %v", i, r.err)
		}
		if f.closedAt(rev.ID) == nil {
			t.Fatalf("iteration %d: publication did not close the candidate", i)
		}
	}
}

// leastPrivilegeRole creates a non-owner role like the deployed runtime role:
// USAGE on the test schema, DML on its tables and sequences, and TEMP (which
// PUBLIC has by default). It returns a function running statements in one
// committed transaction as that role on a new connection, and the quoted test schema. The role
// is dropped when the test ends.
func (f fixture) leastPrivilegeRole() (asRole func(stmts ...string) error, qs string) {
	t := f.t
	t.Helper()
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatal(err)
	}
	role := pgx.Identifier{"learnoo_test_app_" + hex.EncodeToString(b[:])}.Sanitize()
	var schema, dbName string
	if err := f.db.QueryRow(f.ctx, `SELECT current_schema(), current_database()`).Scan(&schema, &dbName); err != nil {
		t.Fatal(err)
	}
	f.mustExec(`CREATE ROLE ` + role + ` NOLOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT`)
	t.Cleanup(func() {
		// Roles are cluster-wide: always remove this one.
		ctx := context.Background()
		_, _ = f.db.Exec(ctx, `DROP OWNED BY `+role)
		if _, err := f.db.Exec(ctx, `DROP ROLE `+role); err != nil {
			t.Errorf("drop test role: %v", err)
		}
	})
	qs = pgx.Identifier{schema}.Sanitize()
	f.mustExec(`GRANT USAGE ON SCHEMA ` + qs + ` TO ` + role)
	f.mustExec(`GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA ` + qs + ` TO ` + role)
	f.mustExec(`GRANT USAGE ON ALL SEQUENCES IN SCHEMA ` + qs + ` TO ` + role)
	f.mustExec(`GRANT TEMP ON DATABASE ` + pgx.Identifier{dbName}.Sanitize() + ` TO ` + role)

	// Each call uses a fresh connection: PL/pgSQL caches plans per session,
	// so a pooled connection that already ran a guard could keep resolving
	// its tables to the real ones and hide a shadowing regression.
	asRole = func(stmts ...string) error {
		conn, err := pgx.ConnectConfig(f.ctx, f.db.Config().ConnConfig.Copy())
		if err != nil {
			return err
		}
		defer func() { _ = conn.Close(context.Background()) }()
		tx, err := conn.Begin(f.ctx)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback(f.ctx) }()
		if _, err := tx.Exec(f.ctx, `SET LOCAL ROLE `+role); err != nil {
			return err
		}
		for _, s := range stmts {
			if _, err := tx.Exec(f.ctx, s); err != nil {
				return err
			}
		}
		return tx.Commit(f.ctx)
	}
	return asRole, qs
}

// A least-privilege role (not the table owner, no CREATE on any schema, but
// TEMP like PUBLIC by default) cannot close a candidate through a trigger of
// its own, yet still closes it by publishing: the publication trigger runs as
// the table owner.
func TestCandidateCloseRequiresTableOwner(t *testing.T) {
	f := newFixture(t)
	rev := f.approved("Least privilege")

	asRole, qs := f.leastPrivilegeRole()

	closeStmt := fmt.Sprintf(`UPDATE %s.lesson_revisions SET candidate_closed_at = now() WHERE id = %d`, qs, rev.ID)
	wantSQLState(t, "direct close as the role", asRole(closeStmt), "LRN01")
	wantSQLState(t, "close from the role's temporary trigger", asRole(
		`CREATE TEMP TABLE probe (x int)`,
		`CREATE FUNCTION pg_temp.close_candidate() RETURNS trigger LANGUAGE plpgsql AS $$
			BEGIN `+closeStmt+`; RETURN NULL; END $$`,
		`CREATE TRIGGER probe_close AFTER INSERT ON pg_temp.probe FOR EACH ROW EXECUTE FUNCTION pg_temp.close_candidate()`,
		`INSERT INTO pg_temp.probe VALUES (1)`,
	), "LRN01")
	// The publication trigger function cannot be borrowed for a fake table.
	wantSQLState(t, "reuse publication trigger", asRole(
		`CREATE TEMP TABLE lessons (id bigint, published_revision_id bigint)`,
		`CREATE TRIGGER fake_publish AFTER INSERT ON pg_temp.lessons FOR EACH ROW EXECUTE FUNCTION `+qs+`.lessons_close_published_candidate()`,
		fmt.Sprintf(`INSERT INTO pg_temp.lessons VALUES (%d, %d)`, rev.LessonID, rev.ID),
	), "42501")
	if f.closedAt(rev.ID) != nil {
		t.Fatal("the role closed the candidate without publishing")
	}
	_, err := f.store.CreateDraft(f.ctx, f.author, rev.LessonID, lessons.Fields{Content: doc("Still blocked", "fr")})
	wantErr(t, err, lessons.ErrActiveCandidateExists)

	// Publication by the same role closes it.
	if err := asRole(
		fmt.Sprintf(`INSERT INTO %s.publication_audit (lesson_id, revision_id, actor_id, action) VALUES (%d, %d, %d, 'publish')`, qs, rev.LessonID, rev.ID, f.publisher),
		fmt.Sprintf(`UPDATE %s.lessons SET published_revision_id = %d WHERE id = %d`, qs, rev.ID, rev.LessonID)); err != nil {
		t.Fatalf("publication as the role: %v", err)
	}
	if f.closedAt(rev.ID) == nil {
		t.Fatal("publication as the role did not close the candidate")
	}
	if _, err := f.store.CreateDraft(f.ctx, f.author, rev.LessonID, lessons.Fields{Content: doc("After publication", "fr")}); err != nil {
		t.Fatalf("draft after publication: %v", err)
	}
}

// wantGuard asserts an LRN01 error whose message contains msg, so the test
// identifies which guard rejected the change.
func wantGuard(t *testing.T, what string, err error, msg string) {
	t.Helper()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "LRN01" || !strings.Contains(pgErr.Message, msg) {
		t.Errorf("%s: want LRN01 %q, got %v", what, msg, err)
	}
}

// The guards cannot be fooled by a temporary table of the calling role that
// shadows the table they read (pg_temp is otherwise searched first).
func TestGuardsIgnoreTemporaryShadowTables(t *testing.T) {
	f := newFixture(t)
	asRole, qs := f.leastPrivilegeRole()

	// A: a shadow lesson_revisions claiming a submitted revision is a draft
	// must not let media be attached to it.
	inReview := f.submitted("Shadowed media")
	img := f.media(f.author)
	before := f.snapshot(inReview.ID)
	wantGuard(t, "media on a submitted revision via shadow", asRole(
		`CREATE TEMP TABLE lesson_revisions (id bigint, lesson_id bigint, status text) ON COMMIT DROP`,
		fmt.Sprintf(`INSERT INTO pg_temp.lesson_revisions VALUES (%d, %d, 'draft')`, inReview.ID, inReview.LessonID),
		fmt.Sprintf(`INSERT INTO %s.revision_media VALUES (%d, %d)`, qs, inReview.ID, img),
	), "media references are immutable")
	if f.snapshot(inReview.ID) != before {
		t.Error("submitted revision's media changed")
	}

	// B: a shadow lesson_revisions claiming approval must not pass the
	// published-pointer guard.
	wantGuard(t, "publish unapproved via shadow", asRole(
		`CREATE TEMP TABLE lesson_revisions (id bigint, lesson_id bigint, status text) ON COMMIT DROP`,
		fmt.Sprintf(`INSERT INTO pg_temp.lesson_revisions VALUES (%d, %d, 'approved')`, inReview.ID, inReview.LessonID),
		fmt.Sprintf(`UPDATE %s.lessons SET published_revision_id = %d WHERE id = %d`, qs, inReview.ID, inReview.LessonID),
	), "only an approved revision")
	if l, err := f.store.Lesson(f.ctx, inReview.LessonID); err != nil || l.PublishedRevisionID != nil {
		t.Errorf("lesson published via shadow: %+v %v", l, err)
	}

	// C: a shadow lessons must neither forge the base revision nor reuse a
	// discarded revision number.
	pub := f.approved("Shadowed lesson")
	if err := f.publish(pub.LessonID, pub.ID); err != nil {
		t.Fatal(err)
	}
	discarded, err := f.store.CreateDraft(f.ctx, f.author, pub.LessonID, lessons.Fields{Content: doc("Discarded", "fr")})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.DiscardDraft(f.ctx, f.author, discarded.ID, discarded.Version); err != nil {
		t.Fatal(err)
	}
	insert := func(number int, base string) string {
		return fmt.Sprintf(`
			INSERT INTO %[1]s.lesson_revisions (lesson_id, revision_number, author_id, base_revision_id, title, objectives,
				content, schema_version, language, subject_id, created_at, updated_at)
			SELECT lesson_id, %[2]d, author_id, %[3]s, title, objectives, content, schema_version, language, subject_id, now(), now()
			  FROM %[1]s.lesson_revisions WHERE id = %[4]d`, qs, number, base, pub.ID)
	}
	shadow := func(published string) []string {
		return []string{
			`CREATE TEMP TABLE lessons (id bigint, published_revision_id bigint, last_revision_number integer) ON COMMIT DROP`,
			fmt.Sprintf(`INSERT INTO pg_temp.lessons VALUES (%d, %s, 0)`, pub.LessonID, published),
		}
	}
	wantGuard(t, "forged base via shadow", asRole(append(shadow("NULL"), insert(3, "NULL"))...),
		"base_revision_id must be the lesson's current published revision")
	wantGuard(t, "reused number via shadow", asRole(append(shadow(fmt.Sprint(pub.ID)), insert(discarded.Number, fmt.Sprint(pub.ID)))...),
		"was already used")
	if n := f.count(`SELECT count(*) FROM lesson_revisions WHERE lesson_id = $1`, pub.LessonID); n != 1 {
		t.Errorf("shadowed inserts stored revisions: %d", n)
	}
	if n := f.count(`SELECT last_revision_number FROM lessons WHERE id = $1`, pub.LessonID); n != discarded.Number {
		t.Errorf("last_revision_number %d, want %d", n, discarded.Number)
	}
	// Without a shadow, the same role can insert a correct draft.
	if err := asRole(insert(discarded.Number+1, fmt.Sprint(pub.ID))); err != nil {
		t.Fatalf("legitimate insert as the role: %v", err)
	}
}
