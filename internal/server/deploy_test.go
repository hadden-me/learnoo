package server_test

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hadden-me/learnoo/internal/accounts"
	"github.com/hadden-me/learnoo/internal/auth"
	"github.com/hadden-me/learnoo/internal/backupcheck"
	"github.com/hadden-me/learnoo/internal/database"
	"github.com/hadden-me/learnoo/internal/database/dbtest"
	"github.com/hadden-me/learnoo/internal/invitations"
	"github.com/hadden-me/learnoo/internal/media"
	"github.com/hadden-me/learnoo/internal/permissions"
	"github.com/hadden-me/learnoo/internal/server"
	"github.com/hadden-me/learnoo/migrations"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Deployment rehearsal (LRN-010): the operator scripts in deploy/ against
// real PostgreSQL. Each test creates uniquely named roles and databases in
// the test cluster (the test role must be a superuser, as in CI and the local
// Compose database) and drops them afterwards. Needs psql, pg_dump,
// pg_restore and bash on PATH. TEST fixtures and simulated users only.

// requireTools skips (or, with LEARNOO_REQUIRE_DB_TESTS=1, fails) without
// the PostgreSQL client tools.
func requireTools(t *testing.T, names ...string) {
	t.Helper()
	for _, n := range names {
		if _, err := exec.LookPath(n); err != nil {
			if os.Getenv("LEARNOO_REQUIRE_DB_TESTS") == "1" {
				t.Fatalf("%s not found on PATH but LEARNOO_REQUIRE_DB_TESTS=1", n)
			}
			t.Skipf("%s not found on PATH", n)
		}
	}
}

func repoFile(t *testing.T, rel string) string {
	t.Helper()
	p, err := filepath.Abs(filepath.Join("..", "..", filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// deployment is one database set up as in production: roles.sql, migrations
// as the owner, grants.sql.
type deployment struct {
	t                  *testing.T
	super              string // superuser URL from LEARNOO_TEST_DATABASE_URL
	owner, runtime, db string
}

func randomSuffix(t *testing.T) string {
	t.Helper()
	var b [5]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b[:])
}

// newRoles reserves unique role names and drops the roles when the test ends
// (after every database created with them, as cleanups run last-in first-out).
func newRoles(t *testing.T) deployment {
	t.Helper()
	requireTools(t, "psql", "pg_dump", "pg_restore", "bash")
	s := randomSuffix(t)
	d := deployment{t: t, super: dbtest.URL(t), owner: "lrn_t_" + s + "_owner", runtime: "lrn_t_" + s + "_app"}
	t.Cleanup(func() {
		admin, err := database.Open(context.Background(), d.super)
		if err != nil {
			t.Error(err)
			return
		}
		defer admin.Close()
		for _, r := range []string{d.runtime, d.owner} {
			if _, err := admin.Exec(context.Background(), `DROP ROLE IF EXISTS "`+r+`"`); err != nil {
				t.Errorf("drop role %s: %v", r, err)
			}
		}
	})
	return d
}

// createDatabase runs roles.sql for a new database named after the roles.
// A per-database stand-in for the "postgres" maintenance database is created
// too, so the PUBLIC revocation there never touches the shared test cluster.
func (d deployment) createDatabase(suffix string) deployment {
	d.t.Helper()
	d.db = d.owner + "_" + suffix
	admin, err := database.Open(context.Background(), d.super)
	if err != nil {
		d.t.Fatal(err)
	}
	defer admin.Close()
	if _, err := admin.Exec(context.Background(), `CREATE DATABASE "`+d.maintenanceDB()+`"`); err != nil {
		d.t.Fatal(err)
	}
	d.t.Cleanup(func() {
		admin, err := database.Open(context.Background(), d.super)
		if err != nil {
			d.t.Error(err)
			return
		}
		defer admin.Close()
		for _, db := range []string{d.db, d.maintenanceDB()} {
			if _, err := admin.Exec(context.Background(), `DROP DATABASE IF EXISTS "`+db+`" WITH (FORCE)`); err != nil {
				d.t.Errorf("drop database %s: %v", db, err)
			}
		}
	})
	d.runRoles()
	return d
}

func (d deployment) maintenanceDB() string { return d.db + "_m" }

func (d deployment) rolesArgs() []string {
	return []string{"owner_role", d.owner, "runtime_role", d.runtime, "dbname", d.db, "maintenance_db", d.maintenanceDB()}
}

func (d deployment) runRoles() {
	d.t.Helper()
	d.psql(d.super, repoFile(d.t, "deploy/db/roles.sql"), d.rolesArgs()...)
}

// urlFor is the test cluster URL for user and database (trust authentication
// in the test cluster; no password).
func (d deployment) urlFor(user, db string) string {
	d.t.Helper()
	u, err := url.Parse(d.super)
	if err != nil {
		d.t.Fatal(err)
	}
	u.User = url.User(user)
	u.Path = "/" + db
	q := u.Query()
	q.Del("search_path")
	u.RawQuery = q.Encode()
	return u.String()
}

func (d deployment) ownerURL() string   { return d.urlFor(d.owner, d.db) }
func (d deployment) runtimeURL() string { return d.urlFor(d.runtime, d.db) }

func (d deployment) psqlErr(dbURL, file string, vars ...string) (string, error) {
	args := []string{"-X", "-q", "-v", "ON_ERROR_STOP=1", "-d", dbURL, "-f", file}
	for i := 0; i+1 < len(vars); i += 2 {
		args = append(args, "-v", vars[i]+"="+vars[i+1])
	}
	out, err := exec.Command("psql", args...).CombinedOutput()
	return string(out), err
}

func (d deployment) psql(dbURL, file string, vars ...string) string {
	d.t.Helper()
	out, err := d.psqlErr(dbURL, file, vars...)
	if err != nil {
		d.t.Fatalf("psql %s: %v\n%s", filepath.Base(file), err, out)
	}
	return out
}

func (d deployment) pool(dbURL string) *pgxpool.Pool {
	d.t.Helper()
	p, err := database.Open(context.Background(), dbURL)
	if err != nil {
		d.t.Fatal(err)
	}
	d.t.Cleanup(p.Close)
	return p
}

// migrate applies the migrations as the owner and re-runs grants.sql, as the
// Compose migrate and grants steps do.
func (d deployment) migrate() {
	d.t.Helper()
	if _, err := database.Migrate(context.Background(), d.pool(d.ownerURL()), migrations.FS); err != nil {
		d.t.Fatal(err)
	}
	d.psql(d.ownerURL(), repoFile(d.t, "deploy/db/grants.sql"), "runtime_role", d.runtime)
}

// runtimeEnv is the server handler running as the runtime role.
func runtimeEnv(t *testing.T, db *pgxpool.Pool, mediaDir string) *env {
	t.Helper()
	if err := database.Check(context.Background(), db, migrations.FS); err != nil {
		t.Fatalf("startup check as the runtime role: %v", err)
	}
	cfg := auth.DefaultConfig
	cfg.Argon2 = testArgon2
	c := &clock{t: time.Now().UTC()}
	svc, err := auth.New(db, cfg, c.Now)
	if err != nil {
		t.Fatal(err)
	}
	ms, err := media.OpenStore(mediaDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ms.Close() })
	return &env{t: t, db: db, h: server.New(db, svc, ms, slog.New(slog.NewTextHandler(io.Discard, nil))), clock: c, cfg: cfg}
}

// seeded is the state left by the representative flow.
type seeded struct {
	withdrawFixture
	student          apiUser
	mediaID          int64
	image            []byte
	lessonPublicID   string
	first, second    revisionBody
	pendingCandidate revisionBody
	resolvedReport   int64 // error report resolved through the API
	openReport       int64 // error report left open
}

// runFlow drives the editorial and reader workflow through the HTTP handler
// as the runtime role: permission grants, upload and link media, draft,
// submit, approve, publish, a second edition, invitation, reading, bookmark,
// withdrawal, restoration, error reports, a refused non-owner draft, an
// account disabled by the operator, and an approved candidate left pending.
func runFlow(t *testing.T, e *env) seeded {
	t.Helper()
	subject := e.newSubject("test-deploy-math")
	sf := studioFixture{env: e, subject: subject, author: e.newUser("test-deploy-author", subject, permissions.Author)}
	rf := reviewFixture{studioFixture: sf,
		reviewer:  e.newUser("test-deploy-reviewer", subject, permissions.Reviewer),
		publisher: e.newUser("test-deploy-publisher", subject, permissions.Publisher)}
	f := withdrawFixture{reviewFixture: rf, admin: e.newUser("test-deploy-admin", subject, permissions.Administrator)}
	s := seeded{withdrawFixture: f}

	s.image = pngBytes(t, 77)
	w := f.uploadBytes(f.author, "image/png", s.image)
	wantStatus(t, "upload", w, http.StatusCreated, "")
	var up struct{ ID int64 }
	if err := json.Unmarshal(w.Body.Bytes(), &up); err != nil || up.ID == 0 {
		t.Fatalf("upload: %v %s", err, w.Body)
	}
	s.mediaID = up.ID

	s.first = f.approvedRev(f.submittedLesson(f.author, "TEST Déploiement première édition", s.mediaID))
	f.publishedRev(s.first)
	s.second = f.approvedRev(f.successorRev(s.first.LessonID, "TEST Déploiement deuxième édition"))
	f.publishedRev(s.second)

	rd := readerFixture{env: e, subject: subject, author: f.author, reviewer: f.reviewer, publisher: f.publisher, admin: f.admin,
		inv: invitations.NewStore(e.db, nil)}
	s.student = rd.invitedStudent("test-deploy-student@example.test", "TEST Élève Déploiement")
	s.lessonPublicID = rd.publicID(s.first.LessonID)
	wantPage(t, "read second edition", rd.get(s.student, lessonPath(s.lessonPublicID)), http.StatusOK, "TEST Déploiement deuxième édition")
	wantRedirect(t, "bookmark", rd.post(s.student, lessonPath(s.lessonPublicID)+"/bookmark", nil), lessonPath(s.lessonPublicID))

	wantStatus(t, "withdraw", f.api(f.admin, http.MethodPost, withdrawPath(s.second.ID),
		map[string]any{"reason": "TEST withdrawal.", "expectedPublishedRevisionId": s.second.ID}), http.StatusOK, "")
	wantStatus(t, "restore", f.api(f.publisher, http.MethodPost, restorePath(s.first.ID),
		map[string]any{"expectedPublishedRevisionId": nil}), http.StatusOK, "")
	wantPage(t, "read restored edition", rd.get(s.student, lessonPath(s.lessonPublicID)), http.StatusOK, "TEST Déploiement première édition")
	wantPage(t, "dashboard bookmark", rd.get(s.student, "/learn"), http.StatusOK, "Mes signets (1)")

	// Error reports (LRN-015): file two, resolve one through the review API.
	current := published{lessonID: s.first.LessonID, revisionID: s.first.ID, publicID: s.lessonPublicID}
	for _, msg := range []string{"TEST erreur résolue.", "TEST erreur ouverte."} {
		wantRedirect(t, "file report", rd.fileReport(s.student, current, msg), lessonPath(s.lessonPublicID)+"/reported")
	}
	if err := e.db.QueryRow(context.Background(),
		`SELECT min(id), max(id) FROM error_reports`).Scan(&s.resolvedReport, &s.openReport); err != nil {
		t.Fatal(err)
	}
	wantStatus(t, "resolve report", f.api(f.reviewer, http.MethodPost,
		fmt.Sprintf("/api/review/error-reports/%d/resolve", s.resolvedReport), map[string]string{"note": "TEST corrigé."}), http.StatusOK, "")

	// Ownership (issue 6): another author cannot start a draft of the lesson.
	other := e.newUser("test-deploy-author-two", subject, permissions.Author)
	wantStatus(t, "non-owner draft", f.api(other, http.MethodPost, fmt.Sprintf("/api/lessons/%d/drafts", s.first.LessonID),
		map[string]any{"content": testDoc("TEST Pas à moi", "fr")}), http.StatusNotFound, "not_found")

	// Account status (LRN-013): the operator disables an account, which
	// revokes its sessions and is audited.
	if changed, err := accounts.NewStore(e.db).Disable(context.Background(), testOperator, other.id); err != nil || !changed {
		t.Fatalf("disable account: changed=%v err=%v", changed, err)
	}
	wantStatus(t, "disabled account", f.api(other, http.MethodGet, "/api/review/error-reports", nil), http.StatusUnauthorized, "unauthenticated")
	var audited int
	if err := e.db.QueryRow(context.Background(),
		`SELECT count(*) FROM account_status_audit WHERE target_user_id = $1 AND action = 'disable'`, other.id).Scan(&audited); err != nil || audited != 1 {
		t.Fatalf("account status audit: %d %v", audited, err)
	}

	s.pendingCandidate = f.approvedRev(f.successorRev(s.first.LessonID, "TEST Déploiement candidate"))
	return s
}

// wantRefused runs sql as the runtime role and expects an error; code, if
// set, is the expected SQLSTATE.
func wantRefused(t *testing.T, db *pgxpool.Pool, what, code, sql string, args ...any) {
	t.Helper()
	_, err := db.Exec(context.Background(), sql, args...)
	if err == nil {
		t.Fatalf("%s: accepted for the runtime role", what)
	}
	var pg *pgconn.PgError
	if !errors.As(err, &pg) {
		t.Fatalf("%s: %v", what, err)
	}
	if code != "" && pg.Code != code {
		t.Fatalf("%s: SQLSTATE %s (%s), want %s", what, pg.Code, pg.Message, code)
	}
}

const (
	insufficientPrivilege = "42501"
	guardRefusal          = "LRN01" // SQLSTATE raised by the editorial guards
)

func TestRuntimeRoleRunsTheAppAndGuardsBindIt(t *testing.T) {
	d := newRoles(t).createDatabase("roles")
	// Before migrations the runtime role cannot start the server.
	if err := database.Check(context.Background(), d.pool(d.runtimeURL()), migrations.FS); !errors.Is(err, database.ErrPending) {
		t.Fatalf("unmigrated: want ErrPending, got %v", err)
	}
	d.migrate()
	// Idempotent: running both scripts again changes nothing and passes their checks.
	d.runRoles()
	d.psql(d.ownerURL(), repoFile(t, "deploy/db/grants.sql"), "runtime_role", d.runtime)

	// The maintenance database is closed to PUBLIC (superusers keep access).
	var maintConnect, maintTemp bool
	if err := d.pool(d.super).QueryRow(context.Background(),
		`SELECT has_database_privilege('public', $1, 'CONNECT'), has_database_privilege('public', $1, 'TEMP')`,
		d.maintenanceDB()).Scan(&maintConnect, &maintTemp); err != nil {
		t.Fatal(err)
	}
	if maintConnect || maintTemp {
		t.Fatalf("maintenance database still open to PUBLIC: connect=%v temp=%v", maintConnect, maintTemp)
	}
	if _, err := database.Open(context.Background(), d.urlFor(d.runtime, d.maintenanceDB())); err == nil {
		t.Fatal("runtime role connected to the maintenance database")
	}

	// grants.sql refuses a runtime role with any membership; roles.sql removes it.
	if _, err := d.pool(d.super).Exec(context.Background(), `GRANT pg_read_all_data TO "`+d.runtime+`"`); err != nil {
		t.Fatal(err)
	}
	if out, err := d.psqlErr(d.ownerURL(), repoFile(t, "deploy/db/grants.sql"), "runtime_role", d.runtime); err == nil ||
		!strings.Contains(out, "must be a member of no role") {
		t.Fatalf("grants.sql accepted a runtime role with a membership: %v\n%s", err, out)
	}
	d.runRoles()

	rt := d.pool(d.runtimeURL())
	s := runFlow(t, runtimeEnv(t, rt, t.TempDir()))
	ctx := context.Background()

	var owns, super, temp, create bool
	if err := rt.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM pg_class WHERE relowner = (SELECT oid FROM pg_roles WHERE rolname = current_user)),
		       (SELECT rolsuper FROM pg_roles WHERE rolname = current_user),
		       has_database_privilege(current_database(), 'TEMP'),
		       has_schema_privilege('public', 'CREATE')`).Scan(&owns, &super, &temp, &create); err != nil {
		t.Fatal(err)
	}
	if owns || super || temp || create {
		t.Fatalf("runtime role: owns=%v super=%v temp=%v create=%v", owns, super, temp, create)
	}

	// No way to create objects that could shadow tables or host triggers.
	wantRefused(t, rt, "temporary table", insufficientPrivilege, `CREATE TEMP TABLE lesson_revisions (id bigint)`)
	wantRefused(t, rt, "table in public", insufficientPrivilege, `CREATE TABLE public.shadow (id bigint)`)
	wantRefused(t, rt, "function in public", insufficientPrivilege,
		`CREATE FUNCTION public.f() RETURNS trigger LANGUAGE plpgsql AS $$BEGIN RETURN NEW; END$$`)
	// No way to switch the guards off.
	wantRefused(t, rt, "disable triggers", insufficientPrivilege, `ALTER TABLE lessons DISABLE TRIGGER ALL`)
	wantRefused(t, rt, "replica mode", insufficientPrivilege, `SET session_replication_role = replica`)
	wantRefused(t, rt, "drop guard", insufficientPrivilege, `DROP TRIGGER zz_guard_lessons_identity ON lessons`)
	wantRefused(t, rt, "truncate audit", insufficientPrivilege, `TRUNCATE publication_audit`)
	wantRefused(t, rt, "forge migration history", insufficientPrivilege, `DELETE FROM schema_migrations`)
	var canExec bool
	if err := rt.QueryRow(ctx, `SELECT has_function_privilege('lessons_close_published_candidate()', 'EXECUTE')
		OR has_function_privilege('revision_withdrawals_close_candidate()', 'EXECUTE')`).Scan(&canExec); err != nil {
		t.Fatal(err)
	}
	if canExec {
		t.Fatal("runtime role may execute a SECURITY DEFINER function")
	}

	// The editorial guards bind the runtime role.
	before := s.withdrawState()
	wantRefused(t, rt, "close a candidate directly", guardRefusal,
		`UPDATE lesson_revisions SET candidate_closed_at = now() WHERE id = $1`, s.pendingCandidate.ID)
	wantRefused(t, rt, "pointer change without audit", guardRefusal,
		`UPDATE lessons SET published_revision_id = NULL WHERE id = $1`, s.first.LessonID)
	wantRefused(t, rt, "pointer to a withdrawn revision", guardRefusal,
		`UPDATE lessons SET published_revision_id = $2 WHERE id = $1`, s.first.LessonID, s.second.ID)
	wantRefused(t, rt, "edit an approved snapshot", guardRefusal,
		`UPDATE lesson_revisions SET title = 'TEST forged' WHERE id = $1`, s.first.ID)
	wantRefused(t, rt, "delete audit", guardRefusal, `DELETE FROM publication_audit`)
	wantRefused(t, rt, "replace a media row", guardRefusal, `DELETE FROM media_assets WHERE id = $1`, s.mediaID)
	// Error report guards (migration 0009).
	wantRefused(t, rt, "truncate error reports", insufficientPrivilege, `TRUNCATE error_reports`)
	wantRefused(t, rt, "close a report without a resolution", guardRefusal,
		`UPDATE error_reports SET status = 'resolved' WHERE id = $1`, s.openReport)
	wantRefused(t, rt, "edit a report", guardRefusal,
		`UPDATE error_reports SET message = 'TEST forged' WHERE id = $1`, s.openReport)
	wantRefused(t, rt, "delete a report", guardRefusal, `DELETE FROM error_reports WHERE id = $1`, s.resolvedReport)
	wantRefused(t, rt, "resolve a closed report again", guardRefusal,
		`INSERT INTO error_report_resolutions (report_id, actor_id, action, note) VALUES ($1, $2, 'dismiss', 'TEST')`,
		s.resolvedReport, s.admin.id)
	wantRefused(t, rt, "resolution by a student", guardRefusal,
		`INSERT INTO error_report_resolutions (report_id, actor_id, action, note) VALUES ($1, $2, 'dismiss', 'TEST')`,
		s.openReport, s.student.id)
	wantRefused(t, rt, "rewrite a resolution", guardRefusal,
		`UPDATE error_report_resolutions SET note = 'TEST forged' WHERE report_id = $1`, s.resolvedReport)
	wantRefused(t, rt, "report a withdrawn revision", guardRefusal,
		`INSERT INTO error_reports (lesson_id, revision_id, reporter_id, message) VALUES ($1, $2, $3, 'TEST')`,
		s.second.LessonID, s.second.ID, s.student.id)
	if after := s.withdrawState(); after != before {
		t.Fatalf("refused statements changed state:\n%s\n%s", before, after)
	}
}

// backup runs deploy/backup.sh and returns the backup directory.
func (d deployment) backup(mediaDir string) string {
	d.t.Helper()
	cmd := exec.Command("bash", repoFile(d.t, "deploy/backup.sh"))
	cmd.Env = append(os.Environ(),
		"LEARNOO_BACKUP_DATABASE_URL="+d.ownerURL(),
		"LEARNOO_BACKUP_MEDIA_DIR="+mediaDir,
		"LEARNOO_BACKUP_DIR="+d.t.TempDir(),
		"LEARNOO_DB_PASSWORD_FILE=")
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			d.t.Fatalf("backup.sh: %v\n%s", err, ee.Stderr)
		}
		d.t.Fatal(err)
	}
	dir := strings.TrimSpace(string(out))
	if !strings.HasPrefix(filepath.Base(dir), "learnoo-") {
		d.t.Fatalf("backup.sh printed %q", out)
	}
	return dir
}

// restore runs deploy/restore.sh into d's database and mediaDir.
func (d deployment) restore(backup, mediaDir string) (string, error) {
	cmd := exec.Command("bash", repoFile(d.t, "deploy/restore.sh"), backup)
	cmd.Env = append(os.Environ(),
		"LEARNOO_RESTORE_DATABASE_URL="+d.ownerURL(),
		"LEARNOO_RESTORE_MEDIA_DIR="+mediaDir,
		"LEARNOO_RUNTIME_ROLE="+d.runtime,
		"LEARNOO_DB_PASSWORD_FILE=", "LEARNOO_MEDIA_OWNER=")
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func verifyRestore(t *testing.T, db *pgxpool.Pool, mediaDir string) *backupcheck.Report {
	t.Helper()
	r, err := backupcheck.Verify(context.Background(), db, mediaDir, migrations.FS)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func copyDir(t *testing.T, src string) string {
	t.Helper()
	dst := filepath.Join(t.TempDir(), filepath.Base(src))
	if out, err := exec.Command("cp", "-R", src, dst).CombinedOutput(); err != nil {
		t.Fatalf("cp: %v %s", err, out)
	}
	if out, err := exec.Command("chmod", "-R", "u+w", dst).CombinedOutput(); err != nil {
		t.Fatalf("chmod: %v %s", err, out)
	}
	return dst
}

func TestBackupRestoreRehearsal(t *testing.T) {
	roles := newRoles(t)
	src := roles.createDatabase("src")
	src.migrate()
	srcMedia := t.TempDir()
	srcRuntime := src.pool(src.runtimeURL())
	s := runFlow(t, runtimeEnv(t, srcRuntime, srcMedia))
	// A crash leftover in the source: excluded from the backup.
	if err := os.WriteFile(filepath.Join(srcMedia, media.TempPrefix+"interrupted"), []byte("x"), 0o444); err != nil {
		t.Fatal(err)
	}
	// A stray file with a name restore.sh would refuse: skipped and counted.
	if err := os.WriteFile(filepath.Join(srcMedia, "stray name.txt"), []byte("x"), 0o444); err != nil {
		t.Fatal(err)
	}
	srcReport := verifyRestore(t, srcRuntime, srcMedia)
	if srcReport.Failed(false) || srcReport.Assets != 1 || srcReport.Referenced != 1 || srcReport.Published != 1 {
		t.Fatalf("source state: %+v", srcReport)
	}

	backup := src.backup(srcMedia)
	for _, f := range []string{"MANIFEST.sha256", "database.dump", "media.tar", "media-files.sha256", "info.txt"} {
		fi, err := os.Stat(filepath.Join(backup, f))
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm()&0o077 != 0 {
			t.Errorf("%s is readable by others: %v", f, fi.Mode())
		}
	}
	info, err := os.ReadFile(filepath.Join(backup, "info.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(info), "latest_migration: "+latestMigration(t)) || !strings.Contains(string(info), "media_files: 1") ||
		!strings.Contains(string(info), "media_files_skipped: 1") {
		t.Fatalf("info.txt:\n%s", info)
	}

	// Restore into a fresh database and an empty media directory.
	dst := roles.createDatabase("dst")
	dstMedia := filepath.Join(t.TempDir(), "media")
	if out, err := dst.restore(backup, dstMedia); err != nil {
		t.Fatalf("restore.sh: %v\n%s", err, out)
	}
	dstRuntime := dst.pool(dst.runtimeURL())
	report := verifyRestore(t, dstRuntime, dstMedia)
	if report.Failed(true) {
		var b strings.Builder
		report.WriteText(&b, true)
		t.Fatalf("restored state fails verification:\n%s", b.String())
	}
	if fmt.Sprint(report.Counts) != fmt.Sprint(srcReport.Counts) {
		t.Fatalf("row counts differ:\nsource   %v\nrestored %v", srcReport.Counts, report.Counts)
	}
	// The restored database keeps the least-privilege setup.
	dst.psql(dst.ownerURL(), repoFile(t, "deploy/db/grants.sql"), "runtime_role", dst.runtime)
	wantRefused(t, dstRuntime, "temporary table after restore", insufficientPrivilege, `CREATE TEMP TABLE x (id int)`)
	wantRefused(t, dstRuntime, "pointer change without audit after restore", guardRefusal,
		`UPDATE lessons SET published_revision_id = NULL WHERE id = $1`, s.first.LessonID)

	// The restored application serves the same lesson, bookmark and image to
	// the same sessions, as the runtime role.
	re := runtimeEnv(t, dstRuntime, dstMedia)
	rd := readerFixture{env: re}
	wantPage(t, "restored lesson", rd.get(s.student, lessonPath(s.lessonPublicID)), http.StatusOK, "TEST Déploiement première édition")
	wantPage(t, "restored bookmark", rd.get(s.student, "/learn"), http.StatusOK, "Mes signets (1)")
	w := re.api(s.author, http.MethodGet, fmt.Sprintf("/api/media/%d", s.mediaID), nil)
	if w.Code != http.StatusOK || !bytes.Equal(w.Body.Bytes(), s.image) {
		t.Fatalf("restored media: %d (%d bytes)", w.Code, w.Body.Len())
	}

	t.Run("restore refuses existing data", func(t *testing.T) {
		if out, err := dst.restore(backup, t.TempDir()); err == nil || !strings.Contains(out, "target database is not empty") {
			t.Fatalf("restore over a non-empty database: %v\n%s", err, out)
		}
		other := roles.createDatabase("nonempty")
		if out, err := other.restore(backup, dstMedia); err == nil || !strings.Contains(out, "media directory") {
			t.Fatalf("restore over a non-empty media directory: %v\n%s", err, out)
		}
	})

	t.Run("checksum mismatch is refused before restoring", func(t *testing.T) {
		for _, name := range []string{"media.tar", "database.dump", "media-files.sha256"} {
			tampered := copyDir(t, backup)
			p := filepath.Join(tampered, name)
			data, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			data[len(data)/2] ^= 0x01
			if err := os.WriteFile(p, data, 0o600); err != nil {
				t.Fatal(err)
			}
			target := roles.createDatabase("tamper" + strings.NewReplacer(".", guardRefusal, "-", "").Replace(name))
			targetMedia := filepath.Join(t.TempDir(), "media")
			out, err := target.restore(tampered, targetMedia)
			if err == nil || !strings.Contains(out, "checksum mismatch") {
				t.Fatalf("%s tampered: %v\n%s", name, err, out)
			}
			var tables int
			if err := target.pool(target.ownerURL()).QueryRow(context.Background(),
				`SELECT count(*) FROM pg_tables WHERE schemaname = 'public'`).Scan(&tables); err != nil {
				t.Fatal(err)
			}
			if entries, _ := os.ReadDir(targetMedia); tables != 0 || len(entries) != 0 {
				t.Fatalf("%s tampered: restore wrote %d tables, %d media entries", name, tables, len(entries))
			}
		}
	})

	t.Run("unsafe media archive is refused before the database", func(t *testing.T) {
		for name, extra := range map[string]*tar.Header{
			"symlink":   {Name: "./ab/cd/evil", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd", Mode: 0o777},
			"hard link": {Name: "./ab/cd/hard", Typeflag: tar.TypeLink, Linkname: "/etc/passwd", Mode: 0o444},
			"unlisted":  {Name: "./ab/cd/extra", Typeflag: tar.TypeReg, Mode: 0o444, Size: 1},
			"traversal": {Name: "../escape", Typeflag: tar.TypeReg, Mode: 0o444, Size: 1},
			"absolute":  {Name: "/tmp/learnoo-absolute", Typeflag: tar.TypeReg, Mode: 0o444, Size: 1},
		} {
			tampered := copyDir(t, backup)
			appendTarEntry(t, filepath.Join(tampered, "media.tar"), extra)
			rewriteManifest(t, tampered)
			target := roles.createDatabase("unsafe" + strings.ReplaceAll(name, " ", ""))
			targetMedia := filepath.Join(t.TempDir(), "media")
			out, err := target.restore(tampered, targetMedia)
			if err == nil || !strings.Contains(out, "media.tar has an entry") {
				t.Fatalf("%s entry: %v\n%s", name, err, out)
			}
			var tables int
			if err := target.pool(target.ownerURL()).QueryRow(context.Background(),
				`SELECT count(*) FROM pg_tables WHERE schemaname = 'public'`).Scan(&tables); err != nil {
				t.Fatal(err)
			}
			if entries, _ := os.ReadDir(targetMedia); tables != 0 || len(entries) != 0 {
				t.Fatalf("%s entry: restore wrote %d tables, %d media entries", name, tables, len(entries))
			}
		}
	})

	t.Run("incomplete backup is refused", func(t *testing.T) {
		partial := copyDir(t, backup)
		if err := os.Remove(filepath.Join(partial, "media.tar")); err != nil {
			t.Fatal(err)
		}
		target := roles.createDatabase("partial")
		if out, err := target.restore(partial, filepath.Join(t.TempDir(), "m")); err == nil || !strings.Contains(out, "incomplete") {
			t.Fatalf("incomplete backup: %v\n%s", err, out)
		}
	})

	rel, err := media.RelPath(storageKey(t, dstRuntime, s.mediaID))
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dstMedia, filepath.FromSlash(rel))
	original, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	t.Run("corrupted media file fails verification", func(t *testing.T) {
		bad := bytes.Clone(original)
		bad[len(bad)-5] ^= 0x01
		replaceFile(t, file, bad)
		wantVerifyProblem(t, verifyRestore(t, dstRuntime, dstMedia), "does not match the recorded SHA-256")
		replaceFile(t, file, original)
	})
	t.Run("missing media file fails verification", func(t *testing.T) {
		if err := os.Remove(file); err != nil {
			t.Fatal(err)
		}
		wantVerifyProblem(t, verifyRestore(t, dstRuntime, dstMedia), "is missing")
		replaceFile(t, file, original)
	})
	if r := verifyRestore(t, dstRuntime, dstMedia); r.Failed(true) {
		t.Fatalf("restored file not accepted again: %q", r.Problems)
	}
}

func storageKey(t *testing.T, db *pgxpool.Pool, id int64) string {
	t.Helper()
	var key string
	if err := db.QueryRow(context.Background(), `SELECT storage_key FROM media_assets WHERE id = $1`, id).Scan(&key); err != nil {
		t.Fatal(err)
	}
	return key
}

func replaceFile(t *testing.T, path string, data []byte) {
	t.Helper()
	_ = os.Remove(path)
	if err := os.WriteFile(path, data, 0o444); err != nil {
		t.Fatal(err)
	}
}

func wantVerifyProblem(t *testing.T, r *backupcheck.Report, substr string) {
	t.Helper()
	for _, p := range r.Problems {
		if strings.Contains(p, substr) {
			return
		}
	}
	t.Fatalf("no problem contains %q: %q", substr, r.Problems)
}

// appendTarEntry rewrites archive with every original entry plus h (and one
// byte of content for regular files).
func appendTarEntry(t *testing.T, archive string, h *tar.Header) {
	t.Helper()
	in, err := os.ReadFile(archive)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	tw := tar.NewWriter(&out)
	tr := tar.NewReader(bytes.NewReader(in))
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := io.Copy(tw, tr); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.WriteHeader(h); err != nil {
		t.Fatal(err)
	}
	if h.Typeflag == tar.TypeReg {
		if _, err := tw.Write([]byte("x")); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(archive, out.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
}

// rewriteManifest recomputes MANIFEST.sha256, as an attacker able to edit
// the backup could, so that only the archive validation stands in the way.
func rewriteManifest(t *testing.T, dir string) {
	t.Helper()
	var b strings.Builder
	for _, f := range []string{"database.dump", "media.tar", "media-files.sha256", "info.txt"} {
		data, err := os.ReadFile(filepath.Join(dir, f))
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&b, "%x  %s\n", sha256.Sum256(data), f)
	}
	if err := os.WriteFile(filepath.Join(dir, "MANIFEST.sha256"), []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
}

func latestMigration(t *testing.T) string {
	t.Helper()
	migs, err := database.Load(migrations.FS)
	if err != nil || len(migs) == 0 {
		t.Fatalf("load migrations: %v", err)
	}
	return migs[len(migs)-1].Name
}
