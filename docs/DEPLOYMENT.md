# Deployment and recovery runbook

Status (LRN-010, 2026-11-24): these are deployment artifacts and a **local** rehearsal only. Nothing has been deployed, no DNS, TLS, reverse-proxy or Cloudflare configuration exists, and no production credential has been created. A rehearsal on the shared server is still an open, human acceptance item on issue 10. Values marked **EXAMPLE** are placeholders for the infrastructure maintainer to decide ([DECISIONS.md](DECISIONS.md#deployment-and-recovery-lrn-010)).

| File | Purpose |
| --- | --- |
| [`Dockerfile`](../Dockerfile) | Application image: `server` only, KaTeX and design CSS embedded, Studio bundle built in, distroless, non-root (uid 65532) |
| [`deploy/compose.production.yaml`](../deploy/compose.production.yaml) | PostgreSQL 16 + one-shot `migrate` and `grants` + `app`; `backup` and `restore` one-shots under the `ops` profile |
| [`deploy/production.env.example`](../deploy/production.env.example) | Every operational variable, with EXAMPLE defaults |
| [`deploy/db/roles.sql`](../deploy/db/roles.sql), [`grants.sql`](../deploy/db/grants.sql), [`initdb.sh`](../deploy/db/initdb.sh) | Least-privilege owner and runtime roles |
| [`deploy/backup.sh`](../deploy/backup.sh), [`deploy/restore.sh`](../deploy/restore.sh) | Backup with checksums; restore into empty targets |
| `server verify-restore` | Checks a restored database and media directory |

The renderer experiment (`cmd/renderer`) is not in the image or the Compose file; it stays a loopback-only local tool.

## Before any real deployment

These block exposing the application to real users.

1. **Client addresses behind the proxy.** Configure `LEARNOO_TRUSTED_PROXIES` (see [Client addresses](#client-addresses-behind-the-proxy)). Left empty, every request comes from the proxy's (or the Docker bridge's) address, so the per-address login, invitation and error-report throttles become one shared bucket: a handful of failed logins from anyone blocks sign-in for everyone for the throttle window.
2. **TLS at the reverse proxy.** Session cookies are `Secure` and `__Host-` prefixed; browsers drop them over plain HTTP, so sign-in only works through HTTPS.
3. **Invitation links.** Set `LEARNOO_PUBLIC_BASE_URL` to the public https site root (for example `https://learnoo.example`); `invite-student` then prints `<base>/invite/<token>`. Without it, it refuses to run in the container (it only prints `http://` loopback links for local development).
4. The values listed in DECISIONS (limits, backup schedule and retention, off-site location and encryption, RPO/RTO, alert destinations), the privacy policy (backups contain personal data) and the LRN-008 release gates.

## Build

```sh
docker build -t learnoo:<version> .      # or: docker compose -f deploy/compose.production.yaml build
```

Stages: Node (pinned) runs `npm ci --ignore-scripts`, `npm run assets` (design CSS and the pinned KaTeX copy) and the Studio build; Go (pinned) builds `./cmd/server` with `CGO_ENABLED=0`; the runtime stage is `gcr.io/distroless/static-debian12:nonroot`. Base images are pinned by version and digest in the `Dockerfile`; update both together. The Studio bundle (built with base `/studio/`) is copied to `/opt/learnoo/studio`, and the image sets `LEARNOO_STUDIO_DIR=/opt/learnoo/studio`, so the server serves it at `/studio` to signed-in staff ([API.md](API.md#studio-pages-get-studio-and-get-studiopath)). The server opens that directory as an `os.Root` at start-up and refuses to start if it has no `index.html`; files are read per request (read-only, owned by root in the image). The renderer experiment is neither built nor shipped. Tag images by version and keep the previous tag: rollback needs it.

## Secrets

Three single-line passwords, generated, never committed (`deploy/secrets/` is git-ignored):

```sh
install -d -m 0700 deploy/secrets
for f in pg_superuser_password db_owner_password db_runtime_password; do
  openssl rand -hex 32 > deploy/secrets/$f && chmod 0444 deploy/secrets/$f
done
```

Compose without Swarm bind-mounts secret files and cannot change their owner or mode, so they must be readable by the container users (postgres 999, app 65532); the 0700 directory protects them on the host. The app reads its password from `LEARNOO_DATABASE_PASSWORD_FILE`, so it never appears in the URL, the environment or the process list. `initdb.sh` reads the role passwords inside psql.

## Database roles

| Role | Used by | Rights |
| --- | --- | --- |
| `postgres` (bootstrap superuser) | first-start initialisation only | everything; peer authentication on the local socket, SCRAM over TCP. `roles.sql` revokes `CONNECT` and `TEMP` on the `postgres` maintenance database from PUBLIC (variable `maintenance_db`), so only superusers can use it |
| `learnoo_owner` | `migrate`, `grants`, `backup`, `restore` | owns the database, tables and `SECURITY DEFINER` trigger functions |
| `learnoo_app` | `serve` (and `verify-restore`) | `CONNECT`; `USAGE` on `public`; `SELECT, INSERT, UPDATE, DELETE` on tables; `SELECT` only on views and `schema_migrations`; `USAGE` on sequences. No `TEMP`, no `CREATE` anywhere, no `TRUNCATE`/`TRIGGER`/`REFERENCES`, owns nothing, member of no role |

The guards (triggers and constraints) bind only a role that is neither superuser nor table owner ([threat model](DECISIONS.md#lesson-persistence-lrn-006)); the runtime role is that role. `grants.sql` ends with checks that fail if the runtime role has any role attribute, `TEMP` or `CREATE`, owns an object, is a member of any role (any `pg_auth_members` row), holds `TRUNCATE`/`TRIGGER`/`REFERENCES`, or can execute a `SECURITY DEFINER` function.

- With Compose, the first start of an empty data volume runs `initdb.sh`: `roles.sql`, then the two passwords. Afterwards every `up` runs `migrate` (owner) then `grants` (owner, `grants.sql`) before `app` starts, so new tables are granted automatically.
- Without Compose: `psql -X -v ON_ERROR_STOP=1 -d postgres -v owner_role=… -v runtime_role=… -v dbname=… -f deploy/db/roles.sql` as a superuser, set passwords with `\password`, run `server migrate` as the owner, then `psql … -U learnoo_owner -d learnoo -v runtime_role=learnoo_app -f deploy/db/grants.sql` **after every migration**. Both scripts are idempotent.
- `server serve` only reads `schema_migrations`; it refuses to start when migrations are pending or unknown, and the runtime role cannot apply them.

## Run and upgrade

```sh
cp deploy/production.env.example deploy/production.env   # decide every EXAMPLE value
C="docker compose -f deploy/compose.production.yaml --env-file deploy/production.env"
$C up -d --wait            # postgres -> migrate -> grants -> app
$C ps                      # app healthy = /readyz answering 200
```

- The database has no published port and is on an internal network. Only the app is published, to `LEARNOO_HTTP_PUBLISH` (default `127.0.0.1:8090`), for the reverse proxy. The image itself keeps the server's loopback default, so `docker run` of the image alone exposes nothing; the Compose file sets `LEARNOO_ADDR=0.0.0.0:8090` and the explicit `LEARNOO_ALLOW_NON_LOOPBACK=1` opt-in for the container network. Any other way of running the image must do the same and publish the port only to the proxy's address.
- `app`, `migrate`, `grants`, `backup` and `restore` run with a read-only root file system and drop all capabilities (`restore` adds back `CHOWN`, `FOWNER`, `DAC_OVERRIDE`); every service uses `no-new-privileges` and log rotation; `app` and `postgres` have CPU, memory and PID limits (EXAMPLE values from variables).
- **Upgrade:** take a backup (below) and keep it; set `LEARNOO_IMAGE` to the new tag; `$C up -d --wait`. `migrate` applies new migrations, `grants` re-grants, the app restarts.
- **Accounts:** `printf '%s\n' "$PASSWORD" | $C run --rm -T app create-user -email … -name …` (password on stdin). Invitations: `$C run --rm app invite-student -email …` with `LEARNOO_PUBLIC_BASE_URL` set (see "Invitation links" above). First administrator and role changes: `printf '%s\n' "$PASSWORD" | $C run --rm -T app bootstrap-admin -email … -name …`, then `$C run --rm app grant|revoke|disable-user|enable-user|show-user …` (LRN-013, [DEVELOPMENT.md](DEVELOPMENT.md#operator-commands-first-administrator-permissions-and-account-status)). Inside the container the audit's operator label is the container user and hostname, not the person who ran the command.

## App memory

`LEARNOO_APP_MEMORY` (EXAMPLE `512m`) is the app container's hard memory limit. Image uploads are what need memory; the budget and its measurements are in [measurements/upload-memory.md](measurements/upload-memory.md):

- `serve` reads the container's cgroup memory limit at start-up and sets the Go memory limit (`GOMEMLIMIT`) to three quarters of it (384 MiB for 512 MiB), so the garbage collector works harder before the process reaches the container limit instead of letting the heap grow to twice its live size. The log shows `Go memory limit set from the container limit`. An explicit `GOMEMLIMIT` in the app's environment takes precedence; without a container limit (local development) none is set.
- At most one image is decoded at a time, the decode estimate is capped at 192 MiB and the stored image at 16 MiB, and at most 4 uploads hold a body or a finished image at once. The computed worst case is about 320 MiB of Go heap (one decode, its output buffer, the other three uploads' bodies or finished images, the idle server), below the 384 MiB Go limit; the largest live heap left after a collection under load was 157 MiB and the container stayed within 512 MiB in every run (figures in the measurements). 512 MiB is therefore the smallest app limit to use; `serve` logs a warning below it. It was measured with Docker Desktop's Linux VM (one CPU, swap disabled), not on the shared server: the infrastructure maintainer still decides the value, and should watch for the container memory limit being hit (Monitoring below).

## Client addresses behind the proxy

The per-address throttles (login, invitations, error reports) key on the client address from `auth.ClientAddr`, which `internal/clientip` resolves once per request:

- `LEARNOO_TRUSTED_PROXIES` (empty by default): comma-separated CIDR ranges or addresses. Only when the **immediate TCP peer** is inside one of them is a forwarding header read; from any other peer every forwarding header is ignored, so clients cannot choose their own throttle key. Ranges broader than `/8` (IPv4) or `/16` (IPv6), including `0.0.0.0/0`, `::/0` and their IPv4-mapped forms, are refused.
- `LEARNOO_CLIENT_ADDRESS_HEADER`:
  - `X-Forwarded-For` (default, recommended): the chain is read from the right, skipping trusted addresses; the first untrusted address is the client. Entries a client prepends are never used. When no untrusted address is found (empty, malformed, too long or all-trusted chain), the result is the nearest hop: the right-most entry if it is a valid trusted address, otherwise the peer; never an entry further left, which a client could have chosen. At most 32 hops are examined. IPv6, bracketed and `address:port` entries are accepted; IPv4-mapped IPv6 is unmapped.
  - `CF-Connecting-IP`: the single address Cloudflare sets. Safe only if the origin proxy accepts connections from Cloudflare alone (firewall or authenticated origin pulls) and passes the header through; otherwise anyone reaching the origin directly can set it.
- **Cloudflare warning.** Do **not** list Cloudflare's published IP ranges in `LEARNOO_TRUSTED_PROXIES` in `X-Forwarded-For` mode. Requests made by any Cloudflare customer's Worker leave from addresses inside those ranges (for example `2a06:98c0:3600::103`), so trusting the ranges as intermediate hops trusts every Cloudflare-originated client, and a Worker can put any address it likes to the left of its own: the right-to-left walk would skip the Worker's address and return the forged entry. When Cloudflare is in front, the safer combination is `CF-Connecting-IP` mode with the origin proxy reachable only from Cloudflare, trusting only the proxy (or the Docker gateway, below), not Cloudflare's ranges. Without Cloudflare, `X-Forwarded-For` mode trusting only the proxy is enough. **The final choice is an open infrastructure decision** ([DECISIONS.md](DECISIONS.md#trusted-proxies-and-public-base-url-issue-46)).
- **What to trust** (infrastructure configuration, not hard-coded): only the address the app container sees for the reverse proxy. With Compose publishing to the host, the app sees the **gateway of the `frontend` network** as the peer (`docker network inspect <project>_frontend --format '{{(index .IPAM.Config 0).Gateway}}'`), whatever process on the host connected. **Warning:** on a shared server, any process or user that can reach the published address (`127.0.0.1:8090` by default) then arrives from that trusted gateway and can choose its own throttle key with a forged header. Prefer one of: run the proxy as a container on the `frontend` network, remove the published port and trust only the proxy container's address (pin it with a fixed subnet and `ipv4_address` in a Compose override); or publish to an address only the proxy can reach. Trust a single address (`/32`), not the whole subnet, where possible. Docker chooses the subnet when the network is created; pin it in an override if the network may be recreated. The proxy must append the client address to `X-Forwarded-For` (nginx: `proxy_add_x_forwarded_for`; Caddy does so by default) or, in `CF-Connecting-IP` mode, pass that header unchanged.
- Verified locally (Docker Desktop, 2026-11-24): without trusted proxies, 30 failed logins with 30 different `X-Forwarded-For` values throttled the next request (header ignored); with the frontend subnet trusted, one client was throttled while another client through the same published port was not. This test also shows the warning above: curl on the host, not a proxy, was trusted.

## Rollback

Migrations are forward-only: there are no down migrations, and an older image refuses a database with a migration it does not know ("database has migration … that this build does not know"). Rolling back an upgrade therefore means restoring the backup taken **before** the upgrade and running the previous image. Data written after that backup is lost unless re-entered: that backup is the recovery point. Run from the repository checkout on the server, in bash:

```sh
C="docker compose -f deploy/compose.production.yaml --env-file deploy/production.env"
P="${LEARNOO_COMPOSE_PROJECT:-learnoo}"     # the Compose project name (volume prefix)
PRE=learnoo-<timestamp of the pre-upgrade backup>

# 0. Check the pre-upgrade backup is present and intact before destroying anything.
#    Its files are 0400 and owned by uid 65532 in a 0700 directory, so the check
#    needs root (path = LEARNOO_BACKUP_HOST_DIR).
sudo sh -c "cd 'deploy/backups/$PRE' && sha256sum -c --quiet MANIFEST.sha256"
# 1. Final backup of the current state, kept for investigation (not restored).
$C --profile ops run --rm backup
# 2. Stop and remove the containers, then the production database and media volumes.
#    IRREVERSIBLE: only after steps 0 and 1 succeeded.
$C --profile ops down
docker volume rm "${P}_pgdata" "${P}_media"
# 3. Pin the previous image: set LEARNOO_IMAGE=<previous tag> in deploy/production.env.
# 4. Fresh PostgreSQL: initdb.sh creates the roles and an empty database.
$C up -d --wait postgres
# 5. Restore the pre-upgrade backup into the empty database and media volume.
$C --profile ops run --rm restore "/backups/$PRE"
# 6. Verify strictly before serving anything.
$C run --rm --no-deps app verify-restore -strict
# 7. Start the previous image (migrate finds nothing to do, grants re-runs, app starts).
$C up -d --wait
$C ps
```

If step 5 or 6 fails, do not start the app: fix the cause, then repeat from step 2 (remove both volumes again) so the restore always starts from empty targets. If an upgrade fails before its migration commits (each migration is one transaction), the database is unchanged and the previous image can simply be started again (steps 3 and 7 only).

## Backups

`deploy/backup.sh` (Compose: `$C --profile ops run --rm backup`) writes `learnoo-<UTC timestamp>/` under `LEARNOO_BACKUP_HOST_DIR`:

| File | Content |
| --- | --- |
| `database.dump` | `pg_dump --format=custom` as the owner role |
| `media.tar` | every regular media file except `.tmp-*` leftovers |
| `media-files.sha256` | SHA-256 of each archived file |
| `info.txt` | format version, time, latest migration, file count, count of skipped files, `pg_dump` version |
| `MANIFEST.sha256` | SHA-256 of the four files above |

Media files whose relative path is not made of `[A-Za-z0-9._-]` segments are skipped with a warning and counted in `info.txt` (stored media keys always qualify, so a skipped file is never referenced by a row; restore refuses such names). The database is dumped before the media files are listed. Media files are immutable and written before the row naming them commits, so every file a dumped row references is in the archive; files that appeared later are only unreferenced extras, reported by verification. Files are created 0400 in a 0700 directory. The script never deletes or rotates anything. The Compose `backup` service runs as the app's uid 65532 (which owns the media files) with no capability and a read-only root, so `LEARNOO_BACKUP_HOST_DIR` must exist and be writable by uid 65532 only (for example `install -d -m 0700 -o 65532 -g 65532 deploy/backups`).

**Schedule, retention, encryption and off-site copy: to be decided by the infrastructure maintainer.** The backup contains personal data (emails, display names, password hashes, live session hashes, invitation records), so it must be encrypted at rest and access-controlled, and retention must follow the privacy policy once decided. An EXAMPLE host cron entry, not a recommendation:

```cron
# EXAMPLE — frequency to be decided by the infrastructure maintainer
15 3 * * * cd /srv/learnoo && docker compose -f deploy/compose.production.yaml --env-file deploy/production.env --profile ops run --rm backup >>/var/log/learnoo-backup.log 2>&1
```

Monitor that a new backup directory appears on schedule and that a restore rehearsal passes regularly; a backup that has never been restored is not evidence.

## Restore rehearsal

Rehearse in an isolated Compose project (separate volumes; preferably on another host, and never with the production project name). `restore.sh` refuses a database that has any table and a media directory that has any file, so it cannot overwrite live data.

```sh
R="docker compose -p learnoo-rehearsal -f deploy/compose.production.yaml --env-file deploy/rehearsal.env"
# rehearsal.env: a copy of production.env with another LEARNOO_HTTP_PUBLISH port
$R up -d --wait postgres                                        # fresh roles and empty database
$R --profile ops run --rm restore /backups/learnoo-<timestamp>  # checks MANIFEST, restores, checks each media file
$R run --rm --no-deps app verify-restore -strict                # must pass before trusting the backup
$R up -d --no-deps --wait app                                   # optional: browse the restored copy
$R --profile ops down -v                                        # discard the rehearsal
```

`restore.sh` works in this order, so that a bad backup is refused before the database is touched:

1. checks `MANIFEST.sha256` and the format line;
2. refuses a database with any relation and a media directory with any entry;
3. validates `media.tar` without extracting: every entry must be a directory or a regular file listed in `media-files.sha256`, with a plain relative name (no `..`, no absolute path); links, devices and unlisted files are refused;
4. extracts into a staging directory inside the target (`.tmp-restore-*`), checks every file against `media-files.sha256` and refuses any extra file;
5. runs `pg_restore --single-transaction --exit-on-error --no-owner` as the owner role (privileges as dumped, so the target needs the same role names, which `initdb.sh`/`roles.sql` create);
6. moves the staged files into place, gives them to the app user (0444 files, 0750 directories) and re-runs `grants.sql`.

The Compose `restore` service runs as root with a read-only root file system and only the `CHOWN`, `FOWNER` and `DAC_OVERRIDE` capabilities. **Partial failures:** steps 1 to 4 leave the database untouched and remove the staging directory; a `pg_restore` failure rolls back its transaction, leaving the database empty; a failure in step 6 leaves a restored database with incomplete media. In that last case (or whenever in doubt) discard both targets (`$R --profile ops down -v`, or for production `docker volume rm` as in Rollback) and restore again from step 1. Always finish with `verify-restore -strict`.

`server verify-restore` (`LEARNOO_DATABASE_URL`, `LEARNOO_MEDIA_DIR`) only reads and reports:

- migrations are current (no pending, edited or unknown migration);
- every `media_assets` row (including every asset referenced by `revision_media`) has a regular file at its content-addressed path with the recorded size and SHA-256, a key matching its hash and content type;
- every published pointer names an approved, non-withdrawn revision of its own lesson, and matches the lesson's latest publication audit row;
- every submitted revision (any status but `draft`) has its submission audit row, recording its author and its `submitted_at`, and no draft has one (`migrations/0011`);
- row counts per table, to compare with the source (`docker compose … exec -u postgres postgres psql -d learnoo` and `SELECT count(*)` on the source at backup time, or the counts printed by `verify-restore` on the source);
- unexpected files (no row names them) and `.tmp-*` leftovers are listed, never deleted; `-strict` turns them into failures.

The automated rehearsal (`TestBackupRestoreRehearsal`, `go test ./internal/server`) does this end to end on every CI run: TEST fixtures with an uploaded image are created through the HTTP API as the runtime role, backed up, restored into a fresh database and empty media directory, verified (counts equal, strict mode), served again to the same sessions; tampered `media.tar`, `database.dump` or `media-files.sha256`, an incomplete backup and non-empty targets are refused; a missing or corrupted media file fails verification.

## Monitoring

- `GET /healthz`: liveness, 200 whenever the process serves HTTP. The image `HEALTHCHECK` uses it.
- `GET /readyz`: readiness, 200 only when the database answers a ping and the media directory is writable (an `access(2)` permission check that writes nothing; a full disk is not detected, so monitor disk space); otherwise 503 with no detail. The Compose `app` health check uses it (`server probe -path /readyz`, as the image has no shell or HTTP client).
- The probes are for container health checks and internal monitoring only. **The reverse proxy must not expose `/healthz` or `/readyz` publicly**: answer them with 404 at the proxy (each `/readyz` request costs a database round trip). Monitoring should call them on the published address from the host, not through the public site.
- Logs: the server writes `log/slog` text records (`time=… level=… msg=… key=value`) to stderr, collected by the Docker `json-file` driver with rotation. It logs start-up, the non-loopback warning, readiness failures and internal errors (with the error, never request bodies, passwords, tokens or URLs); there is no per-request access log, so use the reverse proxy's access log. PostgreSQL logs go to its container log.
- To watch (alert destinations to be decided): app or postgres unhealthy or restarting; `readiness:` errors; disk space of the data, media and backup volumes; missing scheduled backups; failed rehearsals; container memory limits being hit.

## Caching, reverse proxy and Cloudflare

What the application sends today:

| Responses | `Cache-Control` | Notes |
| --- | --- | --- |
| Everything by default (API, media, login JSON, probes, errors, every 404) | `no-store` | `Vary: Cookie` |
| Reader pages and the unversioned `/static/` files | `private, no-store` | sent even to anonymous requests |
| `/studio` and `/studio/…` (the Studio shell, client routes, errors), `/api/catalogue/…`, `/api/lessons/mine`, `/api/preview` | `private, no-store` | |
| Fingerprinted public assets `/assets/<16 hex digits>/…` (reader CSS, `math.js`, KaTeX and its fonts) | `public, max-age=31536000, immutable` | no session read, no `Vary`, never `Set-Cookie`; another version is `404` with `no-store` |
| Fingerprinted Studio bundle files `/studio/assets/<name>-<hash>.<ext>` served to signed-in staff | `private, max-age=31536000, immutable` | browser cache only; session and Studio access still required, `404` with `no-store` otherwise |

Rules for the proxy and CDN (to be configured by the infrastructure maintainer; header tests in this repository do not prove the live configuration):

- Cloudflare and any shared cache must **never cache authenticated pages or API responses**. Do not enable "Cache Everything" or edge HTML caching for this host, and never cache a response that carries `Set-Cookie`.
- **Only `/assets/` may be cached at the edge** (issue #88): one cache rule, "URI path starts with `/assets/`" (a plain prefix, no regular expression), eligible for cache, respecting the origin's `Cache-Control` (or an edge TTL of up to a year); the cookie may be ignored in the cache key for this prefix, since the server never reads it there. Every other path, including `/static/`, `/studio/` and `/studio/assets/`, `/learn`, `/login`, `/api/`, lesson media and the probes, must **bypass** the cache, whatever its extension (a default "cache static file extensions" rule would otherwise catch `/static/site.css` or `/studio/assets/index-….js`). A `404` under `/assets/` is `no-store` and must not be cached either (an edge must not keep "negative" answers).
- Forward the `Cookie`, `Origin` and `Sec-Fetch-Site` headers unchanged (cross-origin protection depends on them) and preserve `Host`.
- After a deployment, pages name the new versions; the old ones answer `404`, so no purge is needed. A browser holding an old page (pages are never stored, so only an open tab) may miss its assets until reloaded.
- The proxy must not rewrite `Cache-Control` on any path. Check after configuring: `curl -sI https://<host>/learn` shows `private, no-store` and no edge cache hit; an asset URL copied from a page's source shows `public, max-age=31536000, immutable` and, on a second request, an edge hit (for Cloudflare, `cf-cache-status: HIT`); the same path with another version is `404`.
