-- Learnoo runtime-role grants (LRN-010). Idempotent. Run with psql, connected
-- to the application database as the owner role (or a superuser), after
-- roles.sql and after EVERY migration, since it grants on existing tables:
--
--   psql -X -v ON_ERROR_STOP=1 -d learnoo -U learnoo_owner \
--        -v runtime_role=learnoo_app -f deploy/db/grants.sql
--
-- Grants exactly: SELECT, INSERT, UPDATE, DELETE on the application tables
-- (UPDATE is also what the guards' FOR SHARE/FOR UPDATE row locks need, e.g.
-- on reviewer_assignments, user_permissions and users), SELECT only on views
-- and on schema_migrations (the startup check reads it), and USAGE on
-- sequences. Never TRUNCATE (statement-level, and the audit tables refuse it
-- only through triggers), REFERENCES or TRIGGER. SECURITY DEFINER functions
-- lose EXECUTE for PUBLIC and the runtime role (firing a trigger needs none).
-- It then checks the runtime role's privileges and fails if anything is
-- wider than this, including any role membership (pg_auth_members).

\set ON_ERROR_STOP on
\if :{?runtime_role}
\else
\set runtime_role learnoo_app
\endif

SELECT set_config('learnoo.runtime_role', :'runtime_role', false) AS r \gset ignored_

REVOKE ALL ON ALL TABLES IN SCHEMA public FROM PUBLIC;
REVOKE ALL ON ALL TABLES IN SCHEMA public FROM :"runtime_role";
GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO :"runtime_role";

REVOKE ALL ON ALL SEQUENCES IN SCHEMA public FROM PUBLIC;
REVOKE ALL ON ALL SEQUENCES IN SCHEMA public FROM :"runtime_role";
GRANT USAGE ON ALL SEQUENCES IN SCHEMA public TO :"runtime_role";

DO $$
DECLARE
    runtime_role text := current_setting('learnoo.runtime_role');
    obj record;
BEGIN
    -- Read-only objects: views and the migration history.
    FOR obj IN
        SELECT c.oid::regclass AS rel
          FROM pg_catalog.pg_class c
         WHERE c.relnamespace = 'public'::regnamespace
           AND (c.relkind IN ('v', 'm') OR c.relname = 'schema_migrations')
    LOOP
        EXECUTE format('REVOKE INSERT, UPDATE, DELETE ON %s FROM %I', obj.rel, runtime_role);
    END LOOP;
    -- SECURITY DEFINER functions run as their owner; nobody else may call them.
    FOR obj IN
        SELECT p.oid::regprocedure AS fn
          FROM pg_catalog.pg_proc p
         WHERE p.pronamespace = 'public'::regnamespace AND p.prosecdef
    LOOP
        EXECUTE format('REVOKE EXECUTE ON FUNCTION %s FROM PUBLIC, %I', obj.fn, runtime_role);
    END LOOP;
END
$$;

-- Verification: fail loudly if the runtime role could weaken the guards.
DO $$
DECLARE
    runtime_role text := current_setting('learnoo.runtime_role');
    rid oid;
    bad text;
BEGIN
    SELECT oid INTO rid FROM pg_catalog.pg_roles WHERE rolname = runtime_role;
    IF rid IS NULL THEN
        RAISE EXCEPTION 'runtime role % does not exist', runtime_role;
    END IF;
    IF EXISTS (SELECT 1 FROM pg_catalog.pg_roles WHERE oid = rid
                AND (rolsuper OR rolcreatedb OR rolcreaterole OR rolreplication OR rolbypassrls)) THEN
        RAISE EXCEPTION 'runtime role % has a role attribute it must not have', runtime_role;
    END IF;
    IF has_database_privilege(rid, current_database(), 'CREATE')
       OR has_database_privilege(rid, current_database(), 'TEMPORARY') THEN
        RAISE EXCEPTION 'runtime role % has CREATE or TEMP on database %; run roles.sql', runtime_role, current_database();
    END IF;
    SELECT string_agg(nspname, ', ') INTO bad FROM pg_catalog.pg_namespace
     WHERE has_schema_privilege(rid, oid, 'CREATE');
    IF bad IS NOT NULL THEN
        RAISE EXCEPTION 'runtime role % has CREATE on schema(s) %', runtime_role, bad;
    END IF;
    SELECT string_agg(x, ', ') INTO bad FROM (
        SELECT 'database ' || datname AS x FROM pg_catalog.pg_database WHERE datdba = rid
        UNION ALL SELECT 'schema ' || nspname FROM pg_catalog.pg_namespace WHERE nspowner = rid
        UNION ALL SELECT 'relation ' || oid::regclass::text FROM pg_catalog.pg_class WHERE relowner = rid
        UNION ALL SELECT 'function ' || oid::regprocedure::text FROM pg_catalog.pg_proc WHERE proowner = rid
        UNION ALL SELECT 'type ' || oid::regtype::text FROM pg_catalog.pg_type WHERE typowner = rid
    ) owned;
    IF bad IS NOT NULL THEN
        RAISE EXCEPTION 'runtime role % owns objects: %', runtime_role, bad;
    END IF;
    SELECT string_agg(r.rolname, ', ') INTO bad
      FROM pg_catalog.pg_auth_members am JOIN pg_catalog.pg_roles r ON r.oid = am.roleid
     WHERE am.member = rid;
    IF bad IS NOT NULL THEN
        RAISE EXCEPTION 'runtime role % must be a member of no role, but is a member of %; run roles.sql', runtime_role, bad;
    END IF;
    SELECT string_agg(DISTINCT r.rolname, ', ') INTO bad
      FROM pg_catalog.pg_class c JOIN pg_catalog.pg_roles r ON r.oid = c.relowner
     WHERE c.relnamespace = 'public'::regnamespace AND pg_has_role(rid, c.relowner, 'MEMBER');
    IF bad IS NOT NULL THEN
        RAISE EXCEPTION 'runtime role % is a member of table owner(s) %', runtime_role, bad;
    END IF;
    SELECT string_agg(c.oid::regclass::text, ', ') INTO bad FROM pg_catalog.pg_class c
     WHERE c.relnamespace = 'public'::regnamespace AND c.relkind IN ('r', 'p', 'v', 'm')
       AND (has_table_privilege(rid, c.oid, 'TRUNCATE') OR has_table_privilege(rid, c.oid, 'TRIGGER')
            OR has_table_privilege(rid, c.oid, 'REFERENCES'));
    IF bad IS NOT NULL THEN
        RAISE EXCEPTION 'runtime role % has TRUNCATE, TRIGGER or REFERENCES on %', runtime_role, bad;
    END IF;
    SELECT string_agg(p.oid::regprocedure::text, ', ') INTO bad FROM pg_catalog.pg_proc p
     WHERE p.pronamespace = 'public'::regnamespace AND p.prosecdef
       AND (has_function_privilege(rid, p.oid, 'EXECUTE')
            OR EXISTS (SELECT 1 FROM aclexplode(coalesce(p.proacl, acldefault('f', p.proowner))) a
                        WHERE a.grantee = 0 AND a.privilege_type = 'EXECUTE'));
    IF bad IS NOT NULL THEN
        RAISE EXCEPTION 'SECURITY DEFINER function(s) executable by PUBLIC or the runtime role: %', bad;
    END IF;
END
$$;
