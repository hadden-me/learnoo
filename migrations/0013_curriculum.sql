-- LMS-B02 (issue #92): curriculum levels, academic sections, editions and
-- their subject assignments with ordered official chapters, retirement of
-- subjects and chapters, and the append-only curriculum_audit
-- (docs/LMS_CONTRACT.md §3, docs/LMS_PRODUCT_SPEC.md §2 "Curriculum
-- integrity" and §9). No curriculum data is seeded and no existing chapter is
-- placed into any edition: an administrator does that, audited.
--
-- Conventions of 0004, 0010 and 0012 apply: guards raise SQLSTATE LRN01 and
-- are named zz_guard_*; every function pins search_path to pg_catalog,
-- pg_temp and reaches other tables only through the schema of the table that
-- fired it (TG_TABLE_SCHEMA, or a schema argument derived from it, with
-- EXECUTE format('%I...') and bound parameters); no function is SECURITY
-- DEFINER. The guards bind the application's least-privilege role, not a
-- superuser or table owner. Names use catalogue_valid_name (0012).
-- catalogue_audit is not touched.
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
--     edition made current under a retired section or level or while one of
--     its assignments names a retired subject or chapter, no assignment of a
--     retired subject, no retired chapter added to a chapter list, no chapter
--     added to a retired subject, no new lesson in a retired subject, no
--     draft that sets a retired chapter other than its base revision's, and
--     no new reviewer grant on a retired subject. Existing references keep
--     resolving.
--   * Edition states: draft -> current -> superseded, current -> retired,
--     superseded -> retired, never back to draft. current_since,
--     superseded_at and retired_at are each stamped by their own transition,
--     at the transaction time, and by nothing else. At most one current
--     edition per section (partial unique index). At commit, an edition
--     superseded by a transaction left its place to an edition of its
--     section made current by that transaction, and an edition made current
--     replaced a previous current edition only by superseding it, never by
--     retiring it.
--   * Assignments and their chapter lists are inserted or deleted only while
--     the edition is draft, and never updated. The guards lock the edition
--     FOR SHARE, which conflicts with the FOR UPDATE that make-current and
--     retire take, so an edit cannot commit after the edition left draft.
--     Composite foreign keys keep an assignment's chapters in its subject.
--   * Audit, to the standard of 0012's catalogue renames. Every change is
--     described by its own curriculum_audit row of the same transaction:
--       1. Insert (zz_guard_curriculum_audit): the row records its own
--          transaction and time; it locks the entry (an edition FOR UPDATE,
--          other entries FOR NO KEY UPDATE, an assignment's edition FOR
--          SHARE) until the transaction ends, and its before values must be
--          the entry's current values. An assignment row's edition_id must
--          be the assignment's edition.
--       2. Update: a change of names, labels, order, retirement or state
--          needs the latest audit row of that kind for the entry, in the same
--          transaction, to record exactly that change (before = OLD, after =
--          NEW). Two changes therefore need two rows. Inserts and deletes
--          (create, add or remove an assignment, chapter-list rows) need a
--          row of the matching action at commit.
--       3. At commit (zz_guard_curriculum_audit_applied), every row was
--          applied: the next row of the same kind for the entry continues
--          from its after values or, for the last one, the entry still has
--          them (a retirement: retired_at is set; an assignment row: the
--          assignment still belongs to edition_id, or a later row of the
--          transaction removed it).
--     The audit is append-only. Value shapes (jsonb objects, exact keys):
--     rename {nameFr, nameAr} or, for editions, {labelFr, labelAr};
--     reorder {displayOrder}; retirement {retired: false} -> {retired: true};
--     edition state changes (make_current, supersede, retire) {state};
--     set_chapters {chapterIds: [...]} in position order; add_assignment
--     after {subjectId}; remove_assignment before {subjectId, chapterIds}.
--     create carries no before values (its after values are informative).
--
-- Global lock order (docs/LMS_CONTRACT.md §2): permission and account rows
-- (permissions.RequireTx, which therefore never waits behind a later guard
-- lock) -> curriculum editions (ascending ID) -> other rows. The guards'
-- locks on sections, levels, subjects and chapters come after the edition
-- locks of the same transaction; no path locks one of those rows and then an
-- edition.

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
    CONSTRAINT education_levels_names CHECK (catalogue_valid_name(name_fr) AND catalogue_valid_name(name_ar)),
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
    CONSTRAINT academic_sections_names CHECK (catalogue_valid_name(name_fr) AND catalogue_valid_name(name_ar)),
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
    CONSTRAINT curriculum_editions_labels CHECK (catalogue_valid_name(label_fr) AND catalogue_valid_name(label_ar)),
    CONSTRAINT curriculum_editions_state CHECK (state IN ('draft', 'current', 'superseded', 'retired')),
    -- Every non-draft edition was current once (only current and superseded
    -- editions can be retired).
    CONSTRAINT curriculum_editions_current_since CHECK ((state = 'draft') = (current_since IS NULL)),
    -- superseded_at is set exactly on editions that were superseded: every
    -- superseded edition, and a retired one only if it was superseded before
    -- (the guard stamps it on the current -> superseded transition only).
    CONSTRAINT curriculum_editions_superseded_at CHECK (
        (state <> 'superseded' OR superseded_at IS NOT NULL)
        AND (superseded_at IS NULL OR (state IN ('superseded', 'retired') AND superseded_at >= current_since))),
    CONSTRAINT curriculum_editions_retired_at CHECK (
        (state = 'retired') = (retired_at IS NOT NULL)
        AND (retired_at IS NULL OR retired_at >= coalesce(superseded_at, current_since))),
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
    CONSTRAINT curriculum_audit_operator_label CHECK (catalogue_valid_name(operator_label)),
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
        (before IS NULL OR jsonb_typeof(before) = 'object') AND (after IS NULL OR jsonb_typeof(after) = 'object')
        AND CASE action
            WHEN 'create' THEN before IS NULL
            WHEN 'add_assignment' THEN before IS NULL AND after IS NOT NULL
            WHEN 'remove_assignment' THEN before IS NOT NULL AND after IS NULL
            ELSE before IS NOT NULL AND after IS NOT NULL AND before <> after
            END)
);

CREATE INDEX curriculum_audit_target_idx ON curriculum_audit (target, target_id, id);
CREATE INDEX curriculum_audit_xact_idx ON curriculum_audit (xact_id);
CREATE INDEX curriculum_audit_edition_idx ON curriculum_audit (edition_id) WHERE edition_id IS NOT NULL;

-- The current values of an entry in the shape its audit rows record for
-- action (NULL when the entry does not exist), optionally locking the entry
-- (lock_mode '' or one of the listed row locks). sch is the schema of the
-- table whose trigger calls it.
CREATE FUNCTION curriculum_entry_values(sch text, target text, target_id bigint, action text, lock_mode text)
RETURNS jsonb
LANGUAGE plpgsql SET search_path = pg_catalog, pg_temp AS $$
DECLARE
    v jsonb;
BEGIN
    IF lock_mode NOT IN ('', 'FOR SHARE', 'FOR NO KEY UPDATE', 'FOR UPDATE') THEN
        RAISE EXCEPTION 'unknown lock mode' USING ERRCODE = 'LRN01';
    END IF;
    IF target = 'assignment' THEN
        IF action = 'set_chapters' THEN
            EXECUTE format('SELECT jsonb_build_object(''chapterIds'', coalesce(
                                (SELECT jsonb_agg(chapter_id ORDER BY position) FROM %I.curriculum_assignment_chapters
                                  WHERE assignment_id = a.id), ''[]''::jsonb))
                              FROM %I.curriculum_assignments a WHERE a.id = $1', sch, sch)
               INTO v USING target_id;
        ELSE
            EXECUTE format('SELECT jsonb_build_object(''subjectId'', a.subject_id, ''chapterIds'', coalesce(
                                (SELECT jsonb_agg(chapter_id ORDER BY position) FROM %I.curriculum_assignment_chapters
                                  WHERE assignment_id = a.id), ''[]''::jsonb))
                              FROM %I.curriculum_assignments a WHERE a.id = $1', sch, sch)
               INTO v USING target_id;
        END IF;
        RETURN v;
    END IF;
    EXECUTE format('SELECT to_jsonb(t) FROM %I.%I t WHERE id = $1 %s', sch,
                   CASE target
                       WHEN 'level' THEN 'education_levels'
                       WHEN 'section' THEN 'academic_sections'
                       WHEN 'edition' THEN 'curriculum_editions'
                       WHEN 'subject' THEN 'subjects'
                       ELSE 'chapters' END,
                   lock_mode)
       INTO v USING target_id;
    IF v IS NULL THEN
        RETURN NULL;
    END IF;
    RETURN CASE
        WHEN action = 'create' THEN '{}'::jsonb
        WHEN target = 'edition' AND action = 'rename' THEN jsonb_build_object('labelFr', v -> 'label_fr', 'labelAr', v -> 'label_ar')
        WHEN action = 'rename' THEN jsonb_build_object('nameFr', v -> 'name_fr', 'nameAr', v -> 'name_ar')
        WHEN action = 'reorder' THEN jsonb_build_object('displayOrder', v -> 'display_order')
        WHEN target = 'edition' THEN jsonb_build_object('state', v -> 'state')
        ELSE jsonb_build_object('retired', v -> 'retired_at' <> 'null'::jsonb)
        END;
END;
$$;

-- Rule 1. Update, delete and truncate: never.
CREATE FUNCTION curriculum_audit_guard() RETURNS trigger
LANGUAGE plpgsql SET search_path = pg_catalog, pg_temp AS $$
DECLARE
    cur jsonb;
    assignment_edition bigint;
    assignment_subject bigint;
BEGIN
    IF TG_OP <> 'INSERT' THEN
        RAISE EXCEPTION 'curriculum_audit is append-only' USING ERRCODE = 'LRN01';
    END IF;
    IF NEW.occurred_at IS DISTINCT FROM now() OR NEW.xact_id IS DISTINCT FROM pg_current_xact_id() THEN
        RAISE EXCEPTION 'a curriculum audit row records its own transaction and its time' USING ERRCODE = 'LRN01';
    END IF;
    IF NEW.target = 'assignment' THEN
        -- The edition first (lock order), then the assignment.
        EXECUTE format('SELECT 1 FROM %I.curriculum_editions WHERE id = $1 FOR SHARE', TG_TABLE_SCHEMA) USING NEW.edition_id;
        EXECUTE format('SELECT edition_id, subject_id FROM %I.curriculum_assignments WHERE id = $1', TG_TABLE_SCHEMA)
           INTO assignment_edition, assignment_subject USING NEW.target_id;
        IF assignment_edition IS DISTINCT FROM NEW.edition_id THEN
            RAISE EXCEPTION 'curriculum audit row names assignment % with another edition, or no such assignment', NEW.target_id
                USING ERRCODE = 'LRN01';
        END IF;
        IF NEW.action = 'add_assignment' THEN
            IF NEW.after IS DISTINCT FROM jsonb_build_object('subjectId', assignment_subject) THEN
                RAISE EXCEPTION 'an add_assignment row records the assignment''s subject' USING ERRCODE = 'LRN01';
            END IF;
            RETURN NEW;
        END IF;
    ELSIF NEW.action = 'create' THEN
        EXECUTE format('SELECT %I.curriculum_entry_values($1, $2, $3, $4, $5)', TG_TABLE_SCHEMA)
           INTO cur USING TG_TABLE_SCHEMA, NEW.target, NEW.target_id, 'create', '';
        IF cur IS NULL THEN
            RAISE EXCEPTION 'curriculum audit row names no existing %', NEW.target USING ERRCODE = 'LRN01';
        END IF;
        RETURN NEW;
    END IF;
    EXECUTE format('SELECT %I.curriculum_entry_values($1, $2, $3, $4, $5)', TG_TABLE_SCHEMA)
       INTO cur USING TG_TABLE_SCHEMA, NEW.target, NEW.target_id, NEW.action,
           CASE WHEN NEW.target = 'assignment' THEN ''
                WHEN NEW.target = 'edition' THEN 'FOR UPDATE'
                ELSE 'FOR NO KEY UPDATE' END;
    IF cur IS NULL THEN
        RAISE EXCEPTION 'curriculum audit row names no existing %', NEW.target USING ERRCODE = 'LRN01';
    END IF;
    IF NEW.before IS DISTINCT FROM cur THEN
        RAISE EXCEPTION 'a curriculum audit row records the entry''s current values as its before values (% %: %)',
            NEW.target, NEW.target_id, cur USING ERRCODE = 'LRN01';
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

-- Rule 3, at commit.
CREATE FUNCTION curriculum_audit_applied() RETURNS trigger
LANGUAGE plpgsql SET search_path = pg_catalog, pg_temp AS $$
DECLARE
    found_next boolean;
    next_before jsonb;
    removed_later boolean;
    assignment_edition bigint;
    cur jsonb;
BEGIN
    EXECUTE format('SELECT true, before FROM %I.curriculum_audit
                     WHERE target = $1 AND target_id = $2 AND action = ANY ($3) AND xact_id = $4 AND id > $5
                     ORDER BY id LIMIT 1', TG_TABLE_SCHEMA)
       INTO found_next, next_before
       USING NEW.target, NEW.target_id, CASE WHEN NEW.target = 'edition' AND NEW.action IN ('make_current', 'supersede', 'retire') THEN ARRAY['make_current', 'supersede', 'retire'] ELSE ARRAY[NEW.action] END, NEW.xact_id, NEW.id;
    IF found_next THEN
        IF NEW.action = 'remove_assignment' OR next_before IS DISTINCT FROM NEW.after THEN
            RAISE EXCEPTION 'curriculum audit row % is not followed by a change from its after values', NEW.id
                USING ERRCODE = 'LRN01';
        END IF;
        RETURN NULL;
    END IF;
    IF NEW.target = 'assignment' THEN
        EXECUTE format('SELECT edition_id FROM %I.curriculum_assignments WHERE id = $1', TG_TABLE_SCHEMA)
           INTO assignment_edition USING NEW.target_id;
        IF NEW.action = 'remove_assignment' THEN
            IF assignment_edition IS NOT NULL THEN
                RAISE EXCEPTION 'curriculum audit row % records a removal that was not applied', NEW.id USING ERRCODE = 'LRN01';
            END IF;
            RETURN NULL;
        END IF;
        IF assignment_edition IS NULL THEN
            EXECUTE format('SELECT EXISTS (SELECT 1 FROM %I.curriculum_audit
                             WHERE target = ''assignment'' AND target_id = $1 AND action = ''remove_assignment''
                               AND xact_id = $2 AND id > $3)', TG_TABLE_SCHEMA)
               INTO removed_later USING NEW.target_id, NEW.xact_id, NEW.id;
            IF NOT removed_later THEN
                RAISE EXCEPTION 'curriculum audit row % names an assignment that no longer exists', NEW.id USING ERRCODE = 'LRN01';
            END IF;
            RETURN NULL;
        END IF;
        IF assignment_edition <> NEW.edition_id THEN
            RAISE EXCEPTION 'curriculum audit row % names another edition than its assignment''s', NEW.id USING ERRCODE = 'LRN01';
        END IF;
        IF NEW.action = 'add_assignment' THEN
            RETURN NULL;
        END IF;
    END IF;
    EXECUTE format('SELECT %I.curriculum_entry_values($1, $2, $3, $4, $5)', TG_TABLE_SCHEMA)
       INTO cur USING TG_TABLE_SCHEMA, NEW.target, NEW.target_id, NEW.action, '';
    IF cur IS DISTINCT FROM NEW.after THEN
        RAISE EXCEPTION 'curriculum audit row % records a change that was not applied (% %: %)',
            NEW.id, NEW.target, NEW.target_id, cur USING ERRCODE = 'LRN01';
    END IF;
    RETURN NULL;
END;
$$;

CREATE CONSTRAINT TRIGGER zz_guard_curriculum_audit_applied
    AFTER INSERT ON curriculum_audit
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW
    WHEN (NEW.action <> 'create')
    EXECUTE FUNCTION curriculum_audit_applied();

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
-- identity immutable, version + 1, retirement one-way at the transaction
-- time, and every change of names, order or retirement audited (rule 2).
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
-- transitions, each stamping its own time (and only its own) once; making
-- an edition current needs its section and level not retired and none of
-- its assignments naming a retired subject or chapter (all share-locked);
-- label and state changes audited (rule 2).
CREATE FUNCTION curriculum_editions_guard() RETURNS trigger
LANGUAGE plpgsql SET search_path = pg_catalog, pg_temp AS $$
DECLARE
    section_retired timestamptz;
    level_retired timestamptz;
    found_row boolean;
    retired_entries text;
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
        IF (NEW.current_since IS DISTINCT FROM OLD.current_since)
               IS DISTINCT FROM (OLD.state = 'draft' AND NEW.state = 'current')
           OR (NEW.superseded_at IS DISTINCT FROM OLD.superseded_at)
               IS DISTINCT FROM (OLD.state = 'current' AND NEW.state = 'superseded')
           OR (NEW.retired_at IS DISTINCT FROM OLD.retired_at)
               IS DISTINCT FROM (OLD.state <> 'retired' AND NEW.state = 'retired')
           OR (NEW.current_since IS DISTINCT FROM OLD.current_since AND NEW.current_since IS DISTINCT FROM now())
           OR (NEW.superseded_at IS DISTINCT FROM OLD.superseded_at AND NEW.superseded_at IS DISTINCT FROM now())
           OR (NEW.retired_at IS DISTINCT FROM OLD.retired_at AND NEW.retired_at IS DISTINCT FROM now()) THEN
            RAISE EXCEPTION 'edition state times are set once, to the transaction time, by their own transition' USING ERRCODE = 'LRN01';
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
    IF TG_OP = 'UPDATE' AND NEW.state = 'current' AND OLD.state = 'draft' THEN
        -- Share-lock the subjects and chapters first, so a concurrent
        -- retirement waits, then read them.
        EXECUTE format('SELECT 1 FROM %I.subjects WHERE id IN (SELECT subject_id FROM %I.curriculum_assignments WHERE edition_id = $1) FOR SHARE',
                       TG_TABLE_SCHEMA, TG_TABLE_SCHEMA) USING NEW.id;
        EXECUTE format('SELECT 1 FROM %I.chapters WHERE id IN (SELECT ac.chapter_id FROM %I.curriculum_assignment_chapters ac
                         JOIN %I.curriculum_assignments a ON a.id = ac.assignment_id WHERE a.edition_id = $1) FOR SHARE',
                       TG_TABLE_SCHEMA, TG_TABLE_SCHEMA, TG_TABLE_SCHEMA) USING NEW.id;
        EXECUTE format('SELECT string_agg(e, '', '' ORDER BY e) FROM (
                          SELECT ''subject '' || sj.id AS e
                            FROM %I.curriculum_assignments a JOIN %I.subjects sj ON sj.id = a.subject_id
                           WHERE a.edition_id = $1 AND sj.retired_at IS NOT NULL
                          UNION ALL
                          SELECT ''chapter '' || c.id
                            FROM %I.curriculum_assignments a
                            JOIN %I.curriculum_assignment_chapters ac ON ac.assignment_id = a.id
                            JOIN %I.chapters c ON c.id = ac.chapter_id
                           WHERE a.edition_id = $1 AND c.retired_at IS NOT NULL) r',
                       TG_TABLE_SCHEMA, TG_TABLE_SCHEMA, TG_TABLE_SCHEMA, TG_TABLE_SCHEMA, TG_TABLE_SCHEMA)
           INTO retired_entries USING NEW.id;
        IF retired_entries IS NOT NULL THEN
            RAISE EXCEPTION 'edition % assigns retired entries (%); it cannot be made current', NEW.id, retired_entries
                USING ERRCODE = 'LRN01';
        END IF;
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER zz_guard_curriculum_editions
    BEFORE INSERT OR UPDATE ON curriculum_editions
    FOR EACH ROW EXECUTE FUNCTION curriculum_editions_guard();

-- At commit: an edition superseded by this transaction left its place to an
-- edition of its section made current by this transaction; a current edition
-- retired by this transaction was not replaced by one (a successor
-- supersedes its predecessor, it never retires it).
CREATE FUNCTION curriculum_editions_succession() RETURNS trigger
LANGUAGE plpgsql SET search_path = pg_catalog, pg_temp AS $$
DECLARE
    successor boolean;
BEGIN
    EXECUTE format('SELECT EXISTS (SELECT 1 FROM %I.curriculum_editions
                     WHERE section_id = $1 AND id <> $2 AND state = ''current'' AND current_since = pg_catalog.now())', TG_TABLE_SCHEMA)
       INTO successor USING NEW.section_id, NEW.id;
    IF NEW.state = 'superseded' AND NOT successor THEN
        RAISE EXCEPTION 'edition % can only be superseded by making another edition of its section current in the same transaction', NEW.id
            USING ERRCODE = 'LRN01';
    END IF;
    IF NEW.state = 'retired' AND successor THEN
        RAISE EXCEPTION 'edition % was replaced as current in this transaction; it must be superseded, not retired', NEW.id
            USING ERRCODE = 'LRN01';
    END IF;
    RETURN NULL;
END;
$$;

CREATE CONSTRAINT TRIGGER zz_guard_curriculum_editions_succession
    AFTER UPDATE ON curriculum_editions
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW
    WHEN (OLD.state = 'current' AND NEW.state IN ('superseded', 'retired'))
    EXECUTE FUNCTION curriculum_editions_succession();

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

-- Subjects and chapters: new entries are not retired and a chapter is not
-- added to a retired subject (share-locked); retirement is one-way at the
-- transaction time and audited (rule 2). Identity, names and order keep
-- their own guards (0004, 0010, 0012).
CREATE FUNCTION curriculum_catalogue_retirement_guard() RETURNS trigger
LANGUAGE plpgsql SET search_path = pg_catalog, pg_temp AS $$
DECLARE
    subject_retired timestamptz;
BEGIN
    IF TG_OP = 'INSERT' THEN
        IF NEW.retired_at IS NOT NULL THEN
            RAISE EXCEPTION 'a new % is not retired', TG_TABLE_NAME USING ERRCODE = 'LRN01';
        END IF;
        IF TG_TABLE_NAME = 'chapters' THEN
            EXECUTE format('SELECT retired_at FROM %I.subjects WHERE id = $1 FOR SHARE', TG_TABLE_SCHEMA)
               INTO subject_retired USING NEW.subject_id;
            IF subject_retired IS NOT NULL THEN
                RAISE EXCEPTION 'subject % is retired; no chapter can be added to it', NEW.subject_id USING ERRCODE = 'LRN01';
            END IF;
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

-- No new lesson in a retired subject (a translation is a new lesson). The
-- subject is share-locked, so a concurrent retirement waits.
CREATE FUNCTION lessons_guard_subject_not_retired() RETURNS trigger
LANGUAGE plpgsql SET search_path = pg_catalog, pg_temp AS $$
DECLARE
    subject_retired timestamptz;
BEGIN
    EXECUTE format('SELECT retired_at FROM %I.subjects WHERE id = $1 FOR SHARE', TG_TABLE_SCHEMA)
       INTO subject_retired USING NEW.subject_id;
    IF subject_retired IS NOT NULL THEN
        RAISE EXCEPTION 'subject % is retired; no lesson can be added to it', NEW.subject_id USING ERRCODE = 'LRN01';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER zz_guard_lessons_subject_not_retired
    BEFORE INSERT ON lessons
    FOR EACH ROW EXECUTE FUNCTION lessons_guard_subject_not_retired();

-- A draft cannot be created with, saved with a change to, or submitted with
-- a retired chapter unless it is the chapter of the draft's base revision.
-- A save that leaves the chapter unchanged is not checked (the author can
-- still edit the text); submission is checked, so a retired chapter never
-- reaches review. Later status changes (review, approval) are not checked.
CREATE FUNCTION lesson_revisions_guard_chapter_not_retired() RETURNS trigger
LANGUAGE plpgsql SET search_path = pg_catalog, pg_temp AS $$
DECLARE
    chapter_retired timestamptz;
    base_chapter bigint;
BEGIN
    IF NEW.chapter_id IS NULL THEN
        RETURN NEW;
    END IF;
    IF TG_OP = 'INSERT' THEN
        IF NEW.status <> 'draft' THEN
            RETURN NEW;
        END IF;
    ELSIF OLD.status <> 'draft'
       OR (NEW.status = 'draft' AND NEW.chapter_id IS NOT DISTINCT FROM OLD.chapter_id) THEN
        RETURN NEW;
    END IF;
    EXECUTE format('SELECT retired_at FROM %I.chapters WHERE id = $1 FOR SHARE', TG_TABLE_SCHEMA)
       INTO chapter_retired USING NEW.chapter_id;
    IF chapter_retired IS NULL THEN
        RETURN NEW;
    END IF;
    IF NEW.base_revision_id IS NOT NULL THEN
        EXECUTE format('SELECT chapter_id FROM %I.lesson_revisions WHERE id = $1', TG_TABLE_SCHEMA)
           INTO base_chapter USING NEW.base_revision_id;
    END IF;
    IF base_chapter IS DISTINCT FROM NEW.chapter_id THEN
        RAISE EXCEPTION 'chapter % is retired; a draft keeps it only from its base revision', NEW.chapter_id
            USING ERRCODE = 'LRN01';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER zz_guard_lesson_revisions_chapter_not_retired
    BEFORE INSERT OR UPDATE ON lesson_revisions
    FOR EACH ROW EXECUTE FUNCTION lesson_revisions_guard_chapter_not_retired();

-- No new reviewer grant on a retired subject (existing grants remain, and can
-- be revoked).
CREATE FUNCTION reviewer_assignments_guard_subject_not_retired() RETURNS trigger
LANGUAGE plpgsql SET search_path = pg_catalog, pg_temp AS $$
DECLARE
    subject_retired timestamptz;
BEGIN
    EXECUTE format('SELECT retired_at FROM %I.subjects WHERE id = $1 FOR SHARE', TG_TABLE_SCHEMA)
       INTO subject_retired USING NEW.subject_id;
    IF subject_retired IS NOT NULL THEN
        RAISE EXCEPTION 'subject % is retired; no reviewer can be newly assigned to it', NEW.subject_id USING ERRCODE = 'LRN01';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER zz_guard_reviewer_assignments_subject_not_retired
    BEFORE INSERT ON reviewer_assignments
    FOR EACH ROW EXECUTE FUNCTION reviewer_assignments_guard_subject_not_retired();

-- Rule 2: a change of names, labels, order, retirement or state of a level,
-- section, edition, subject or chapter needs the latest audit row of that
-- kind for the entry, in this transaction, to record exactly that change.
CREATE FUNCTION curriculum_update_audited() RETURNS trigger
LANGUAGE plpgsql SET search_path = pg_catalog, pg_temp AS $$
DECLARE
    o jsonb := to_jsonb(OLD);
    n jsonb := to_jsonb(NEW);
    tgt text := CASE TG_TABLE_NAME
        WHEN 'education_levels' THEN 'level'
        WHEN 'academic_sections' THEN 'section'
        WHEN 'curriculum_editions' THEN 'edition'
        WHEN 'subjects' THEN 'subject'
        ELSE 'chapter' END;
    changes jsonb[] := ARRAY[]::jsonb[];
    c jsonb;
    found_action text;
    found_before jsonb;
    found_after jsonb;
BEGIN
    IF tgt IN ('level', 'section') THEN
        IF (n -> 'name_fr', n -> 'name_ar') IS DISTINCT FROM (o -> 'name_fr', o -> 'name_ar') THEN
            changes := array_append(changes, jsonb_build_object('action', 'rename',
                'before', jsonb_build_object('nameFr', o -> 'name_fr', 'nameAr', o -> 'name_ar'),
                'after', jsonb_build_object('nameFr', n -> 'name_fr', 'nameAr', n -> 'name_ar')));
        END IF;
        IF n -> 'display_order' IS DISTINCT FROM o -> 'display_order' THEN
            changes := array_append(changes, jsonb_build_object('action', 'reorder',
                'before', jsonb_build_object('displayOrder', o -> 'display_order'),
                'after', jsonb_build_object('displayOrder', n -> 'display_order')));
        END IF;
    END IF;
    IF tgt = 'edition' THEN
        IF (n -> 'label_fr', n -> 'label_ar') IS DISTINCT FROM (o -> 'label_fr', o -> 'label_ar') THEN
            changes := array_append(changes, jsonb_build_object('action', 'rename',
                'before', jsonb_build_object('labelFr', o -> 'label_fr', 'labelAr', o -> 'label_ar'),
                'after', jsonb_build_object('labelFr', n -> 'label_fr', 'labelAr', n -> 'label_ar')));
        END IF;
        IF n -> 'state' IS DISTINCT FROM o -> 'state' THEN
            changes := array_append(changes, jsonb_build_object(
                'action', CASE n ->> 'state' WHEN 'current' THEN 'make_current' WHEN 'superseded' THEN 'supersede' ELSE 'retire' END,
                'before', jsonb_build_object('state', o -> 'state'),
                'after', jsonb_build_object('state', n -> 'state')));
        END IF;
    ELSIF n -> 'retired_at' IS DISTINCT FROM o -> 'retired_at' THEN
        changes := array_append(changes, jsonb_build_object('action', 'retire',
            'before', jsonb_build_object('retired', o -> 'retired_at' <> 'null'::jsonb),
            'after', jsonb_build_object('retired', n -> 'retired_at' <> 'null'::jsonb)));
    END IF;
    FOREACH c IN ARRAY changes LOOP
        EXECUTE format('SELECT action, before, after FROM %I.curriculum_audit
                         WHERE target = $1 AND target_id = $2 AND action = ANY ($3)
                           AND xact_id = pg_catalog.pg_current_xact_id()
                         ORDER BY id DESC LIMIT 1', TG_TABLE_SCHEMA)
           INTO found_action, found_before, found_after
           USING tgt, (n ->> 'id')::bigint,
                 CASE WHEN tgt = 'edition' AND c ->> 'action' IN ('make_current', 'supersede', 'retire')
                      THEN ARRAY['make_current', 'supersede', 'retire'] ELSE ARRAY[c ->> 'action'] END;
        IF found_action IS DISTINCT FROM c ->> 'action' OR found_before IS DISTINCT FROM c -> 'before'
           OR found_after IS DISTINCT FROM c -> 'after' THEN
            RAISE EXCEPTION '% % changed (%) without its curriculum_audit row of the same transaction recording exactly that change',
                tgt, n ->> 'id', c ->> 'action' USING ERRCODE = 'LRN01';
        END IF;
    END LOOP;
    RETURN NEW;
END;
$$;

CREATE TRIGGER zz_guard_education_levels_change_audited
    BEFORE UPDATE ON education_levels
    FOR EACH ROW EXECUTE FUNCTION curriculum_update_audited();
CREATE TRIGGER zz_guard_academic_sections_change_audited
    BEFORE UPDATE ON academic_sections
    FOR EACH ROW EXECUTE FUNCTION curriculum_update_audited();
CREATE TRIGGER zz_guard_curriculum_editions_change_audited
    BEFORE UPDATE ON curriculum_editions
    FOR EACH ROW EXECUTE FUNCTION curriculum_update_audited();
CREATE TRIGGER zz_guard_subjects_retirement_audited
    BEFORE UPDATE ON subjects
    FOR EACH ROW
    WHEN (OLD.retired_at IS DISTINCT FROM NEW.retired_at)
    EXECUTE FUNCTION curriculum_update_audited();
CREATE TRIGGER zz_guard_chapters_retirement_audited
    BEFORE UPDATE ON chapters
    FOR EACH ROW
    WHEN (OLD.retired_at IS DISTINCT FROM NEW.retired_at)
    EXECUTE FUNCTION curriculum_update_audited();

-- Inserts and deletes need, at commit, an audit row of the matching action
-- from the same transaction (updates are checked by rule 2 in the guards).
CREATE FUNCTION curriculum_change_audited() RETURNS trigger
LANGUAGE plpgsql SET search_path = pg_catalog, pg_temp AS $$
DECLARE
    tgt text;
    tgt_id bigint;
    act text;
    audited boolean;
    r jsonb;
BEGIN
    IF TG_OP = 'DELETE' THEN
        r := to_jsonb(OLD);
    ELSE
        r := to_jsonb(NEW);
    END IF;
    CASE TG_TABLE_NAME
    WHEN 'curriculum_assignments' THEN
        tgt := 'assignment';
        tgt_id := (r ->> 'id')::bigint;
        act := CASE TG_OP WHEN 'INSERT' THEN 'add_assignment' ELSE 'remove_assignment' END;
    WHEN 'curriculum_assignment_chapters' THEN
        tgt := 'assignment';
        tgt_id := (r ->> 'assignment_id')::bigint;
        act := 'set_chapters';
    ELSE
        tgt := CASE TG_TABLE_NAME
            WHEN 'education_levels' THEN 'level'
            WHEN 'academic_sections' THEN 'section'
            ELSE 'edition' END;
        tgt_id := (r ->> 'id')::bigint;
        act := 'create';
    END CASE;
    EXECUTE format('SELECT EXISTS (SELECT 1 FROM %I.curriculum_audit
                     WHERE target = $1 AND target_id = $2
                       AND (action = $3 OR ($4 AND action = ''remove_assignment''))
                       AND xact_id = pg_catalog.pg_current_xact_id())', TG_TABLE_SCHEMA)
       INTO audited
       USING tgt, tgt_id, act, TG_TABLE_NAME = 'curriculum_assignment_chapters' AND TG_OP = 'DELETE';
    IF NOT audited THEN
        RAISE EXCEPTION '% % changed (%) without a curriculum_audit row of the same transaction', tgt, tgt_id, act
            USING ERRCODE = 'LRN01';
    END IF;
    RETURN NULL;
END;
$$;

CREATE CONSTRAINT TRIGGER zz_guard_education_levels_audited
    AFTER INSERT ON education_levels
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION curriculum_change_audited();
CREATE CONSTRAINT TRIGGER zz_guard_academic_sections_audited
    AFTER INSERT ON academic_sections
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION curriculum_change_audited();
CREATE CONSTRAINT TRIGGER zz_guard_curriculum_editions_audited
    AFTER INSERT ON curriculum_editions
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
