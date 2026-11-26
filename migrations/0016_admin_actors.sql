-- LMS-B10 (issue #100): signed-in administrators as actors of catalogue
-- display-name and order changes and of student invitations, so the
-- administration pages can use the stores the operator commands use, with
-- the administrator recorded as actor_id instead of a self-asserted operator
-- label. Numbered 0016 because 0014 (courses, B03) and 0015 (course review,
-- B04) are reserved by slices in flight; it depends on neither.
--
-- Conventions of 0004, 0008, 0010 and 0012 apply: guards raise SQLSTATE
-- LRN01 and are named zz_guard_*; every function pins search_path to
-- pg_catalog, pg_temp and reaches other tables only through the schema of the
-- table that fired it (TG_TABLE_SCHEMA, EXECUTE format('%I...') with bound
-- parameters); no function is SECURITY DEFINER. The guards bind the
-- application's least-privilege role, not a superuser or table owner.
--
-- Rules kept here:
--
--   * Exactly one of actor_id (a signed-in administrator, checked by the
--     application inside the same transaction with permissions.RequireTx)
--     and operator_label (a local operator tool) names who made the change,
--     as in permission_audit (0002), account_status_audit (0003) and
--     curriculum_audit (0013). Existing rows all carry an operator label
--     and keep it.
--   * Only renames and reorders may name an actor_id: adding subjects and
--     chapters stays an operator command.
--   * An invitation's issuer (operator_label or issued_by) is part of its
--     immutable identity, and the issue audit row copies it.

ALTER TABLE catalogue_audit
    ADD COLUMN actor_id bigint REFERENCES users (id),
    ALTER COLUMN operator_label DROP NOT NULL,
    ADD CONSTRAINT catalogue_audit_actor CHECK ((actor_id IS NULL) = (operator_label IS NOT NULL)),
    ADD CONSTRAINT catalogue_audit_actor_scope CHECK (
        actor_id IS NULL OR action IN ('rename_subject', 'rename_chapter', 'reorder_subject', 'reorder_chapter'));

ALTER TABLE student_invitations
    ADD COLUMN issued_by bigint REFERENCES users (id),
    ALTER COLUMN operator_label DROP NOT NULL,
    ADD CONSTRAINT student_invitations_issuer CHECK ((issued_by IS NULL) = (operator_label IS NOT NULL));

ALTER TABLE student_invitation_audit
    ADD COLUMN actor_id bigint REFERENCES users (id),
    DROP CONSTRAINT student_invitation_audit_actor,
    ADD CONSTRAINT student_invitation_audit_actor CHECK (
        (action = 'issue' AND (operator_label IS NULL) <> (actor_id IS NULL) AND user_id IS NULL)
        OR (action = 'accept' AND operator_label IS NULL AND actor_id IS NULL AND user_id IS NOT NULL));

-- 0008's guard with issued_by added to the immutable identity.
CREATE OR REPLACE FUNCTION student_invitations_guard() RETURNS trigger
LANGUAGE plpgsql SET search_path = pg_catalog, pg_temp AS $$
DECLARE
    account_email text;
BEGIN
    IF TG_OP = 'INSERT' THEN
        IF NEW.accepted_at IS NOT NULL OR NEW.accepted_user_id IS NOT NULL THEN
            RAISE EXCEPTION 'an invitation is created unaccepted' USING ERRCODE = 'LRN01';
        END IF;
        RETURN NEW;
    END IF;
    IF TG_OP <> 'UPDATE' THEN
        RAISE EXCEPTION 'student invitations are never deleted' USING ERRCODE = 'LRN01';
    END IF;
    IF (NEW.id, NEW.token_hash, NEW.email, NEW.operator_label, NEW.issued_by, NEW.created_at, NEW.expires_at)
       IS DISTINCT FROM (OLD.id, OLD.token_hash, OLD.email, OLD.operator_label, OLD.issued_by, OLD.created_at, OLD.expires_at) THEN
        RAISE EXCEPTION 'invitation identity is immutable' USING ERRCODE = 'LRN01';
    END IF;
    IF OLD.accepted_at IS NOT NULL THEN
        RAISE EXCEPTION 'invitation % was already accepted', OLD.id USING ERRCODE = 'LRN01';
    END IF;
    IF NEW.accepted_at IS DISTINCT FROM now() OR now() >= OLD.expires_at THEN
        RAISE EXCEPTION 'invitation % is accepted only before it expires, at the transaction time', OLD.id USING ERRCODE = 'LRN01';
    END IF;
    EXECUTE format('SELECT email FROM %I.users WHERE id = $1', TG_TABLE_SCHEMA)
       INTO account_email USING NEW.accepted_user_id;
    IF account_email IS DISTINCT FROM OLD.email THEN
        RAISE EXCEPTION 'invitation % must be accepted by an account with its email', OLD.id USING ERRCODE = 'LRN01';
    END IF;
    RETURN NEW;
END;
$$;

-- 0008's audit trigger, recording the issuing administrator as actor_id.
CREATE OR REPLACE FUNCTION student_invitations_audit() RETURNS trigger
LANGUAGE plpgsql SET search_path = pg_catalog, pg_temp AS $$
BEGIN
    IF TG_OP = 'INSERT' THEN
        EXECUTE format('INSERT INTO %I.student_invitation_audit (invitation_id, action, operator_label, actor_id) VALUES ($1, $2, $3, $4)', TG_TABLE_SCHEMA)
           USING NEW.id, 'issue', NEW.operator_label, NEW.issued_by;
    ELSIF OLD.accepted_at IS NULL AND NEW.accepted_at IS NOT NULL THEN
        EXECUTE format('INSERT INTO %I.student_invitation_audit (invitation_id, action, user_id) VALUES ($1, $2, $3)', TG_TABLE_SCHEMA)
           USING NEW.id, 'accept', NEW.accepted_user_id;
    END IF;
    RETURN NULL;
END;
$$;
