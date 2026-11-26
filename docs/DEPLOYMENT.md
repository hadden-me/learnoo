# Deployment and recovery runbook

Status (LRN-010, 2026-11-24; reference proxy issue 87, 2026-11-26): these are deployment artifacts and a **local** rehearsal only. Nothing has been deployed, no DNS, real certificate or Cloudflare configuration exists, and no production credential has been created. A reference reverse-proxy configuration exists and is tested locally only ([Reference reverse proxy](#reference-reverse-proxy)). A rehearsal on the shared server is still an open, human acceptance item on issue 10. Values marked **EXAMPLE** are placeholders for the infrastructure maintainer to decide ([DECISIONS.md](DECISIONS.md#deployment-and-recovery-lrn-010)).

| File | Purpose |
| --- | --- |
| [`Dockerfile`](../Dockerfile) | Application image: `server` only, KaTeX and design CSS embedded, Studio bundle built in, distroless, non-root (uid 65532) |
| [`deploy/compose.production.yaml`](../deploy/compose.production.yaml) | PostgreSQL 16 + one-shot `migrate` and `grants` + `app`; `backup` and `restore` one-shots under the `ops` profile |
| [`deploy/production.env.example`](../deploy/production.env.example) | Every operational variable, with EXAMPLE defaults |
| [`deploy/db/roles.sql`](../deploy/db/roles.sql), [`grants.sql`](../deploy/db/grants.sql), [`initdb.sh`](../deploy/db/initdb.sh) | Least-privilege owner and runtime roles |
| [`deploy/backup.sh`](../deploy/backup.sh), [`deploy/restore.sh`](../deploy/restore.sh) | Backup with checksums; restore into empty targets |
| `server verify-restore` | Checks a restored database and media directory |
| [`deploy/compose.proxy.yaml`](../deploy/compose.proxy.yaml), [`deploy/proxy/Caddyfile`](../deploy/proxy/Caddyfile) | Reference reverse proxy (Caddy, pinned): Compose override, issue 87 |
| [`deploy/proxy/test-proxy.sh`](../deploy/proxy/test-proxy.sh), [`deploy/proxy/compose.test.yaml`](../deploy/proxy/compose.test.yaml) | Local test of the proxy in an isolated Compose project |

The renderer experiment (`cmd/renderer`) is not in the image or the Compose file; it stays a loopback-only local tool.

## Before any real deployment

These block exposing the application to real users.

1. **Client addresses behind the proxy.** Configure `LEARNOO_TRUSTED_PROXIES` (see [Client addresses](#client-addresses-behind-the-proxy)). Left empty, every request comes from the proxy's (or the Docker bridge's) address, so the per-address login, invitation and error-report throttles become one shared bucket: a handful of failed logins from anyone blocks sign-in for everyone for the throttle window.
2. **TLS at the reverse proxy.** Session cookies are `Secure` and `__Host-` prefixed; browsers drop them over plain HTTP, so sign-in only works through HTTPS. The [reference proxy](#reference-reverse-proxy) does this with a local certificate; a real domain needs the adaptation described there.
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
- **What to trust** (infrastructure configuration, not hard-coded): only the address the app container sees for the reverse proxy. With Compose publishing to the host, the app sees the **gateway of the `frontend` network** as the peer (`docker network inspect <project>_frontend --format '{{(index .IPAM.Config 0).Gateway}}'`), whatever process on the host connected. **Warning:** on a shared server, any process or user that can reach the published address (`127.0.0.1:8090` by default) then arrives from that trusted gateway and can choose its own throttle key with a forged header. Prefer one of: run the proxy as a container on the `frontend` network, remove the published port and trust only the proxy container's address (pin it with a fixed subnet and `ipv4_address` in a Compose override); or publish to an address only the proxy can reach. Trust a single address (`/32`), not the whole subnet, where possible. The [reference proxy](#reference-reverse-proxy) override does exactly this. Docker chooses the subnet when the network is created; pin it in an override if the network may be recreated. The proxy must append the client address to `X-Forwarded-For` (nginx: `proxy_add_x_forwarded_for`; Caddy does so by default) or, in `CF-Connecting-IP` mode, pass that header unchanged.
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
- The probes are for container health checks and internal monitoring only. **The reverse proxy must not expose `/healthz` or `/readyz` publicly**: answer them with 404 at the proxy (each `/readyz` request costs a database round trip). Monitoring should call them on the published address from the host, not through the public site; with the [reference proxy](#reference-reverse-proxy) the app has no published address, so use the container health status or `server probe` inside the container.
- Logs: the server writes `log/slog` text records (`time=… level=… msg=… key=value`) to stderr, collected by the Docker `json-file` driver with rotation. It logs start-up, the non-loopback warning, readiness failures and internal errors (with the error, never request bodies, passwords, tokens or URLs); there is no per-request access log, so use the reverse proxy's access log. PostgreSQL logs go to its container log.
- To watch (alert destinations to be decided): app or postgres unhealthy or restarting; `readiness:` errors; disk space of the data, media and backup volumes; missing scheduled backups; failed rehearsals; container memory limits being hit.

## Reference reverse proxy

A reference configuration to adapt (issue 87), not a deployed one: Caddy `2.11.3-alpine`, pinned by digest, in the Compose override [`deploy/compose.proxy.yaml`](../deploy/compose.proxy.yaml) with its [`Caddyfile`](../deploy/proxy/Caddyfile). Choices are recorded in [DECISIONS.md](DECISIONS.md#reference-reverse-proxy-issue-87).

```sh
C="docker compose -f deploy/compose.production.yaml -f deploy/compose.proxy.yaml --env-file deploy/production.env"
$C up -d --wait            # postgres -> migrate -> grants -> app (healthy) -> proxy
```

| Variable | Default | Meaning |
| --- | --- | --- |
| `LEARNOO_PROXY_SITE` | `localhost` | Caddy site address (the public host name) |
| `LEARNOO_PROXY_TLS` | `internal` | `internal` = Caddy's local CA (testing only); an ACME contact email for a public certificate; or `cert.pem key.pem` paths |
| `LEARNOO_PROXY_HTTP_PUBLISH`, `LEARNOO_PROXY_HTTPS_PUBLISH` | `127.0.0.1:8080`, `127.0.0.1:8443` | Host addresses published for the container's ports 8080 (HTTP) and 8443 (HTTPS); loopback for testing, the host's 80 and 443 on a server |
| `LEARNOO_PROXY_HSTS_MAX_AGE` | `31536000` (EXAMPLE) | HSTS `max-age`; no `includeSubDomains`, no `preload` |
| `LEARNOO_PROXY_MAX_BODY_BYTES` | `6291456` (6 MiB) | Request body limit |
| `LEARNOO_FRONTEND_SUBNET`, `LEARNOO_FRONTEND_IP_RANGE`, `LEARNOO_PROXY_IPV4` | `172.30.87.0/24`, `172.30.87.128/25`, `172.30.87.10` (EXAMPLE) | Pinned `frontend` network, dynamic range (must exclude the proxy address) and the proxy's fixed address |

What the override and the Caddyfile do:

- **Only the proxy is published.** The app's `ports` are reset (`!reset []`, Compose 2.24+); it is reachable only on the `frontend` network. The app's container health check (`server probe -path /readyz`, inside the app container) is unaffected.
- **Client addresses.** The proxy has a fixed address in a pinned subnet and the override sets the app's `LEARNOO_TRUSTED_PROXIES` to exactly that `/32` (and `X-Forwarded-For` mode), whatever the env file says. Caddy trusts no incoming forwarding header (no `trusted_proxies`), so it **replaces** any client-supplied `X-Forwarded-For` with the single address of the connecting client; the app reads that one hop. Host processes can no longer arrive from a trusted gateway, which removes the published-port warning in [Client addresses](#client-addresses-behind-the-proxy). If the stack already ran without the override, its `frontend` network has a Docker-chosen subnet: `$C down` (without `-v`) and `up` again to recreate it.
- **TLS and redirect.** HTTPS for `LEARNOO_PROXY_SITE` with automatic certificates; HTTP answers `308` to `https://<host><path and query>` on the default port (an explicit redirect, since Caddy's own would name the container's port 8443), so publish HTTPS as the host's 443 on a server. HSTS on HTTPS responses only. HTTP/3 is off (it would need UDP 443).
- **Probes.** `/healthz`, `/readyz` and anything under them answer `404` with an empty body, whatever the case, percent-encoding or dot segments of the path. Monitoring reads the container health status or runs `$C exec app /usr/local/bin/learnoo-server probe -path /readyz`, not through the site.
- **Body limit.** 6 MiB: the 5 MiB media upload (`internal/media`) plus headroom; lesson JSON is at most 256 KiB, login and forms a few KiB, and the app enforces each limit itself. A declared `Content-Length` above the limit is answered `413` by the proxy before anything reaches the app. Bodies are streamed, so a body without a length (chunked, or HTTP/2) reaches the app, which stops at its own, lower limit (`413` from the app); the proxy's cut-off at 6 MiB is a backstop.
- **Timeouts.** Proxy: read header 10 s, read body 60 s, write 90 s, idle 2 min; upstream dial 5 s, upstream response header 60 s (reference values, in the Caddyfile). **The binding limit for uploads is the app's**, not the proxy's: the server reads each request, body included, within 15 s (`ReadTimeout`) and answers a slower upload with `408 upload_timeout`. The proxy streams the body, so a 5 MiB image must arrive at about 3 Mbit/s or more. Whether slow connections (mobile, school networks) need request buffering at the proxy (`request_buffers`, which would let the proxy absorb the slow upload and send it to the app at once) or a longer app read timeout is an **open decision** for the maintainer ([DECISIONS.md](DECISIONS.md#reference-reverse-proxy-issue-87)); nothing is changed here.
- **Headers.** HSTS is added; `Server` and `Via` are removed. The app's own headers (per-page `Content-Security-Policy`, `X-Frame-Options`, `Referrer-Policy`, `X-Content-Type-Options`, `Cache-Control`, `Vary`) are never overridden; the proxy only fills them in, with the app's strict defaults (`no-store`, `nosniff`, `DENY`, `same-origin`, `default-src 'none'`), on responses the app did not produce: redirects, probe 404s, the proxy's `413`, and errors Caddy raises itself (`502` with the app down, and any other, through `handle_errors`, whose body is just the status code). `Host`, `Cookie`, `Origin` and `Sec-Fetch-Site` are passed unchanged.
- **No caching.** Stock Caddy has no response cache, so nothing is stored at the proxy and the app's `Cache-Control` reaches browsers and any CDN unchanged. The only responses meant for shared caches are those the app marks `public`: the fingerprinted `/assets/<version>/<name>` files (issue 88, `public, max-age=31536000, immutable`, no `Set-Cookie`), the only edge-cacheable prefix (rules in [Caching, reverse proxy and Cloudflare](#caching-reverse-proxy-and-cloudflare)). Everything else, including `/static/` and the Studio's `/studio/assets/` (`private` browser caching only), must bypass every proxy or edge cache. If a cache layer is ever added to the proxy, it must store only responses whose `Cache-Control` is `public`, never `private` or `no-store`.
- **Access log.** JSON on stdout (`http.log.access`, rotated by the Docker `json-file` driver). The raw request target is filtered: `/invite/<token>` becomes `/invite/REDACTED`; any other path that names `invite` in any case or contains a percent-escape (`//invite/…`, `/./invite/…`, `/Invite/…`, `/%69nvite/…`, which the app also routes to the invitation page) becomes `REDACTED`; any query string becomes `?REDACTED`. `Cookie`, `Authorization`, `Proxy-Authorization`, `Referer`, `X-CSRF-Token`, `Set-Cookie` and `Location` are dropped. Caddy's error log uses the same request filter. The admin API is off.
- **Container.** Runs as uid/gid 65532, not root, listening on 8080/8443 inside the container; read-only root, `no-new-privileges`, CPU/memory/PID limits (EXAMPLE). All capabilities are dropped except `NET_BIND_SERVICE`, which the process does not use (it binds unprivileged ports) but keeps as its only effective capability: the image's `caddy` binary carries that file capability, and the kernel refuses to execute it if the capability is outside the bounding set. Removing it entirely needs a derived image with the file capability stripped (`setcap -r`), left to the maintainer. Certificates live in the `proxy_data` volume (the image's `/data/caddy` and `/config/caddy` are world-writable, so no ownership change is needed); with `tls internal` it also holds the local CA key: keep it, and never reuse it outside testing.

### Local test

`deploy/proxy/test-proxy.sh` builds the image, starts postgres, the app and the proxy in an isolated Compose project (`learnoo-proxytest-<pid>`, its own subnets `172.30.187.0/24` and `172.30.188.0/24`, loopback ports 18080/18443, throwaway secrets in a temporary directory), checks with curl (`--path-as-is`, so `//` and `/./` are sent unchanged), then runs `down -v` and removes the image it built. It needs Docker with Compose 2.24+, `curl` and `openssl` on the host; `LEARNOO_TEST_IMAGE=<tag>` reuses an existing image, `LEARNOO_TEST_KEEP=1` leaves the stack up. It checks:

- the app is healthy (internal probe), has no published port and trusts only the proxy's `/32`; the proxy runs as uid 65532 with no capability but `NET_BIND_SERVICE`;
- `http://` → `308` `https://` (query kept, default port) with `nosniff` and `no-store` and no `Server`; HSTS on HTTPS only; the app's CSP kept; no `Server` or `Via`; `/login` 200; anonymous `/studio` and `/learn` → `303` to `/login`;
- `/healthz`, `/readyz` and variants (`/HEALTHZ`, `/%68ealthz`, `/readyz/`, `/readyz?x=1`, `//healthz`, `/./readyz`, `HEAD`) → 404 with an empty body, HSTS, `no-store`, `nosniff`, the strict CSP and no `Server`/`Via`;
- a 7 MiB body with `Content-Length` → `413` from the proxy with the same headers; a 5 MiB one reaches the app; a 7 MiB upload streamed over HTTP/2 without a length by a signed-in administrator → `413` (the app's limit) with HSTS and no `Server`/`Via`;
- two users signed in through the proxy, twice in alternation, each get their own `/learn` page with `Cache-Control: private, no-store` and `Vary: Cookie`; a signed-out request gets a redirect without either name; no `Age`/cache-status header appears;
- a fingerprinted `/assets/<version>/<name>` URL taken from the login page passes through as `public, max-age=31536000, immutable`, without `Set-Cookie` or `Vary` even when a session cookie is sent; `/static/site.css` stays `private, no-store`; a `/studio/assets/` bundle file is `private, max-age=31536000, immutable` for the administrator and `404` `private, no-store` signed out;
- throttle keyed on the real client: from curl containers with fixed addresses on a test-only `edge` network ([`compose.test.yaml`](../deploy/proxy/compose.test.yaml); on Docker Desktop every host process shares one gateway address), client A's 31 failed sign-ins, each with a different forged `X-Forwarded-For`, end in `429`; client B is not throttled, even when it forges A's address; A stays throttled;
- with a live invitation token, `/invite/<t>`, `//invite/<t>`, `/./invite/<t>`, `/%69nvite/<t>` (served by the app), `/Invite/<t>` and `/invite/<t>?x=1` are requested; then, with the app stopped, `/invite/<t>` and `/%69nvite/<t>` answer `502` with HSTS, `no-store`, `nosniff`, the strict CSP and no `Server`/`Via`; the proxy logs (access and error) contain `/invite/REDACTED` but not the token, the query string or the session cookie value.

Verified on Docker Desktop (macOS, Docker 27.3, Compose 2.30) on 2026-11-26: 72 checks passed, teardown left no container, network, volume or built image. The script passes `shellcheck` (`docker run --rm -v "$PWD:/mnt:ro" -w /mnt koalaman/shellcheck:v0.10.0 deploy/proxy/test-proxy.sh`). This is a local test of the reference configuration; it does not prove the configuration of a real server or CDN.

### Adapting to a real domain (infrastructure decisions)

1. DNS for the host name; `LEARNOO_PROXY_SITE=learnoo.example`, `LEARNOO_PUBLIC_BASE_URL=https://learnoo.example`.
2. `LEARNOO_PROXY_TLS=<ACME contact email>` (Let's Encrypt via HTTP-01 or TLS-ALPN-01: ports 80 and 443 must be reachable), or a certificate file pair mounted into the container. Never `internal` on a public site.
3. Publish the container's 8080 and 8443 as the host's `0.0.0.0:80` and `0.0.0.0:443` (`LEARNOO_PROXY_HTTP_PUBLISH=0.0.0.0:80`, `LEARNOO_PROXY_HTTPS_PUBLISH=0.0.0.0:443`, and the IPv6 equivalents if used), with the firewall allowing only those. HTTPS must be the host's 443: the redirect names the default port.
4. Start HSTS with a short `max-age` (for example 300) and raise it once HTTPS is confirmed; `includeSubDomains` and `preload` are deliberately absent and are a decision for the domain owner.
5. Choose a `frontend` subnet that does not collide with other networks on the server.
6. If the shared server already runs a reverse proxy on 80/443, either run this one behind it on another address or port the rules into it: TLS, redirect and HSTS; the client address forwarded as one hop that client headers cannot influence, and the app trusting only that proxy's address; probes 404; the body limit; the timeouts; no caching except `public` responses; the redacted access log.

**Cloudflare** (whether to use it is open, [issue 46](DECISIONS.md#trusted-proxies-and-public-base-url-issue-46)). Behind Cloudflare, the reference proxy would see Cloudflare's edge as the client, so every visitor behind one edge would share a throttle key. The safe combination recorded in [Client addresses](#client-addresses-behind-the-proxy) is `LEARNOO_CLIENT_ADDRESS_HEADER=CF-Connecting-IP` (the override's `X-Forwarded-For` line changed accordingly; Caddy passes that header through unchanged) with the origin reachable **only** from Cloudflare (firewall allow-list or authenticated origin pulls, which Caddy can require with `client_auth`), still trusting only the proxy's `/32`, never Cloudflare's ranges. Use SSL mode "Full (strict)" with a valid origin certificate (ACME or a Cloudflare origin certificate), and cache rules that bypass the cache for the whole host except `/assets/` (see [below](#caching-reverse-proxy-and-cloudflare)). The header choice, origin lockdown and cache rules remain infrastructure decisions; none is configured here.

## Caching, reverse proxy and Cloudflare

What the application sends today:

| Responses | `Cache-Control` | Notes |
| --- | --- | --- |
| Everything by default (API, media, login JSON, probes, errors, every 404) | `no-store` | `Vary: Cookie` |
| Reader pages and the unversioned `/static/` files | `private, no-store` | sent even to anonymous requests |
| `/studio` and `/studio/…` (the Studio shell, client routes, errors), `/api/catalogue/…`, `/api/lessons/mine`, `/api/preview` | `private, no-store` | |
| Fingerprinted public assets `/assets/<16 hex digits>/…` (reader CSS, `math.js`, KaTeX and its fonts) | `public, max-age=31536000, immutable` | no session read, no `Vary`, never `Set-Cookie`; another version is `404` with `no-store` |
| Fingerprinted Studio bundle files `/studio/assets/<name>-<hash>.<ext>` served to signed-in staff | `private, max-age=31536000, immutable` | browser cache only; session and Studio access still required, `404` with `no-store` otherwise |
| Public pages `/`, `/catalogue`, `/catalogue/<level>/<section>[/<subject>]` and course overviews `/courses/<publicId>` (LMS-B07) | `public, no-cache` | the same for every visitor: no session read, no `Vary`, never `Set-Cookie`; strong `ETag`, `If-None-Match` answers `304` |
| Their `404` notices (unknown or unreadable course, unknown catalogue path, draft edition) | `no-store` | no `ETag` |
| Course covers `/courses/<publicId>/cover` (published revision of a readable course only) | `public, no-cache` | `ETag`, media CSP; every other request, including after suspension or archive, is a JSON `404` with `no-store` |

Rules for the proxy and CDN (to be configured by the infrastructure maintainer; header tests in this repository do not prove the live configuration):

- Cloudflare and any shared cache must **never cache authenticated pages or API responses**. Do not enable "Cache Everything" or edge HTML caching for this host, and never cache a response that carries `Set-Cookie`.
- **Only `/assets/` may be cached at the edge without revalidation** (issue #88): one cache rule, "URI path starts with `/assets/`" (a plain prefix, no regular expression), eligible for cache, respecting the origin's `Cache-Control` (or an edge TTL of up to a year); the cookie may be ignored in the cache key for this prefix, since the server never reads it there. Every other path except the public pages below (revalidated), including `/static/`, `/studio/` and `/studio/assets/`, `/learn`, `/login`, `/api/`, lesson media and the probes, must **bypass** the cache, whatever its extension (a default "cache static file extensions" rule would otherwise catch `/static/site.css` or `/studio/assets/index-….js`). A `404` under `/assets/` is `no-store` and must not be cached either (an edge must not keep "negative" answers).
- **Public pages and course covers** (`/`, `/catalogue` and `/catalogue/…`, `/courses/…`) may be stored by an edge **only with revalidation**: a cache that honours `no-cache` must send every request to the origin with `If-None-Match` and serve its copy only on `304`, so a suspension, withdrawal or unpublication takes effect on the next request. The cookie may be left out of the cache key for these paths (the server never reads it there). If the edge cannot revalidate on every request, these paths must bypass it like every other path. Never cache their `404`s. The cover is public, but lesson media (`/learn/lessons/<id>/media/…`, `/static/` media) are not and must always bypass; `/courses/` must not be confused with `/api/courses/`, which is private.
- Forward the `Cookie`, `Origin` and `Sec-Fetch-Site` headers unchanged (cross-origin protection depends on them) and preserve `Host`.
- After a deployment, pages name the new versions; the old ones answer `404`, so no purge is needed. A browser holding an old page (pages are never stored, so only an open tab) may miss its assets until reloaded.
- The proxy must not rewrite `Cache-Control` on any path. Check after configuring: `curl -sI https://<host>/learn` shows `private, no-store` and no edge cache hit; an asset URL copied from a page's source shows `public, max-age=31536000, immutable` and, on a second request, an edge hit (for Cloudflare, `cf-cache-status: HIT`); the same path with another version is `404`.
