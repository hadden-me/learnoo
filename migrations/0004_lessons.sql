-- LRN-006 persistence core: chapters, lesson identity, reviewed revision
-- snapshots and immutable media. Review decisions, publication and withdrawal
-- (LRN-007, LRN-009) and curriculum data (LRN-014) are added by later migrations.
--
-- Guards raise SQLSTATE LRN01 so callers can map them. PostgreSQL fires
-- BEFORE triggers of one event in name order, so a later trigger could change
-- NEW after a guard checked it. Guard triggers are therefore named zz_guard_*
-- to run last; a future trigger that modifies NEW must sort before them.
-- Triggers that write other rows instead are AFTER triggers (they see only
-- rows the guards accepted) and keep the lock order of every other path:
-- the lesson row first, then its revision rows.
--
-- Every trigger function pins search_path to pg_catalog, pg_temp (pg_temp
-- last) and names any other table through the schema of the table that
-- fired it (TG_TABLE_SCHEMA, with EXECUTE format('%I...') and bound
-- parameters). PUBLIC has TEMP by default and pg_temp is otherwise searched
-- first for relations, so an unqualified reference could be shadowed by a
-- temporary table of the calling role. Unqualified table names would not
-- resolve at all under the pinned path, so a future function cannot silently
-- reintroduce that.
--
-- These guards bind the application's database role. A superuser or table
-- owner can bypass them (session_replication_role, DISABLE TRIGGER, dropping
-- constraints); the deployed server must use a separate least-privilege role
-- that does not own these tables (docs/DECISIONS.md "Lesson persistence").

-- Minimal chapter identifier inside a subject. No names, ordering or seed rows:
-- LRN-014 extends it. Identity values are never reused, a chapter's subject and
-- code never change and chapters are never deleted, so an ID is never repurposed.
CREATE TABLE chapters (
    id         bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    subject_id bigint      NOT NULL REFERENCES subjects (id),
    code       text        NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT chapters_code_format CHECK (code ~ '^[a-z0-9][a-z0-9-]{0,62}$'),
    CONSTRAINT chapters_subject_code_key UNIQUE (subject_id, code),
    -- Target of the composite foreign key that keeps a revision's chapter
    -- inside the revision's subject.
    CONSTRAINT chapters_id_subject_key UNIQUE (id, subject_id)
);

CREATE FUNCTION chapters_guard() RETURNS trigger
LANGUAGE plpgsql SET search_path = pg_catalog, pg_temp AS $$
BEGIN
    IF TG_OP <> 'UPDATE' THEN
        RAISE EXCEPTION 'chapters are never deleted, so their IDs are never reused' USING ERRCODE = 'LRN01';
    END IF;
    IF (NEW.id, NEW.subject_id, NEW.code, NEW.created_at)
       IS DISTINCT FROM (OLD.id, OLD.subject_id, OLD.code, OLD.created_at) THEN
        RAISE EXCEPTION 'chapter identity is immutable' USING ERRCODE = 'LRN01';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER zz_guard_chapters_identity
    BEFORE UPDATE OR DELETE ON chapters
    FOR EACH ROW EXECUTE FUNCTION chapters_guard();

CREATE TRIGGER zz_guard_chapters_no_truncate
    BEFORE TRUNCATE ON chapters
    FOR EACH STATEMENT EXECUTE FUNCTION chapters_guard();

-- A translation group fixes one subject for all of its lessons.
CREATE TABLE translation_groups (
    id         uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    subject_id bigint      NOT NULL REFERENCES subjects (id),
    created_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT translation_groups_id_subject_key UNIQUE (id, subject_id)
);

CREATE FUNCTION translation_groups_guard_update() RETURNS trigger
LANGUAGE plpgsql SET search_path = pg_catalog, pg_temp AS $$
BEGIN
    RAISE EXCEPTION 'translation groups are immutable' USING ERRCODE = 'LRN01';
END;
$$;

CREATE TRIGGER zz_guard_translation_groups_immutable
    BEFORE UPDATE ON translation_groups
    FOR EACH ROW EXECUTE FUNCTION translation_groups_guard_update();

-- Stable lesson identity. Bookmarks and URLs reference public_id, which is
-- random and never derived from the title. One lesson per language per
-- translation group; every lesson of a group has the group's subject. The
-- subject is fixed for the lesson's lifetime.
CREATE TABLE lessons (
    id                    bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    public_id             uuid        NOT NULL DEFAULT gen_random_uuid(),
    translation_group_id  uuid        NOT NULL,
    language              text        NOT NULL,
    subject_id            bigint      NOT NULL REFERENCES subjects (id),
    -- Publication is this pointer, not a revision status. Set only by the
    -- publication workflow (LRN-007/LRN-009); foreign key and guard below.
    published_revision_id bigint,
    archived_at           timestamptz,
    -- Highest revision number ever used; numbers are never reused, even
    -- after a draft is discarded. Maintained by a lesson_revisions trigger.
    last_revision_number  integer     NOT NULL DEFAULT 0,
    created_at            timestamptz NOT NULL,
    created_by            bigint      NOT NULL REFERENCES users (id),
    CONSTRAINT lessons_language CHECK (language IN ('fr', 'ar')),
    CONSTRAINT lessons_last_revision_number CHECK (last_revision_number >= 0),
    CONSTRAINT lessons_public_id_key UNIQUE (public_id),
    CONSTRAINT lessons_translation_language_key UNIQUE (translation_group_id, language),
    CONSTRAINT lessons_translation_group_fkey FOREIGN KEY (translation_group_id, subject_id)
        REFERENCES translation_groups (id, subject_id),
    -- Target of the composite foreign key that pins each revision's language
    -- and subject to its lesson's.
    CONSTRAINT lessons_id_language_subject_key UNIQUE (id, language, subject_id)
);

CREATE FUNCTION lessons_guard_update() RETURNS trigger
LANGUAGE plpgsql SET search_path = pg_catalog, pg_temp AS $$
BEGIN
    IF (NEW.id, NEW.public_id, NEW.translation_group_id, NEW.language, NEW.subject_id, NEW.created_at, NEW.created_by)
       IS DISTINCT FROM
       (OLD.id, OLD.public_id, OLD.translation_group_id, OLD.language, OLD.subject_id, OLD.created_at, OLD.created_by) THEN
        RAISE EXCEPTION 'lesson identity is immutable' USING ERRCODE = 'LRN01';
    END IF;
    IF NEW.last_revision_number < OLD.last_revision_number THEN
        RAISE EXCEPTION 'revision numbers are never reused' USING ERRCODE = 'LRN01';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER zz_guard_lessons_identity
    BEFORE UPDATE ON lessons
    FOR EACH ROW EXECUTE FUNCTION lessons_guard_update();

-- A revision is the reviewed snapshot. title, objectives, language and
-- schema_version are columns for querying but must equal the content JSON
-- (schema v1). Exercises and solutions are embedded in content.
-- Publication is not a status; withdrawal is added by LRN-009.
CREATE TABLE lesson_revisions (
    id               bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    lesson_id        bigint      NOT NULL,
    revision_number  integer     NOT NULL,
    status           text        NOT NULL DEFAULT 'draft',
    author_id        bigint      NOT NULL REFERENCES users (id),
    -- The lesson's published revision when this draft was created (NULL if
    -- none). Publication compares it with the current pointer (LRN-007).
    base_revision_id bigint,
    title            text        NOT NULL,
    objectives       text        NOT NULL,
    prerequisites    text        NOT NULL DEFAULT '',
    content          jsonb       NOT NULL,
    schema_version   integer     NOT NULL,
    language         text        NOT NULL,
    subject_id       bigint      NOT NULL,
    -- Nullable until LRN-014 seeds chapters.
    chapter_id       bigint,
    -- Optimistic concurrency token for draft saves.
    version          integer     NOT NULL DEFAULT 1,
    created_at       timestamptz NOT NULL,
    updated_at       timestamptz NOT NULL,
    submitted_at     timestamptz,
    -- When an approved revision stopped being the lesson's active candidate:
    -- set once, to the transaction time, by the publication trigger on
    -- lessons below (and, from LRN-009, by an audited withdrawal trigger),
    -- both SECURITY DEFINER functions of the table owner. A direct UPDATE or
    -- another role's trigger cannot set it. Never cleared or changed afterwards.
    candidate_closed_at timestamptz,
    CONSTRAINT lesson_revisions_status CHECK (status IN ('draft', 'in_review', 'changes_requested', 'approved')),
    CONSTRAINT lesson_revisions_number_positive CHECK (revision_number >= 1),
    CONSTRAINT lesson_revisions_version_positive CHECK (version >= 1),
    CONSTRAINT lesson_revisions_submitted CHECK ((status = 'draft') = (submitted_at IS NULL)),
    CONSTRAINT lesson_revisions_candidate_closed CHECK (candidate_closed_at IS NULL OR status = 'approved'),
    CONSTRAINT lesson_revisions_content_object CHECK (jsonb_typeof(content) = 'object'),
    CONSTRAINT lesson_revisions_matches_content CHECK (
        title IS NOT DISTINCT FROM content ->> 'title'
        AND objectives = coalesce(content ->> 'objective', '')
        AND language IS NOT DISTINCT FROM content ->> 'language'
        AND schema_version IS NOT DISTINCT FROM (content ->> 'schemaVersion')::integer),
    CONSTRAINT lesson_revisions_prerequisites_length CHECK (char_length(prerequisites) <= 2000),
    CONSTRAINT lesson_revisions_number_key UNIQUE (lesson_id, revision_number),
    CONSTRAINT lesson_revisions_lesson_id_key UNIQUE (lesson_id, id),
    CONSTRAINT lesson_revisions_lesson_fkey FOREIGN KEY (lesson_id, language, subject_id)
        REFERENCES lessons (id, language, subject_id),
    CONSTRAINT lesson_revisions_chapter_fkey FOREIGN KEY (chapter_id, subject_id)
        REFERENCES chapters (id, subject_id),
    CONSTRAINT lesson_revisions_base_fkey FOREIGN KEY (lesson_id, base_revision_id)
        REFERENCES lesson_revisions (lesson_id, id)
);

-- One active candidate per lesson: a draft, a revision in review, or an
-- approved revision that has never been published (maintainer decision,
-- docs/DECISIONS.md "Lesson persistence"). It blocks another draft until it
-- is published or withdrawn; either sets candidate_closed_at. An approved
-- revision that was ever published stays closed, so restoring it (LRN-009)
-- or publishing a successor never blocks drafts. changes_requested is
-- deliberately not active: it is retained as history while its successor
-- draft is created.
--
-- LRN-009 withdrawal of an approved, never-published revision must close it
-- through its own audited trigger path (for example an AFTER INSERT trigger
-- on the withdrawal audit table that sets candidate_closed_at, SECURITY
-- DEFINER and owned by the table owner, like the publication trigger), or this
-- predicate must exclude withdrawn revisions; otherwise the withdrawn
-- revision keeps blocking new drafts of the lesson. The update guard below
-- accepts the change only from inside a trigger.
CREATE UNIQUE INDEX lesson_revisions_one_active_candidate
    ON lesson_revisions (lesson_id)
    WHERE status IN ('draft', 'in_review') OR (status = 'approved' AND candidate_closed_at IS NULL);

CREATE INDEX lesson_revisions_author_idx ON lesson_revisions (author_id);
CREATE INDEX lesson_revisions_chapter_idx ON lesson_revisions (chapter_id) WHERE chapter_id IS NOT NULL;

ALTER TABLE lessons
    ADD CONSTRAINT lessons_published_revision_fkey FOREIGN KEY (id, published_revision_id)
        REFERENCES lesson_revisions (lesson_id, id) DEFERRABLE INITIALLY DEFERRED;

-- The published pointer may name only an approved revision of the same
-- lesson (or be NULL). The approved revision is share-locked so its status
-- cannot change before the pointer commits. The expected-base comparison is
-- LRN-007's publication transaction.
CREATE FUNCTION lessons_guard_published() RETURNS trigger
LANGUAGE plpgsql SET search_path = pg_catalog, pg_temp AS $$
DECLARE
    st text;
BEGIN
    IF NEW.published_revision_id IS NULL
       OR (TG_OP = 'UPDATE' AND NEW.published_revision_id IS NOT DISTINCT FROM OLD.published_revision_id) THEN
        RETURN NEW;
    END IF;
    -- EXECUTE does not set FOUND; a missing row leaves st NULL.
    EXECUTE format('SELECT status FROM %I.lesson_revisions WHERE id = $1 AND lesson_id = $2 FOR SHARE', TG_TABLE_SCHEMA)
       INTO st USING NEW.published_revision_id, NEW.id;
    IF st IS DISTINCT FROM 'approved' THEN
        RAISE EXCEPTION 'only an approved revision of this lesson can be published' USING ERRCODE = 'LRN01';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER zz_guard_lessons_published_approved
    BEFORE INSERT OR UPDATE ON lessons
    FOR EACH ROW EXECUTE FUNCTION lessons_guard_published();

-- Publishing an approved revision closes it as a candidate, releasing the
-- lesson for a new draft. AFTER, so it runs only once the guard above has
-- accepted the pointer (and share-locked the revision); the lesson row is
-- already locked by this UPDATE, so the order stays lesson, then revision.
-- The share lock is upgraded only while the lesson row is held, and every
-- other path that share-locks a revision of this lesson locks the lesson
-- first or fails without waiting, so the upgrade cannot deadlock.
-- Re-pointing to a revision that was already published finds it closed and
-- changes nothing.
--
-- SECURITY DEFINER: the update guard accepts a close only from the owner of
-- lesson_revisions, so the application role (which does not own the tables)
-- can close a candidate only by publishing. The function therefore pins its
-- search_path (pg_temp last, so temporary objects cannot shadow anything),
-- names the table through the schema of the table that fired it, refuses to
-- run for any relation its owner does not own (such as a temporary "lessons"
-- table of another role), and is not executable by PUBLIC. Firing a trigger
-- needs no EXECUTE privilege; only creating one does.
CREATE FUNCTION lessons_close_published_candidate() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $$
BEGIN
    IF TG_WHEN <> 'AFTER' OR TG_LEVEL <> 'ROW' OR TG_TABLE_NAME <> 'lessons'
       OR NOT EXISTS (SELECT 1 FROM pg_catalog.pg_class c
                       WHERE c.oid = TG_RELID
                         AND c.relpersistence = 'p'
                         AND c.relowner = (SELECT r.oid FROM pg_catalog.pg_roles r WHERE r.rolname = current_user)) THEN
        RAISE EXCEPTION 'lessons_close_published_candidate may only fire on the lessons table' USING ERRCODE = 'LRN01';
    END IF;
    IF NEW.published_revision_id IS NULL
       OR (TG_OP = 'UPDATE' AND NEW.published_revision_id IS NOT DISTINCT FROM OLD.published_revision_id) THEN
        RETURN NULL;
    END IF;
    EXECUTE format('UPDATE %I.lesson_revisions SET candidate_closed_at = pg_catalog.now()
                     WHERE id = $1 AND lesson_id = $2 AND candidate_closed_at IS NULL', TG_TABLE_SCHEMA)
      USING NEW.published_revision_id, NEW.id;
    RETURN NULL;
END;
$$;

REVOKE EXECUTE ON FUNCTION lessons_close_published_candidate() FROM PUBLIC;

CREATE TRIGGER lessons_close_published_candidate
    AFTER INSERT OR UPDATE OF published_revision_id ON lessons
    FOR EACH ROW EXECUTE FUNCTION lessons_close_published_candidate();

CREATE FUNCTION lesson_revisions_guard_insert() RETURNS trigger
LANGUAGE plpgsql SET search_path = pg_catalog, pg_temp AS $$
DECLARE
    published bigint;
BEGIN
    IF NEW.status <> 'draft' THEN
        RAISE EXCEPTION 'a revision must be created as a draft' USING ERRCODE = 'LRN01';
    END IF;
    EXECUTE format('SELECT published_revision_id FROM %I.lessons WHERE id = $1', TG_TABLE_SCHEMA)
       INTO published USING NEW.lesson_id;
    IF NEW.base_revision_id IS DISTINCT FROM published THEN
        RAISE EXCEPTION 'base_revision_id must be the lesson''s current published revision' USING ERRCODE = 'LRN01';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER zz_guard_lesson_revisions_insert_as_draft
    BEFORE INSERT ON lesson_revisions
    FOR EACH ROW EXECUTE FUNCTION lesson_revisions_guard_insert();

-- Lesson ownership (maintainer decision of 2026-11-25, docs/DECISIONS.md
-- "Lesson ownership"): a lesson's creator (lessons.created_by) is its owner.
-- Only the owner authors its revisions, and every lesson of a translation
-- group has the same owner, so adding a translation needs ownership of the
-- source lesson. internal/lessons checks the acting user against the owner
-- together with the author permission; administrators get no exception.
-- The database cannot know who is acting, so these guards enforce only the
-- consistency the rule implies, for every writer: a revision's author_id
-- equals its lesson's created_by, and a translation group has a single
-- owner. They stop rows that contradict an existing owner, not a writer
-- that names the owner itself.
--
-- Snapshots. created_by never changes (zz_guard_lessons_identity), so an
-- owner read here cannot be stale. What a stale snapshot can hide is a row
-- inserted after it, so both guards refuse when they find nothing: a lesson
-- the inserting transaction cannot see has no owner, and a translation
-- group without a visible lesson accepts its first lesson only from the
-- transaction that created the group (the group row's xmin is the current
-- transaction ID; CreateLesson inserts both in one transaction, without a
-- savepoint). A writer whose snapshot predates a group and its first lesson
-- therefore cannot add a lesson of another owner to it.
CREATE FUNCTION lesson_revisions_guard_owner() RETURNS trigger
LANGUAGE plpgsql SET search_path = pg_catalog, pg_temp AS $$
DECLARE
    owner bigint;
BEGIN
    -- EXECUTE does not set FOUND; a missing or invisible lesson leaves owner NULL.
    EXECUTE format('SELECT created_by FROM %I.lessons WHERE id = $1', TG_TABLE_SCHEMA)
       INTO owner USING NEW.lesson_id;
    IF NEW.author_id IS DISTINCT FROM owner THEN
        RAISE EXCEPTION 'a revision must be authored by the lesson''s owner' USING ERRCODE = 'LRN01';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER zz_guard_lesson_revisions_author_owns_lesson
    BEFORE INSERT ON lesson_revisions
    FOR EACH ROW EXECUTE FUNCTION lesson_revisions_guard_owner();

CREATE FUNCTION lessons_guard_translation_owner() RETURNS trigger
LANGUAGE plpgsql SET search_path = pg_catalog, pg_temp AS $$
DECLARE
    lessons_seen bigint;
    other_owner  boolean;
    group_xmin   xid;
BEGIN
    EXECUTE format('SELECT count(*), coalesce(bool_or(created_by IS DISTINCT FROM $2), false)
                      FROM %I.lessons WHERE translation_group_id = $1', TG_TABLE_SCHEMA)
       INTO lessons_seen, other_owner USING NEW.translation_group_id, NEW.created_by;
    IF other_owner THEN
        RAISE EXCEPTION 'only the owner of a translation group''s lessons can add a lesson to it' USING ERRCODE = 'LRN01';
    END IF;
    IF lessons_seen = 0 THEN
        EXECUTE format('SELECT xmin FROM %I.translation_groups WHERE id = $1', TG_TABLE_SCHEMA)
           INTO group_xmin USING NEW.translation_group_id;
        IF group_xmin IS DISTINCT FROM xid(pg_current_xact_id()) THEN
            RAISE EXCEPTION 'a translation group''s first lesson must be created in the transaction that created the group'
                USING ERRCODE = 'LRN01';
        END IF;
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER zz_guard_lessons_translation_owner
    BEFORE INSERT ON lessons
    FOR EACH ROW EXECUTE FUNCTION lessons_guard_translation_owner();

-- A new revision number must exceed every number the lesson has used, so a
-- discarded draft's number is never reused. AFTER INSERT, so the unique
-- indexes (including the one-active-candidate index) are checked first.
CREATE FUNCTION lesson_revisions_advance_number() RETURNS trigger
LANGUAGE plpgsql SET search_path = pg_catalog, pg_temp AS $$
DECLARE
    n bigint;
BEGIN
    EXECUTE format('UPDATE %I.lessons SET last_revision_number = $1
                     WHERE id = $2 AND last_revision_number < $1', TG_TABLE_SCHEMA)
      USING NEW.revision_number, NEW.lesson_id;
    GET DIAGNOSTICS n = ROW_COUNT;
    IF n <> 1 THEN
        RAISE EXCEPTION 'revision number % was already used for lesson %', NEW.revision_number, NEW.lesson_id
            USING ERRCODE = 'LRN01';
    END IF;
    RETURN NULL;
END;
$$;

CREATE TRIGGER zz_guard_lesson_revisions_number_never_reused
    AFTER INSERT ON lesson_revisions
    FOR EACH ROW EXECUTE FUNCTION lesson_revisions_advance_number();

-- Submitted snapshots are immutable: once a revision leaves draft, only its
-- status may change, only forward, and updated_at only together with it.
-- Identity columns never change. New columns are frozen by default because
-- the comparison covers the whole row. The one other permitted change is
-- closing an approved candidate: candidate_closed_at from NULL to the
-- transaction time, alone, on an approved revision, only from inside a
-- trigger (pg_trigger_depth() > 1: this guard is itself depth 1), and only
-- while current_user owns lesson_revisions. Any role may create temporary
-- tables and triggers (PUBLIC has TEMP by default), so the depth alone does
-- not prove the caller is one of ours; the owner check does, because the
-- only trigger functions running as the owner are SECURITY DEFINER functions
-- the owner created. Today that is the publication trigger; LRN-009's
-- audited withdrawal trigger must likewise be SECURITY DEFINER, owned by the
-- table owner, with a pinned search_path. A direct UPDATE, or a trigger of
-- the application role, therefore cannot release a lesson without
-- publication or an audited withdrawal, nor backdate the closing time.
CREATE FUNCTION lesson_revisions_guard_update() RETURNS trigger
LANGUAGE plpgsql SET search_path = pg_catalog, pg_temp AS $$
BEGIN
    IF (NEW.id, NEW.lesson_id, NEW.revision_number, NEW.author_id, NEW.base_revision_id,
        NEW.language, NEW.subject_id, NEW.created_at)
       IS DISTINCT FROM
       (OLD.id, OLD.lesson_id, OLD.revision_number, OLD.author_id, OLD.base_revision_id,
        OLD.language, OLD.subject_id, OLD.created_at) THEN
        RAISE EXCEPTION 'revision identity is immutable' USING ERRCODE = 'LRN01';
    END IF;
    IF NEW.candidate_closed_at IS DISTINCT FROM OLD.candidate_closed_at THEN
        IF pg_trigger_depth() < 2
           OR NOT EXISTS (SELECT 1 FROM pg_catalog.pg_class c
                           WHERE c.oid = TG_RELID
                             AND c.relowner = (SELECT r.oid FROM pg_catalog.pg_roles r WHERE r.rolname = current_user)) THEN
            RAISE EXCEPTION 'revision % candidate_closed_at is set only by publication or audited withdrawal', OLD.id
                USING ERRCODE = 'LRN01';
        END IF;
        IF OLD.candidate_closed_at IS NOT NULL OR NEW.candidate_closed_at IS DISTINCT FROM now()
           OR OLD.status <> 'approved' OR NEW.status <> 'approved'
           OR to_jsonb(NEW) - 'candidate_closed_at' IS DISTINCT FROM to_jsonb(OLD) - 'candidate_closed_at' THEN
            RAISE EXCEPTION 'revision % candidate_closed_at can only be set once, to the transaction time, alone, on an approved revision', OLD.id
                USING ERRCODE = 'LRN01';
        END IF;
        RETURN NEW;
    END IF;
    IF OLD.status <> 'draft'
       AND (to_jsonb(NEW) - 'status' - 'updated_at' IS DISTINCT FROM to_jsonb(OLD) - 'status' - 'updated_at'
            OR (NEW.status = OLD.status AND NEW.updated_at IS DISTINCT FROM OLD.updated_at)) THEN
        RAISE EXCEPTION 'revision % is submitted and immutable', OLD.id USING ERRCODE = 'LRN01';
    END IF;
    IF NEW.status IS DISTINCT FROM OLD.status
       AND (OLD.status, NEW.status) NOT IN (
           ('draft', 'in_review'),
           ('in_review', 'changes_requested'),
           ('in_review', 'approved')) THEN
        RAISE EXCEPTION 'revision status cannot change from % to %', OLD.status, NEW.status USING ERRCODE = 'LRN01';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER zz_guard_lesson_revisions_immutable_snapshot
    BEFORE UPDATE ON lesson_revisions
    FOR EACH ROW EXECUTE FUNCTION lesson_revisions_guard_update();

CREATE FUNCTION lesson_revisions_guard_delete() RETURNS trigger
LANGUAGE plpgsql SET search_path = pg_catalog, pg_temp AS $$
BEGIN
    IF OLD.status <> 'draft' THEN
        RAISE EXCEPTION 'revision % is submitted and cannot be deleted', OLD.id USING ERRCODE = 'LRN01';
    END IF;
    RETURN OLD;
END;
$$;

CREATE TRIGGER zz_guard_lesson_revisions_keep_submitted
    BEFORE DELETE ON lesson_revisions
    FOR EACH ROW EXECUTE FUNCTION lesson_revisions_guard_delete();

-- Uploaded images. Rows are immutable: a replacement is a new asset with a
-- new ID. Upload validation and storage are LRN-006 slice 2.
CREATE TABLE media_assets (
    id           bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    sha256       bytea       NOT NULL,
    content_type text        NOT NULL,
    byte_size    bigint      NOT NULL,
    width        integer     NOT NULL,
    height       integer     NOT NULL,
    storage_key  text        NOT NULL,
    created_at   timestamptz NOT NULL,
    created_by   bigint      NOT NULL REFERENCES users (id),
    CONSTRAINT media_assets_sha256_length CHECK (octet_length(sha256) = 32),
    CONSTRAINT media_assets_content_type CHECK (content_type IN ('image/png', 'image/jpeg', 'image/webp')),
    CONSTRAINT media_assets_byte_size CHECK (byte_size > 0),
    CONSTRAINT media_assets_dimensions CHECK (width > 0 AND height > 0),
    -- Relative path segments of [a-z0-9_-] only: no traversal, no absolute path.
    CONSTRAINT media_assets_storage_key CHECK (
        char_length(storage_key) <= 255
        AND storage_key ~ '^[a-z0-9][a-z0-9_-]*(/[a-z0-9][a-z0-9_-]*)*\.(png|jpg|webp)$'),
    CONSTRAINT media_assets_sha256_key UNIQUE (sha256),
    CONSTRAINT media_assets_storage_key_key UNIQUE (storage_key)
);

CREATE FUNCTION media_assets_immutable() RETURNS trigger
LANGUAGE plpgsql SET search_path = pg_catalog, pg_temp AS $$
BEGIN
    RAISE EXCEPTION 'media assets are immutable; upload a new asset instead' USING ERRCODE = 'LRN01';
END;
$$;

CREATE TRIGGER zz_guard_media_assets_no_update_delete
    BEFORE UPDATE OR DELETE ON media_assets
    FOR EACH ROW EXECUTE FUNCTION media_assets_immutable();

CREATE TRIGGER zz_guard_media_assets_no_truncate
    BEFORE TRUNCATE ON media_assets
    FOR EACH STATEMENT EXECUTE FUNCTION media_assets_immutable();

-- Media referenced by a revision snapshot. Changes only while the revision
-- is a draft.
CREATE TABLE revision_media (
    revision_id    bigint NOT NULL REFERENCES lesson_revisions (id) ON DELETE CASCADE,
    media_asset_id bigint NOT NULL REFERENCES media_assets (id),
    PRIMARY KEY (revision_id, media_asset_id)
);

CREATE INDEX revision_media_asset_idx ON revision_media (media_asset_id);

-- FOR SHARE conflicts with the status update of a concurrent submission, so a
-- reference cannot be added or removed after the snapshot is frozen. A missing
-- revision row means a draft is being deleted (submitted revisions cannot be).
CREATE FUNCTION revision_media_guard() RETURNS trigger
LANGUAGE plpgsql SET search_path = pg_catalog, pg_temp AS $$
DECLARE
    rev    bigint;
    st     text;
BEGIN
    FOREACH rev IN ARRAY ARRAY[
        CASE WHEN TG_OP <> 'INSERT' THEN OLD.revision_id END,
        CASE WHEN TG_OP <> 'DELETE' THEN NEW.revision_id END]
    LOOP
        CONTINUE WHEN rev IS NULL;
        -- EXECUTE does not set FOUND; a missing row leaves st NULL.
        EXECUTE format('SELECT status FROM %I.lesson_revisions WHERE id = $1 FOR SHARE', TG_TABLE_SCHEMA)
           INTO st USING rev;
        IF st IS NOT NULL AND st <> 'draft' THEN
            RAISE EXCEPTION 'revision % is submitted; its media references are immutable', rev USING ERRCODE = 'LRN01';
        END IF;
    END LOOP;
    IF TG_OP = 'DELETE' THEN
        RETURN OLD;
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER zz_guard_revision_media_draft_only
    BEFORE INSERT OR UPDATE OR DELETE ON revision_media
    FOR EACH ROW EXECUTE FUNCTION revision_media_guard();

CREATE FUNCTION lesson_snapshots_no_truncate() RETURNS trigger
LANGUAGE plpgsql SET search_path = pg_catalog, pg_temp AS $$
BEGIN
    RAISE EXCEPTION 'lesson snapshots cannot be truncated' USING ERRCODE = 'LRN01';
END;
$$;

CREATE TRIGGER zz_guard_lesson_revisions_no_truncate
    BEFORE TRUNCATE ON lesson_revisions
    FOR EACH STATEMENT EXECUTE FUNCTION lesson_snapshots_no_truncate();

CREATE TRIGGER zz_guard_revision_media_no_truncate
    BEFORE TRUNCATE ON revision_media
    FOR EACH STATEMENT EXECUTE FUNCTION lesson_snapshots_no_truncate();
