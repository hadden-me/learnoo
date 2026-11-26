-- LMS-B03 (issue #93): course identities, course revisions, their curriculum
-- placements, modules and ordered steps, with owner draft CRUD
-- (docs/LMS_CONTRACT.md §4 and §5, docs/LMS_PRODUCT_SPEC.md §2-4). Review,
-- submission audit, publication, suspension, archive, withdrawal, transfer
-- and lesson dependency effects are LMS-B04 (migration 0015): the columns
-- they need exist here, and the guards below refuse every change they will
-- make (a revision leaving draft, the published pointer, archive, owner,
-- dependency_count, candidate_closed_at, withdrawn_at) until B04 replaces or
-- drops the guard concerned.
--
-- Conventions of 0004 and later apply: guards raise SQLSTATE LRN01 and are
-- named zz_guard_*; every trigger function pins search_path to
-- pg_catalog, pg_temp and reaches other tables only through the schema of the
-- table that fired it (TG_TABLE_SCHEMA, EXECUTE format('%I...') with bound
-- parameters), so a temporary table of the calling role cannot shadow them;
-- no function is SECURITY DEFINER. The SQL functions callers share
-- (course_step_revision_eligible, course_revision_placement_problems,
-- course_revision_placement_valid) have SQL-standard bodies, so their tables
-- are bound when they are created. The guards bind the application's
-- least-privilege role, not a superuser or table owner.
--
-- Rules kept here:
--
--   * Identity. A course's id, public_id, subject, language, creator and
--     creation time never change; it is created by its owner (owner_id =
--     created_by), unpublished, unarchived, without revisions or dependency
--     events. In B03 nothing else about a course changes except
--     last_revision_number, which only grows, to the course's highest
--     revision number: revision numbers are never reused, even after a draft
--     is discarded.
--   * Revisions mirror lesson_revisions (0004): created as a draft at
--     version 1 whose base_revision_id and compare_revision_id are the
--     course's published pointer (NULL until B04 publishes; B04 adds the
--     audit fallback of contract §5 to the comparison); identity columns
--     never change; a draft update increments version by one; a submitted
--     revision is immutable except a forward status change; only drafts are
--     deleted. In B03 a revision cannot leave draft at all
--     (zz_guard_course_revisions_not_submittable, dropped by B04 together
--     with the course submission audit it introduces). candidate_closed_at
--     and withdrawn_at are set only by a trigger running as the table owner
--     (as candidate_closed_at in 0004), so only B04's owner-run triggers can.
--   * Author owns course. On insert and on every update of a draft, a
--     revision's author_id equals its course's owner_id; the guard locks the
--     course FOR SHARE (the application has already locked it, so the lock
--     order course -> revision holds).
--   * Cover. cover_media_id names only an asset the revision's author
--     uploaded (media_uploads); cover_alt is required with a cover and absent
--     without one.
--   * One active candidate per course: the 0004 predicate (a draft, a
--     revision in review, or an approved revision whose candidate_closed_at
--     is NULL), excluding withdrawn revisions.
--   * Children. Placements, modules and steps change only while their
--     revision is a draft; the guard locks the revision FOR SHARE, like the
--     revision_media guard (0004), and FOR UPDATE for steps so that
--     concurrent step inserts serialize. A step names the course owner's own
--     lesson at any revision, or another author's lesson only at a revision
--     course_step_revision_eligible accepts (lesson and course share-locked). A placement may name only an assignment
--     whose edition left draft (an assignment of a draft edition can still be
--     removed or edited). Composite foreign keys keep every child in its
--     revision's subject and language, a module's chapter in that subject, a
--     step's lesson in that subject and language and its pinned revision in
--     that lesson.
--   * Delete. A course is deleted only while none of its revisions ever left
--     draft (a submitted revision never returns to draft and is never
--     deleted) and it was never published; deletion cascades to its draft
--     revisions and their children, never to lessons or media.
--   * Limits (contract §4): title, module and step titles <= 200 characters,
--     summary <= 1000, objectives and prerequisites <= 2000, cover_alt <= 300;
--     plain text without control characters (line feeds only in multi-line
--     fields), bidirectional formatting or line/paragraph separators (the
--     error report rule of 0009), trimmed. At most 50 modules (positions
--     1-50, unique) and 200 steps per revision (counted by the step guard).
--
-- Global lock order (contract §2): permission and account rows -> curriculum
-- editions (ascending ID) -> lessons (ascending ID, then the pinned lesson
-- revisions) -> course -> course revision -> child rows.

-- The course text rule, listed by code point so the check does not depend
-- on the locale. Line feeds are allowed only when multiline.
CREATE FUNCTION course_valid_text(t text, max_len integer, multiline boolean) RETURNS boolean
LANGUAGE sql IMMUTABLE PARALLEL SAFE STRICT
SET search_path = pg_catalog, pg_temp
RETURN char_length(t) <= max_len
    AND t = btrim(t, E' \n')
    AND t !~ E'[\\u0001-\\u0009\\u000B-\\u001F\\u007F-\\u009F\\u061C\\u200E\\u200F\\u2028-\\u202E\\u2066-\\u2069]'
    AND (multiline OR strpos(t, E'\n') = 0);

CREATE TABLE courses (
    id                    bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    public_id             uuid        NOT NULL DEFAULT gen_random_uuid(),
    owner_id              bigint      NOT NULL REFERENCES users (id),
    subject_id            bigint      NOT NULL REFERENCES subjects (id),
    language              text        NOT NULL,
    -- Set only by B04's audited publication workflow.
    published_revision_id bigint,
    archived_at           timestamptz,
    -- Highest revision number ever used (revision trigger below).
    last_revision_number  integer     NOT NULL DEFAULT 0,
    -- Lesson dependency events (B04 triggers only).
    dependency_count      integer     NOT NULL DEFAULT 0,
    created_at            timestamptz NOT NULL DEFAULT now(),
    created_by            bigint      NOT NULL REFERENCES users (id),
    CONSTRAINT courses_language CHECK (language IN ('fr', 'ar')),
    CONSTRAINT courses_last_revision_number CHECK (last_revision_number >= 0),
    CONSTRAINT courses_dependency_count CHECK (dependency_count >= 0),
    CONSTRAINT courses_public_id_key UNIQUE (public_id),
    -- Target of the composite foreign key that pins each revision's subject
    -- and language to its course's.
    CONSTRAINT courses_id_subject_language_key UNIQUE (id, subject_id, language)
);

CREATE INDEX courses_owner_idx ON courses (owner_id, id);
CREATE INDEX courses_subject_idx ON courses (subject_id);

CREATE TABLE course_revisions (
    id                  bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    course_id           bigint      NOT NULL,
    subject_id          bigint      NOT NULL,
    language            text        NOT NULL,
    revision_number     integer     NOT NULL,
    status              text        NOT NULL DEFAULT 'draft',
    -- Derived by the server; amendments are created by B04.
    kind                text        NOT NULL DEFAULT 'ordinary',
    author_id           bigint      NOT NULL REFERENCES users (id),
    -- The course's published revision when the draft was created.
    base_revision_id    bigint,
    -- The comparison revision of contract §5, set when the draft is created.
    compare_revision_id bigint,
    -- Drafts may be incomplete: B04's submission requires the fields it lists.
    title               text        NOT NULL DEFAULT '',
    summary             text        NOT NULL DEFAULT '',
    objectives          text        NOT NULL DEFAULT '',
    prerequisites       text        NOT NULL DEFAULT '',
    cover_media_id      bigint      REFERENCES media_assets (id),
    cover_alt           text,
    version             integer     NOT NULL DEFAULT 1,
    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now(),
    submitted_at        timestamptz,
    candidate_closed_at timestamptz,
    withdrawn_at        timestamptz,
    CONSTRAINT course_revisions_status CHECK (status IN ('draft', 'in_review', 'changes_requested', 'approved')),
    CONSTRAINT course_revisions_kind CHECK (kind IN ('ordinary', 'amendment')),
    CONSTRAINT course_revisions_amendment_compares CHECK (kind = 'ordinary' OR compare_revision_id IS NOT NULL),
    CONSTRAINT course_revisions_number_positive CHECK (revision_number >= 1),
    CONSTRAINT course_revisions_version_positive CHECK (version >= 1),
    CONSTRAINT course_revisions_submitted CHECK ((status = 'draft') = (submitted_at IS NULL)),
    CONSTRAINT course_revisions_candidate_closed CHECK (candidate_closed_at IS NULL OR status = 'approved'),
    CONSTRAINT course_revisions_withdrawn CHECK (withdrawn_at IS NULL OR status IN ('in_review', 'approved')),
    CONSTRAINT course_revisions_title CHECK (course_valid_text(title, 200, false)),
    CONSTRAINT course_revisions_summary CHECK (course_valid_text(summary, 1000, true)),
    CONSTRAINT course_revisions_objectives CHECK (course_valid_text(objectives, 2000, true)),
    CONSTRAINT course_revisions_prerequisites CHECK (course_valid_text(prerequisites, 2000, true)),
    CONSTRAINT course_revisions_cover_alt CHECK (course_valid_text(cover_alt, 300, false) AND cover_alt <> ''),
    CONSTRAINT course_revisions_cover_alt_with_cover CHECK ((cover_media_id IS NULL) = (cover_alt IS NULL)),
    CONSTRAINT course_revisions_number_key UNIQUE (course_id, revision_number),
    CONSTRAINT course_revisions_course_id_key UNIQUE (course_id, id),
    -- Targets of the placement, module and step foreign keys.
    CONSTRAINT course_revisions_id_subject_key UNIQUE (id, subject_id),
    CONSTRAINT course_revisions_id_subject_language_key UNIQUE (id, subject_id, language),
    CONSTRAINT course_revisions_course_fkey FOREIGN KEY (course_id, subject_id, language)
        REFERENCES courses (id, subject_id, language) ON DELETE CASCADE,
    CONSTRAINT course_revisions_base_fkey FOREIGN KEY (course_id, base_revision_id)
        REFERENCES course_revisions (course_id, id),
    CONSTRAINT course_revisions_compare_fkey FOREIGN KEY (course_id, compare_revision_id)
        REFERENCES course_revisions (course_id, id)
);

-- One active candidate per course, with the 0004 predicate; a withdrawn
-- revision (B04) is never active. changes_requested is history and does not
-- block the correction draft.
CREATE UNIQUE INDEX course_revisions_one_active_candidate
    ON course_revisions (course_id)
    WHERE withdrawn_at IS NULL
      AND (status IN ('draft', 'in_review') OR (status = 'approved' AND candidate_closed_at IS NULL));

CREATE INDEX course_revisions_author_idx ON course_revisions (author_id);
CREATE INDEX course_revisions_cover_idx ON course_revisions (cover_media_id) WHERE cover_media_id IS NOT NULL;

ALTER TABLE courses
    ADD CONSTRAINT courses_published_revision_fkey FOREIGN KEY (id, published_revision_id)
        REFERENCES course_revisions (course_id, id) DEFERRABLE INITIALLY DEFERRED;

CREATE TABLE course_revision_assignments (
    course_revision_id bigint NOT NULL,
    subject_id         bigint NOT NULL,
    assignment_id      bigint NOT NULL,
    CONSTRAINT course_revision_assignments_pkey PRIMARY KEY (course_revision_id, assignment_id),
    CONSTRAINT course_revision_assignments_revision_fkey FOREIGN KEY (course_revision_id, subject_id)
        REFERENCES course_revisions (id, subject_id) ON DELETE CASCADE,
    CONSTRAINT course_revision_assignments_assignment_fkey FOREIGN KEY (assignment_id, subject_id)
        REFERENCES curriculum_assignments (id, subject_id)
);

CREATE INDEX course_revision_assignments_assignment_idx ON course_revision_assignments (assignment_id);

CREATE TABLE course_modules (
    course_revision_id bigint  NOT NULL,
    subject_id         bigint  NOT NULL,
    language           text    NOT NULL,
    -- Stable across revisions of the course.
    module_key         uuid    NOT NULL,
    position           integer NOT NULL,
    title              text    NOT NULL,
    chapter_id         bigint,
    CONSTRAINT course_modules_position CHECK (position BETWEEN 1 AND 50),
    CONSTRAINT course_modules_title CHECK (course_valid_text(title, 200, false) AND title <> ''),
    CONSTRAINT course_modules_pkey PRIMARY KEY (course_revision_id, module_key),
    CONSTRAINT course_modules_position_key UNIQUE (course_revision_id, position),
    CONSTRAINT course_modules_revision_fkey FOREIGN KEY (course_revision_id, subject_id, language)
        REFERENCES course_revisions (id, subject_id, language) ON DELETE CASCADE,
    CONSTRAINT course_modules_chapter_fkey FOREIGN KEY (chapter_id, subject_id)
        REFERENCES chapters (id, subject_id)
);

CREATE INDEX course_modules_chapter_idx ON course_modules (chapter_id) WHERE chapter_id IS NOT NULL;

CREATE TABLE course_steps (
    course_revision_id bigint  NOT NULL,
    module_key         uuid    NOT NULL,
    position           integer NOT NULL,
    -- Course-authored, reviewed with the structure; the only title shown for
    -- an unreadable step (contract §5).
    title              text    NOT NULL,
    lesson_id          bigint  NOT NULL,
    subject_id         bigint  NOT NULL,
    language           text    NOT NULL,
    lesson_revision_id bigint  NOT NULL,
    CONSTRAINT course_steps_position CHECK (position BETWEEN 1 AND 200),
    CONSTRAINT course_steps_title CHECK (course_valid_text(title, 200, false) AND title <> ''),
    CONSTRAINT course_steps_pkey PRIMARY KEY (course_revision_id, lesson_id),
    CONSTRAINT course_steps_position_key UNIQUE (course_revision_id, module_key, position),
    CONSTRAINT course_steps_lesson_fkey FOREIGN KEY (lesson_id, language, subject_id)
        REFERENCES lessons (id, language, subject_id),
    CONSTRAINT course_steps_lesson_revision_fkey FOREIGN KEY (lesson_id, lesson_revision_id)
        REFERENCES lesson_revisions (lesson_id, id),
    CONSTRAINT course_steps_revision_fkey FOREIGN KEY (course_revision_id, subject_id, language)
        REFERENCES course_revisions (id, subject_id, language) ON DELETE CASCADE,
    CONSTRAINT course_steps_module_fkey FOREIGN KEY (course_revision_id, module_key)
        REFERENCES course_modules (course_revision_id, module_key) ON DELETE CASCADE
);

CREATE INDEX course_steps_lesson_idx ON course_steps (lesson_id);
CREATE INDEX course_steps_lesson_revision_idx ON course_steps (lesson_revision_id);

-- ---- courses ----

CREATE FUNCTION courses_guard() RETURNS trigger
LANGUAGE plpgsql SET search_path = pg_catalog, pg_temp AS $$
DECLARE
    history boolean;
    top     integer;
BEGIN
    IF TG_OP = 'INSERT' THEN
        IF NEW.owner_id IS DISTINCT FROM NEW.created_by OR NEW.published_revision_id IS NOT NULL
           OR NEW.archived_at IS NOT NULL OR NEW.last_revision_number <> 0 OR NEW.dependency_count <> 0
           OR NEW.created_at IS DISTINCT FROM now() THEN
            RAISE EXCEPTION 'a course is created by its owner at the transaction time, unpublished, unarchived, without revisions or dependency events'
                USING ERRCODE = 'LRN01';
        END IF;
        RETURN NEW;
    END IF;
    IF TG_OP = 'UPDATE' THEN
        IF (NEW.id, NEW.public_id, NEW.subject_id, NEW.language, NEW.created_at, NEW.created_by)
           IS DISTINCT FROM (OLD.id, OLD.public_id, OLD.subject_id, OLD.language, OLD.created_at, OLD.created_by) THEN
            RAISE EXCEPTION 'course identity is immutable' USING ERRCODE = 'LRN01';
        END IF;
        IF NEW.last_revision_number < OLD.last_revision_number THEN
            RAISE EXCEPTION 'revision numbers are never reused' USING ERRCODE = 'LRN01';
        END IF;
        -- It grows only to the highest number used, which is what the
        -- advance trigger on course_revisions sets after an insert.
        IF NEW.last_revision_number > OLD.last_revision_number THEN
            EXECUTE format('SELECT max(revision_number) FROM %I.course_revisions WHERE course_id = $1', TG_TABLE_SCHEMA)
               INTO top USING OLD.id;
            IF NEW.last_revision_number IS DISTINCT FROM top THEN
                RAISE EXCEPTION 'course % last_revision_number can only become its highest revision number', OLD.id
                    USING ERRCODE = 'LRN01';
            END IF;
        END IF;
        -- B04 replaces this function to admit its audited workflows.
        IF to_jsonb(NEW) - 'last_revision_number' IS DISTINCT FROM to_jsonb(OLD) - 'last_revision_number' THEN
            RAISE EXCEPTION 'course % owner, publication, archive and dependency state change only through their audited workflows', OLD.id
                USING ERRCODE = 'LRN01';
        END IF;
        RETURN NEW;
    END IF;
    -- DELETE: only a course whose revisions never left draft and that was
    -- never published. A submitted revision never returns to draft and is
    -- never deleted, so its existence is the history.
    EXECUTE format('SELECT EXISTS (SELECT 1 FROM %I.course_revisions WHERE course_id = $1 AND status <> ''draft'')', TG_TABLE_SCHEMA)
       INTO history USING OLD.id;
    IF history OR OLD.published_revision_id IS NOT NULL THEN
        RAISE EXCEPTION 'course % has submitted history and is never deleted', OLD.id USING ERRCODE = 'LRN01';
    END IF;
    RETURN OLD;
END;
$$;

CREATE TRIGGER zz_guard_courses
    BEFORE INSERT OR UPDATE OR DELETE ON courses
    FOR EACH ROW EXECUTE FUNCTION courses_guard();

CREATE FUNCTION course_tables_no_truncate() RETURNS trigger
LANGUAGE plpgsql SET search_path = pg_catalog, pg_temp AS $$
BEGIN
    RAISE EXCEPTION '% cannot be truncated', TG_TABLE_NAME USING ERRCODE = 'LRN01';
END;
$$;

CREATE TRIGGER zz_guard_courses_no_truncate
    BEFORE TRUNCATE ON courses FOR EACH STATEMENT EXECUTE FUNCTION course_tables_no_truncate();
CREATE TRIGGER zz_guard_course_revisions_no_truncate
    BEFORE TRUNCATE ON course_revisions FOR EACH STATEMENT EXECUTE FUNCTION course_tables_no_truncate();
CREATE TRIGGER zz_guard_course_revision_assignments_no_truncate
    BEFORE TRUNCATE ON course_revision_assignments FOR EACH STATEMENT EXECUTE FUNCTION course_tables_no_truncate();
CREATE TRIGGER zz_guard_course_modules_no_truncate
    BEFORE TRUNCATE ON course_modules FOR EACH STATEMENT EXECUTE FUNCTION course_tables_no_truncate();
CREATE TRIGGER zz_guard_course_steps_no_truncate
    BEFORE TRUNCATE ON course_steps FOR EACH STATEMENT EXECUTE FUNCTION course_tables_no_truncate();

-- ---- course revisions ----

-- Insert: a draft at version 1, at the transaction time, never closed or
-- withdrawn, ordinary (B04 creates amendments), drafted against the course's
-- published pointer. Reads the course FOR SHARE (the application has locked
-- it already).
CREATE FUNCTION course_revisions_guard_insert() RETURNS trigger
LANGUAGE plpgsql SET search_path = pg_catalog, pg_temp AS $$
DECLARE
    found_course boolean;
    published    bigint;
BEGIN
    IF NEW.status <> 'draft' OR NEW.version <> 1 OR NEW.submitted_at IS NOT NULL
       OR NEW.candidate_closed_at IS NOT NULL OR NEW.withdrawn_at IS NOT NULL
       OR NEW.created_at IS DISTINCT FROM now() OR NEW.updated_at IS DISTINCT FROM now() THEN
        RAISE EXCEPTION 'a course revision is created as an open draft at version 1, at the transaction time' USING ERRCODE = 'LRN01';
    END IF;
    IF NEW.kind <> 'ordinary' THEN
        RAISE EXCEPTION 'course amendments are created only by the amendment workflow' USING ERRCODE = 'LRN01';
    END IF;
    -- EXECUTE does not set FOUND; a missing row leaves found_course NULL.
    EXECUTE format('SELECT true, published_revision_id FROM %I.courses WHERE id = $1 FOR SHARE', TG_TABLE_SCHEMA)
       INTO found_course, published USING NEW.course_id;
    IF found_course IS NULL THEN
        RAISE EXCEPTION 'course % does not exist', NEW.course_id USING ERRCODE = 'LRN01';
    END IF;
    IF NEW.base_revision_id IS DISTINCT FROM published THEN
        RAISE EXCEPTION 'base_revision_id must be the course''s current published revision' USING ERRCODE = 'LRN01';
    END IF;
    -- Contract §5: the pointer, or (B04) the last publish or restore when it
    -- is NULL. Until B04 no course is ever published, so it is the pointer.
    IF NEW.compare_revision_id IS DISTINCT FROM published THEN
        RAISE EXCEPTION 'compare_revision_id must be the course''s comparison revision' USING ERRCODE = 'LRN01';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER zz_guard_course_revisions_insert_as_draft
    BEFORE INSERT ON course_revisions
    FOR EACH ROW EXECUTE FUNCTION course_revisions_guard_insert();

-- On insert and on every update of a draft, the author is the course's owner.
-- owner_id changes only through B04's transfer, which locks the course FOR
-- UPDATE: the share lock here keeps it stable until this write commits. A
-- course the writer cannot see has no owner and refuses.
CREATE FUNCTION course_revisions_guard_owner() RETURNS trigger
LANGUAGE plpgsql SET search_path = pg_catalog, pg_temp AS $$
DECLARE
    owner bigint;
BEGIN
    IF TG_OP = 'UPDATE' AND OLD.status <> 'draft' THEN
        RETURN NEW;
    END IF;
    EXECUTE format('SELECT owner_id FROM %I.courses WHERE id = $1 FOR SHARE', TG_TABLE_SCHEMA)
       INTO owner USING NEW.course_id;
    IF NEW.author_id IS DISTINCT FROM owner THEN
        RAISE EXCEPTION 'a course revision must be authored by the course''s owner' USING ERRCODE = 'LRN01';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER zz_guard_course_revisions_author_owns_course
    BEFORE INSERT OR UPDATE ON course_revisions
    FOR EACH ROW EXECUTE FUNCTION course_revisions_guard_owner();

-- The cover is an asset the revision's author uploaded. Checked whenever it
-- is set or changed; a submitted revision cannot change it (snapshot guard).
-- media_uploads is append-only, so a stale snapshot can only miss a link
-- and refuse.
CREATE FUNCTION course_revisions_guard_cover() RETURNS trigger
LANGUAGE plpgsql SET search_path = pg_catalog, pg_temp AS $$
DECLARE
    uploaded boolean;
BEGIN
    IF NEW.cover_media_id IS NULL
       OR (TG_OP = 'UPDATE' AND NEW.cover_media_id IS NOT DISTINCT FROM OLD.cover_media_id) THEN
        RETURN NEW;
    END IF;
    EXECUTE format('SELECT EXISTS (SELECT 1 FROM %I.media_uploads WHERE user_id = $1 AND media_asset_id = $2)', TG_TABLE_SCHEMA)
       INTO uploaded USING NEW.author_id, NEW.cover_media_id;
    IF NOT uploaded THEN
        RAISE EXCEPTION 'a course cover must be an image the revision''s author uploaded' USING ERRCODE = 'LRN01';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER zz_guard_course_revisions_cover_uploaded
    BEFORE INSERT OR UPDATE ON course_revisions
    FOR EACH ROW EXECUTE FUNCTION course_revisions_guard_cover();

-- A new revision number exceeds every number the course used. AFTER
-- INSERT, so the unique indexes are checked first.
CREATE FUNCTION course_revisions_advance_number() RETURNS trigger
LANGUAGE plpgsql SET search_path = pg_catalog, pg_temp AS $$
DECLARE
    n bigint;
BEGIN
    EXECUTE format('UPDATE %I.courses SET last_revision_number = $1
                     WHERE id = $2 AND last_revision_number < $1', TG_TABLE_SCHEMA)
      USING NEW.revision_number, NEW.course_id;
    GET DIAGNOSTICS n = ROW_COUNT;
    IF n <> 1 THEN
        RAISE EXCEPTION 'revision number % was already used for course %', NEW.revision_number, NEW.course_id
            USING ERRCODE = 'LRN01';
    END IF;
    RETURN NULL;
END;
$$;

CREATE TRIGGER zz_guard_course_revisions_number_never_reused
    AFTER INSERT ON course_revisions
    FOR EACH ROW EXECUTE FUNCTION course_revisions_advance_number();

-- Snapshots, as lesson_revisions_guard_update (0004): identity never
-- changes; candidate_closed_at and withdrawn_at are set once, to the
-- transaction time, alone, only from inside a trigger running as the owner
-- of course_revisions (B04's SECURITY DEFINER triggers); a draft update
-- increments version by one; a submitted revision changes only its status,
-- forward, with updated_at.
CREATE FUNCTION course_revisions_guard_update() RETURNS trigger
LANGUAGE plpgsql SET search_path = pg_catalog, pg_temp AS $$
DECLARE
    col text;
BEGIN
    IF (NEW.id, NEW.course_id, NEW.revision_number, NEW.author_id, NEW.base_revision_id, NEW.compare_revision_id,
        NEW.language, NEW.subject_id, NEW.created_at)
       IS DISTINCT FROM
       (OLD.id, OLD.course_id, OLD.revision_number, OLD.author_id, OLD.base_revision_id, OLD.compare_revision_id,
        OLD.language, OLD.subject_id, OLD.created_at) THEN
        RAISE EXCEPTION 'course revision identity is immutable' USING ERRCODE = 'LRN01';
    END IF;
    FOREACH col IN ARRAY ARRAY['candidate_closed_at', 'withdrawn_at'] LOOP
        IF to_jsonb(NEW) -> col IS DISTINCT FROM to_jsonb(OLD) -> col THEN
            IF pg_trigger_depth() < 2
               OR NOT EXISTS (SELECT 1 FROM pg_catalog.pg_class c
                               WHERE c.oid = TG_RELID
                                 AND c.relowner = (SELECT r.oid FROM pg_catalog.pg_roles r WHERE r.rolname = current_user)) THEN
                RAISE EXCEPTION 'course revision % % is set only by its audited workflow', OLD.id, col USING ERRCODE = 'LRN01';
            END IF;
            IF to_jsonb(OLD) ->> col IS NOT NULL OR (to_jsonb(NEW) ->> col)::timestamptz IS DISTINCT FROM now()
               OR to_jsonb(NEW) - col IS DISTINCT FROM to_jsonb(OLD) - col THEN
                RAISE EXCEPTION 'course revision % % can only be set once, to the transaction time, alone', OLD.id, col
                    USING ERRCODE = 'LRN01';
            END IF;
            RETURN NEW;
        END IF;
    END LOOP;
    IF OLD.status <> 'draft'
       AND (to_jsonb(NEW) - 'status' - 'updated_at' IS DISTINCT FROM to_jsonb(OLD) - 'status' - 'updated_at'
            OR (NEW.status = OLD.status AND NEW.updated_at IS DISTINCT FROM OLD.updated_at)) THEN
        RAISE EXCEPTION 'course revision % is submitted and immutable', OLD.id USING ERRCODE = 'LRN01';
    END IF;
    IF NEW.status IS DISTINCT FROM OLD.status
       AND (OLD.status, NEW.status) NOT IN (
           ('draft', 'in_review'),
           ('in_review', 'changes_requested'),
           ('in_review', 'approved')) THEN
        RAISE EXCEPTION 'course revision status cannot change from % to %', OLD.status, NEW.status USING ERRCODE = 'LRN01';
    END IF;
    IF OLD.status = 'draft' AND NEW.status = 'draft' AND NEW.version IS DISTINCT FROM OLD.version + 1 THEN
        RAISE EXCEPTION 'every update of course draft % increments its version by one', OLD.id USING ERRCODE = 'LRN01';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER zz_guard_course_revisions_immutable_snapshot
    BEFORE UPDATE ON course_revisions
    FOR EACH ROW EXECUTE FUNCTION course_revisions_guard_update();

-- Until B04 adds course submission (with its audit) and review, no course
-- revision leaves draft. B04 drops this trigger.
CREATE FUNCTION course_revisions_guard_not_submittable() RETURNS trigger
LANGUAGE plpgsql SET search_path = pg_catalog, pg_temp AS $$
BEGIN
    IF NEW.status IS DISTINCT FROM OLD.status THEN
        RAISE EXCEPTION 'course revisions cannot be submitted or decided yet' USING ERRCODE = 'LRN01';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER zz_guard_course_revisions_not_submittable
    BEFORE UPDATE ON course_revisions
    FOR EACH ROW EXECUTE FUNCTION course_revisions_guard_not_submittable();

CREATE FUNCTION course_revisions_guard_delete() RETURNS trigger
LANGUAGE plpgsql SET search_path = pg_catalog, pg_temp AS $$
BEGIN
    IF OLD.status <> 'draft' THEN
        RAISE EXCEPTION 'course revision % is submitted and cannot be deleted', OLD.id USING ERRCODE = 'LRN01';
    END IF;
    RETURN OLD;
END;
$$;

CREATE TRIGGER zz_guard_course_revisions_keep_submitted
    BEFORE DELETE ON course_revisions
    FOR EACH ROW EXECUTE FUNCTION course_revisions_guard_delete();

-- ---- placements, modules and steps ----

-- Changes only while the revision is a draft. FOR SHARE conflicts with the
-- status update of a concurrent submission (B04), so a child row cannot be
-- added, changed or removed after the snapshot is frozen. A missing revision
-- row means a draft is being deleted (ON DELETE CASCADE; submitted revisions
-- cannot be). New placements name only assignments of editions that left
-- draft (edition share-locked: an edition never returns to draft). New steps
-- are counted: at most 200 per revision.
CREATE FUNCTION course_children_guard() RETURNS trigger
LANGUAGE plpgsql SET search_path = pg_catalog, pg_temp AS $$
DECLARE
    rev          bigint;
    st           text;
    ed_st        text;
    asg          bigint;
    n            bigint;
    lock_mode    text;
    o            jsonb;
    nw           jsonb;
    lesson_owner bigint;
    course_owner bigint;
    eligible     boolean;
BEGIN
    -- Columns are read through to_jsonb: this function serves three tables.
    IF TG_OP <> 'INSERT' THEN
        o := to_jsonb(OLD);
    END IF;
    IF TG_OP <> 'DELETE' THEN
        nw := to_jsonb(NEW);
    END IF;
    -- Steps lock the revision FOR UPDATE so that concurrent step inserts
    -- into one draft serialize and the count below is exact (at READ
    -- COMMITTED, where each statement re-reads after the lock).
    lock_mode := CASE WHEN TG_TABLE_NAME = 'course_steps' THEN 'FOR UPDATE' ELSE 'FOR SHARE' END;
    FOREACH rev IN ARRAY ARRAY[(o ->> 'course_revision_id')::bigint, (nw ->> 'course_revision_id')::bigint]
    LOOP
        CONTINUE WHEN rev IS NULL;
        -- EXECUTE does not set FOUND; a missing row leaves st NULL.
        st := NULL;
        EXECUTE format('SELECT status FROM %I.course_revisions WHERE id = $1 ' || lock_mode, TG_TABLE_SCHEMA)
           INTO st USING rev;
        IF st IS NOT NULL AND st <> 'draft' THEN
            RAISE EXCEPTION 'course revision % is submitted; its % are immutable', rev, TG_TABLE_NAME USING ERRCODE = 'LRN01';
        END IF;
    END LOOP;
    IF TG_OP = 'DELETE' THEN
        RETURN OLD;
    END IF;
    IF TG_TABLE_NAME = 'course_revision_assignments' THEN
        asg := (nw ->> 'assignment_id')::bigint;
        IF TG_OP = 'INSERT' OR asg IS DISTINCT FROM (o ->> 'assignment_id')::bigint THEN
            EXECUTE format('SELECT e.state FROM %1$I.curriculum_assignments a JOIN %1$I.curriculum_editions e ON e.id = a.edition_id
                             WHERE a.id = $1 FOR SHARE OF e', TG_TABLE_SCHEMA)
               INTO ed_st USING asg;
            IF ed_st IS NULL OR ed_st = 'draft' THEN
                RAISE EXCEPTION 'assignment % is not in an edition that left draft', asg USING ERRCODE = 'LRN01';
            END IF;
        END IF;
    END IF;
    IF TG_TABLE_NAME = 'course_steps' THEN
        -- At most 200 steps per revision, for a new step or one moved into
        -- another revision.
        IF TG_OP = 'INSERT' OR nw -> 'course_revision_id' IS DISTINCT FROM o -> 'course_revision_id' THEN
            EXECUTE format('SELECT count(*) FROM %I.course_steps WHERE course_revision_id = $1', TG_TABLE_SCHEMA)
               INTO n USING (nw ->> 'course_revision_id')::bigint;
            IF n >= 200 THEN
                RAISE EXCEPTION 'a course revision has at most 200 steps' USING ERRCODE = 'LRN01';
            END IF;
        END IF;
        -- Attachability (contract §4): the course owner's own lesson at any
        -- revision, or another author's lesson only at an eligible revision
        -- (course_step_revision_eligible). The lesson is share-locked (the
        -- application has locked it already), so it cannot be unpublished,
        -- archived or have the revision withdrawn before this commits; the
        -- course is share-locked so its owner cannot change.
        IF TG_OP = 'INSERT'
           OR (nw -> 'lesson_id', nw -> 'lesson_revision_id', nw -> 'course_revision_id')
              IS DISTINCT FROM (o -> 'lesson_id', o -> 'lesson_revision_id', o -> 'course_revision_id') THEN
            EXECUTE format('SELECT created_by FROM %I.lessons WHERE id = $1 FOR SHARE', TG_TABLE_SCHEMA)
               INTO lesson_owner USING (nw ->> 'lesson_id')::bigint;
            EXECUTE format('SELECT c.owner_id FROM %1$I.course_revisions r JOIN %1$I.courses c ON c.id = r.course_id
                             WHERE r.id = $1 FOR SHARE OF c', TG_TABLE_SCHEMA)
               INTO course_owner USING (nw ->> 'course_revision_id')::bigint;
            IF lesson_owner IS NULL OR lesson_owner IS DISTINCT FROM course_owner THEN
                EXECUTE format('SELECT %I.course_step_revision_eligible($1, $2)', TG_TABLE_SCHEMA)
                   INTO eligible USING (nw ->> 'lesson_id')::bigint, (nw ->> 'lesson_revision_id')::bigint;
                IF eligible IS NOT TRUE THEN
                    RAISE EXCEPTION 'lesson % is another author''s and revision % of it is not eligible', nw ->> 'lesson_id', nw ->> 'lesson_revision_id'
                        USING ERRCODE = 'LRN01';
                END IF;
            END IF;
        END IF;
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER zz_guard_course_revision_assignments_draft_only
    BEFORE INSERT OR UPDATE OR DELETE ON course_revision_assignments
    FOR EACH ROW EXECUTE FUNCTION course_children_guard();
CREATE TRIGGER zz_guard_course_modules_draft_only
    BEFORE INSERT OR UPDATE OR DELETE ON course_modules
    FOR EACH ROW EXECUTE FUNCTION course_children_guard();
CREATE TRIGGER zz_guard_course_steps_draft_only
    BEFORE INSERT OR UPDATE OR DELETE ON course_steps
    FOR EACH ROW EXECUTE FUNCTION course_children_guard();

-- ---- shared definitions (contract §3 and §5) ----

-- Eligible lesson revision: approved, not withdrawn, published at least once
-- (a 'publish' row in publication_audit), and its lesson currently published
-- and not archived.
CREATE FUNCTION course_step_revision_eligible(lesson_id bigint, revision_id bigint) RETURNS boolean
LANGUAGE sql STABLE PARALLEL SAFE
SET search_path = pg_catalog, pg_temp
BEGIN ATOMIC
    SELECT EXISTS (
        SELECT 1
          FROM lesson_revisions r
          JOIN lessons l ON l.id = r.lesson_id
         WHERE r.id = course_step_revision_eligible.revision_id
           AND r.lesson_id = course_step_revision_eligible.lesson_id
           AND r.status = 'approved'
           AND NOT EXISTS (SELECT 1 FROM revision_withdrawals w WHERE w.revision_id = r.id)
           AND EXISTS (SELECT 1 FROM publication_audit p WHERE p.revision_id = r.id AND p.action = 'publish')
           AND l.published_revision_id IS NOT NULL
           AND l.archived_at IS NULL);
END;

-- Placement problems of a course revision for a check mode: 'save',
-- 'submit' or 'publish' (contract §3). One row per problem:
--   unknown_mode                  the mode is none of the three;
--   amendment_assignments_changed an amendment's assignments differ from its
--                                 comparison revision's (assignment named);
--   assignment_not_selectable     save/submit of an ordinary revision: an
--                                 assignment not on the comparison revision
--                                 is not selectable now
--                                 (curriculum_assignment_selectable);
--   assignment_retired            submit: a carried assignment's edition,
--                                 section or level is retired; publish: any
--                                 assignment's edition is neither current nor
--                                 superseded, or its section or level is
--                                 retired (on save a carried retired
--                                 assignment is only flagged, by the caller);
--   no_assignment                 submit/publish: no assignment at all;
--   chapter_outside_assignments   a module chapter is in none of the
--                                 revision's assignments;
--   chapter_retired               a module chapter is retired and not
--                                 inherited unchanged (same module_key and
--                                 chapter on the comparison revision).
-- Amendments are exempt from the assignment state rules (spec rule 2).
CREATE FUNCTION course_revision_placement_problems(rev_id bigint, check_mode text DEFAULT 'save')
RETURNS TABLE (problem text, assignment bigint, module uuid, chapter bigint)
LANGUAGE sql STABLE PARALLEL SAFE
SET search_path = pg_catalog, pg_temp
BEGIN ATOMIC
    WITH r AS (
        SELECT cr.id, cr.kind, cr.compare_revision_id
          FROM course_revisions cr
         WHERE cr.id = course_revision_placement_problems.rev_id),
    mine AS (
        SELECT cra.assignment_id,
               EXISTS (SELECT 1 FROM course_revision_assignments p
                        WHERE p.course_revision_id = r.compare_revision_id
                          AND p.assignment_id = cra.assignment_id) AS carried,
               e.state AS edition_state,
               (s.retired_at IS NOT NULL OR lv.retired_at IS NOT NULL) AS parent_retired
          FROM r
          JOIN course_revision_assignments cra ON cra.course_revision_id = r.id
          JOIN curriculum_assignments a ON a.id = cra.assignment_id
          JOIN curriculum_editions e ON e.id = a.edition_id
          JOIN academic_sections s ON s.id = e.section_id
          JOIN education_levels lv ON lv.id = s.level_id)
    SELECT 'unknown_mode', NULL::bigint, NULL::uuid, NULL::bigint
     WHERE course_revision_placement_problems.check_mode IS DISTINCT FROM 'save'
       AND course_revision_placement_problems.check_mode IS DISTINCT FROM 'submit'
       AND course_revision_placement_problems.check_mode IS DISTINCT FROM 'publish'
    UNION ALL
    SELECT 'amendment_assignments_changed', x.assignment_id, NULL, NULL
      FROM r
      JOIN LATERAL (
            (SELECT assignment_id FROM course_revision_assignments WHERE course_revision_id = r.id
             EXCEPT SELECT assignment_id FROM course_revision_assignments WHERE course_revision_id = r.compare_revision_id)
            UNION ALL
            (SELECT assignment_id FROM course_revision_assignments WHERE course_revision_id = r.compare_revision_id
             EXCEPT SELECT assignment_id FROM course_revision_assignments WHERE course_revision_id = r.id)) x ON true
     WHERE r.kind = 'amendment'
    UNION ALL
    SELECT 'assignment_not_selectable', m.assignment_id, NULL, NULL
      FROM r JOIN mine m ON true
     WHERE r.kind = 'ordinary'
       AND course_revision_placement_problems.check_mode IN ('save', 'submit')
       AND NOT m.carried
       AND NOT curriculum_assignment_selectable(m.assignment_id)
    UNION ALL
    SELECT 'assignment_retired', m.assignment_id, NULL, NULL
      FROM r JOIN mine m ON true
     WHERE r.kind = 'ordinary'
       AND ((course_revision_placement_problems.check_mode = 'submit' AND m.carried
             AND (m.edition_state NOT IN ('current', 'superseded') OR m.parent_retired))
         OR (course_revision_placement_problems.check_mode = 'publish'
             AND (m.edition_state NOT IN ('current', 'superseded') OR m.parent_retired)))
    UNION ALL
    SELECT 'no_assignment', NULL, NULL, NULL
      FROM r
     WHERE course_revision_placement_problems.check_mode IN ('submit', 'publish')
       AND NOT EXISTS (SELECT 1 FROM course_revision_assignments WHERE course_revision_id = r.id)
    UNION ALL
    SELECT 'chapter_outside_assignments', NULL, cm.module_key, cm.chapter_id
      FROM r JOIN course_modules cm ON cm.course_revision_id = r.id
     WHERE cm.chapter_id IS NOT NULL
       AND NOT EXISTS (SELECT 1 FROM course_revision_assignments cra
                         JOIN curriculum_assignment_chapters ac ON ac.assignment_id = cra.assignment_id
                        WHERE cra.course_revision_id = r.id AND ac.chapter_id = cm.chapter_id)
    UNION ALL
    SELECT 'chapter_retired', NULL, cm.module_key, cm.chapter_id
      FROM r
      JOIN course_modules cm ON cm.course_revision_id = r.id
      JOIN chapters ch ON ch.id = cm.chapter_id
     WHERE ch.retired_at IS NOT NULL
       AND NOT EXISTS (SELECT 1 FROM course_modules pm
                        WHERE pm.course_revision_id = r.compare_revision_id
                          AND pm.module_key = cm.module_key AND pm.chapter_id = cm.chapter_id);
END;

-- Placement validity (contract §3): the one function save, submit and
-- publish use.
CREATE FUNCTION course_revision_placement_valid(rev_id bigint, check_mode text DEFAULT 'save') RETURNS boolean
LANGUAGE sql STABLE PARALLEL SAFE
SET search_path = pg_catalog, pg_temp
BEGIN ATOMIC
    SELECT NOT EXISTS (SELECT 1 FROM course_revision_placement_problems(
        course_revision_placement_valid.rev_id, course_revision_placement_valid.check_mode));
END;
