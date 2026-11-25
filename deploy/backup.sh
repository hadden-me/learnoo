#!/usr/bin/env bash
# Learnoo backup (LRN-010): a PostgreSQL custom-format dump plus a tar of the
# media directory, with SHA-256 checksums. Writes one new directory
#   $LEARNOO_BACKUP_DIR/learnoo-<UTC timestamp>/
#     database.dump       pg_dump --format=custom
#     media.tar           every stored media file (temporary .tmp-* excluded)
#     media-files.sha256  SHA-256 of each file in media.tar
#     info.txt            format, time, tool versions, latest migration
#     MANIFEST.sha256     SHA-256 of the four files above
# and prints its path. Nothing is deleted or rotated: retention, scheduling,
# encryption and the off-site copy are the infrastructure maintainer's
# (docs/DEPLOYMENT.md). The backup contains personal data (emails, names,
# password hashes, sessions): keep it encrypted at rest and access-controlled.
#
# Environment:
#   LEARNOO_BACKUP_DATABASE_URL  libpq URL of the database, as the owner role
#                                (no password in it; see the next variable)
#   LEARNOO_DB_PASSWORD_FILE     optional file holding that role's password
#   LEARNOO_BACKUP_MEDIA_DIR     the media directory (LEARNOO_MEDIA_DIR)
#   LEARNOO_BACKUP_DIR           parent directory for the backup
#
# Consistency: the database is dumped first, then the media files are listed.
# Media files are immutable and written before the row that names them
# commits, so every file a dumped row references is already on disk when the
# listing starts. Files that appear later are only unreferenced extras, which
# verification reports.
set -euo pipefail
umask 077

die() { echo "backup: $*" >&2; exit 1; }
: "${LEARNOO_BACKUP_DATABASE_URL:?set LEARNOO_BACKUP_DATABASE_URL}"
: "${LEARNOO_BACKUP_MEDIA_DIR:?set LEARNOO_BACKUP_MEDIA_DIR}"
: "${LEARNOO_BACKUP_DIR:?set LEARNOO_BACKUP_DIR}"
[ -d "$LEARNOO_BACKUP_MEDIA_DIR" ] || die "media directory $LEARNOO_BACKUP_MEDIA_DIR does not exist"
[ -d "$LEARNOO_BACKUP_DIR" ] || die "backup directory $LEARNOO_BACKUP_DIR does not exist"
if [ -n "${LEARNOO_DB_PASSWORD_FILE:-}" ]; then
	[ -r "$LEARNOO_DB_PASSWORD_FILE" ] || die "cannot read LEARNOO_DB_PASSWORD_FILE"
	PGPASSWORD="$(cat "$LEARNOO_DB_PASSWORD_FILE")"
	export PGPASSWORD
fi

sha256() {
	if command -v sha256sum >/dev/null 2>&1; then sha256sum "$@"; else shasum -a 256 "$@"; fi
}

name="learnoo-$(date -u +%Y%m%dT%H%M%SZ)"
final="$LEARNOO_BACKUP_DIR/$name"
work="$final.partial"
if [ -e "$final" ] || [ -e "$work" ]; then die "$final already exists"; fi
mkdir "$work"
trap 'rm -rf "$work"' EXIT

pg_dump --format=custom --no-password --file="$work/database.dump" --dbname="$LEARNOO_BACKUP_DATABASE_URL"
migration="$(psql -X -At --no-password --dbname="$LEARNOO_BACKUP_DATABASE_URL" \
	-c 'SELECT max(name) FROM schema_migrations')"

# List regular files (relative, sorted, NUL-separated), hash them, then
# archive exactly that list. Temporary files of interrupted writes are
# skipped, and so are names outside [A-Za-z0-9._-] path segments: stored
# media keys always fit, and restore.sh refuses any other name. Skipped
# files cannot be referenced by the database; they are counted in info.txt.
list="$work/.files"
skipped=0
: >"$list"
while IFS= read -r -d '' f; do
	if [[ "$f" =~ ^\./[A-Za-z0-9._-]+(/[A-Za-z0-9._-]+)*$ ]] && [[ ! "$f" =~ (^|/)\.\.(/|$) ]]; then
		printf '%s\0' "$f" >>"$list"
	else
		skipped=$((skipped + 1))
	fi
done < <(cd "$LEARNOO_BACKUP_MEDIA_DIR" && find . -type f ! -name '.tmp-*' -print0 | LC_ALL=C sort -z)
if [ "$skipped" -gt 0 ]; then
	echo "backup: skipped $skipped media file(s) with unsupported names (not referenced by any row)" >&2
fi
if [ -s "$list" ]; then
	(cd "$LEARNOO_BACKUP_MEDIA_DIR" && xargs -0 sh -c '
		if command -v sha256sum >/dev/null 2>&1; then sha256sum "$@"; else shasum -a 256 "$@"; fi' sh) \
		<"$list" >"$work/media-files.sha256"
	tar -C "$LEARNOO_BACKUP_MEDIA_DIR" --null -T "$list" -cf "$work/media.tar"
else
	: >"$work/media-files.sha256"
	tar -C "$LEARNOO_BACKUP_MEDIA_DIR" -cf "$work/media.tar" --files-from /dev/null
fi
rm "$list"

{
	echo "format: learnoo-backup-1"
	echo "created_at: $(date -u +%Y-%m-%dT%H:%M:%SZ)"
	echo "latest_migration: $migration"
	echo "media_files: $(wc -l <"$work/media-files.sha256" | tr -d ' ')"
	echo "media_files_skipped: $skipped"
	echo "pg_dump: $(pg_dump --version)"
} >"$work/info.txt"

(cd "$work" && sha256 database.dump media.tar media-files.sha256 info.txt >MANIFEST.sha256)
chmod 0400 "$work"/*
mv "$work" "$final"
trap - EXIT
echo "$final"
