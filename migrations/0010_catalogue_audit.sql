-- Studio integration (issue #65): append-only record of every subject and
-- chapter added by the operator commands add-subject and add-chapter.
-- Subjects (0002) and chapters (0004) keep their minimal shape: codes only,
-- no names, ordering or seed rows. Those are LRN-014.
--
-- Conventions of 0004_lessons.sql and later apply: guards raise SQLSTATE
-- LRN01 and are named zz_guard_* so they fire after any trigger that changes
-- NEW; every function pins search_path to pg_catalog, pg_temp and reaches
-- other tables only through the schema of the table that fired it
-- (TG_TABLE_SCHEMA, EXECUTE format('%I...') with bound parameters). No
-- function is SECURITY DEFINER; the guards bind the application's
-- least-privilege role, not a superuser or table owner.
--
-- Rules kept here:
--
--   * Each row records one addition: add_subject names the subject,
--     add_chapter the chapter and its subject (the composite foreign key
--     keeps the pair consistent).
--   * Only the operator tools add catalogue entries in this slice, so the
--     actor is a self-asserted operator label (<os user>@<hostname>), with
--     the same character rule as permission_audit and account_status_audit.
--   * Rows record the transaction time and their own top-level transaction
--     (xact_id), and are append-only: update, delete and truncate are refused.
--   * Every new subject and chapter must be audited: a deferred constraint
--     trigger on subjects and chapters requires, at commit, the matching
--     catalogue_audit row written by the same transaction (as the publication
--     pointer requires its audit row, 0006). A direct INSERT without it fails
--     at commit. Restores are unaffected: pg_restore loads the data before it
--     creates triggers.
--   * A subject's identity (id, code, created_at) never changes, as a
--     chapter's already does not (zz_guard_chapters_identity, 0004): reviewer
--     grants, lessons and the audit name subjects by ID and the Studio and
--     reader by code, so a rename would silently re-scope them. Renaming is
--     LRN-014's to design, with its own audit.

CREATE TABLE catalogue_audit (
    id             bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    occurred_at    timestamptz NOT NULL DEFAULT now(),
    xact_id        xid8        NOT NULL DEFAULT pg_current_xact_id(),
    operator_label text        NOT NULL,
    action         text        NOT NULL,
    subject_id     bigint      NOT NULL REFERENCES subjects (id),
    chapter_id     bigint,
    CONSTRAINT catalogue_audit_action CHECK (action IN ('add_subject', 'add_chapter')),
    CONSTRAINT catalogue_audit_scope CHECK ((action = 'add_chapter') = (chapter_id IS NOT NULL)),
    CONSTRAINT catalogue_audit_chapter_fkey FOREIGN KEY (chapter_id, subject_id)
        REFERENCES chapters (id, subject_id),
    CONSTRAINT catalogue_audit_operator_label CHECK (
        char_length(operator_label) BETWEEN 1 AND 200
        AND operator_label = btrim(operator_label)
        -- The rule of account_status_audit_operator_label (0003), listed by
        -- code point so the check does not depend on the locale.
        AND operator_label !~ E'[\\u0001-\\u001F\\u007F-\\u009F\\u00AD\\u0600-\\u0605\\u061C\\u06DD\\u070F\\u0890-\\u0891\\u08E2\\u180E\\u200B-\\u200F\\u2028-\\u202E\\u2060-\\u2064\\u2066-\\u206F\\uFEFF\\uFFF9-\\uFFFB\\U000110BD\\U000110CD\\U00013430-\\U0001343F\\U0001BCA0-\\U0001BCA3\\U0001D173-\\U0001D17A\\U000E0001\\U000E0020-\\U000E007F]')
);

CREATE INDEX catalogue_audit_subject_idx ON catalogue_audit (subject_id, occurred_at);
CREATE INDEX catalogue_audit_chapter_idx ON catalogue_audit (chapter_id) WHERE chapter_id IS NOT NULL;

-- Insert: at the transaction time, by its own transaction. Update, delete and
-- truncate: never.
CREATE FUNCTION catalogue_audit_guard() RETURNS trigger
LANGUAGE plpgsql SET search_path = pg_catalog, pg_temp AS $$
BEGIN
    IF TG_OP = 'INSERT' THEN
        IF NEW.occurred_at IS DISTINCT FROM now() OR NEW.xact_id IS DISTINCT FROM pg_current_xact_id() THEN
            RAISE EXCEPTION 'a catalogue audit row records its own transaction and its time' USING ERRCODE = 'LRN01';
        END IF;
        RETURN NEW;
    END IF;
    RAISE EXCEPTION 'catalogue_audit is append-only' USING ERRCODE = 'LRN01';
END;
$$;

CREATE TRIGGER zz_guard_catalogue_audit
    BEFORE INSERT OR UPDATE OR DELETE ON catalogue_audit
    FOR EACH ROW EXECUTE FUNCTION catalogue_audit_guard();

CREATE TRIGGER zz_guard_catalogue_audit_no_truncate
    BEFORE TRUNCATE ON catalogue_audit
    FOR EACH STATEMENT EXECUTE FUNCTION catalogue_audit_guard();

-- At commit, a new subject or chapter must have its catalogue_audit row from
-- the same transaction.
CREATE FUNCTION catalogue_entry_audited() RETURNS trigger
LANGUAGE plpgsql SET search_path = pg_catalog, pg_temp AS $$
DECLARE
    audited boolean;
BEGIN
    IF TG_TABLE_NAME = 'subjects' THEN
        EXECUTE format('SELECT EXISTS (SELECT 1 FROM %I.catalogue_audit
                         WHERE action = ''add_subject'' AND subject_id = $1
                           AND xact_id = pg_catalog.pg_current_xact_id())', TG_TABLE_SCHEMA)
           INTO audited USING NEW.id;
    ELSE
        EXECUTE format('SELECT EXISTS (SELECT 1 FROM %I.catalogue_audit
                         WHERE action = ''add_chapter'' AND chapter_id = $1 AND subject_id = $2
                           AND xact_id = pg_catalog.pg_current_xact_id())', TG_TABLE_SCHEMA)
           INTO audited USING NEW.id, NEW.subject_id;
    END IF;
    IF NOT audited THEN
        RAISE EXCEPTION '% % was added without a catalogue_audit row of the same transaction', TG_TABLE_NAME, NEW.id
            USING ERRCODE = 'LRN01';
    END IF;
    RETURN NULL;
END;
$$;

CREATE CONSTRAINT TRIGGER zz_guard_subjects_audited
    AFTER INSERT ON subjects
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION catalogue_entry_audited();

CREATE CONSTRAINT TRIGGER zz_guard_chapters_audited
    AFTER INSERT ON chapters
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION catalogue_entry_audited();

-- Update: a subject's identity is immutable. Deletion stays governed by the
-- foreign keys (an audited subject is always referenced).
CREATE FUNCTION subjects_identity_guard() RETURNS trigger
LANGUAGE plpgsql SET search_path = pg_catalog, pg_temp AS $$
BEGIN
    IF (NEW.id, NEW.code, NEW.created_at) IS DISTINCT FROM (OLD.id, OLD.code, OLD.created_at) THEN
        RAISE EXCEPTION 'subject identity is immutable' USING ERRCODE = 'LRN01';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER zz_guard_subjects_identity
    BEFORE UPDATE ON subjects
    FOR EACH ROW EXECUTE FUNCTION subjects_identity_guard();
