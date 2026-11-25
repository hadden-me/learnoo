// Command server runs the Learnoo application server and its local operator
// commands.
//
//	server serve                                   start the HTTP server (default)
//	server migrate [-check]                        apply or verify database migrations
//	server create-user -email E -name N            create an account; password on stdin
//	server bootstrap-admin -email E -name N        create the first administrator; password on stdin
//	server grant -email E -permission P [-subject CODE]
//	server revoke -email E -permission P [-subject CODE]
//	server show-user -email E                      status, permissions, reviewer subjects and audit
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
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/hadden-me/learnoo/internal/auth"
	"github.com/hadden-me/learnoo/internal/database"
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
	case "show-user":
		return showUser(ctx, args, stdout)
	default:
		return fmt.Errorf("unknown command %q (want serve, migrate, create-user, bootstrap-admin, grant, revoke or show-user)", cmd)
	}
}

func databaseURL() (string, error) {
	url := os.Getenv("LEARNOO_DATABASE_URL")
	if url == "" {
		return "", errors.New("LEARNOO_DATABASE_URL is not set")
	}
	return url, nil
}

func serve(ctx context.Context, args []string, log *slog.Logger) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	addr := os.Getenv("LEARNOO_ADDR")
	if addr == "" {
		addr = defaultAddr
	}
	if err := requireLoopback(addr); err != nil {
		return err
	}
	url, err := databaseURL()
	if err != nil {
		return err
	}
	pool, err := database.Open(ctx, url)
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
	srv := &http.Server{
		Addr:              addr,
		Handler:           server.New(pool, svc, log),
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

// requireLoopback keeps the server off public interfaces until deployment
// work defines TLS termination and the trusted proxy chain (client address
// for throttling depends on it).
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
		return fmt.Errorf("LEARNOO_ADDR %q is not a loopback address; public binding waits for the deployment work", addr)
	}
	return nil
}

func migrate(ctx context.Context, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("migrate", flag.ContinueOnError)
	check := fs.Bool("check", false, "verify every migration is applied unchanged; apply nothing")
	if err := fs.Parse(args); err != nil {
		return err
	}
	url, err := databaseURL()
	if err != nil {
		return err
	}
	pool, err := database.Open(ctx, url)
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
		fmt.Fprintf(stdout, "  %s %s %s by %s\n", a.OccurredAt.UTC().Format(time.RFC3339), a.Action, what, permissionActorName(a))
	}
	return nil
}

// operatorActor is the actor of every operator command: an operator
// audited with the local <os user>@<hostname> label.
func operatorActor() (permissions.Actor, error) {
	return permissions.OperatorAs(permissions.LocalOperatorLabel())
}

func permissionActorName(a permissions.AuditEntry) string {
	if a.ActorID == nil && a.OperatorLabel != "" {
		return "operator " + a.OperatorLabel
	}
	return actorName(a.ActorID)
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
	url, err := databaseURL()
	if err != nil {
		return nil, err
	}
	pool, err := database.Open(ctx, url)
	if err != nil {
		return nil, err
	}
	if err := database.Check(ctx, pool, migrations.FS); err != nil {
		pool.Close()
		return nil, fmt.Errorf("%w (run: server migrate)", err)
	}
	return pool, nil
}
