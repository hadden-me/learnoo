-- LRN-005 permissions (role model confirmed 2026-11-24, docs/DECISIONS.md "Role model").
-- Four independent permissions. author, publisher and administrator are global;
-- reviewer is granted per subject. No permission implies another.
--
-- The append-only triggers on permission_audit constrain only roles that
-- neither own the table nor are superusers: the owner can drop or disable
-- the triggers, and a superuser can also skip them (session_replication_role).
-- The deployed application must therefore connect as a least-privilege role
-- that does not own these tables (deployment work, LRN-010).
-- Grant and audit times default to now(), the transaction time.

-- Minimal subject identifier, only so reviewer grants have a scope to reference.
-- LRN-014 extends it (section, names, seed). No rows are seeded here.
CREATE TABLE subjects (
    id         bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    code       text        NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT subjects_code_format CHECK (code ~ '^[a-z0-9][a-z0-9-]{0,62}$')
);

CREATE UNIQUE INDEX subjects_code_key ON subjects (code);

CREATE TABLE user_permissions (
    user_id    bigint      NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    permission text        NOT NULL,
    granted_at timestamptz NOT NULL DEFAULT now(),
    -- NULL means the grant was made by an operator tool, not a signed-in user.
    granted_by bigint      REFERENCES users (id) ON DELETE SET NULL,
    PRIMARY KEY (user_id, permission),
    CONSTRAINT user_permissions_global CHECK (permission IN ('author', 'publisher', 'administrator'))
);

CREATE TABLE reviewer_assignments (
    user_id    bigint      NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    subject_id bigint      NOT NULL REFERENCES subjects (id) ON DELETE RESTRICT,
    granted_at timestamptz NOT NULL DEFAULT now(),
    granted_by bigint      REFERENCES users (id) ON DELETE SET NULL,
    PRIMARY KEY (user_id, subject_id)
);

CREATE INDEX reviewer_assignments_subject_idx ON reviewer_assignments (subject_id);

-- Append-only record of every grant and revoke. Users and subjects referenced
-- here cannot be deleted while audit rows exist; deletion policy is LRN-022.
CREATE TABLE permission_audit (
    id             bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    occurred_at    timestamptz NOT NULL DEFAULT now(),
    actor_id       bigint      REFERENCES users (id),
    target_user_id bigint      NOT NULL REFERENCES users (id),
    action         text        NOT NULL CHECK (action IN ('grant', 'revoke')),
    permission     text        NOT NULL CHECK (permission IN ('author', 'reviewer', 'publisher', 'administrator')),
    subject_id     bigint      REFERENCES subjects (id),
    CONSTRAINT permission_audit_scope CHECK ((permission = 'reviewer') = (subject_id IS NOT NULL))
);

CREATE INDEX permission_audit_target_idx ON permission_audit (target_user_id, occurred_at);

CREATE FUNCTION permission_audit_append_only() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'permission_audit is append-only';
END;
$$;

CREATE TRIGGER permission_audit_no_update_delete
    BEFORE UPDATE OR DELETE ON permission_audit
    FOR EACH ROW EXECUTE FUNCTION permission_audit_append_only();

CREATE TRIGGER permission_audit_no_truncate
    BEFORE TRUNCATE ON permission_audit
    FOR EACH STATEMENT EXECUTE FUNCTION permission_audit_append_only();
