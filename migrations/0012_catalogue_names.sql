-- LRN-014 (issue #86), technical part: French and Arabic display names and
-- a display order for subjects (0002) and chapters (0004). No curriculum
-- data is seeded: names and orders are set by the operator commands
-- set-subject-name, set-chapter-name and set-order once a teacher has
-- confirmed them.
--
-- Conventions of 0004 and 0010 apply: guards raise SQLSTATE LRN01 and are
-- named zz_guard_*; every function pins search_path to pg_catalog, pg_temp
-- and reaches other tables only through the schema of the table that fired
-- it (TG_TABLE_SCHEMA, EXECUTE format('%I...') with bound parameters); no
-- function is SECURITY DEFINER. The guards bind the application's
-- least-privilege role, not a superuser or table owner.
--
-- Rules kept here:
--
--   * Identity stays immutable (zz_guard_subjects_identity, 0010;
--     zz_guard_chapters_identity, 0004): id, code, created_at and a chapter's
--     subject_id. Names (name_fr, name_ar) and sort_order are mutable display
--     metadata.
--   * Names are NULL until set; readers show the code instead. A name is
--     trimmed, 1 to 200 characters, without control (Cc), format (Cf) or
--     line/paragraph separator (Zl, Zp) characters (the operator label rule
--     of 0002/0003/0010). Arabic text is allowed. sort_order is 0 to 9999,
--     0 by default; lists sort by sort_order, then code.
--   * Names are display-only. Lesson revisions store chapter_id and
--     submitted snapshots contain no catalogue name, so renaming a chapter
--     changes how published lessons are labelled without touching reviewed
--     content. It is an administrator catalogue action, and audited.
--   * New entries start without names and with sort_order 0, so every name
--     and order a reader sees went through an audited change.
--   * Every change of names or order is audited by the same transaction in
--     catalogue_audit with the old and new values: rename_subject and
--     rename_chapter record both names, reorder_subject and reorder_chapter
--     the order. Three guards keep the audit exact:
--       1. An audit row's old values must be the entry's current values, and
--          its new values must differ (zz_guard_catalogue_audit). The guard
--          locks the entry (FOR NO KEY UPDATE) until the transaction ends,
--          so two transactions cannot both audit a change from the same
--          values.
--       2. An update that changes the names (the order) of an entry needs
--          the latest rename (reorder) row of the same transaction for that
--          entry to record exactly that change (zz_guard_*_metadata).
--       3. At commit, every rename or reorder row must have been applied:
--          the next row of the same kind, entry and transaction continues
--          from its new values or, for the last one, the entry still has them
--          (zz_guard_catalogue_audit_applied, deferred).
--     A direct UPDATE by the runtime role without its audit row therefore
--     fails, as does an audit row describing a change that did not happen.

-- The name rule, shared by the CHECK constraints below.
CREATE FUNCTION catalogue_valid_name(name text) RETURNS boolean
LANGUAGE sql IMMUTABLE PARALLEL SAFE STRICT
SET search_path = pg_catalog, pg_temp
RETURN char_length(name) BETWEEN 1 AND 200
    AND name = btrim(name)
    -- The rule of catalogue_audit_operator_label (0010), listed by code
    -- point so the check does not depend on the locale.
    AND name !~ E'[\\u0001-\\u001F\\u007F-\\u009F\\u00AD\\u0600-\\u0605\\u061C\\u06DD\\u070F\\u0890-\\u0891\\u08E2\\u180E\\u200B-\\u200F\\u2028-\\u202E\\u2060-\\u2064\\u2066-\\u206F\\uFEFF\\uFFF9-\\uFFFB\\U000110BD\\U000110CD\\U00013430-\\U0001343F\\U0001BCA0-\\U0001BCA3\\U0001D173-\\U0001D17A\\U000E0001\\U000E0020-\\U000E007F]';

ALTER TABLE subjects
    ADD COLUMN name_fr    text,
    ADD COLUMN name_ar    text,
    ADD COLUMN sort_order integer NOT NULL DEFAULT 0,
    ADD CONSTRAINT subjects_name_fr CHECK (catalogue_valid_name(name_fr)),
    ADD CONSTRAINT subjects_name_ar CHECK (catalogue_valid_name(name_ar)),
    ADD CONSTRAINT subjects_sort_order CHECK (sort_order BETWEEN 0 AND 9999);

ALTER TABLE chapters
    ADD COLUMN name_fr    text,
    ADD COLUMN name_ar    text,
    ADD COLUMN sort_order integer NOT NULL DEFAULT 0,
    ADD CONSTRAINT chapters_name_fr CHECK (catalogue_valid_name(name_fr)),
    ADD CONSTRAINT chapters_name_ar CHECK (catalogue_valid_name(name_ar)),
    ADD CONSTRAINT chapters_sort_order CHECK (sort_order BETWEEN 0 AND 9999);

ALTER TABLE catalogue_audit
    ADD COLUMN old_name_fr    text,
    ADD COLUMN new_name_fr    text,
    ADD COLUMN old_name_ar    text,
    ADD COLUMN new_name_ar    text,
    ADD COLUMN old_sort_order integer,
    ADD COLUMN new_sort_order integer,
    DROP CONSTRAINT catalogue_audit_action,
    DROP CONSTRAINT catalogue_audit_scope,
    ADD CONSTRAINT catalogue_audit_action CHECK (action IN (
        'add_subject', 'add_chapter', 'rename_subject', 'rename_chapter', 'reorder_subject', 'reorder_chapter')),
    ADD CONSTRAINT catalogue_audit_scope CHECK (
        (action IN ('add_chapter', 'rename_chapter', 'reorder_chapter')) = (chapter_id IS NOT NULL)),
    -- Additions carry no values; a rename carries both names (old and new)
    -- and changes at least one; a reorder carries a changed order.
    ADD CONSTRAINT catalogue_audit_values CHECK (CASE
        WHEN action IN ('rename_subject', 'rename_chapter') THEN
            old_sort_order IS NULL AND new_sort_order IS NULL
            AND (old_name_fr, old_name_ar) IS DISTINCT FROM (new_name_fr, new_name_ar)
        WHEN action IN ('reorder_subject', 'reorder_chapter') THEN
            num_nulls(old_name_fr, new_name_fr, old_name_ar, new_name_ar) = 4
            AND old_sort_order IS NOT NULL AND new_sort_order IS NOT NULL
            AND old_sort_order <> new_sort_order
        ELSE
            num_nulls(old_name_fr, new_name_fr, old_name_ar, new_name_ar, old_sort_order, new_sort_order) = 6
        END),
    ADD CONSTRAINT catalogue_audit_names CHECK (
        catalogue_valid_name(old_name_fr) AND catalogue_valid_name(new_name_fr)
        AND catalogue_valid_name(old_name_ar) AND catalogue_valid_name(new_name_ar)),
    ADD CONSTRAINT catalogue_audit_sort_order CHECK (
        old_sort_order BETWEEN 0 AND 9999 AND new_sort_order BETWEEN 0 AND 9999);

-- Insert: at the transaction time, by its own transaction; a rename or
-- reorder starts from the entry's current values (rule 1). Update, delete
-- and truncate: never.
CREATE OR REPLACE FUNCTION catalogue_audit_guard() RETURNS trigger
LANGUAGE plpgsql SET search_path = pg_catalog, pg_temp AS $$
DECLARE
    found_entry boolean;
    cur_fr text;
    cur_ar text;
    cur_order integer;
BEGIN
    IF TG_OP <> 'INSERT' THEN
        RAISE EXCEPTION 'catalogue_audit is append-only' USING ERRCODE = 'LRN01';
    END IF;
    IF NEW.occurred_at IS DISTINCT FROM now() OR NEW.xact_id IS DISTINCT FROM pg_current_xact_id() THEN
        RAISE EXCEPTION 'a catalogue audit row records its own transaction and its time' USING ERRCODE = 'LRN01';
    END IF;
    IF NEW.action IN ('add_subject', 'add_chapter') THEN
        RETURN NEW;
    END IF;
    -- The entry is locked until the transaction ends, as the operator
    -- commands lock it before writing their row: a concurrent change waits,
    -- and at READ COMMITTED this read sees the latest committed values (a
    -- row built on stale old values is refused); at REPEATABLE READ or
    -- SERIALIZABLE a concurrently changed entry is a serialization failure.
    IF NEW.chapter_id IS NULL THEN
        EXECUTE format('SELECT true, name_fr, name_ar, sort_order FROM %I.subjects WHERE id = $1
                         FOR NO KEY UPDATE', TG_TABLE_SCHEMA)
           INTO found_entry, cur_fr, cur_ar, cur_order USING NEW.subject_id;
    ELSE
        EXECUTE format('SELECT true, name_fr, name_ar, sort_order FROM %I.chapters WHERE id = $1 AND subject_id = $2
                         FOR NO KEY UPDATE', TG_TABLE_SCHEMA)
           INTO found_entry, cur_fr, cur_ar, cur_order USING NEW.chapter_id, NEW.subject_id;
    END IF;
    IF found_entry IS NULL THEN
        RAISE EXCEPTION 'catalogue audit row names no existing entry' USING ERRCODE = 'LRN01';
    END IF;
    IF NEW.action IN ('rename_subject', 'rename_chapter')
       AND (NEW.old_name_fr, NEW.old_name_ar) IS DISTINCT FROM (cur_fr, cur_ar) THEN
        RAISE EXCEPTION 'a rename records the current names as its old values' USING ERRCODE = 'LRN01';
    END IF;
    IF NEW.action IN ('reorder_subject', 'reorder_chapter') AND NEW.old_sort_order IS DISTINCT FROM cur_order THEN
        RAISE EXCEPTION 'a reorder records the current order as its old value' USING ERRCODE = 'LRN01';
    END IF;
    RETURN NEW;
END;
$$;

-- Insert: no names, order 0. Update: a change of names or order needs the
-- latest matching audit row of the same transaction (rule 2). Identity is
-- checked by the identity guards.
CREATE FUNCTION catalogue_metadata_guard() RETURNS trigger
LANGUAGE plpgsql SET search_path = pg_catalog, pg_temp AS $$
DECLARE
    kind text;
    entry_column text;
    found_row boolean;
    old_fr text;
    old_ar text;
    new_fr text;
    new_ar text;
    old_order integer;
    new_order integer;
BEGIN
    IF TG_TABLE_NAME = 'subjects' THEN
        kind := 'subject';
        entry_column := 'subject_id';
    ELSE
        kind := 'chapter';
        entry_column := 'chapter_id';
    END IF;
    IF TG_OP = 'INSERT' THEN
        IF NEW.name_fr IS NOT NULL OR NEW.name_ar IS NOT NULL OR NEW.sort_order <> 0 THEN
            RAISE EXCEPTION 'a new % has no names and order 0; set them with an audited change', kind
                USING ERRCODE = 'LRN01';
        END IF;
        RETURN NEW;
    END IF;
    IF (NEW.name_fr, NEW.name_ar) IS DISTINCT FROM (OLD.name_fr, OLD.name_ar) THEN
        EXECUTE format('SELECT true, old_name_fr, old_name_ar, new_name_fr, new_name_ar FROM %I.catalogue_audit
                         WHERE action = $1 AND %I = $2 AND xact_id = pg_catalog.pg_current_xact_id()
                         ORDER BY id DESC LIMIT 1', TG_TABLE_SCHEMA, entry_column)
           INTO found_row, old_fr, old_ar, new_fr, new_ar USING 'rename_' || kind, NEW.id;
        IF found_row IS NULL OR (old_fr, old_ar, new_fr, new_ar)
           IS DISTINCT FROM (OLD.name_fr, OLD.name_ar, NEW.name_fr, NEW.name_ar) THEN
            RAISE EXCEPTION '% % was renamed without its catalogue_audit row of the same transaction', kind, NEW.id
                USING ERRCODE = 'LRN01';
        END IF;
    END IF;
    IF NEW.sort_order IS DISTINCT FROM OLD.sort_order THEN
        found_row := NULL;
        EXECUTE format('SELECT true, old_sort_order, new_sort_order FROM %I.catalogue_audit
                         WHERE action = $1 AND %I = $2 AND xact_id = pg_catalog.pg_current_xact_id()
                         ORDER BY id DESC LIMIT 1', TG_TABLE_SCHEMA, entry_column)
           INTO found_row, old_order, new_order USING 'reorder_' || kind, NEW.id;
        IF found_row IS NULL OR (old_order, new_order) IS DISTINCT FROM (OLD.sort_order, NEW.sort_order) THEN
            RAISE EXCEPTION '% % was reordered without its catalogue_audit row of the same transaction', kind, NEW.id
                USING ERRCODE = 'LRN01';
        END IF;
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER zz_guard_subjects_metadata
    BEFORE INSERT OR UPDATE ON subjects
    FOR EACH ROW EXECUTE FUNCTION catalogue_metadata_guard();

CREATE TRIGGER zz_guard_chapters_metadata
    BEFORE INSERT OR UPDATE ON chapters
    FOR EACH ROW EXECUTE FUNCTION catalogue_metadata_guard();

-- At commit, every rename or reorder row was applied (rule 3).
CREATE FUNCTION catalogue_audit_applied() RETURNS trigger
LANGUAGE plpgsql SET search_path = pg_catalog, pg_temp AS $$
DECLARE
    found_row boolean;
    next_fr text;
    next_ar text;
    next_order integer;
BEGIN
    EXECUTE format('SELECT true, old_name_fr, old_name_ar, old_sort_order FROM %I.catalogue_audit
                     WHERE action = $1 AND subject_id = $2 AND chapter_id IS NOT DISTINCT FROM $3
                       AND xact_id = $4 AND id > $5
                     ORDER BY id LIMIT 1', TG_TABLE_SCHEMA)
       INTO found_row, next_fr, next_ar, next_order
       USING NEW.action, NEW.subject_id, NEW.chapter_id, NEW.xact_id, NEW.id;
    IF found_row IS NULL THEN
        -- The last row of its kind: the entry must still have its values.
        IF NEW.chapter_id IS NULL THEN
            EXECUTE format('SELECT true, name_fr, name_ar, sort_order FROM %I.subjects WHERE id = $1', TG_TABLE_SCHEMA)
               INTO found_row, next_fr, next_ar, next_order USING NEW.subject_id;
        ELSE
            EXECUTE format('SELECT true, name_fr, name_ar, sort_order FROM %I.chapters WHERE id = $1', TG_TABLE_SCHEMA)
               INTO found_row, next_fr, next_ar, next_order USING NEW.chapter_id;
        END IF;
    END IF;
    IF NEW.action IN ('rename_subject', 'rename_chapter') THEN
        IF found_row IS NULL OR (next_fr, next_ar) IS DISTINCT FROM (NEW.new_name_fr, NEW.new_name_ar) THEN
            RAISE EXCEPTION 'catalogue audit row % records a rename that was not applied', NEW.id USING ERRCODE = 'LRN01';
        END IF;
    ELSIF found_row IS NULL OR next_order IS DISTINCT FROM NEW.new_sort_order THEN
        RAISE EXCEPTION 'catalogue audit row % records a reorder that was not applied', NEW.id USING ERRCODE = 'LRN01';
    END IF;
    RETURN NULL;
END;
$$;

CREATE CONSTRAINT TRIGGER zz_guard_catalogue_audit_applied
    AFTER INSERT ON catalogue_audit
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW
    WHEN (NEW.action NOT IN ('add_subject', 'add_chapter'))
    EXECUTE FUNCTION catalogue_audit_applied();
