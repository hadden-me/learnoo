-- LMS-B02 (issue #92): curriculum levels, academic sections, editions and
-- their subject assignments with ordered official chapters, retirement of
-- subjects and chapters, and the append-only curriculum_audit
-- (docs/LMS_CONTRACT.md §3, docs/LMS_PRODUCT_SPEC.md §2 "Curriculum
-- integrity" and §9). No curriculum data is seeded and no existing chapter is
-- placed into any edition: an administrator does that, audited.
--
-- Conventions of 0004 and 0010 apply: guards raise SQLSTATE LRN01 and are
-- named zz_guard_*; every trigger function pins search_path to pg_catalog,
-- pg_temp and reaches other tables only through the schema of the table that
-- fired it (TG_TABLE_SCHEMA, EXECUTE format('%I...') with bound parameters);
-- no function is SECURITY DEFINER. The guards bind the application's
-- least-privilege role, not a superuser or table owner. This migration does
-- not depend on the display-name columns of 0012 and does not touch
-- catalogue_audit.
--
-- Rules kept here:
--
--   * Identities persist. Levels, sections and editions are never deleted
--     or truncated; their id, code, parent and created_at never change, so a
--     code is never repurposed. Names, labels and display order are mutable
--     display metadata. Every UPDATE of a level, section or edition
--     increments its version by exactly one (optimistic concurrency).
--   * Retirement is one-way: retired_at goes from NULL to the transaction
--     time once and never changes again, on levels, sections, editions,
--     subjects and chapters. A retired entry cannot be newly selected: no
--     section under a retired level, no edition under a retired section, no
--     edition made current under a retired section or level, no assignment
--     of a retired subject, no retired chapter added to a chapter list.
--     Existing references keep resolving.
--   * Edition states: draft -> current -> superseded, current -> retired,
--     superseded -> retired, never back to draft. At most one current edition
--     per section (partial unique index). An edition that becomes superseded
--     does so in the transaction that makes another edition of its section
--     current (deferred check).
--   * Assignments and their chapter lists are inserted or deleted only while
--     the edition is draft, and never updated. The guards lock the edition
--     FOR SHARE, which conflicts with the FOR UPDATE that make-current and
--     retire take, so an edit cannot commit after the edition left draft.
--     Composite foreign keys keep an assignment's chapters in its subject.
--   * Every create, rename, reorder, retirement, state change and assignment
--     edit needs a curriculum_audit row of the same transaction with the
--     matching target and action (deferred constraint triggers, checked at
--     commit, as catalogue_audit does for additions in 0010). The audit is
--     append-only and records its own transaction and time. Its before/after
--     values are written by internal/curriculum; the database checks that
--     the row exists, not its values.
--
-- Global lock order (docs/LMS_CONTRACT.md §2): permission and account rows
-- (permissions.RequireTx) -> curriculum editions (ascending ID) -> other rows.
-- The guards' later share locks on sections, levels, subjects and chapters
-- come after the edition locks of the same transaction; no path locks one of
-- those rows and then an edition.

-- The name rule (the operator label rule of 0002/0003/0010), listed by code
-- point so the check does not depend on the locale.
CREATE FUNCTION curriculum_valid_name(name text) RETURNS boolean
LANGUAGE sql IMMUTABLE PARALLEL SAFE STRICT
SET search_path = pg_catalog, pg_temp
RETURN char_length(name) BETWEEN 1 AND 200
    AND name = btrim(name)
    AND name !~ E'[\\u0001-\\u001F\\u007F-\\u009F\\u00AD\\u0600-\\u0605\\u061C\\u06DD\\u070F\\u0890-\\u0891\\u08E2\\u180E\\u200B-\\u200F\\u2028-\\u202E\\u2060-\\u2064\\u2066-\\u206F\\uFEFF\\uFFF9-\\uFFFB\\U000110BD\\U000110CD\\U00013430-\\U0001343F\\U0001BCA0-\\U0001BCA3\\U0001D173-\\U0001D17A\\U000E0001\\U000E0020-\\U000E007F]';

CREATE TABLE education_levels (
    id            bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    code          text        NOT NULL,
    name_fr       text        NOT NULL,
    name_ar       text        NOT NULL,
    display_order integer     NOT NULL DEFAULT 0,
    retired_at    timestamptz,
    version       integer     NOT NULL DEFAULT 1,
    created_at    timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT education_levels_code_format CHECK (code ~ '^[a-z0-9][a-z0-9-]{0,62}$'),
    CONSTRAINT education_levels_names CHECK (curriculum_valid_name(name_fr) AND curriculum_valid_name(name_ar)),
    CONSTRAINT education_levels_display_order CHECK (display_order BETWEEN 0 AND 9999),
    CONSTRAINT education_levels_version CHECK (version >= 1),
    CONSTRAINT education_levels_code_key UNIQUE (code)
);

CREATE TABLE academic_sections (
    id            bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    level_id      bigint      NOT NULL REFERENCES education_levels (id),
    code          text        NOT NULL,
    name_fr       text        NOT NULL,
    name_ar       text        NOT NULL,
    display_order integer     NOT NULL DEFAULT 0,
    retired_at    timestamptz,
    version       integer     NOT NULL DEFAULT 1,
    created_at    timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT academic_sections_code_format CHECK (code ~ '^[a-z0-9][a-z0-9-]{0,62}$'),
    CONSTRAINT academic_sections_names CHECK (curriculum_valid_name(name_fr) AND curriculum_valid_name(name_ar)),
    CONSTRAINT academic_sections_display_order CHECK (display_order BETWEEN 0 AND 9999),
    CONSTRAINT academic_sections_version CHECK (version >= 1),
    CONSTRAINT academic_sections_level_code_key UNIQUE (level_id, code)
);

CREATE TABLE curriculum_editions (
    id            bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    section_id    bigint      NOT NULL REFERENCES academic_sections (id),
    code          text        NOT NULL,
    label_fr      text        NOT NULL,
    label_ar      text        NOT NULL,
    state         text        NOT NULL DEFAULT 'draft',
    current_since timestamptz,
    superseded_at timestamptz,
    retired_at    timestamptz,
    version       integer     NOT NULL DEFAULT 1,
    created_at    timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT curriculum_editions_code_format CHECK (code ~ '^[a-z0-9][a-z0-9-]{0,62}$'),
    CONSTRAINT curriculum_editions_labels CHECK (curriculum_valid_name(label_fr) AND curriculum_valid_name(label_ar)),
    CONSTRAINT curriculum_editions_state CHECK (state IN ('draft', 'current', 'superseded', 'retired')),
    -- Every non-draft edition was current once (only current and superseded
    -- editions can be retired).
    CONSTRAINT curriculum_editions_current_since CHECK ((state = 'draft') = (current_since IS NULL)),
    CONSTRAINT curriculum_editions_superseded_at CHECK (
        (state = 'superseded' AND superseded_at IS NOT NULL)
        OR (state IN ('draft', 'current') AND superseded_at IS NULL)
        OR state = 'retired'),
    CONSTRAINT curriculum_editions_retired_at CHECK ((state = 'retired') = (retired_at IS NOT NULL)),
    CONSTRAINT curriculum_editions_version CHECK (version >= 1),
    CONSTRAINT curriculum_editions_section_code_key UNIQUE (section_id, code)
);

CREATE UNIQUE INDEX curriculum_editions_one_current
    ON curriculum_editions (section_id) WHERE state = 'current';

CREATE TABLE curriculum_assignments (
    id         bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    edition_id bigint      NOT NULL REFERENCES curriculum_editions (id),
    subject_id bigint      NOT NULL REFERENCES subjects (id),
    created_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT curriculum_assignments_edition_subject_key UNIQUE (edition_id, subject_id),
    -- Target of the composite foreign keys that keep chapters (and, from
    -- B03, course placements) in the assignment's subject.
    CONSTRAINT curriculum_assignments_id_subject_key UNIQUE (id, subject_id)
);

CREATE INDEX curriculum_assignments_subject_idx ON curriculum_assignments (subject_id);

-- position orders the edition's chapters for the subject, independently of
-- the catalogue display order.
CREATE TABLE curriculum_assignment_chapters (
    assignment_id bigint  NOT NULL,
    subject_id    bigint  NOT NULL,
    chapter_id    bigint  NOT NULL,
    position      integer NOT NULL,
    CONSTRAINT curriculum_assignment_chapters_position CHECK (position BETWEEN 1 AND 9999),
    CONSTRAINT curriculum_assignment_chapters_pkey PRIMARY KEY (assignment_id, chapter_id),
    CONSTRAINT curriculum_assignment_chapters_position_key UNIQUE (assignment_id, position),
    CONSTRAINT curriculum_assignment_chapters_assignment_fkey FOREIGN KEY (assignment_id, subject_id)
        REFERENCES curriculum_assignments (id, subject_id) ON DELETE CASCADE,
    CONSTRAINT curriculum_assignment_chapters_chapter_fkey FOREIGN KEY (chapter_id, subject_id)
        REFERENCES chapters (id, subject_id)
);

CREATE INDEX curriculum_assignment_chapters_chapter_idx ON curriculum_assignment_chapters (chapter_id);

-- Existing rows keep NULL: nothing is retired by the upgrade.
ALTER TABLE subjects ADD COLUMN retired_at timestamptz;
ALTER TABLE chapters ADD COLUMN retired_at timestamptz;

CREATE TABLE curriculum_audit (
    id             bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    occurred_at    timestamptz NOT NULL DEFAULT now(),
    xact_id        xid8        NOT NULL DEFAULT pg_current_xact_id(),
    -- Exactly one of actor_id (a signed-in administrator) and operator_label
    -- (a self-asserted local operator identity) is set, as in permission_audit.
    actor_id       bigint      REFERENCES users (id),
    operator_label text,
    target         text        NOT NULL,
    -- The ID of the level, section, edition, assignment, subject or chapter.
    -- Assignments can be removed while their edition is a draft, so target_id
    -- has no foreign key; edition_id names their edition.
    target_id      bigint      NOT NULL,
    edition_id     bigint      REFERENCES curriculum_editions (id),
    action         text        NOT NULL,
    before         jsonb,
    after          jsonb,
    CONSTRAINT curriculum_audit_actor CHECK ((actor_id IS NULL) = (operator_label IS NOT NULL)),
    CONSTRAINT curriculum_audit_operator_label CHECK (curriculum_valid_name(operator_label)),
    CONSTRAINT curriculum_audit_target CHECK (target IN ('level', 'section', 'edition', 'assignment', 'subject', 'chapter')),
    CONSTRAINT curriculum_audit_action CHECK (CASE target
        WHEN 'level'      THEN action IN ('create', 'rename', 'reorder', 'retire')
        WHEN 'section'    THEN action IN ('create', 'rename', 'reorder', 'retire')
        WHEN 'edition'    THEN action IN ('create', 'rename', 'make_current', 'supersede', 'retire')
        WHEN 'assignment' THEN action IN ('add_assignment', 'remove_assignment', 'set_chapters')
        ELSE action = 'retire'
        END),
    CONSTRAINT curriculum_audit_edition CHECK (
        (target IN ('edition', 'assignment')) = (edition_id IS NOT NULL)
        AND (target <> 'edition' OR edition_id = target_id)),
    CONSTRAINT curriculum_audit_values CHECK (
        (before IS NULL OR jsonb_typeof(before) = 'object') AND (after IS NULL OR jsonb_typeof(after) = 'object'))
);

CREATE INDEX curriculum_audit_target_idx ON curriculum_audit (target, target_id, id);
CREATE INDEX curriculum_audit_xact_idx ON curriculum_audit (xact_id);
CREATE INDEX curriculum_audit_edition_idx ON curriculum_audit (edition_id) WHERE edition_id IS NOT NULL;

-- Insert: at the transaction time, by its own transaction, naming an
-- existing entry (an assignment that still exists must belong to edition_id).
-- Update, delete and truncate: never.
CREATE FUNCTION curriculum_audit_guard() RETURNS trigger
LANGUAGE plpgsql SET search_path = pg_catalog, pg_temp AS $$
DECLARE
    tbl text;
    found_row boolean;
    assignment_edition bigint;
BEGIN
    IF TG_OP <> 'INSERT' THEN
        RAISE EXCEPTION 'curriculum_audit is append-only' USING ERRCODE = 'LRN01';
    END IF;
    IF NEW.occurred_at IS DISTINCT FROM now() OR NEW.xact_id IS DISTINCT FROM pg_current_xact_id() THEN
        RAISE EXCEPTION 'a curriculum audit row records its own transaction and its time' USING ERRCODE = 'LRN01';
    END IF;
    IF NEW.target = 'assignment' THEN
        EXECUTE format('SELECT edition_id FROM %I.curriculum_assignments WHERE id = $1', TG_TABLE_SCHEMA)
           INTO assignment_edition USING NEW.target_id;
        IF assignment_edition IS NOT NULL AND assignment_edition <> NEW.edition_id THEN
            RAISE EXCEPTION 'curriculum audit row names assignment % with another edition', NEW.target_id
                USING ERRCODE = 'LRN01';
        END IF;
        RETURN NEW;
    END IF;
    tbl := CASE NEW.target
        WHEN 'level' THEN 'education_levels'
        WHEN 'section' THEN 'academic_sections'
        WHEN 'edition' THEN 'curriculum_editions'
        WHEN 'subject' THEN 'subjects'
        ELSE 'chapters' END;
    EXECUTE format('SELECT true FROM %I.%I WHERE id = $1', TG_TABLE_SCHEMA, tbl)
       INTO found_row USING NEW.target_id;
    IF found_row IS NULL THEN
        RAISE EXCEPTION 'curriculum audit row names no existing %', NEW.target USING ERRCODE = 'LRN01';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER zz_guard_curriculum_audit
    BEFORE INSERT OR UPDATE OR DELETE ON curriculum_audit
    FOR EACH ROW EXECUTE FUNCTION curriculum_audit_guard();

CREATE TRIGGER zz_guard_curriculum_audit_no_truncate
    BEFORE TRUNCATE ON curriculum_audit
    FOR EACH STATEMENT EXECUTE FUNCTION curriculum_audit_guard();

-- Identities persist: no delete, no truncate.
CREATE FUNCTION curriculum_keep_identities() RETURNS trigger
LANGUAGE plpgsql SET search_path = pg_catalog, pg_temp AS $$
BEGIN
    RAISE EXCEPTION '% rows are never deleted, so their IDs and codes are never reused', TG_TABLE_NAME
        USING ERRCODE = 'LRN01';
END;
$$;

CREATE TRIGGER zz_guard_education_levels_keep
    BEFORE DELETE ON education_levels
    FOR EACH ROW EXECUTE FUNCTION curriculum_keep_identities();
CREATE TRIGGER zz_guard_education_levels_no_truncate
    BEFORE TRUNCATE ON education_levels
    FOR EACH STATEMENT EXECUTE FUNCTION curriculum_keep_identities();
CREATE TRIGGER zz_guard_academic_sections_keep
    BEFORE DELETE ON academic_sections
    FOR EACH ROW EXECUTE FUNCTION curriculum_keep_identities();
CREATE TRIGGER zz_guard_academic_sections_no_truncate
    BEFORE TRUNCATE ON academic_sections
    FOR EACH STATEMENT EXECUTE FUNCTION curriculum_keep_identities();
CREATE TRIGGER zz_guard_curriculum_editions_keep
    BEFORE DELETE ON curriculum_editions
    FOR EACH ROW EXECUTE FUNCTION curriculum_keep_identities();
CREATE TRIGGER zz_guard_curriculum_editions_no_truncate
    BEFORE TRUNCATE ON curriculum_editions
    FOR EACH STATEMENT EXECUTE FUNCTION curriculum_keep_identities();
-- Assignments are deleted row by row while their edition is a draft (guard
-- below); truncation would bypass that guard.
CREATE TRIGGER zz_guard_curriculum_assignments_no_truncate
    BEFORE TRUNCATE ON curriculum_assignments
    FOR EACH STATEMENT EXECUTE FUNCTION curriculum_keep_identities();
CREATE TRIGGER zz_guard_curriculum_assignment_chapters_no_truncate
    BEFORE TRUNCATE ON curriculum_assignment_chapters
    FOR EACH STATEMENT EXECUTE FUNCTION curriculum_keep_identities();

-- Levels and sections. Insert: version 1, not retired, and a section's level
-- not retired (share-locked, so a concurrent retirement waits). Update:
-- identity immutable, version + 1, retirement one-way at the transaction time.
CREATE FUNCTION curriculum_hierarchy_guard() RETURNS trigger
LANGUAGE plpgsql SET search_path = pg_catalog, pg_temp AS $$
DECLARE
    parent_retired timestamptz;
    found_row boolean;
BEGIN
    IF TG_OP = 'INSERT' THEN
        IF NEW.retired_at IS NOT NULL OR NEW.version <> 1 THEN
            RAISE EXCEPTION 'a new % row starts at version 1, not retired', TG_TABLE_NAME USING ERRCODE = 'LRN01';
        END IF;
        IF TG_TABLE_NAME = 'academic_sections' THEN
            EXECUTE format('SELECT true, retired_at FROM %I.education_levels WHERE id = $1 FOR SHARE', TG_TABLE_SCHEMA)
               INTO found_row, parent_retired USING NEW.level_id;
            IF found_row AND parent_retired IS NOT NULL THEN
                RAISE EXCEPTION 'level % is retired; no section can be added to it', NEW.level_id USING ERRCODE = 'LRN01';
            END IF;
        END IF;
        RETURN NEW;
    END IF;
    IF (to_jsonb(NEW) ->> 'id', to_jsonb(NEW) ->> 'code', to_jsonb(NEW) ->> 'created_at', to_jsonb(NEW) ->> 'level_id')
       IS DISTINCT FROM
       (to_jsonb(OLD) ->> 'id', to_jsonb(OLD) ->> 'code', to_jsonb(OLD) ->> 'created_at', to_jsonb(OLD) ->> 'level_id') THEN
        RAISE EXCEPTION '% identity is immutable', TG_TABLE_NAME USING ERRCODE = 'LRN01';
    END IF;
    IF NEW.version IS DISTINCT FROM OLD.version + 1 THEN
        RAISE EXCEPTION 'every % update increments its version by one', TG_TABLE_NAME USING ERRCODE = 'LRN01';
    END IF;
    IF NEW.retired_at IS DISTINCT FROM OLD.retired_at
       AND (OLD.retired_at IS NOT NULL OR NEW.retired_at IS DISTINCT FROM now()) THEN
        RAISE EXCEPTION 'retirement is set once, to the transaction time, and never undone' USING ERRCODE = 'LRN01';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER zz_guard_education_levels
    BEFORE INSERT OR UPDATE ON education_levels
    FOR EACH ROW EXECUTE FUNCTION curriculum_hierarchy_guard();
CREATE TRIGGER zz_guard_academic_sections
    BEFORE INSERT OR UPDATE ON academic_sections
    FOR EACH ROW EXECUTE FUNCTION curriculum_hierarchy_guard();

-- Editions. Insert: a draft at version 1 in a section that is not retired.
-- Update: identity immutable, version + 1, only the forward state
-- transitions, each stamping its own time once; making an edition current
-- needs its section and level not retired (share-locked).
CREATE FUNCTION curriculum_editions_guard() RETURNS trigger
LANGUAGE plpgsql SET search_path = pg_catalog, pg_temp AS $$
DECLARE
    section_retired timestamptz;
    level_retired timestamptz;
    found_row boolean;
BEGIN
    IF TG_OP = 'INSERT' THEN
        IF NEW.state <> 'draft' OR NEW.version <> 1 THEN
            RAISE EXCEPTION 'an edition is created as a draft at version 1' USING ERRCODE = 'LRN01';
        END IF;
    ELSE
        IF (NEW.id, NEW.section_id, NEW.code, NEW.created_at) IS DISTINCT FROM (OLD.id, OLD.section_id, OLD.code, OLD.created_at) THEN
            RAISE EXCEPTION 'edition identity is immutable' USING ERRCODE = 'LRN01';
        END IF;
        IF NEW.version IS DISTINCT FROM OLD.version + 1 THEN
            RAISE EXCEPTION 'every curriculum_editions update increments its version by one' USING ERRCODE = 'LRN01';
        END IF;
        IF NEW.state IS DISTINCT FROM OLD.state
           AND (OLD.state, NEW.state) NOT IN (
               ('draft', 'current'), ('current', 'superseded'), ('current', 'retired'), ('superseded', 'retired')) THEN
            RAISE EXCEPTION 'edition state cannot change from % to %', OLD.state, NEW.state USING ERRCODE = 'LRN01';
        END IF;
        IF (NEW.current_since IS DISTINCT FROM OLD.current_since
                AND (OLD.current_since IS NOT NULL OR NEW.current_since IS DISTINCT FROM now()))
           OR (NEW.superseded_at IS DISTINCT FROM OLD.superseded_at
                AND (OLD.superseded_at IS NOT NULL OR NEW.superseded_at IS DISTINCT FROM now()))
           OR (NEW.retired_at IS DISTINCT FROM OLD.retired_at
                AND (OLD.retired_at IS NOT NULL OR NEW.retired_at IS DISTINCT FROM now())) THEN
            RAISE EXCEPTION 'edition state times are set once, to the transaction time' USING ERRCODE = 'LRN01';
        END IF;
    END IF;
    IF TG_OP = 'INSERT' OR (NEW.state = 'current' AND OLD.state = 'draft') THEN
        EXECUTE format('SELECT true, s.retired_at, l.retired_at
                          FROM %I.academic_sections s JOIN %I.education_levels l ON l.id = s.level_id
                         WHERE s.id = $1 FOR SHARE OF s, l', TG_TABLE_SCHEMA, TG_TABLE_SCHEMA)
           INTO found_row, section_retired, level_retired USING NEW.section_id;
        IF found_row AND (section_retired IS NOT NULL OR level_retired IS NOT NULL) THEN
            RAISE EXCEPTION 'section % or its level is retired', NEW.section_id USING ERRCODE = 'LRN01';
        END IF;
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER zz_guard_curriculum_editions
    BEFORE INSERT OR UPDATE ON curriculum_editions
    FOR EACH ROW EXECUTE FUNCTION curriculum_editions_guard();

-- At commit, an edition superseded by this transaction left its place to an
-- edition of the same section made current by this transaction.
CREATE FUNCTION curriculum_editions_superseded_by_successor() RETURNS trigger
LANGUAGE plpgsql SET search_path = pg_catalog, pg_temp AS $$
DECLARE
    successor boolean;
BEGIN
    EXECUTE format('SELECT EXISTS (SELECT 1 FROM %I.curriculum_editions
                     WHERE section_id = $1 AND state = ''current'' AND current_since = pg_catalog.now())', TG_TABLE_SCHEMA)
       INTO successor USING NEW.section_id;
    IF NOT successor THEN
        RAISE EXCEPTION 'edition % can only be superseded by making another edition of its section current in the same transaction', NEW.id
            USING ERRCODE = 'LRN01';
    END IF;
    RETURN NULL;
END;
$$;

CREATE CONSTRAINT TRIGGER zz_guard_curriculum_editions_superseded
    AFTER UPDATE ON curriculum_editions
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW
    WHEN (NEW.state = 'superseded' AND OLD.state = 'current')
    EXECUTE FUNCTION curriculum_editions_superseded_by_successor();

-- Assignments and chapter lists change only while the edition is a draft.
-- The edition is locked FOR SHARE, which conflicts with the FOR UPDATE of
-- make-current and retire: an edit waits for, or blocks, a state change and
-- then sees its result. Rows are never updated. A missing assignment on a
-- chapter-list delete means the assignment is being deleted (ON DELETE
-- CASCADE), which this guard allowed only for a draft edition. New rows
-- cannot name a retired subject or chapter (share-locked, so a concurrent
-- retirement waits).
CREATE FUNCTION curriculum_assignments_guard() RETURNS trigger
LANGUAGE plpgsql SET search_path = pg_catalog, pg_temp AS $$
DECLARE
    edition bigint;
    st text;
    entry_retired timestamptz;
    row_data record;
BEGIN
    IF TG_OP = 'UPDATE' THEN
        RAISE EXCEPTION '% rows are never updated; remove and add instead', TG_TABLE_NAME USING ERRCODE = 'LRN01';
    END IF;
    IF TG_OP = 'DELETE' THEN
        row_data := OLD;
    ELSE
        row_data := NEW;
    END IF;
    IF TG_TABLE_NAME = 'curriculum_assignments' THEN
        edition := row_data.edition_id;
    ELSE
        EXECUTE format('SELECT edition_id FROM %I.curriculum_assignments WHERE id = $1', TG_TABLE_SCHEMA)
           INTO edition USING row_data.assignment_id;
        IF edition IS NULL THEN
            IF TG_OP = 'DELETE' THEN
                RETURN OLD;
            END IF;
            RAISE EXCEPTION 'assignment % does not exist', row_data.assignment_id USING ERRCODE = 'LRN01';
        END IF;
    END IF;
    -- EXECUTE does not set FOUND; a missing row leaves st NULL.
    EXECUTE format('SELECT state FROM %I.curriculum_editions WHERE id = $1 FOR SHARE', TG_TABLE_SCHEMA)
       INTO st USING edition;
    IF st IS DISTINCT FROM 'draft' THEN
        RAISE EXCEPTION 'edition % is not a draft; its assignments and chapter lists are immutable', edition
            USING ERRCODE = 'LRN01';
    END IF;
    IF TG_OP = 'INSERT' THEN
        IF TG_TABLE_NAME = 'curriculum_assignments' THEN
            EXECUTE format('SELECT retired_at FROM %I.subjects WHERE id = $1 FOR SHARE', TG_TABLE_SCHEMA)
               INTO entry_retired USING NEW.subject_id;
        ELSE
            EXECUTE format('SELECT retired_at FROM %I.chapters WHERE id = $1 FOR SHARE', TG_TABLE_SCHEMA)
               INTO entry_retired USING NEW.chapter_id;
        END IF;
        IF entry_retired IS NOT NULL THEN
            RAISE EXCEPTION 'a retired % cannot be newly selected',
                CASE TG_TABLE_NAME WHEN 'curriculum_assignments' THEN 'subject' ELSE 'chapter' END
                USING ERRCODE = 'LRN01';
        END IF;
        RETURN NEW;
    END IF;
    RETURN OLD;
END;
$$;

CREATE TRIGGER zz_guard_curriculum_assignments_draft_only
    BEFORE INSERT OR UPDATE OR DELETE ON curriculum_assignments
    FOR EACH ROW EXECUTE FUNCTION curriculum_assignments_guard();
CREATE TRIGGER zz_guard_curriculum_assignment_chapters_draft_only
    BEFORE INSERT OR UPDATE OR DELETE ON curriculum_assignment_chapters
    FOR EACH ROW EXECUTE FUNCTION curriculum_assignments_guard();

-- Subjects and chapters: new entries are not retired; retirement is one-way
-- at the transaction time. Identity, names and order keep their own guards
-- (0004, 0010, 0012).
CREATE FUNCTION curriculum_catalogue_retirement_guard() RETURNS trigger
LANGUAGE plpgsql SET search_path = pg_catalog, pg_temp AS $$
BEGIN
    IF TG_OP = 'INSERT' THEN
        IF NEW.retired_at IS NOT NULL THEN
            RAISE EXCEPTION 'a new % is not retired', TG_TABLE_NAME USING ERRCODE = 'LRN01';
        END IF;
        RETURN NEW;
    END IF;
    IF NEW.retired_at IS DISTINCT FROM OLD.retired_at
       AND (OLD.retired_at IS NOT NULL OR NEW.retired_at IS DISTINCT FROM now()) THEN
        RAISE EXCEPTION 'retirement is set once, to the transaction time, and never undone' USING ERRCODE = 'LRN01';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER zz_guard_subjects_retirement
    BEFORE INSERT OR UPDATE ON subjects
    FOR EACH ROW EXECUTE FUNCTION curriculum_catalogue_retirement_guard();
CREATE TRIGGER zz_guard_chapters_retirement
    BEFORE INSERT OR UPDATE ON chapters
    FOR EACH ROW EXECUTE FUNCTION curriculum_catalogue_retirement_guard();

-- At commit, every audited change has its curriculum_audit row of the same
-- transaction: the action is derived from the change (OLD and NEW as of the
-- statement), the target from the table.
CREATE FUNCTION curriculum_change_audited() RETURNS trigger
LANGUAGE plpgsql SET search_path = pg_catalog, pg_temp AS $$
DECLARE
    tgt text;
    tgt_id bigint;
    actions text[] := ARRAY[]::text[];
    a text;
    audited boolean;
    o jsonb;
    n jsonb;
BEGIN
    IF TG_OP <> 'INSERT' THEN
        o := to_jsonb(OLD);
    END IF;
    IF TG_OP <> 'DELETE' THEN
        n := to_jsonb(NEW);
    END IF;
    CASE TG_TABLE_NAME
    WHEN 'education_levels', 'academic_sections', 'curriculum_editions', 'subjects', 'chapters' THEN
        tgt := CASE TG_TABLE_NAME
            WHEN 'education_levels' THEN 'level'
            WHEN 'academic_sections' THEN 'section'
            WHEN 'curriculum_editions' THEN 'edition'
            WHEN 'subjects' THEN 'subject'
            ELSE 'chapter' END;
        tgt_id := (n ->> 'id')::bigint;
        IF TG_OP = 'INSERT' THEN
            actions := ARRAY['create'];
        ELSE
            IF TG_TABLE_NAME IN ('education_levels', 'academic_sections')
               AND (n -> 'name_fr', n -> 'name_ar') IS DISTINCT FROM (o -> 'name_fr', o -> 'name_ar') THEN
                actions := array_append(actions, 'rename');
            END IF;
            IF TG_TABLE_NAME = 'curriculum_editions'
               AND (n -> 'label_fr', n -> 'label_ar') IS DISTINCT FROM (o -> 'label_fr', o -> 'label_ar') THEN
                actions := array_append(actions, 'rename');
            END IF;
            IF TG_TABLE_NAME IN ('education_levels', 'academic_sections')
               AND n -> 'display_order' IS DISTINCT FROM o -> 'display_order' THEN
                actions := array_append(actions, 'reorder');
            END IF;
            IF TG_TABLE_NAME = 'curriculum_editions' AND n -> 'state' IS DISTINCT FROM o -> 'state' THEN
                actions := array_append(actions, CASE n ->> 'state'
                    WHEN 'current' THEN 'make_current'
                    WHEN 'superseded' THEN 'supersede'
                    ELSE 'retire' END);
            ELSIF n -> 'retired_at' IS DISTINCT FROM o -> 'retired_at' THEN
                actions := array_append(actions, 'retire');
            END IF;
        END IF;
    WHEN 'curriculum_assignments' THEN
        tgt := 'assignment';
        tgt_id := coalesce(n, o) ->> 'id';
        actions := ARRAY[CASE TG_OP WHEN 'INSERT' THEN 'add_assignment' ELSE 'remove_assignment' END];
    ELSE
        tgt := 'assignment';
        tgt_id := coalesce(n, o) ->> 'assignment_id';
        actions := ARRAY['set_chapters'];
    END CASE;
    FOREACH a IN ARRAY actions LOOP
        EXECUTE format('SELECT EXISTS (SELECT 1 FROM %I.curriculum_audit
                         WHERE target = $1 AND target_id = $2
                           AND (action = $3 OR ($4 AND action = ''remove_assignment''))
                           AND xact_id = pg_catalog.pg_current_xact_id())', TG_TABLE_SCHEMA)
           INTO audited
           USING tgt, tgt_id, a, TG_TABLE_NAME = 'curriculum_assignment_chapters' AND TG_OP = 'DELETE';
        IF NOT audited THEN
            RAISE EXCEPTION '% % changed (%) without a curriculum_audit row of the same transaction', tgt, tgt_id, a
                USING ERRCODE = 'LRN01';
        END IF;
    END LOOP;
    RETURN NULL;
END;
$$;

CREATE CONSTRAINT TRIGGER zz_guard_education_levels_audited
    AFTER INSERT OR UPDATE ON education_levels
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION curriculum_change_audited();
CREATE CONSTRAINT TRIGGER zz_guard_academic_sections_audited
    AFTER INSERT OR UPDATE ON academic_sections
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION curriculum_change_audited();
CREATE CONSTRAINT TRIGGER zz_guard_curriculum_editions_audited
    AFTER INSERT OR UPDATE ON curriculum_editions
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION curriculum_change_audited();
CREATE CONSTRAINT TRIGGER zz_guard_curriculum_assignments_audited
    AFTER INSERT OR DELETE ON curriculum_assignments
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION curriculum_change_audited();
CREATE CONSTRAINT TRIGGER zz_guard_curriculum_assignment_chapters_audited
    AFTER INSERT OR DELETE ON curriculum_assignment_chapters
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION curriculum_change_audited();
CREATE CONSTRAINT TRIGGER zz_guard_subjects_retirement_audited
    AFTER UPDATE ON subjects
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW
    WHEN (OLD.retired_at IS DISTINCT FROM NEW.retired_at)
    EXECUTE FUNCTION curriculum_change_audited();
CREATE CONSTRAINT TRIGGER zz_guard_chapters_retirement_audited
    AFTER UPDATE ON chapters
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW
    WHEN (OLD.retired_at IS DISTINCT FROM NEW.retired_at)
    EXECUTE FUNCTION curriculum_change_audited();

-- The selection rule B03's placement validity builds on: an assignment can be
-- newly selected only while its edition is current and neither its section,
-- its level nor its subject is retired. A SQL-standard body: its tables are
-- bound when the function is created, so neither search_path nor a temporary
-- table of the caller can redirect them.
CREATE FUNCTION curriculum_assignment_selectable(assignment_id bigint) RETURNS boolean
LANGUAGE sql STABLE PARALLEL SAFE
SET search_path = pg_catalog, pg_temp
BEGIN ATOMIC
    SELECT EXISTS (
        SELECT 1
          FROM curriculum_assignments a
          JOIN curriculum_editions e ON e.id = a.edition_id
          JOIN academic_sections s ON s.id = e.section_id
          JOIN education_levels l ON l.id = s.level_id
          JOIN subjects sj ON sj.id = a.subject_id
         WHERE a.id = curriculum_assignment_selectable.assignment_id
           AND e.state = 'current'
           AND s.retired_at IS NULL
           AND l.retired_at IS NULL
           AND sj.retired_at IS NULL);
END;
