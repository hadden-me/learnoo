#!/bin/sh
# First-start initialisation for the postgres container (LRN-010), mounted
# into /docker-entrypoint-initdb.d. Runs once, when the data volume is empty,
# as the bootstrap superuser over the local socket. Creates the owner and
# runtime roles and the database (roles.sql), then sets the two login
# passwords from the mounted secret files. Passwords are read by psql itself
# (backquote expansion), so they never appear in arguments or the environment.
set -eu

: "${LEARNOO_DB_NAME:?}" "${LEARNOO_OWNER_ROLE:?}" "${LEARNOO_RUNTIME_ROLE:?}"
: "${LEARNOO_OWNER_PASSWORD_FILE:?}" "${LEARNOO_RUNTIME_PASSWORD_FILE:?}"
for f in "$LEARNOO_OWNER_PASSWORD_FILE" "$LEARNOO_RUNTIME_PASSWORD_FILE"; do
	if [ ! -s "$f" ]; then
		echo "initdb: password file $f is missing or empty" >&2
		exit 1
	fi
done

psql -X -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname postgres \
	-v owner_role="$LEARNOO_OWNER_ROLE" -v runtime_role="$LEARNOO_RUNTIME_ROLE" \
	-v dbname="$LEARNOO_DB_NAME" -f /learnoo-db/roles.sql

psql -X -q -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname postgres \
	-v owner_role="$LEARNOO_OWNER_ROLE" -v runtime_role="$LEARNOO_RUNTIME_ROLE" <<'SQL'
\set owner_pw `cat "$LEARNOO_OWNER_PASSWORD_FILE"`
\set runtime_pw `cat "$LEARNOO_RUNTIME_PASSWORD_FILE"`
SET password_encryption = 'scram-sha-256';
ALTER ROLE :"owner_role" PASSWORD :'owner_pw';
ALTER ROLE :"runtime_role" PASSWORD :'runtime_pw';
\unset owner_pw
\unset runtime_pw
SQL
echo "initdb: roles $LEARNOO_OWNER_ROLE (owner) and $LEARNOO_RUNTIME_ROLE (runtime) ready for database $LEARNOO_DB_NAME"
