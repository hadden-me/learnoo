-- Learnoo database roles (LRN-010). Idempotent; run as a superuser with psql:
--
--   psql -X -v ON_ERROR_STOP=1 -d postgres \
--        -v owner_role=learnoo_owner -v runtime_role=learnoo_app -v dbname=learnoo \
--        [-v maintenance_db=postgres] -f deploy/db/roles.sql
--
-- Creates (or corrects) two login roles and the database:
--   owner_role   owns the database, the schema objects and the SECURITY DEFINER
--                trigger functions; runs migrations. Never used by the server.
--   runtime_role used by `server serve`. Owns nothing, is not a member of the
--                owner, has no TEMP on the database and no CREATE on any
--                schema, so it cannot create tables, functions or triggers and
--                cannot disable the guards (docs/DECISIONS.md, threat model).
-- It sets no password: set them separately (see deploy/db/initdb.sh or
-- `\password` in psql) so no secret is written in this file or its arguments.
-- It ends by running grants.sql, which must also be re-run after every
-- migration (it grants on the tables that exist at that time).

\set ON_ERROR_STOP on
\if :{?owner_role}
\else
\set owner_role learnoo_owner
\endif
\if :{?runtime_role}
\else
\set runtime_role learnoo_app
\endif
\if :{?dbname}
\else
\set dbname learnoo
\endif
-- The maintenance database the roles must not use (PUBLIC can CONNECT to it
-- and create TEMP objects by default). Superusers keep their access.
\if :{?maintenance_db}
\else
\set maintenance_db postgres
\endif

SELECT set_config('learnoo.owner_role', :'owner_role', false) AS o,
       set_config('learnoo.runtime_role', :'runtime_role', false) AS r \gset ignored_

DO $$
DECLARE
    owner_role   text := current_setting('learnoo.owner_role');
    runtime_role text := current_setting('learnoo.runtime_role');
BEGIN
    IF NOT (SELECT rolsuper FROM pg_catalog.pg_roles WHERE rolname = current_user) THEN
        RAISE EXCEPTION 'roles.sql must run as a superuser';
    END IF;
    IF owner_role = runtime_role THEN
        RAISE EXCEPTION 'owner_role and runtime_role must differ';
    END IF;
    IF owner_role = current_user OR runtime_role = current_user THEN
        RAISE EXCEPTION 'owner_role and runtime_role must not be the superuser running this script';
    END IF;
END
$$;

SELECT format('CREATE ROLE %I', :'owner_role')
 WHERE NOT EXISTS (SELECT 1 FROM pg_catalog.pg_roles WHERE rolname = :'owner_role') \gexec
SELECT format('CREATE ROLE %I', :'runtime_role')
 WHERE NOT EXISTS (SELECT 1 FROM pg_catalog.pg_roles WHERE rolname = :'runtime_role') \gexec

ALTER ROLE :"owner_role"   LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS INHERIT;
ALTER ROLE :"runtime_role" LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS NOINHERIT;

-- The runtime role must not gain the owner's rights through membership, in
-- either direction or through any other role.
SELECT format('REVOKE %I FROM %I', r.rolname, m.rolname)
  FROM pg_catalog.pg_auth_members am
  JOIN pg_catalog.pg_roles r ON r.oid = am.roleid
  JOIN pg_catalog.pg_roles m ON m.oid = am.member
 WHERE m.rolname = :'runtime_role' \gexec

SELECT format('CREATE DATABASE %I OWNER %I', :'dbname', :'owner_role')
 WHERE NOT EXISTS (SELECT 1 FROM pg_catalog.pg_database WHERE datname = :'dbname') \gexec
ALTER DATABASE :"dbname" OWNER TO :"owner_role";

-- PUBLIC has CONNECT and TEMP on new databases by default. Keep only CONNECT
-- for the runtime role; the owner has every database privilege implicitly.
REVOKE ALL ON DATABASE :"dbname" FROM PUBLIC;
REVOKE ALL ON DATABASE :"dbname" FROM :"runtime_role";
GRANT CONNECT ON DATABASE :"dbname" TO :"runtime_role";
SELECT format('REVOKE CONNECT, TEMPORARY ON DATABASE %I FROM PUBLIC', :'maintenance_db')
 WHERE :'maintenance_db' <> :'dbname'
   AND EXISTS (SELECT 1 FROM pg_catalog.pg_database WHERE datname = :'maintenance_db') \gexec

\connect :"dbname"

-- The public schema is owned by pg_database_owner (PostgreSQL 15+), that is
-- the owner role. Only USAGE for the runtime role; nothing for PUBLIC.
REVOKE ALL ON SCHEMA public FROM PUBLIC;
REVOKE ALL ON SCHEMA public FROM :"runtime_role";
GRANT USAGE ON SCHEMA public TO :"runtime_role";

-- No other schema may grant CREATE to the runtime role or PUBLIC.
SELECT format('REVOKE CREATE ON SCHEMA %I FROM PUBLIC, %I', n.nspname, :'runtime_role')
  FROM pg_catalog.pg_namespace n
 WHERE n.nspname NOT LIKE 'pg\_%' AND n.nspname <> 'information_schema' \gexec

\ir grants.sql
