// Command server runs the Learnoo application server and its local operator
// commands.
//
//	server serve                                   start the HTTP server (default)
//	server migrate [-check]                        apply or verify database migrations
//	server create-user -email E -name N            create an account; password on stdin
//	server bootstrap-admin -email E -name N        create the first administrator; password on stdin
//	server grant -email E -permission P [-subject CODE]
//	server revoke -email E -permission P [-subject CODE]
//	server disable-user -email E                   disable an account and revoke its sessions
//	server enable-user -email E                    re-enable a disabled account
//	server show-user -email E                      status, permissions, reviewer subjects and audit
//	server invite-student -email E [-ttl D]        print a one-time Student invitation link
//	server add-subject -code C                     add a catalogue subject (audited)
//	server add-chapter -subject S -code C          add a chapter to a subject (audited)
//	server set-subject-name -code C [-fr N] [-ar N]  set a subject's display names (audited)
//	server set-chapter-name -subject S -code C [-fr N] [-ar N]
//	                                               set a chapter's display names (audited)
//	server set-order -subject S [-chapter C] -order N
//	                                               set a subject's or chapter's display order (audited)
//	server show-catalogue [-subject S]             subjects and chapters with names and order
//	server verify-restore [-strict]                check a restored database and media directory
//	server probe [-path P]                         container health check: GET P on LEARNOO_ADDR
//
// Operator commands act as permissions.OperatorAs(permissions.LocalOperatorLabel()):
// anyone with write access to the database is trusted, and the audit records
// the self-asserted <os user>@<hostname> label. Configuration comes from
// environment variables; see docs/DEVELOPMENT.md.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/hadden-me/learnoo/internal/accounts"
	"github.com/hadden-me/learnoo/internal/auth"
	"github.com/hadden-me/learnoo/internal/backupcheck"
	"github.com/hadden-me/learnoo/internal/catalogue"
	"github.com/hadden-me/learnoo/internal/clientip"
	"github.com/hadden-me/learnoo/internal/database"
	"github.com/hadden-me/learnoo/internal/invitations"
	"github.com/hadden-me/learnoo/internal/media"
	"github.com/hadden-me/learnoo/internal/permissions"
	"github.com/hadden-me/learnoo/internal/server"
	"github.com/hadden-me/learnoo/internal/users"
	"github.com/hadden-me/learnoo/migrations"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const defaultAddr = "127.0.0.1:8090"

// MinPasswordLen is enforced when creating accounts.
const MinPasswordLen = 12

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if err := run(os.Args[1:], os.Stdin, os.Stdout, log); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(args []string, stdin io.Reader, stdout io.Writer, log *slog.Logger) error {
	cmd := "serve"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	switch cmd {
	case "serve":
		return serve(ctx, args, log)
	case "migrate":
		return migrate(ctx, args, stdout)
	case "create-user":
		return createUser(ctx, args, stdin, stdout)
	case "bootstrap-admin":
		return bootstrapAdmin(ctx, args, stdin, stdout)
	case "grant":
		return changePermission(ctx, "grant", args, stdout)
	case "revoke":
		return changePermission(ctx, "revoke", args, stdout)
	case "disable-user":
		return changeStatus(ctx, "disable", args, stdout)
	case "enable-user":
		return changeStatus(ctx, "enable", args, stdout)
	case "show-user":
		return showUser(ctx, args, stdout)
	case "invite-student":
		return inviteStudent(ctx, args, stdout)
	case "add-subject":
		return addSubject(ctx, args, stdout)
	case "add-chapter":
		return addChapter(ctx, args, stdout)
	case "set-subject-name":
		return setSubjectName(ctx, args, stdout)
	case "set-chapter-name":
		return setChapterName(ctx, args, stdout)
	case "set-order":
		return setOrder(ctx, args, stdout)
	case "show-catalogue":
		return showCatalogue(ctx, args, stdout)
	case "verify-restore":
		return verifyRestore(ctx, args, stdout)
	case "probe":
		return probe(ctx, args)
	default:
		return fmt.Errorf("unknown command %q (want serve, migrate, create-user, bootstrap-admin, grant, revoke, disable-user, enable-user, show-user, invite-student, add-subject, add-chapter, set-subject-name, set-chapter-name, set-order, show-catalogue, verify-restore or probe)", cmd)
	}
}

func databaseURL() (string, error) {
	url := os.Getenv("LEARNOO_DATABASE_URL")
	if url == "" {
		return "", errors.New("LEARNOO_DATABASE_URL is not set")
	}
	return url, nil
}

// openDatabase connects with LEARNOO_DATABASE_URL. If
// LEARNOO_DATABASE_PASSWORD_FILE is set, the password is read from that file
// (a container secret) instead of the URL, so it never appears in the
// environment or the process list. One trailing newline is ignored.
func openDatabase(ctx context.Context) (*pgxpool.Pool, error) {
	url, err := databaseURL()
	if err != nil {
		return nil, err
	}
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		// Never echo the URL: it may contain a password.
		return nil, errors.New("parse database URL: invalid format")
	}
	if file := os.Getenv("LEARNOO_DATABASE_PASSWORD_FILE"); file != "" {
		b, err := os.ReadFile(file)
		if err != nil {
			return nil, fmt.Errorf("LEARNOO_DATABASE_PASSWORD_FILE: %w", err)
		}
		pw := strings.TrimSuffix(strings.TrimSuffix(string(b), "\n"), "\r")
		if pw == "" {
			return nil, errors.New("LEARNOO_DATABASE_PASSWORD_FILE is empty")
		}
		cfg.ConnConfig.Password = pw
	}
	return database.OpenConfig(ctx, cfg)
}

func serve(ctx context.Context, args []string, log *slog.Logger) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	applyMemoryLimit(log, os.ReadFile)
	addr := os.Getenv("LEARNOO_ADDR")
	if addr == "" {
		addr = defaultAddr
	}
	resolver, err := clientip.FromEnv(os.Getenv("LEARNOO_TRUSTED_PROXIES"), os.Getenv("LEARNOO_CLIENT_ADDRESS_HEADER"))
	if err != nil {
		return err
	}
	if os.Getenv("LEARNOO_ALLOW_NON_LOOPBACK") == "1" {
		// Container deployments listen on the container interface; Compose
		// publishes the port to a host address for the reverse proxy.
		log.Warn("listening beyond loopback (LEARNOO_ALLOW_NON_LOOPBACK=1)", "addr", addr)
		if resolver.Trusted() == 0 {
			log.Warn("no trusted proxies (LEARNOO_TRUSTED_PROXIES): behind a reverse proxy every client shares the proxy's address in the throttles")
		}
	} else if err := requireLoopback(addr); err != nil {
		return err
	}
	if raw := os.Getenv("LEARNOO_PUBLIC_BASE_URL"); raw != "" {
		// Not used by serve, but a bad value should stop a deployment early
		// rather than when the first invitation is issued.
		if _, err := publicBaseURL(raw); err != nil {
			return err
		}
	}
	if resolver.Trusted() > 0 {
		log.Info("client addresses from trusted proxies", "ranges", resolver.Trusted(), "header", string(resolver.Header()))
	}
	mediaDir, err := mediaDirectory()
	if err != nil {
		return err
	}
	pool, err := openDatabase(ctx)
	if err != nil {
		return err
	}
	defer pool.Close()
	if err := database.Check(ctx, pool, migrations.FS); err != nil {
		return fmt.Errorf("%w (run: server migrate)", err)
	}
	svc, err := auth.New(pool, auth.DefaultConfig, nil)
	if err != nil {
		return err
	}
	mediaStore, err := media.OpenStore(mediaDir)
	if err != nil {
		return err
	}
	defer mediaStore.Close()
	var opts []server.Option
	studio, err := openStudio()
	if err != nil {
		return err
	}
	if studio != nil {
		defer studio.Close()
		opts = append(opts, server.WithStudio(studio.FS()))
		log.Info("serving the Studio", "dir", studio.Name())
	} else {
		log.Warn("LEARNOO_STUDIO_DIR is not set: /studio is not served")
	}
	srv := &http.Server{
		Addr:              addr,
		Handler:           clientip.Middleware(resolver, server.New(pool, svc, mediaStore, log, opts...)),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}
	errc := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", "http://"+addr)
		errc <- srv.ListenAndServe()
	}()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}

// mediaDirectory returns LEARNOO_MEDIA_DIR. There is no default, so uploads
// are never written into the working directory or the repository by accident.
func mediaDirectory() (string, error) {
	dir := os.Getenv("LEARNOO_MEDIA_DIR")
	if dir == "" {
		return "", errors.New("LEARNOO_MEDIA_DIR is not set (absolute path of the persistent media directory)")
	}
	if !filepath.IsAbs(dir) {
		return "", fmt.Errorf("LEARNOO_MEDIA_DIR %q must be an absolute path", dir)
	}
	return filepath.Clean(dir), nil
}

// openStudio opens LEARNOO_STUDIO_DIR, the built Studio bundle
// (web/studio/dist; /opt/learnoo/studio in the image), as an os.Root so
// served paths cannot leave it. Unset means the Studio is not served. A set
// value must be an absolute directory containing index.html.
func openStudio() (*os.Root, error) {
	dir := os.Getenv("LEARNOO_STUDIO_DIR")
	if dir == "" {
		return nil, nil
	}
	if !filepath.IsAbs(dir) {
		return nil, fmt.Errorf("LEARNOO_STUDIO_DIR %q must be an absolute path", dir)
	}
	root, err := os.OpenRoot(filepath.Clean(dir))
	if err != nil {
		return nil, fmt.Errorf("LEARNOO_STUDIO_DIR: %w", err)
	}
	if fi, err := root.Stat("index.html"); err != nil || !fi.Mode().IsRegular() {
		root.Close()
		return nil, fmt.Errorf("LEARNOO_STUDIO_DIR %q has no index.html (build it: cd web/studio && npm run build)", dir)
	}
	return root, nil
}

// requireLoopback keeps the server off public interfaces by default, also in
// the container image. The production Compose file opts out with
// LEARNOO_ALLOW_NON_LOOPBACK=1 because the server must listen on the
// container network, and publishes the port only to the address the reverse
// proxy uses. Trusted proxy headers are still not
// read, so throttling sees the proxy's address (docs/DEPLOYMENT.md).
func requireLoopback(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("LEARNOO_ADDR: %w", err)
	}
	if host == "localhost" {
		return nil
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("LEARNOO_ADDR %q is not a loopback address; set LEARNOO_ALLOW_NON_LOOPBACK=1 only inside a container behind the reverse proxy (docs/DEPLOYMENT.md)", addr)
	}
	return nil
}

func migrate(ctx context.Context, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("migrate", flag.ContinueOnError)
	check := fs.Bool("check", false, "verify every migration is applied unchanged; apply nothing")
	if err := fs.Parse(args); err != nil {
		return err
	}
	pool, err := openDatabase(ctx)
	if err != nil {
		return err
	}
	defer pool.Close()
	if *check {
		if err := database.Check(ctx, pool, migrations.FS); err != nil {
			return err
		}
		fmt.Fprintln(stdout, "migrations up to date")
		return nil
	}
	applied, err := database.Migrate(ctx, pool, migrations.FS)
	for _, name := range applied {
		fmt.Fprintln(stdout, "applied", name)
	}
	if err != nil {
		return err
	}
	if len(applied) == 0 {
		fmt.Fprintln(stdout, "migrations up to date")
	}
	return nil
}

// createUser makes an account with no permissions. It is a local/operator
// tool, not public registration.
func createUser(ctx context.Context, args []string, stdin io.Reader, stdout io.Writer) error {
	fs := flag.NewFlagSet("create-user", flag.ContinueOnError)
	email := fs.String("email", "", "account email (required)")
	name := fs.String("name", "", "display name (required)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *email == "" || *name == "" {
		return errors.New("create-user needs -email and -name; the password is read from stdin")
	}
	password, err := readPassword(stdin)
	if err != nil {
		return err
	}
	pool, err := openMigrated(ctx)
	if err != nil {
		return err
	}
	defer pool.Close()
	hash, err := auth.HashPassword(password, auth.DefaultArgon2)
	if err != nil {
		return err
	}
	u, err := users.Create(ctx, pool, *email, *name, hash)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "created user %d (%s)\n", u.ID, u.Email)
	return nil
}

// bootstrapAdmin creates the first administrator: the account and its
// Administrator defaults in one transaction, refused while any active
// administrator exists.
func bootstrapAdmin(ctx context.Context, args []string, stdin io.Reader, stdout io.Writer) error {
	fs := flag.NewFlagSet("bootstrap-admin", flag.ContinueOnError)
	email := fs.String("email", "", "administrator email (required)")
	name := fs.String("name", "", "display name (required)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *email == "" || *name == "" || fs.NArg() > 0 {
		return errors.New("bootstrap-admin needs -email and -name; the password is read from stdin")
	}
	password, err := readPassword(stdin)
	if err != nil {
		return err
	}
	pool, err := openMigrated(ctx)
	if err != nil {
		return err
	}
	defer pool.Close()
	hash, err := auth.HashPassword(password, auth.DefaultArgon2)
	if err != nil {
		return err
	}
	actor, err := operatorActor()
	if err != nil {
		return err
	}
	var u users.User
	_, err = permissions.NewStore(pool).Bootstrap(ctx, actor, func(tx pgx.Tx) (int64, error) {
		var err error
		u, err = users.Create(ctx, tx, *email, *name, hash)
		return u.ID, err
	})
	if errors.Is(err, permissions.ErrAdministratorExists) {
		return fmt.Errorf("%w; bootstrap-admin only sets up the first one (use grant)", err)
	}
	if errors.Is(err, users.ErrEmailTaken) {
		return fmt.Errorf("%w; for an existing account use grant -permission administrator (and publisher, author)", err)
	}
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "created administrator %d (%s) with administrator, publisher, author\n", u.ID, u.Email)
	return nil
}

// changePermission grants or revokes one permission. Every argument is
// validated before any change, and the change itself is one transaction.
func changePermission(ctx context.Context, action string, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet(action, flag.ContinueOnError)
	email := fs.String("email", "", "target account email (required)")
	perm := fs.String("permission", "", "author, publisher, administrator or reviewer (required)")
	subject := fs.String("subject", "", "subject code; required for reviewer, refused otherwise")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *email == "" || *perm == "" || fs.NArg() > 0 {
		return fmt.Errorf("%s needs -email and -permission (and -subject for reviewer)", action)
	}
	p, err := permissions.Parse(*perm)
	if err != nil {
		return err
	}
	if p.Global() && *subject != "" {
		return fmt.Errorf("%s is a global permission: -subject is not allowed", p)
	}
	// Subject codes are stored lower-case; accept the same leniency as emails.
	*subject = strings.ToLower(strings.TrimSpace(*subject))
	if p == permissions.Reviewer && *subject == "" {
		return errors.New("reviewer is granted per subject: give -subject CODE")
	}
	pool, err := openMigrated(ctx)
	if err != nil {
		return err
	}
	defer pool.Close()
	u, err := lookupUser(ctx, pool, *email)
	if err != nil {
		return err
	}
	actor, err := operatorActor()
	if err != nil {
		return err
	}
	store := permissions.NewStore(pool)
	what := string(p)
	var changed bool
	if p == permissions.Reviewer {
		id, err := store.SubjectID(ctx, *subject)
		if errors.Is(err, permissions.ErrUnknownSubject) {
			return fmt.Errorf("unknown subject code %q", *subject)
		}
		if err != nil {
			return err
		}
		what = fmt.Sprintf("reviewer for subject %s", *subject)
		if action == "grant" {
			changed, err = store.GrantReviewer(ctx, actor, u.ID, id)
		} else {
			changed, err = store.RevokeReviewer(ctx, actor, u.ID, id)
		}
		if err != nil {
			return err
		}
	} else {
		if action == "grant" {
			changed, err = store.Grant(ctx, actor, u.ID, p)
		} else {
			changed, err = store.Revoke(ctx, actor, u.ID, p)
		}
		if errors.Is(err, permissions.ErrLastAdministrator) {
			return fmt.Errorf("%w: grant administrator to another active account first", err)
		}
		if err != nil {
			return err
		}
	}
	switch {
	case changed && action == "grant":
		fmt.Fprintf(stdout, "granted %s to %s\n", what, u.Email)
	case changed:
		fmt.Fprintf(stdout, "revoked %s from %s\n", what, u.Email)
	case action == "grant":
		fmt.Fprintf(stdout, "no change: %s already holds %s\n", u.Email, what)
	default:
		fmt.Fprintf(stdout, "no change: %s does not hold %s\n", u.Email, what)
	}
	return nil
}

// changeStatus disables or enables an account.
func changeStatus(ctx context.Context, action string, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet(action+"-user", flag.ContinueOnError)
	email := fs.String("email", "", "account email (required)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *email == "" || fs.NArg() > 0 {
		return fmt.Errorf("%s-user needs -email", action)
	}
	pool, err := openMigrated(ctx)
	if err != nil {
		return err
	}
	defer pool.Close()
	u, err := lookupUser(ctx, pool, *email)
	if err != nil {
		return err
	}
	actor, err := operatorActor()
	if err != nil {
		return err
	}
	store := accounts.NewStore(pool)
	var changed bool
	if action == "disable" {
		changed, err = store.Disable(ctx, actor, u.ID)
	} else {
		changed, err = store.Enable(ctx, actor, u.ID)
	}
	if errors.Is(err, permissions.ErrLastAdministrator) {
		return fmt.Errorf("%w: grant administrator to another active account first", err)
	}
	if err != nil {
		return err
	}
	switch {
	case changed && action == "disable":
		fmt.Fprintf(stdout, "disabled %s and revoked its sessions\n", u.Email)
	case changed:
		fmt.Fprintf(stdout, "enabled %s\n", u.Email)
	case action == "disable":
		fmt.Fprintf(stdout, "no change: %s is already disabled\n", u.Email)
	default:
		fmt.Fprintf(stdout, "no change: %s is already enabled\n", u.Email)
	}
	return nil
}

// showUser prints a user's status, stored permissions and their history.
func showUser(ctx context.Context, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("show-user", flag.ContinueOnError)
	email := fs.String("email", "", "account email (required)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *email == "" || fs.NArg() > 0 {
		return errors.New("show-user needs -email")
	}
	pool, err := openMigrated(ctx)
	if err != nil {
		return err
	}
	defer pool.Close()
	u, err := lookupUser(ctx, pool, *email)
	if err != nil {
		return err
	}
	store := permissions.NewStore(pool)
	global, subjects, err := store.Grants(ctx, u.ID)
	if err != nil {
		return err
	}
	audit, err := store.Audit(ctx, u.ID)
	if err != nil {
		return err
	}
	statusAudit, err := accounts.NewStore(pool).Audit(ctx, u.ID)
	if err != nil {
		return err
	}
	status := "active"
	if u.Disabled {
		status = "disabled (holds no effective permissions)"
	}
	fmt.Fprintf(stdout, "user %d %s (%s)\n", u.ID, u.Email, u.DisplayName)
	fmt.Fprintf(stdout, "status: %s\n", status)
	names := make([]string, len(global))
	for i, p := range global {
		names[i] = string(p)
	}
	fmt.Fprintf(stdout, "global permissions: %s\n", listOrNone(names))
	fmt.Fprintf(stdout, "reviewer subjects: %s\n", listOrNone(subjects))
	fmt.Fprintln(stdout, "permission audit:")
	if len(audit) == 0 {
		fmt.Fprintln(stdout, "  (none)")
	}
	for _, a := range audit {
		what := string(a.Permission)
		if a.SubjectID != nil {
			what += " subject=" + a.SubjectCode
		}
		fmt.Fprintf(stdout, "  %s %s %s by %s\n", a.OccurredAt.UTC().Format(time.RFC3339), a.Action, what, labelledActorName(a.ActorID, a.OperatorLabel))
	}
	fmt.Fprintln(stdout, "account status audit:")
	if len(statusAudit) == 0 {
		fmt.Fprintln(stdout, "  (none)")
	}
	for _, a := range statusAudit {
		fmt.Fprintf(stdout, "  %s %s by %s\n", a.OccurredAt.UTC().Format(time.RFC3339), a.Action, labelledActorName(a.ActorID, a.OperatorLabel))
	}
	return nil
}

// operatorActor is the actor of every operator command: an operator
// audited with the local <os user>@<hostname> label.
func operatorActor() (permissions.Actor, error) {
	return permissions.OperatorAs(permissions.LocalOperatorLabel())
}

// labelledActorName names an audit actor: "operator <label>" for operator
// tools, "user <id>" for signed-in users.
func labelledActorName(id *int64, label string) string {
	if id == nil && label != "" {
		return "operator " + label
	}
	return actorName(id)
}

func actorName(id *int64) string {
	if id == nil {
		return "operator"
	}
	return fmt.Sprintf("user %d", *id)
}

func listOrNone(items []string) string {
	if len(items) == 0 {
		return "(none)"
	}
	return strings.Join(items, ", ")
}

func lookupUser(ctx context.Context, pool *pgxpool.Pool, email string) (users.User, error) {
	u, err := users.ByEmail(ctx, pool, email)
	if errors.Is(err, users.ErrNotFound) {
		return users.User{}, fmt.Errorf("no account with email %q", email)
	}
	return u, err
}

// readPassword reads one line from stdin and enforces the length rule.
func readPassword(stdin io.Reader) (string, error) {
	line, err := bufio.NewReader(io.LimitReader(stdin, auth.MaxPasswordBytes+2)).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", fmt.Errorf("read password: %w", err)
	}
	password := strings.TrimRight(line, "\r\n")
	if n := utf8.RuneCountInString(password); n < MinPasswordLen {
		return "", fmt.Errorf("password must be at least %d characters", MinPasswordLen)
	}
	if len(password) > auth.MaxPasswordBytes {
		return "", fmt.Errorf("password must be at most %d bytes", auth.MaxPasswordBytes)
	}
	return password, nil
}

// openMigrated opens the database and refuses to continue while migrations
// are pending or the history differs.
func openMigrated(ctx context.Context) (*pgxpool.Pool, error) {
	pool, err := openDatabase(ctx)
	if err != nil {
		return nil, err
	}
	if err := database.Check(ctx, pool, migrations.FS); err != nil {
		pool.Close()
		return nil, fmt.Errorf("%w (run: server migrate)", err)
	}
	return pool, nil
}

// inviteStudent issues a single-use Student invitation (no permissions) and
// prints its link once. Only the token's hash is stored, so the link cannot
// be shown again. The invitation is recorded with the local operator label.
// Nothing secret is read from the command line.
func inviteStudent(ctx context.Context, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("invite-student", flag.ContinueOnError)
	email := fs.String("email", "", "invited student's email (required)")
	ttl := fs.Duration("ttl", invitations.DefaultTTL, "how long the link stays valid (1h to 720h)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *email == "" || fs.NArg() > 0 {
		return errors.New("invite-student needs -email (and optionally -ttl); it takes no other arguments")
	}
	base, err := invitationBase()
	if err != nil {
		return err
	}
	pool, err := openMigrated(ctx)
	if err != nil {
		return err
	}
	defer pool.Close()
	token, inv, err := invitations.NewStore(pool, nil).Issue(ctx, *email, permissions.LocalOperatorLabel(), *ttl)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "invitation %d for %s, valid until %s, issued by %s\n",
		inv.ID, inv.Email, inv.ExpiresAt.UTC().Format(time.RFC3339), inv.OperatorLabel)
	fmt.Fprintln(stdout, "one-time link (shown only now; send it privately, do not paste it into shared logs or issues):")
	fmt.Fprintf(stdout, "%s/invite/%s\n", base, token)
	return nil
}

// addSubject adds a catalogue subject identified by its code. Names,
// ordering and the pilot catalogue are LRN-014; nothing is seeded. The
// addition is audited with the local operator label.
func addSubject(ctx context.Context, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("add-subject", flag.ContinueOnError)
	code := fs.String("code", "", "subject code: lower-case letters, digits and hyphens (required)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *code == "" || fs.NArg() > 0 {
		return errors.New("add-subject needs -code and takes no other arguments")
	}
	if !catalogue.ValidCode(*code) {
		return fmt.Errorf("subject code %q: %w", *code, catalogue.ErrInvalidCode)
	}
	actor, err := operatorActor()
	if err != nil {
		return err
	}
	pool, err := openMigrated(ctx)
	if err != nil {
		return err
	}
	defer pool.Close()
	sj, created, err := catalogue.NewStore(pool).AddSubject(ctx, actor, *code)
	if err != nil {
		return err
	}
	if !created {
		fmt.Fprintf(stdout, "no change: subject %s already exists (id %d)\n", sj.Code, sj.ID)
		return nil
	}
	fmt.Fprintf(stdout, "added subject %s (id %d)\n", sj.Code, sj.ID)
	return nil
}

// addChapter adds a chapter, identified by its code, to an existing subject.
// The addition is audited with the local operator label.
func addChapter(ctx context.Context, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("add-chapter", flag.ContinueOnError)
	subject := fs.String("subject", "", "code of an existing subject (required)")
	code := fs.String("code", "", "chapter code: lower-case letters, digits and hyphens (required)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *subject == "" || *code == "" || fs.NArg() > 0 {
		return errors.New("add-chapter needs -subject and -code and takes no other arguments")
	}
	if !catalogue.ValidCode(*subject) {
		return fmt.Errorf("subject code %q: %w", *subject, catalogue.ErrInvalidCode)
	}
	if !catalogue.ValidCode(*code) {
		return fmt.Errorf("chapter code %q: %w", *code, catalogue.ErrInvalidCode)
	}
	actor, err := operatorActor()
	if err != nil {
		return err
	}
	pool, err := openMigrated(ctx)
	if err != nil {
		return err
	}
	defer pool.Close()
	sj, ch, created, err := catalogue.NewStore(pool).AddChapter(ctx, actor, *subject, *code)
	if errors.Is(err, catalogue.ErrNotFound) {
		return fmt.Errorf("unknown subject code %q (add it with add-subject)", *subject)
	}
	if err != nil {
		return err
	}
	if !created {
		fmt.Fprintf(stdout, "no change: chapter %s already exists in subject %s (id %d)\n", ch.Code, sj.Code, ch.ID)
		return nil
	}
	fmt.Fprintf(stdout, "added chapter %s to subject %s (id %d)\n", ch.Code, sj.Code, ch.ID)
	return nil
}

// nameFlags registers -fr and -ar on fs. After parsing, the returned
// function gives the names that were set on the command line (nil when a
// flag was not given), so an explicit -fr "" is refused rather than ignored.
func nameFlags(fs *flag.FlagSet) func() (fr, ar *string) {
	frValue := fs.String("fr", "", "French display name, 1 to 200 characters")
	arValue := fs.String("ar", "", "Arabic display name, 1 to 200 characters")
	return func() (fr, ar *string) {
		fs.Visit(func(f *flag.Flag) {
			switch f.Name {
			case "fr":
				fr = frValue
			case "ar":
				ar = arValue
			}
		})
		return fr, ar
	}
}

// checkNames validates the names given on the command line before the
// database is touched.
func checkNames(fr, ar *string) error {
	for _, n := range []struct {
		flag  string
		value *string
	}{{"-fr", fr}, {"-ar", ar}} {
		if n.value == nil {
			continue
		}
		if _, err := catalogue.NormalizeName(*n.value); err != nil {
			return fmt.Errorf("%s %q: %w", n.flag, *n.value, err)
		}
	}
	return nil
}

// displayNames formats an entry's names for operator output.
func displayNames(n catalogue.Names) string {
	q := func(p *string) string {
		if p == nil {
			return "-"
		}
		return strconv.Quote(*p)
	}
	return "fr " + q(n.Fr) + ", ar " + q(n.Ar)
}

// setSubjectName sets a subject's French and/or Arabic display name. Names
// are display metadata, not identity: the code stays. The change is audited
// with the old and new names and the local operator label.
func setSubjectName(ctx context.Context, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("set-subject-name", flag.ContinueOnError)
	code := fs.String("code", "", "code of an existing subject (required)")
	names := nameFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	fr, ar := names()
	if *code == "" || (fr == nil && ar == nil) || fs.NArg() > 0 {
		return errors.New("set-subject-name needs -code and at least one of -fr and -ar, and takes no other arguments")
	}
	if !catalogue.ValidCode(*code) {
		return fmt.Errorf("subject code %q: %w", *code, catalogue.ErrInvalidCode)
	}
	if err := checkNames(fr, ar); err != nil {
		return err
	}
	actor, err := operatorActor()
	if err != nil {
		return err
	}
	pool, err := openMigrated(ctx)
	if err != nil {
		return err
	}
	defer pool.Close()
	sj, changed, err := catalogue.NewStore(pool).SetSubjectNames(ctx, actor, *code, fr, ar)
	if errors.Is(err, catalogue.ErrNotFound) {
		return fmt.Errorf("unknown subject code %q", *code)
	}
	if err != nil {
		return err
	}
	if !changed {
		fmt.Fprintf(stdout, "no change: subject %s already has these names (%s)\n", sj.Code, displayNames(sj.Names))
		return nil
	}
	fmt.Fprintf(stdout, "renamed subject %s: %s\n", sj.Code, displayNames(sj.Names))
	return nil
}

// setChapterName sets a chapter's French and/or Arabic display name. A
// chapter's name is shown with every lesson placed in it, published ones
// included, but is not part of any reviewed snapshot (revisions store the
// chapter's ID). The change is audited like setSubjectName.
func setChapterName(ctx context.Context, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("set-chapter-name", flag.ContinueOnError)
	subject := fs.String("subject", "", "code of an existing subject (required)")
	code := fs.String("code", "", "code of an existing chapter of that subject (required)")
	names := nameFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	fr, ar := names()
	if *subject == "" || *code == "" || (fr == nil && ar == nil) || fs.NArg() > 0 {
		return errors.New("set-chapter-name needs -subject, -code and at least one of -fr and -ar, and takes no other arguments")
	}
	if !catalogue.ValidCode(*subject) {
		return fmt.Errorf("subject code %q: %w", *subject, catalogue.ErrInvalidCode)
	}
	if !catalogue.ValidCode(*code) {
		return fmt.Errorf("chapter code %q: %w", *code, catalogue.ErrInvalidCode)
	}
	if err := checkNames(fr, ar); err != nil {
		return err
	}
	actor, err := operatorActor()
	if err != nil {
		return err
	}
	pool, err := openMigrated(ctx)
	if err != nil {
		return err
	}
	defer pool.Close()
	sj, ch, changed, err := catalogue.NewStore(pool).SetChapterNames(ctx, actor, *subject, *code, fr, ar)
	switch {
	case errors.Is(err, catalogue.ErrNotFound):
		return fmt.Errorf("unknown subject code %q", *subject)
	case errors.Is(err, catalogue.ErrChapterNotFound):
		return fmt.Errorf("unknown chapter code %q in subject %s", *code, *subject)
	case err != nil:
		return err
	}
	if !changed {
		fmt.Fprintf(stdout, "no change: chapter %s of subject %s already has these names (%s)\n", ch.Code, sj.Code, displayNames(ch.Names))
		return nil
	}
	fmt.Fprintf(stdout, "renamed chapter %s of subject %s: %s\n", ch.Code, sj.Code, displayNames(ch.Names))
	return nil
}

// setOrder sets the display order (0 to 9999, lists sort by order, then
// code) of a subject, or of a chapter with -chapter. The change is audited
// with the old and new order and the local operator label.
func setOrder(ctx context.Context, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("set-order", flag.ContinueOnError)
	subject := fs.String("subject", "", "code of an existing subject (required)")
	chapter := fs.String("chapter", "", "code of a chapter of that subject; without it the subject is reordered")
	order := fs.Int("order", -1, "display order, 0 to 9999 (required)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	given := false
	fs.Visit(func(f *flag.Flag) { given = given || f.Name == "order" })
	if *subject == "" || !given || fs.NArg() > 0 {
		return errors.New("set-order needs -subject and -order (and optionally -chapter), and takes no other arguments")
	}
	if !catalogue.ValidCode(*subject) {
		return fmt.Errorf("subject code %q: %w", *subject, catalogue.ErrInvalidCode)
	}
	if *chapter != "" && !catalogue.ValidCode(*chapter) {
		return fmt.Errorf("chapter code %q: %w", *chapter, catalogue.ErrInvalidCode)
	}
	if !catalogue.ValidSortOrder(*order) {
		return fmt.Errorf("order %d: %w", *order, catalogue.ErrInvalidSortOrder)
	}
	actor, err := operatorActor()
	if err != nil {
		return err
	}
	pool, err := openMigrated(ctx)
	if err != nil {
		return err
	}
	defer pool.Close()
	store := catalogue.NewStore(pool)
	if *chapter == "" {
		sj, changed, err := store.SetSubjectOrder(ctx, actor, *subject, *order)
		if errors.Is(err, catalogue.ErrNotFound) {
			return fmt.Errorf("unknown subject code %q", *subject)
		}
		if err != nil {
			return err
		}
		if !changed {
			fmt.Fprintf(stdout, "no change: subject %s already has order %d\n", sj.Code, sj.SortOrder)
			return nil
		}
		fmt.Fprintf(stdout, "subject %s now has order %d\n", sj.Code, sj.SortOrder)
		return nil
	}
	sj, ch, changed, err := store.SetChapterOrder(ctx, actor, *subject, *chapter, *order)
	switch {
	case errors.Is(err, catalogue.ErrNotFound):
		return fmt.Errorf("unknown subject code %q", *subject)
	case errors.Is(err, catalogue.ErrChapterNotFound):
		return fmt.Errorf("unknown chapter code %q in subject %s", *chapter, *subject)
	case err != nil:
		return err
	}
	if !changed {
		fmt.Fprintf(stdout, "no change: chapter %s of subject %s already has order %d\n", ch.Code, sj.Code, ch.SortOrder)
		return nil
	}
	fmt.Fprintf(stdout, "chapter %s of subject %s now has order %d\n", ch.Code, sj.Code, ch.SortOrder)
	return nil
}

// showCatalogue prints the subjects (or one subject with -subject) and
// their chapters in display order, with IDs, orders and names ("-" when
// unset). It reads only.
func showCatalogue(ctx context.Context, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("show-catalogue", flag.ContinueOnError)
	only := fs.String("subject", "", "show only the subject with this code")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return errors.New("show-catalogue takes only -subject")
	}
	if *only != "" && !catalogue.ValidCode(*only) {
		return fmt.Errorf("subject code %q: %w", *only, catalogue.ErrInvalidCode)
	}
	pool, err := openMigrated(ctx)
	if err != nil {
		return err
	}
	defer pool.Close()
	store := catalogue.NewStore(pool)
	codes := []string{*only}
	if *only == "" {
		subjects, err := store.Subjects(ctx)
		if err != nil {
			return err
		}
		if len(subjects) == 0 {
			fmt.Fprintln(stdout, "the catalogue is empty (add subjects with add-subject)")
			return nil
		}
		codes = codes[:0]
		for _, s := range subjects {
			codes = append(codes, s.Code)
		}
	}
	for _, code := range codes {
		sj, chapters, err := store.Chapters(ctx, code)
		if errors.Is(err, catalogue.ErrNotFound) {
			return fmt.Errorf("unknown subject code %q", code)
		}
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "subject %s (id %d, order %d): %s\n", sj.Code, sj.ID, sj.SortOrder, displayNames(sj.Names))
		for _, c := range chapters {
			fmt.Fprintf(stdout, "  chapter %s (id %d, order %d): %s\n", c.Code, c.ID, c.SortOrder, displayNames(c.Names))
		}
	}
	return nil
}

// invitationBase is the scheme and host invitation links start with:
// LEARNOO_PUBLIC_BASE_URL when set, otherwise http:// and the loopback
// LEARNOO_ADDR (local development).
func invitationBase() (string, error) {
	if raw := os.Getenv("LEARNOO_PUBLIC_BASE_URL"); raw != "" {
		return publicBaseURL(raw)
	}
	addr := os.Getenv("LEARNOO_ADDR")
	if addr == "" {
		addr = defaultAddr
	}
	if err := requireLoopback(addr); err != nil {
		return "", fmt.Errorf("%w; set LEARNOO_PUBLIC_BASE_URL to print public links", err)
	}
	return "http://" + addr, nil
}

// publicBaseURL validates LEARNOO_PUBLIC_BASE_URL: an absolute https URL of
// the site root (no credentials, path, query or fragment), returned without
// a trailing slash. The value is printed in links, so it must hold no secret.
func publicBaseURL(raw string) (string, error) {
	bad := func(why string) (string, error) {
		return "", fmt.Errorf("LEARNOO_PUBLIC_BASE_URL must be an https URL of the site root (%s)", why)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return bad("unparsable")
	}
	switch {
	case u.Scheme != "https":
		return bad("scheme is not https")
	case u.Opaque != "" || u.Host == "" || u.Hostname() == "":
		return bad("no host")
	case u.User != nil:
		return bad("credentials are not allowed")
	case u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(raw, "#"):
		return bad("query and fragment are not allowed")
	case u.Path != "" && u.Path != "/":
		return bad("a path is not supported")
	case strings.ContainsAny(raw, " \t\r\n\\"):
		return bad("invalid characters")
	case strings.HasSuffix(u.Host, ":"):
		return bad("empty port")
	}
	if p := u.Port(); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 || n > 65535 || p[0] == '0' {
			return bad("port must be 1 to 65535")
		}
	}
	if !validHostname(u.Hostname()) {
		return bad("host must be an ASCII (IDNA) name or an IP address without zone")
	}
	return "https://" + strings.ToLower(u.Host), nil
}

// validHostname accepts an IP address without zone, or a DNS name of ASCII
// letters, digits and hyphens (internationalized names in their xn-- IDNA
// form), labels of 1 to 63 characters, 253 at most in all.
func validHostname(h string) bool {
	if a, err := netip.ParseAddr(h); err == nil {
		return a.Zone() == ""
	}
	if h == "" || len(h) > 253 || strings.HasSuffix(h, ".") {
		return false
	}
	for _, label := range strings.Split(h, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for i := 0; i < len(label); i++ {
			c := label[i]
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-') {
				return false
			}
		}
	}
	return true
}

// verifyRestore checks a restored database (LEARNOO_DATABASE_URL) and media
// directory (LEARNOO_MEDIA_DIR): migrations current, every media asset file
// present with its recorded size and SHA-256, published pointers consistent.
// Unexpected files are listed, never deleted; -strict makes them fail.
func verifyRestore(ctx context.Context, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("verify-restore", flag.ContinueOnError)
	strict := fs.Bool("strict", false, "also fail on unexpected or temporary files in the media directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return errors.New("verify-restore takes no arguments")
	}
	mediaDir, err := mediaDirectory()
	if err != nil {
		return err
	}
	if fi, err := os.Stat(mediaDir); err != nil || !fi.IsDir() {
		return fmt.Errorf("LEARNOO_MEDIA_DIR %q is not an existing directory", mediaDir)
	}
	pool, err := openDatabase(ctx)
	if err != nil {
		return err
	}
	defer pool.Close()
	report, err := backupcheck.Verify(ctx, pool, mediaDir, migrations.FS)
	if err != nil {
		return err
	}
	report.WriteText(stdout, *strict)
	if report.Failed(*strict) {
		return errors.New("restore verification failed")
	}
	return nil
}

// probe is the container health check (the runtime image has no shell or
// HTTP client): it requests path on the local listener and succeeds only on
// 200. A wildcard listen address is probed on loopback.
func probe(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("probe", flag.ContinueOnError)
	p := fs.String("path", "/readyz", "path to request (/healthz or /readyz)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *p != "/healthz" && *p != "/readyz" {
		return errors.New("probe -path must be /healthz or /readyz")
	}
	addr := os.Getenv("LEARNOO_ADDR")
	if addr == "" {
		addr = defaultAddr
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("LEARNOO_ADDR: %w", err)
	}
	if ip := net.ParseIP(host); host == "" || (ip != nil && ip.IsUnspecified()) {
		host = "127.0.0.1"
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+net.JoinHostPort(host, port)+*p, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: status %d", *p, resp.StatusCode)
	}
	return nil
}
