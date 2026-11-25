# Development

Two programs live in this repository and stay separate:

- `cmd/server`: the application server: PostgreSQL, migrations, accounts, sessions and login (LRN-005), server-side permission checks, operator commands (LRN-013), and the Studio API for lesson drafts and media (LRN-006). No review, publication or reader pages yet.
- `cmd/renderer`: the loopback-only renderer experiment (LRN-004), below. It has no accounts or database and does not share code paths with the server.

## Application server

### Versions

| Component | Version |
| --- | --- |
| Go | 1.25 or newer (`go.mod` declares 1.25.0; tested with 1.25.4) |
| PostgreSQL | 16 (Compose and CI use `postgres:16.13`; tested locally with Homebrew 16.13) |
| `github.com/jackc/pgx/v5` | v5.12.0 (driver and pool) |
| `golang.org/x/crypto` | v0.55.0 (Argon2id) |
| `golang.org/x/image` | v0.45.0 (WebP decoding only; newer releases require Go 1.26) |

Migrations use a small built-in runner (`internal/database`), not an external tool. See [DECISIONS.md](DECISIONS.md#application-server-foundation-lrn-005).

### Environment variables

No secrets have defaults or belong in the repository. Put local values in your shell or an ignored `.env` file.

| Variable | Used by | Meaning |
| --- | --- | --- |
| `LEARNOO_DATABASE_URL` | `serve`, `migrate` and every operator command | PostgreSQL URL, e.g. `postgres://learnoo@127.0.0.1:5432/learnoo?sslmode=disable` |
| `LEARNOO_MEDIA_DIR` | `serve` | Required. Absolute path of the persistent directory for uploaded images (created 0750 if missing). No default, so uploads never land in the repository or working directory. Must support hard links; back it up with the database |
| `LEARNOO_ADDR` | `serve` | Listen address. Default `127.0.0.1:8090`. Only loopback addresses are accepted until deployment work defines TLS and the proxy chain |
| `LEARNOO_TEST_DATABASE_URL` | `go test` | Disposable database for database-backed tests. Unset: those tests are skipped |
| `LEARNOO_REQUIRE_DB_TESTS` | `go test` | `1` makes database-backed tests fail instead of skip when the URL is missing (CI sets it) |

### Local PostgreSQL

Option A, Docker Compose (loopback-only, trust authentication, no stored password):

```sh
docker compose up -d postgres
export LEARNOO_DATABASE_URL='postgres://learnoo@127.0.0.1:5432/learnoo?sslmode=disable'
export LEARNOO_TEST_DATABASE_URL='postgres://learnoo@127.0.0.1:5432/learnoo_test?sslmode=disable'
```

`scripts/dev-db-init.sql` creates `learnoo_test` when the volume is first created. `docker compose down -v` deletes all local data.

Option B, an existing local PostgreSQL 16 (for example Homebrew `postgresql@16`): create a `learnoo` role and the `learnoo` and `learnoo_test` databases owned by it, then export the same variables with your own connection details. Tests need permission to create schemas in the test database.

### Migrate, create an account, start

```sh
go run ./cmd/server migrate           # apply pending migrations; safe to repeat
go run ./cmd/server migrate -check    # exit non-zero if anything is pending or was edited
read -rs PW && printf '%s\n' "$PW" | go run ./cmd/server create-user -email you@example.test -name 'Your Name'; unset PW
LEARNOO_MEDIA_DIR="$HOME/.local/share/learnoo/media" go run ./cmd/server serve   # http://127.0.0.1:8090
```

`serve` refuses to start while migrations are pending or the applied history differs from the files. `create-user` reads the password (12 to 1024 characters) from stdin so it never appears in shell history or process lists; it creates an account with no permissions (a Student) and is not public registration.

### Operator commands: first administrator, permissions and account status

These commands act as the trusted local operator: anyone who can run them against the database is trusted ([DECISIONS.md](DECISIONS.md#role-administration-cli-lrn-013)). Changes are audited with no actor and the self-asserted `<os user>@<hostname>` label of the machine running the command. There is no HTTP equivalent. Every command refuses to run while migrations are pending, validates its arguments before changing anything, and exits non-zero with a message on any error. Secrets are never command-line arguments: passwords are read from stdin.

Bootstrap the first administrator on a new database (account plus `administrator`, `publisher`, `author`, each audited, in one transaction). It is refused while any active administrator exists, including when two runs race:

```sh
read -rs PW && printf '%s\n' "$PW" | go run ./cmd/server bootstrap-admin -email admin@example.test -name 'Admin Name'; unset PW
```

`bootstrap-admin` always creates a new account; if the email is already registered it is refused and nothing changes. To make an existing account the first administrator, grant `administrator`, `publisher` and `author` to it with `grant` (below).

Grant or revoke a global permission (`author`, `publisher`, `administrator`) by email, or `reviewer` for one subject by its `subjects.code` (trimmed and lower-cased, like emails; subjects are created by LRN-014, none are seeded yet):

```sh
go run ./cmd/server grant  -email teacher@example.test -permission author
go run ./cmd/server grant  -email teacher@example.test -permission reviewer -subject math
go run ./cmd/server revoke -email teacher@example.test -permission reviewer -subject math
go run ./cmd/server revoke -email teacher@example.test -permission publisher
```

Each effective change writes one `permission_audit` row in the same transaction. Repeating a grant or revoke prints `no change: … already holds …` or `no change: … does not hold …`, exits 0 and writes no audit row. Rejected: an unknown email, permission or subject code, `reviewer` without `-subject`, and a global permission with `-subject`. Revoking `administrator` from the last active administrator (holds `administrator`, account not disabled) is refused; grant it to someone else first.

Disable or re-enable an account by email:

```sh
go run ./cmd/server disable-user -email teacher@example.test
go run ./cmd/server enable-user  -email teacher@example.test
```

Disabling sets `users.disabled_at` and deletes the account's sessions in one transaction; the account can no longer sign in (login answers `invalid_credentials`, as for a wrong password) and holds no effective permissions, but its grants stay stored and apply again once it is enabled. Enabling also deletes any sessions, so the account starts signed out. Each effective change writes one `account_status_audit` row with the operator label; repeating a command prints `no change: … is already disabled` or `… already enabled`, exits 0 and writes no audit row. Disabling the last active administrator is refused; grant `administrator` to another active account first.

Show a user's status, stored permissions, reviewer subjects, permission audit and account status audit history, with the operator label or user id of each change (read-only):

```sh
go run ./cmd/server show-user -email teacher@example.test
```

Try it (curl stores `Secure` cookies for `127.0.0.1`; browsers do the same for loopback):

```sh
curl -s -c jar -H 'Content-Type: application/json' \
  -d '{"email":"you@example.test","password":"…"}' http://127.0.0.1:8090/api/auth/login
curl -s -b jar http://127.0.0.1:8090/api/auth/session
```

Studio endpoints need the `author` permission. Grant it with the operator command above:

```sh
go run ./cmd/server grant -email you@example.test -permission author
```

Then sign in and call the Studio API. The password and CSRF token are passed on stdin, never as command-line arguments (`-H @-` reads headers from stdin; curl 7.55 or newer):

```sh
read -rs PW && printf '{"email":"you@example.test","password":"%s"}' "$PW" \
  | curl -s -c jar -H 'Content-Type: application/json' --data-binary @- \
      http://127.0.0.1:8090/api/auth/login > session.json; unset PW
CSRF=$(sed -n 's/.*"csrfToken":"\([^"]*\)".*/\1/p' session.json)
# lesson.json: {"subjectId": 1, "language": "fr", "content": {…schema v1 document…}}
printf 'X-CSRF-Token: %s\n' "$CSRF" | curl -s -b jar -H @- -H 'Content-Type: application/json' \
  --data-binary @lesson.json http://127.0.0.1:8090/api/lessons
printf 'X-CSRF-Token: %s\n' "$CSRF" | curl -s -b jar -H @- -H 'Content-Type: image/png' \
  --data-binary @diagram.png http://127.0.0.1:8090/api/media
curl -s -b jar -o served.png http://127.0.0.1:8090/api/media/1
rm -f jar session.json
```

Subjects (and chapters) have no seed or endpoint yet (LRN-014); insert simulated rows such as `INSERT INTO subjects (code) VALUES ('simulated-math')` locally.

Endpoints, errors, CSRF, authorization and throttling are specified in [API.md](API.md).

### Migrations

Files live in `migrations/` as `NNNN_description.sql` and are embedded in the binary. Each runs in its own transaction under a PostgreSQL advisory lock; `schema_migrations` records a SHA-256 checksum of each file. The runner refuses a database whose history contains an unknown version, an edited file or an out-of-order gap. Never edit a merged migration: add the next number. Only one open PR adds migrations at a time ([§5.4](CAHIER_DES_CHARGES.md#54-shared-contracts-agree-first-then-build-in-parallel)).

### Tests

```sh
go test ./...
go vet ./...
```

With `LEARNOO_TEST_DATABASE_URL` set, each database-backed test creates an isolated schema, migrates it and drops it afterwards, so tests may run in parallel and leave nothing behind. They cover migration repeatability and tamper detection, cookie attributes, enumeration-safe errors, anonymous access, idle and absolute expiry, rotation on login, logout revocation, CSRF rejection, strict login JSON (exact, unique keys), per-account and per-address throttling and concurrent attempts, plus permission deny-by-default, per-subject reviewer scope, no implied permissions (reviewer never implies publisher), administrator-only grants, audit records and their append-only protection, the operator commands (bootstrap, grant, revoke, disable-user, enable-user, show-user, invalid input leaving the database unchanged, the operator label in both audits), account disable and enable (session revocation, enable purging sessions, the last-administrator refusal, the status audit's exactly-one-actor and label constraints and its append-only protection), concurrent bootstrap, concurrent revocations and disables of the last two administrators in every mix (by the operator, and by the two administrators acting on each other, which must not deadlock), logins racing a disable (no session survives, the refused login is indistinguishable from a bad password), and type-checked source scans. One scan fails if `internal/server` or any module package it imports refers to `permissions.OperatorAs`. The other, over every module package, allows `permissions.Store` change methods and `OperatorAs` only in `cmd/server/main.go` and `internal/permissions/permissions.go`, allows `accounts.Store.Disable` and `Enable` only in `cmd/server/main.go`, flags any of them inside any function taking an `http.ResponseWriter`, flags SQL literals that write `user_permissions`, `reviewer_assignments` or `permission_audit` outside `internal/permissions`, and flags SQL literals that write `account_status_audit` or set `users.disabled_at` outside `internal/accounts` (disabling an account can remove an active administrator, so it must go through the last-administrator guard). Self-tests check the rules against probe handlers and type-checked probe files. Method calls, method values, method expressions and methods promoted by embedding are resolved by type; table names may be schema-qualified or quoted. The scans guard against accidental paths, not a proof: they cannot see calls through interfaces or SQL built by concatenation or at run time. A route test only checks that a few plausible admin paths return 404. Lesson persistence tests (LRN-006) cover foreign-key and uniqueness rejections, concurrent draft creation (exactly one succeeds), concurrent saves of one draft version (one wins, the other conflicts), submitted snapshots unchanged after Store and direct SQL edits, submission racing a media change, revoked and disabled authors refused on their next call, near-limit documents that save and submit, canonical stored content (case-folded duplicate keys refused, spacing and key order normalised, non-canonical stored content refused at submission), approved-only publication pointers, approved-unpublished revisions blocking new drafts until publication (including publication racing draft creation, and restoration of an earlier revision), the once-only `candidate_closed_at` rule (including a non-owner role with only DML grants and `TEMP` trying a temporary trigger), translation subject rules, draft discard, never-reused revision numbers, changes-requested successors, an untouched published pointer, author/ownership denials (only the lesson's owner holding `author` starts a draft or translation: another author, an administrator with or without `author`, the owner without `author` and a disabled owner are refused; direct SQL by a non-owner, a `REPEATABLE READ`/`SERIALIZABLE` writer with a stale snapshot, and temporary shadow tables are refused by the database guards), schema v1 rejection before storage, immutable media rows, and temporary shadow tables for every guard. The Studio API tests (LRN-006) run through the real server handler: anonymous 401, CSRF and cross-origin rejection, 403 for accounts without `author`, enumeration-safe 404 for other authors' revisions and for drafts or translations of lessons the caller does not own, create/save/submit/discard, stale saves (409, winner intact), saves after submission, concurrent draft creation over HTTP, invalid and oversized bodies storing nothing, translations, strict JSON bodies (case-sensitive, duplicate-free keys, nothing after the object), clean validation messages (NUL, wrong types), image upload and de-duplication (including concurrent identical uploads and cross-author uploads keeping the first uploader), object-scoped media reads and references, the in-transaction `author` recheck on upload, slow uploads hitting the read timeout (408), SVG/GIF/type-mismatch/trailing-data/oversized rejections without unbounded reads, media serving headers, and media ID tampering. Review decisions and publication do not exist yet, so these tests simulate them with direct SQL. They use simulated users, subjects, chapters and `unreviewed` test documents only. Media tests write to `t.TempDir()`. Run the concurrency tests under the race detector with `go test -race ./internal/permissions ./internal/accounts ./internal/auth ./internal/lessons ./internal/media ./internal/studioapi ./internal/server ./cmd/server`.

### Pending

Permissions are implemented in `internal/permissions` and `migrations/0002_permissions.sql` (see [API.md](API.md#permissions)). The operator commands above administer them (LRN-013). Lesson persistence (LRN-006 slice 1) is implemented in `internal/lessons` and `migrations/0004_lessons.sql`; see [DECISIONS.md](DECISIONS.md#lesson-persistence-lrn-006). Image validation and content-addressed storage (slice 2) are in `internal/media`; see [DECISIONS.md](DECISIONS.md#media-upload-limits-lrn-006). The Studio API for drafts and media (slice 3) is in `internal/studioapi`, mounted by `internal/server`, with media access links in `migrations/0005_media_uploads.sql`; see [API.md](API.md#studio-lesson-drafts-and-media-lrn-006) and [DECISIONS.md](DECISIONS.md#studio-api-lrn-006). The Studio UI (`web/studio`) is not connected to it yet. Its database guards (triggers and constraints) bind only a role that is neither a superuser nor the owner of the tables; locally and in tests one role owns the schema and runs the server, so the guards are not a security boundary there. Deployment (LRN-010) must add a separate least-privilege runtime role distinct from the migration owner, put `LEARNOO_MEDIA_DIR` on a persistent, backed-up volume, and configure the CDN so `/api/` (including media) bypasses shared caches. A draft whose author lost the `author` permission blocks new drafts of its lesson until an administrative discard exists (LRN-019/issue 9). Not yet implemented: Studio integration with the API, a lesson/draft listing endpoint, catalogue endpoints and seed (LRN-014), review decisions and publication (LRN-007), reviewer and reader access to revisions and media (LRN-007, LRN-008), withdrawal and restoration (LRN-009), the web admin console (LRN-019), self-approval prevention in review (LRN-007), cleanup of unreferenced media files. Registration, recovery, verification, deletion (LRN-022), deployment, TLS termination and trusted-proxy client addresses are not implemented.

## Local renderer experiment

This is the LRN-004 renderer experiment, not the production application. It deliberately has no accounts, database, teacher workflow or publishing. It binds only to 127.0.0.1 and accepts only fixtures marked `unreviewed`. Do not expose it through a public reverse proxy or tunnel.

### Prerequisites

- Go 1.25 or newer (tested with 1.25.4).
- Node.js 22 and npm (tested with Node 22.20.0).

### Run

From the repository root:

```sh
npm ci --ignore-scripts
npm run assets
go run ./cmd/renderer
```

Open http://127.0.0.1:8080/lesson. Change the local port with `go run ./cmd/renderer -port 8081`.

- `/`: landing-page UI example; local only, no registration or publication.
- `/lesson`: unreviewed French complex-numbers fixture.
- `/bidi`: isolated Arabic/French directionality fixture.
- `/failure`: intentionally invalid formula with readable fallback.
- `/lab`: edit raw JSON, preview with the same Go renderer, save/reopen on the current browser only.
- `POST /preview`: validates JSON and returns the canonical HTML used by the reader.

Browser-side KaTeX is copied from the pinned npm package and served locally with its license. No CDN requests are needed. YouTube loads only on a click when an actual video ID is present; the initial lesson has a pending-selection placeholder. The original logo banner is not loaded on every lesson page.

The JSON laboratory is not TipTap. No server-side draft storage or approval behavior is simulated. See [fixture details](fixtures/README.md).

### Design foundations

The reader consumes generated design tokens. Edit `web/design/tokens.json`, run `npm run design:build`, and rebuild the Go server. See [shared design exports](../web/design/README.md) for plain CSS and future Tailwind v4 Studio integration.

## Studio editor prototype (LRN-003)

`web/studio` is a local React/TypeScript/TipTap prototype with its own `package.json` and lockfile. It edits the restricted lesson JSON through a validated adapter; it has no accounts, server storage or publication. Saves stay in the current browser (`localStorage`) or a downloaded file. Verified versions: React 19.3.1, TipTap 3.35.0, Vite 8.3.8, TypeScript 6.0.3, Vitest 5.1.1, jsdom 29.1.1, on Node 22.20.0.

Start the renderer in one terminal, then the Studio in another:

```sh
go run ./cmd/renderer
cd web/studio
npm ci --ignore-scripts
npm run dev
```

Open http://127.0.0.1:5173. Vite binds to loopback and proxies `/preview` and `/static` to the renderer (`LEARNOO_RENDERER_URL`, default `http://127.0.0.1:8080`; `LEARNOO_STUDIO_PORT` changes 5173). The preview pane shows the renderer's canonical HTML; the Studio has no second renderer.

Checks, from `web/studio`:

```sh
npm run typecheck
npm test
npm run build
```

`npm test` covers fixture round trips, save/reopen, paste filtering, invalid input and the Go-parity cases in `test/contract/cases.json`, which `go test ./internal/rendering` also runs. `test/golden/studio-features.json` is an editor-authored export that Go validates and renders; regenerate it deliberately with `UPDATE_GOLDEN=1 npm test`. The root `npm run test:browser` also builds the Studio and serves it on `LEARNOO_STUDIO_TEST_PORT` (default 18180), so install its dependencies first.

Keyboard: `Ctrl/Cmd+B` bold, `Ctrl/Cmd+Alt+2` section heading, `$…$` inline formula, `$$ ` on an empty line for a display formula, `- ` for a list. Toolbar buttons are reachable with Tab; selected formulas, media, containers and paragraph language/direction are edited in the "Propriétés de la sélection" panel.

## Combined verification

```sh
npm run design:check
npm run design:contrast
go test ./...
go vet ./...
npx playwright install chromium
npm run test:browser
go test ./internal/rendering -run '^$' -bench BenchmarkRenderLesson -benchmem
```

Playwright always starts its own loopback server on port 18080 and refuses to reuse an existing process, in development and CI alike. Set `LEARNOO_TEST_PORT=18081` to select another port. Tests derive URLs and request-origin checks from this configuration; the interactive preview on 8080 is independent. Results and screenshots are written to ignored `test-results/`. Run `npm run assets` before compiling: Go embeds the prepared local browser assets into the binary.

For a restricted environment, put the Go build cache in a writable temporary directory by prefixing commands with `GOCACHE=/tmp/learnoo-go-cache`.

### Continuous integration

[`.github/workflows/ci.yml`](../.github/workflows/ci.yml) runs the commands above on every pull request to `develop` or `main` (including fork PRs) and on pushes to those branches. GitHub reports the check as **`verify`** under the **CI** workflow; select that check from the list when configuring branch protection rather than typing a name. It runs on Ubuntu 24.04 with Go 1.25.x and Node 22, in this order: `npm ci --ignore-scripts`, `npm run design:check`, `npm run assets`, `npm run design:contrast`, then in `web/studio` `npm ci --ignore-scripts`, `npm run typecheck`, `npm test`, `npm run build`, then `go test ./...`, `go vet ./...`, `npx playwright install --with-deps chromium`, `npm run test:browser`. Any failing step fails the job; no step is allowed to fail. A `postgres:16.13` service container (loopback port, trust authentication, no secret) backs the database tests; the job sets `LEARNOO_TEST_DATABASE_URL` and `LEARNOO_REQUIRE_DB_TESTS=1`, so those tests fail rather than skip if the database is unreachable.

Security: the workflow uses the `pull_request` trigger (never `pull_request_target`), a read-only `contents: read` token, no repository secrets, and checkout without persisted credentials. Third-party actions are pinned to commit SHAs. If a step fails, the `test-results/` folder (local fixture screenshots and metrics only) is uploaded as the `playwright-test-results` artifact for 7 days.

`design:check` runs before `assets` on purpose: `assets` regenerates the design files, so checking afterwards would hide generated CSS that was not committed. Making `verify` a required status check in branch protection is a maintainer setting and is not changed by the workflow.

### Security limits

Schema allowlists, escaped templates, local image allowlists, content/depth/formula limits, CSP, same-origin preview checks, and KaTeX `trust: false` reduce rendering risks. They do not establish production security. The only HTTP mutation is a read-only rendering request; browser saves are local fixtures. Authentication, durable storage, role checks, review and publication invariants belong to later work and remain required for the real platform.

### Dependency licensing

KaTeX, React, TipTap, Vite and Vitest are MIT-licensed; Playwright and TypeScript are Apache-2.0-licensed; their own terms apply. The project's interim notice does not override dependencies. `scripts/copy-katex.mjs` preserves the KaTeX license in the generated vendor directory.
