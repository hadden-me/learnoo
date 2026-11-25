#!/usr/bin/env bash
# Learnoo restore (LRN-010) of a backup made by deploy/backup.sh into an
# EMPTY database and an EMPTY media directory, for a rehearsal in an isolated
# environment or a real recovery. It never overwrites: it refuses a database
# that already has tables or a media directory that has files.
#
#   deploy/restore.sh /path/to/learnoo-<timestamp>
#
# Steps, in this order:
#   1. check MANIFEST.sha256 and the backup format;
#   2. refuse a non-empty database or media directory;
#   3. validate media.tar without extracting: every entry must be a regular
#      file listed in media-files.sha256 or a directory, with a plain relative
#      name (no "..", no absolute path, no links or devices);
#   4. extract into a staging directory inside the target (".tmp-restore-*")
#      and check every file against media-files.sha256, with no extra file;
#   5. pg_restore as the owner role in one transaction (objects owned by that
#      role, privileges as dumped);
#   6. move the staged files into place, set ownership and modes, and re-run
#      deploy/db/grants.sql if LEARNOO_RUNTIME_ROLE is set.
# Then run `server verify-restore -strict` against the result.
#
# Failures: steps 1-4 leave the database untouched and remove the staging
# directory. A pg_restore failure rolls its transaction back (empty database
# again). A failure in step 6 leaves a restored database with incomplete
# media: drop and recreate the database (or remove the rehearsal project's
# volumes), empty the media directory, and run the restore again.
#
# Prerequisite: the target database and the owner and runtime roles exist,
# created by deploy/db/roles.sql in the target cluster (same role names as
# the source; privileges in the dump name them).
#
# Environment:
#   LEARNOO_RESTORE_DATABASE_URL  libpq URL of the empty target database, as
#                                 the owner role (no password in it)
#   LEARNOO_DB_PASSWORD_FILE      optional file holding that role's password
#   LEARNOO_RESTORE_MEDIA_DIR     target media directory (created if absent)
#   LEARNOO_MEDIA_OWNER           optional uid:gid for the restored files and
#                                 directories (the app container: 65532:65532)
#   LEARNOO_RUNTIME_ROLE          optional; if set, grants.sql is re-run for it
set -euo pipefail
umask 027

die() { echo "restore: $*" >&2; exit 1; }
[ $# -eq 1 ] || die "usage: restore.sh BACKUP_DIRECTORY"
backup="$(cd "$1" && pwd)" || die "no backup directory $1"
: "${LEARNOO_RESTORE_DATABASE_URL:?set LEARNOO_RESTORE_DATABASE_URL}"
: "${LEARNOO_RESTORE_MEDIA_DIR:?set LEARNOO_RESTORE_MEDIA_DIR}"
if [ -n "${LEARNOO_DB_PASSWORD_FILE:-}" ]; then
	[ -r "$LEARNOO_DB_PASSWORD_FILE" ] || die "cannot read LEARNOO_DB_PASSWORD_FILE"
	PGPASSWORD="$(cat "$LEARNOO_DB_PASSWORD_FILE")"
	export PGPASSWORD
fi
here="$(cd "$(dirname "$0")" && pwd)"

sha256() {
	if command -v sha256sum >/dev/null 2>&1; then sha256sum "$@"; else shasum -a 256 "$@"; fi
}

# 1. Integrity of the backup itself.
for f in MANIFEST.sha256 database.dump media.tar media-files.sha256 info.txt; do
	[ -f "$backup/$f" ] || die "backup is incomplete: $f is missing"
done
(cd "$backup" && sha256 -c --quiet MANIFEST.sha256) || die "checksum mismatch in $backup; not restoring"
grep -qx 'format: learnoo-backup-1' "$backup/info.txt" || die "unknown backup format"

# 2. Never restore over existing data.
psqlq() { psql -X -At -v ON_ERROR_STOP=1 --no-password --dbname="$LEARNOO_RESTORE_DATABASE_URL" "$@"; }
tables="$(psqlq -c "SELECT count(*) FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
                    WHERE n.nspname NOT IN ('pg_catalog', 'information_schema') AND n.nspname NOT LIKE 'pg\_%'
                      AND c.relkind IN ('r', 'p', 'v', 'm', 'S', 'f')")"
[ "$tables" = "0" ] || die "target database is not empty ($tables relations); refusing to restore"
mkdir -p "$LEARNOO_RESTORE_MEDIA_DIR"
media="$(cd "$LEARNOO_RESTORE_MEDIA_DIR" && pwd)"
[ -z "$(ls -A "$media")" ] || die "target media directory $media is not empty; refusing to restore"

# 3. Validate the archive before extracting anything.
listed="$(mktemp)"
trap 'rm -f "$listed"' EXIT
safe_name() { [[ "$1" =~ ^\./[A-Za-z0-9._-]+(/[A-Za-z0-9._-]+)*/?$ ]] && [[ ! "$1" =~ (^|/)\.\.(/|$) ]]; }
while IFS= read -r line; do
	name="${line#*  }"
	safe_name "$name" || die "media-files.sha256 lists an unsafe name; not restoring"
	printf '%s\n' "$name" >>"$listed"
done <"$backup/media-files.sha256"
names="$(tar -tf "$backup/media.tar")"
types="$(tar -tvf "$backup/media.tar" | cut -c1)"
[ "$(printf '%s\n' "$names" | grep -c .)" = "$(printf '%s\n' "$types" | grep -c .)" ] ||
	die "cannot list media.tar reliably; not restoring"
while IFS=$'\t' read -r type name; do
	[ -n "$name" ] || continue
	case "$type" in
	d) [ "$name" = "./" ] || safe_name "$name" || die "media.tar has an unsafe directory entry; not restoring" ;;
	-) if ! safe_name "$name" || ! grep -qxF -- "$name" "$listed"; then
		die "media.tar has an entry not listed in media-files.sha256: $name; not restoring"
	fi ;;
	*) die "media.tar has an entry that is not a regular file or directory: $name; not restoring" ;;
	esac
done < <(paste <(printf '%s\n' "$types") <(printf '%s\n' "$names"))

# 4. Extract into staging inside the target and check every file.
staging="$(mktemp -d "$media/.tmp-restore-XXXXXX")"
trap 'rm -f "$listed"; chmod -R u+w "$staging" 2>/dev/null; rm -rf "$staging"' EXIT
tar -C "$staging" --no-same-owner -xf "$backup/media.tar"
if [ -s "$backup/media-files.sha256" ]; then
	(cd "$staging" && sha256 -c --quiet "$backup/media-files.sha256") || die "media files do not match media-files.sha256; not restoring"
fi
extra="$(cd "$staging" && { find . ! -type d | LC_ALL=C sort | grep -vxF -f "$listed" || :; })"
[ -z "$extra" ] || die "media.tar produced unlisted files; not restoring"

# 5. Database, in one transaction: all or nothing.
pg_restore --no-password --exit-on-error --single-transaction --no-owner \
	--dbname="$LEARNOO_RESTORE_DATABASE_URL" "$backup/database.dump"

# 6. Move the media into place.
find "$staging" -mindepth 1 -maxdepth 1 -exec mv {} "$media"/ \;
rmdir "$staging"
trap 'rm -f "$listed"' EXIT
if [ -n "${LEARNOO_MEDIA_OWNER:-}" ]; then
	chown -R "$LEARNOO_MEDIA_OWNER" "$media"
fi
find "$media" -type d -exec chmod 0750 {} +
find "$media" -type f -exec chmod 0444 {} +

# 7. Runtime grants for the restored tables (idempotent).
if [ -n "${LEARNOO_RUNTIME_ROLE:-}" ]; then
	psqlq -q -v runtime_role="$LEARNOO_RUNTIME_ROLE" -f "$here/db/grants.sql"
fi

echo "restored $(basename "$backup") ($(grep '^latest_migration:' "$backup/info.txt"))"
echo "next: run 'server verify-restore -strict' with LEARNOO_DATABASE_URL and LEARNOO_MEDIA_DIR=$media"
