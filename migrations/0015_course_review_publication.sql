-- LMS-B04 (issue #94): course submission, independent review, publication,
-- amendments, suspension, archive, withdrawal, transfer and lesson
-- dependency effects (docs/LMS_CONTRACT.md §5-§7, docs/LMS_PRODUCT_SPEC.md
-- §4-5, maintainer decisions of 2026-11-26).
--
-- Conventions of 0004 and later apply: guards raise SQLSTATE LRN01 and are
-- named zz_guard_*; every trigger function pins search_path to
-- pg_catalog, pg_temp and reaches other tables only through the schema of the
-- table that fired it (TG_TABLE_SCHEMA, EXECUTE format('%I...') with bound
-- parameters), so a temporary table of the calling role cannot shadow them.
-- Shared SQL functions have SQL-standard bodies (tables bound at creation).
-- The only SECURITY DEFINER functions are the three owner-run triggers that
-- write columns or tables the guards reserve to them (candidate close,
-- revision withdrawal, dependency effects); they are hardened like
-- lessons_close_published_candidate (0004) and
-- revision_withdrawals_close_candidate (0007). The guards bind the
-- application's least-privilege role, not a superuser or table owner.
--
-- 0014 is extended here, not edited (as 0007 extended 0006):
--
--   * zz_guard_course_revisions_not_submittable is dropped; a revision now
--     leaves draft only with a course_submission_audit row of the same
--     transaction (the pattern of 0011), and leaves in_review only with a
--     course_review_decisions row (the pattern of 0006).
--   * courses_guard is replaced: owner, archive and dependency_count change
--     only through their audited workflows (transfer and archive rows of
--     this transaction, the owner-run dependency trigger); the published
--     pointer changes only with a course_publication_audit row of this
--     transaction (new guard).
--   * course_revisions_guard_insert is replaced: compare_revision_id is the
--     comparison revision of contract §5 (the pointer, else the revision of
--     the last publish or restore row, else NULL), and an amendment may be
--     inserted when it has one.
--   * course_children_guard is replaced: its eligibility check for another
--     author's lesson now refuses outside READ COMMITTED, like every guard
--     below that evaluates eligibility.
--
-- Snapshots. Eligibility (contract §5) depends on facts that are only
-- inserted (revision_withdrawals, publication_audit rows). Every guard that
-- evaluates it share-locks the lessons concerned first, and every lesson
-- withdrawal, unpublication and archive updates the lesson row, so a stale
-- REPEATABLE READ writer would already fail with 40001; the guards
-- nevertheless refuse to run outside READ COMMITTED, where each statement
-- takes a fresh snapshot after the locks are granted, so no snapshot can
-- admit a withdrawn, unpublished or archived lesson. The dependency triggers
-- refuse outside READ COMMITTED unconditionally (contract §7).
--
-- Global lock order (contract §2): permission and account rows ->
-- curriculum editions (ascending) -> lessons (ascending) -> pinned lesson
-- revisions (ascending) -> course -> course revision -> child rows. The
-- dependency triggers run under the lesson lock their writer holds, then
-- lock the affected courses in ascending ID.

-- ---- shared definitions ----

-- Comparison revision (contract §5): the course's published pointer, or if
-- it is NULL the revision of the last publish or restore row, or NULL.
CREATE TABLE course_publication_audit (
    id                   bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    course_id            bigint      NOT NULL REFERENCES courses (id),
    -- The new pointer; NULL for suspend and archive.
    revision_id          bigint,
    -- The pointer it replaced.
    previous_revision_id bigint,
    actor_id             bigint      NOT NULL REFERENCES users (id),
    action               text        NOT NULL,
    reason               text,
    occurred_at          timestamptz NOT NULL DEFAULT now(),
    xact_id              xid8        NOT NULL DEFAULT pg_current_xact_id(),
    CONSTRAINT course_publication_audit_action CHECK (action IN ('publish', 'restore', 'suspend', 'archive')),
    CONSTRAINT course_publication_audit_target CHECK ((action IN ('suspend', 'archive')) = (revision_id IS NULL)),
    CONSTRAINT course_publication_audit_previous CHECK (action IN ('publish', 'restore') OR previous_revision_id IS NOT NULL),
    CONSTRAINT course_publication_audit_changes_pointer CHECK (previous_revision_id IS DISTINCT FROM revision_id),
    CONSTRAINT course_publication_audit_reason CHECK ((action IN ('suspend', 'archive')) = (reason IS NOT NULL)),
    CONSTRAINT course_publication_audit_reason_not_blank CHECK (reason IS NULL OR reason ~ '[^[:space:]]'),
    CONSTRAINT course_publication_audit_reason_length CHECK (char_length(reason) <= 2000),
    CONSTRAINT course_publication_audit_revision_fkey FOREIGN KEY (course_id, revision_id)
        REFERENCES course_revisions (course_id, id),
    CONSTRAINT course_publication_audit_previous_fkey FOREIGN KEY (course_id, previous_revision_id)
        REFERENCES course_revisions (course_id, id)
);

CREATE INDEX course_publication_audit_course_idx ON course_publication_audit (course_id, id);
CREATE INDEX course_publication_audit_revision_idx ON course_publication_audit (revision_id) WHERE action = 'publish';
CREATE INDEX course_publication_audit_actor_idx ON course_publication_audit (actor_id);

CREATE FUNCTION course_comparison_revision(cid bigint) RETURNS bigint
LANGUAGE sql STABLE PARALLEL SAFE
SET search_path = pg_catalog, pg_temp
BEGIN ATOMIC
    SELECT coalesce(
        (SELECT c.published_revision_id FROM courses c WHERE c.id = course_comparison_revision.cid),
        (SELECT a.revision_id FROM course_publication_audit a
          WHERE a.course_id = course_comparison_revision.cid AND a.action IN ('publish', 'restore')
          ORDER BY a.id DESC LIMIT 1));
END;

-- Reviewer disqualifications (contract §6), one row per reason; the user is
-- eligible when there is none:
--   unknown_revision      the course revision does not exist;
--   not_subject_reviewer  the user is not an active reviewer of its subject;
--   course_author         the user authored the course revision;
--   lesson_author         the user authored a lesson revision the course
--                         revision adds or replaces relative to its
--                         comparison revision (every step when it has none).
CREATE FUNCTION course_reviewer_problems(uid bigint, rev_id bigint)
RETURNS TABLE (reason text, lesson_id bigint, lesson_revision_id bigint)
LANGUAGE sql STABLE PARALLEL SAFE
SET search_path = pg_catalog, pg_temp
BEGIN ATOMIC
    SELECT 'unknown_revision', NULL::bigint, NULL::bigint
     WHERE NOT EXISTS (SELECT 1 FROM course_revisions r WHERE r.id = course_reviewer_problems.rev_id)
    UNION ALL
    SELECT 'not_subject_reviewer', NULL, NULL
      FROM course_revisions r
     WHERE r.id = course_reviewer_problems.rev_id
       AND NOT EXISTS (SELECT 1 FROM reviewer_assignments a JOIN users u ON u.id = a.user_id
                        WHERE a.user_id = course_reviewer_problems.uid AND a.subject_id = r.subject_id
                          AND u.disabled_at IS NULL)
    UNION ALL
    SELECT 'course_author', NULL, NULL
      FROM course_revisions r
     WHERE r.id = course_reviewer_problems.rev_id AND r.author_id = course_reviewer_problems.uid
    UNION ALL
    SELECT 'lesson_author', s.lesson_id, s.lesson_revision_id
      FROM course_revisions r
      JOIN course_steps s ON s.course_revision_id = r.id
      JOIN lesson_revisions lr ON lr.id = s.lesson_revision_id
     WHERE r.id = course_reviewer_problems.rev_id
       AND lr.author_id = course_reviewer_problems.uid
       AND NOT EXISTS (SELECT 1 FROM course_steps p
                        WHERE p.course_revision_id = r.compare_revision_id
                          AND p.lesson_id = s.lesson_id AND p.lesson_revision_id = s.lesson_revision_id);
END;

CREATE FUNCTION course_reviewer_eligible(uid bigint, rev_id bigint) RETURNS boolean
LANGUAGE sql STABLE PARALLEL SAFE
SET search_path = pg_catalog, pg_temp
BEGIN ATOMIC
    SELECT NOT EXISTS (SELECT 1 FROM course_reviewer_problems(course_reviewer_eligible.uid, course_reviewer_eligible.rev_id));
END;

-- The kind of a course revision as the server derives it (contract §6): an
-- amendment differs from its comparison revision only by steps that keep
-- their lesson, module, position and title and pin a newer eligible
-- revision of that lesson, and at least one step does so. Anything else
-- (including no change at all, or no comparison revision) is ordinary.
-- course_revision_structure_amends checks everything but the overview
-- fields, so that a save can derive the kind in the UPDATE that writes them
-- (course_revision_derived_kind compares the stored ones).
CREATE FUNCTION course_revision_structure_amends(rev_id bigint) RETURNS boolean
LANGUAGE sql STABLE PARALLEL SAFE
SET search_path = pg_catalog, pg_temp
BEGIN ATOMIC
    SELECT EXISTS (
        SELECT 1
          FROM course_revisions r
          JOIN course_revisions c ON c.id = r.compare_revision_id AND c.course_id = r.course_id
         WHERE r.id = course_revision_structure_amends.rev_id
           AND NOT EXISTS ((SELECT assignment_id FROM course_revision_assignments WHERE course_revision_id = r.id
                            EXCEPT SELECT assignment_id FROM course_revision_assignments WHERE course_revision_id = c.id)
                           UNION ALL
                           (SELECT assignment_id FROM course_revision_assignments WHERE course_revision_id = c.id
                            EXCEPT SELECT assignment_id FROM course_revision_assignments WHERE course_revision_id = r.id))
           AND NOT EXISTS ((SELECT module_key, position, title, chapter_id FROM course_modules WHERE course_revision_id = r.id
                            EXCEPT SELECT module_key, position, title, chapter_id FROM course_modules WHERE course_revision_id = c.id)
                           UNION ALL
                           (SELECT module_key, position, title, chapter_id FROM course_modules WHERE course_revision_id = c.id
                            EXCEPT SELECT module_key, position, title, chapter_id FROM course_modules WHERE course_revision_id = r.id))
           AND NOT EXISTS ((SELECT module_key, position, title, lesson_id FROM course_steps WHERE course_revision_id = r.id
                            EXCEPT SELECT module_key, position, title, lesson_id FROM course_steps WHERE course_revision_id = c.id)
                           UNION ALL
                           (SELECT module_key, position, title, lesson_id FROM course_steps WHERE course_revision_id = c.id
                            EXCEPT SELECT module_key, position, title, lesson_id FROM course_steps WHERE course_revision_id = r.id))
           AND NOT EXISTS (SELECT 1
                             FROM course_steps s
                             JOIN course_steps p ON p.course_revision_id = c.id AND p.lesson_id = s.lesson_id
                             JOIN lesson_revisions sr ON sr.id = s.lesson_revision_id
                             JOIN lesson_revisions pr ON pr.id = p.lesson_revision_id
                            WHERE s.course_revision_id = r.id
                              AND s.lesson_revision_id <> p.lesson_revision_id
                              AND (sr.revision_number <= pr.revision_number
                                   OR NOT course_step_revision_eligible(s.lesson_id, s.lesson_revision_id)))
           AND EXISTS (SELECT 1
                         FROM course_steps s
                         JOIN course_steps p ON p.course_revision_id = c.id AND p.lesson_id = s.lesson_id
                        WHERE s.course_revision_id = r.id AND s.lesson_revision_id <> p.lesson_revision_id));
END;

CREATE FUNCTION course_revision_derived_kind(rev_id bigint) RETURNS text
LANGUAGE sql STABLE PARALLEL SAFE
SET search_path = pg_catalog, pg_temp
BEGIN ATOMIC
    SELECT CASE WHEN EXISTS (
        SELECT 1
          FROM course_revisions r
          JOIN course_revisions c ON c.id = r.compare_revision_id AND c.course_id = r.course_id
         WHERE r.id = course_revision_derived_kind.rev_id
           AND (r.title, r.summary, r.objectives, r.prerequisites, r.cover_media_id, r.cover_alt)
               IS NOT DISTINCT FROM (c.title, c.summary, c.objectives, c.prerequisites, c.cover_media_id, c.cover_alt))
       AND course_revision_structure_amends(course_revision_derived_kind.rev_id)
    THEN 'amendment' ELSE 'ordinary' END;
END;

-- Newer revision notice (contract §5): steps of the course's comparison
-- revision whose lesson has an eligible revision with a higher number than
-- the pinned one, with the newest such revision.
CREATE FUNCTION course_newer_revisions(cid bigint)
RETURNS TABLE (module_key uuid, step_position integer, step_title text, lesson_id bigint,
               pinned_revision_id bigint, pinned_number integer, newer_revision_id bigint, newer_number integer)
LANGUAGE sql STABLE PARALLEL SAFE
SET search_path = pg_catalog, pg_temp
BEGIN ATOMIC
    SELECT s.module_key, s.position, s.title, s.lesson_id, s.lesson_revision_id, pr.revision_number, n.id, n.revision_number
      FROM course_steps s
      JOIN lesson_revisions pr ON pr.id = s.lesson_revision_id
      JOIN LATERAL (SELECT r.id, r.revision_number FROM lesson_revisions r
                     WHERE r.lesson_id = s.lesson_id AND r.revision_number > pr.revision_number
                       AND course_step_revision_eligible(r.lesson_id, r.id)
                     ORDER BY r.revision_number DESC LIMIT 1) n ON true
     WHERE s.course_revision_id = course_comparison_revision(course_newer_revisions.cid)
     ORDER BY s.module_key, s.position;
END;

-- ---- courses: one archived course has no pointer ----

ALTER TABLE courses
    ADD CONSTRAINT courses_archived_unpublished CHECK (archived_at IS NULL OR published_revision_id IS NULL);

-- ---- course revisions: comparison revision and amendments ----

-- Replaces 0014's insert guard: the comparison revision is the pointer, or
-- the revision of the course's last publish or restore row, or NULL; an
-- amendment needs one (CHECK course_revisions_amendment_compares) and its
-- kind is checked against course_revision_derived_kind at submission.
CREATE OR REPLACE FUNCTION course_revisions_guard_insert() RETURNS trigger
LANGUAGE plpgsql SET search_path = pg_catalog, pg_temp AS $$
DECLARE
    found_course boolean;
    published    bigint;
    comparison   bigint;
BEGIN
    IF NEW.status <> 'draft' OR NEW.version <> 1 OR NEW.submitted_at IS NOT NULL
       OR NEW.candidate_closed_at IS NOT NULL OR NEW.withdrawn_at IS NOT NULL
       OR NEW.created_at IS DISTINCT FROM now() OR NEW.updated_at IS DISTINCT FROM now() THEN
        RAISE EXCEPTION 'a course revision is created as an open draft at version 1, at the transaction time' USING ERRCODE = 'LRN01';
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
    comparison := published;
    IF comparison IS NULL THEN
        EXECUTE format('SELECT revision_id FROM %I.course_publication_audit
                         WHERE course_id = $1 AND action IN (''publish'', ''restore'') ORDER BY id DESC LIMIT 1', TG_TABLE_SCHEMA)
           INTO comparison USING NEW.course_id;
    END IF;
    IF NEW.compare_revision_id IS DISTINCT FROM comparison THEN
        RAISE EXCEPTION 'compare_revision_id must be the course''s comparison revision' USING ERRCODE = 'LRN01';
    END IF;
    RETURN NEW;
END;
$$;

DROP TRIGGER zz_guard_course_revisions_not_submittable ON course_revisions;
DROP FUNCTION course_revisions_guard_not_submittable();

-- Replaces 0014's children guard; only the READ COMMITTED requirement before
-- the eligibility check of another author's lesson is new.
CREATE OR REPLACE FUNCTION course_children_guard() RETURNS trigger
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
    IF TG_OP <> 'INSERT' THEN
        o := to_jsonb(OLD);
    END IF;
    IF TG_OP <> 'DELETE' THEN
        nw := to_jsonb(NEW);
    END IF;
    lock_mode := CASE WHEN TG_TABLE_NAME = 'course_steps' THEN 'FOR UPDATE' ELSE 'FOR SHARE' END;
    FOREACH rev IN ARRAY ARRAY[(o ->> 'course_revision_id')::bigint, (nw ->> 'course_revision_id')::bigint]
    LOOP
        CONTINUE WHEN rev IS NULL;
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
        IF TG_OP = 'INSERT' OR nw -> 'course_revision_id' IS DISTINCT FROM o -> 'course_revision_id' THEN
            EXECUTE format('SELECT count(*) FROM %I.course_steps WHERE course_revision_id = $1', TG_TABLE_SCHEMA)
               INTO n USING (nw ->> 'course_revision_id')::bigint;
            IF n >= 200 THEN
                RAISE EXCEPTION 'a course revision has at most 200 steps' USING ERRCODE = 'LRN01';
            END IF;
        END IF;
        IF TG_OP = 'INSERT'
           OR (nw -> 'lesson_id', nw -> 'lesson_revision_id', nw -> 'course_revision_id')
              IS DISTINCT FROM (o -> 'lesson_id', o -> 'lesson_revision_id', o -> 'course_revision_id') THEN
            EXECUTE format('SELECT created_by FROM %I.lessons WHERE id = $1 FOR SHARE', TG_TABLE_SCHEMA)
               INTO lesson_owner USING (nw ->> 'lesson_id')::bigint;
            EXECUTE format('SELECT c.owner_id FROM %1$I.course_revisions r JOIN %1$I.courses c ON c.id = r.course_id
                             WHERE r.id = $1 FOR SHARE OF c', TG_TABLE_SCHEMA)
               INTO course_owner USING (nw ->> 'course_revision_id')::bigint;
            IF lesson_owner IS NULL OR lesson_owner IS DISTINCT FROM course_owner THEN
                -- Eligibility reads facts that are only inserted; see
                -- "Snapshots" above.
                IF pg_catalog.current_setting('transaction_isolation') <> 'read committed' THEN
                    RAISE EXCEPTION 'attaching another author''s lesson requires READ COMMITTED' USING ERRCODE = 'LRN01';
                END IF;
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

-- ---- submission (the pattern of 0011) ----

CREATE TABLE course_submission_audit (
    id           bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    course_id    bigint      NOT NULL REFERENCES courses (id),
    revision_id  bigint      NOT NULL,
    -- Always the revision's author (insert guard).
    actor_id     bigint      NOT NULL REFERENCES users (id),
    submitted_at timestamptz NOT NULL DEFAULT now(),
    xact_id      xid8        NOT NULL DEFAULT pg_current_xact_id(),
    CONSTRAINT course_submission_audit_revision_key UNIQUE (revision_id),
    CONSTRAINT course_submission_audit_revision_fkey FOREIGN KEY (course_id, revision_id)
        REFERENCES course_revisions (course_id, id)
);

CREATE INDEX course_submission_audit_course_idx ON course_submission_audit (course_id, id);
CREATE INDEX course_submission_audit_actor_idx ON course_submission_audit (actor_id);

-- Locks, in contract §2 order: the editions of the revision's placements,
-- the lessons of its steps and their pinned revisions (FOR SHARE,
-- ascending), the course FOR SHARE, the revision FOR UPDATE (its status
-- changes next). The steps are read before the revision lock and again
-- after it: a concurrent save that changed them makes this submission fail
-- (retry) rather than check rows it did not lock. Then: a draft of a course
-- that is not archived, submitted by its author who is an active author,
-- complete (title, summary, objectives, at least one module, every module
-- with a step), every step eligible, placement valid in submit mode and the
-- stored kind equal to the derived kind.
CREATE FUNCTION course_submission_audit_guard_insert() RETURNS trigger
LANGUAGE plpgsql SET search_path = pg_catalog, pg_temp AS $$
DECLARE
    course      bigint;
    archived    boolean;
    author      bigint;
    st          text;
    knd         text;
    derived     text;
    before_ids  bigint[];
    after_ids   bigint[];
    allowed     boolean;
    incomplete  boolean;
    ineligible  bigint;
    valid       boolean;
BEGIN
    IF NEW.submitted_at IS DISTINCT FROM now() OR NEW.xact_id IS DISTINCT FROM pg_current_xact_id() THEN
        RAISE EXCEPTION 'a course submission audit row records its own transaction and its time' USING ERRCODE = 'LRN01';
    END IF;
    IF pg_catalog.current_setting('transaction_isolation') <> 'read committed' THEN
        RAISE EXCEPTION 'course submission requires READ COMMITTED' USING ERRCODE = 'LRN01';
    END IF;
    EXECUTE format('SELECT course_id FROM %I.course_revisions WHERE id = $1', TG_TABLE_SCHEMA)
       INTO course USING NEW.revision_id;
    IF course IS NULL OR course IS DISTINCT FROM NEW.course_id THEN
        RAISE EXCEPTION 'revision % is not a revision of course %', NEW.revision_id, NEW.course_id USING ERRCODE = 'LRN01';
    END IF;
    EXECUTE format('SELECT 1 FROM %1$I.curriculum_editions WHERE id IN (
                        SELECT a.edition_id FROM %1$I.course_revision_assignments cra
                          JOIN %1$I.curriculum_assignments a ON a.id = cra.assignment_id
                         WHERE cra.course_revision_id = $1) ORDER BY id FOR SHARE', TG_TABLE_SCHEMA) USING NEW.revision_id;
    EXECUTE format('SELECT coalesce(array_agg(lesson_id ORDER BY lesson_id), ''{}'') FROM %I.course_steps WHERE course_revision_id = $1',
                   TG_TABLE_SCHEMA) INTO before_ids USING NEW.revision_id;
    EXECUTE format('SELECT 1 FROM %I.lessons WHERE id = ANY ($1) ORDER BY id FOR SHARE', TG_TABLE_SCHEMA) USING before_ids;
    EXECUTE format('SELECT 1 FROM %1$I.lesson_revisions WHERE id IN (SELECT lesson_revision_id FROM %1$I.course_steps
                     WHERE course_revision_id = $1) ORDER BY id FOR SHARE', TG_TABLE_SCHEMA) USING NEW.revision_id;
    EXECUTE format('SELECT archived_at IS NOT NULL FROM %I.courses WHERE id = $1 FOR SHARE', TG_TABLE_SCHEMA)
       INTO archived USING NEW.course_id;
    IF archived IS DISTINCT FROM false THEN
        RAISE EXCEPTION 'course % is archived', NEW.course_id USING ERRCODE = 'LRN01';
    END IF;
    EXECUTE format('SELECT author_id, status, kind FROM %I.course_revisions WHERE id = $1 FOR UPDATE', TG_TABLE_SCHEMA)
       INTO author, st, knd USING NEW.revision_id;
    IF st IS DISTINCT FROM 'draft' THEN
        RAISE EXCEPTION 'course revision % is not a draft', NEW.revision_id USING ERRCODE = 'LRN01';
    END IF;
    EXECUTE format('SELECT coalesce(array_agg(lesson_id ORDER BY lesson_id), ''{}'') FROM %I.course_steps WHERE course_revision_id = $1',
                   TG_TABLE_SCHEMA) INTO after_ids USING NEW.revision_id;
    IF after_ids IS DISTINCT FROM before_ids THEN
        RAISE EXCEPTION 'course revision % changed concurrently; retry the submission', NEW.revision_id USING ERRCODE = 'LRN01';
    END IF;
    IF NEW.actor_id IS DISTINCT FROM author THEN
        RAISE EXCEPTION 'only the author of course revision % can submit it', NEW.revision_id USING ERRCODE = 'LRN01';
    END IF;
    EXECUTE format('SELECT EXISTS (
                        SELECT 1 FROM %1$I.user_permissions p JOIN %1$I.users u ON u.id = p.user_id
                         WHERE p.user_id = $1 AND p.permission = ''author'' AND u.disabled_at IS NULL
                           FOR SHARE OF p, u)', TG_TABLE_SCHEMA)
       INTO allowed USING NEW.actor_id;
    IF NOT allowed THEN
        RAISE EXCEPTION 'user % is not an active author', NEW.actor_id USING ERRCODE = 'LRN01';
    END IF;
    EXECUTE format('SELECT r.title = '''' OR r.summary = '''' OR r.objectives = ''''
                        OR NOT EXISTS (SELECT 1 FROM %1$I.course_modules m WHERE m.course_revision_id = r.id)
                        OR EXISTS (SELECT 1 FROM %1$I.course_modules m WHERE m.course_revision_id = r.id
                                      AND NOT EXISTS (SELECT 1 FROM %1$I.course_steps s
                                                       WHERE s.course_revision_id = r.id AND s.module_key = m.module_key))
                      FROM %1$I.course_revisions r WHERE r.id = $1', TG_TABLE_SCHEMA)
       INTO incomplete USING NEW.revision_id;
    IF incomplete THEN
        RAISE EXCEPTION 'course revision % is incomplete', NEW.revision_id USING ERRCODE = 'LRN01';
    END IF;
    EXECUTE format('SELECT lesson_id FROM %1$I.course_steps
                     WHERE course_revision_id = $1 AND NOT %1$I.course_step_revision_eligible(lesson_id, lesson_revision_id)
                     ORDER BY lesson_id LIMIT 1', TG_TABLE_SCHEMA)
       INTO ineligible USING NEW.revision_id;
    IF ineligible IS NOT NULL THEN
        RAISE EXCEPTION 'course revision %: the step of lesson % is not eligible', NEW.revision_id, ineligible USING ERRCODE = 'LRN01';
    END IF;
    EXECUTE format('SELECT %I.course_revision_placement_valid($1, ''submit'')', TG_TABLE_SCHEMA) INTO valid USING NEW.revision_id;
    IF valid IS NOT TRUE THEN
        RAISE EXCEPTION 'course revision %: placement invalid for submission', NEW.revision_id USING ERRCODE = 'LRN01';
    END IF;
    EXECUTE format('SELECT %I.course_revision_derived_kind($1)', TG_TABLE_SCHEMA) INTO derived USING NEW.revision_id;
    IF knd IS DISTINCT FROM derived THEN
        RAISE EXCEPTION 'course revision % is stored as % but is %; save it again', NEW.revision_id, knd, derived USING ERRCODE = 'LRN01';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER zz_guard_course_submission_audit_insert
    BEFORE INSERT ON course_submission_audit
    FOR EACH ROW EXECUTE FUNCTION course_submission_audit_guard_insert();

CREATE FUNCTION course_submission_audit_check_applied() RETURNS trigger
LANGUAGE plpgsql SET search_path = pg_catalog, pg_temp AS $$
DECLARE
    st        text;
    submitted timestamptz;
BEGIN
    EXECUTE format('SELECT status, submitted_at FROM %I.course_revisions WHERE id = $1', TG_TABLE_SCHEMA)
       INTO st, submitted USING NEW.revision_id;
    IF st IS NULL OR st = 'draft' OR submitted IS DISTINCT FROM NEW.submitted_at THEN
        RAISE EXCEPTION 'submission of course revision % was audited but not applied', NEW.revision_id USING ERRCODE = 'LRN01';
    END IF;
    RETURN NULL;
END;
$$;

CREATE CONSTRAINT TRIGGER zz_guard_course_submission_audit_applied
    AFTER INSERT ON course_submission_audit
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION course_submission_audit_check_applied();

-- A revision leaves draft only with a submission audit row of this
-- transaction, at the transaction time, and the submitting UPDATE changes
-- only status, submitted_at, updated_at and version (by one): the reviewed
-- content is exactly the checked draft.
CREATE FUNCTION course_revisions_guard_submission() RETURNS trigger
LANGUAGE plpgsql SET search_path = pg_catalog, pg_temp AS $$
DECLARE
    audited boolean;
BEGIN
    IF OLD.status <> 'draft' OR NEW.status IS NOT DISTINCT FROM OLD.status THEN
        RETURN NEW;
    END IF;
    IF NEW.submitted_at IS DISTINCT FROM now() OR NEW.updated_at IS DISTINCT FROM now() OR NEW.version IS DISTINCT FROM OLD.version + 1
       OR to_jsonb(NEW) - 'status' - 'submitted_at' - 'updated_at' - 'version'
          IS DISTINCT FROM to_jsonb(OLD) - 'status' - 'submitted_at' - 'updated_at' - 'version' THEN
        RAISE EXCEPTION 'submitting course revision % changes only its status, submitted_at and updated_at (the transaction time) and version (by one)', OLD.id
            USING ERRCODE = 'LRN01';
    END IF;
    EXECUTE format('SELECT EXISTS (
                        SELECT 1 FROM %I.course_submission_audit
                         WHERE revision_id = $1 AND course_id = $2 AND actor_id = $3
                           AND submitted_at = $4 AND xact_id = pg_catalog.pg_current_xact_id())', TG_TABLE_SCHEMA)
       INTO audited USING OLD.id, OLD.course_id, OLD.author_id, NEW.submitted_at;
    IF NOT audited THEN
        RAISE EXCEPTION 'course revision % leaves draft only with a submission audit row in the same transaction', OLD.id
            USING ERRCODE = 'LRN01';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER zz_guard_course_revisions_submission_audited
    BEFORE UPDATE ON course_revisions
    FOR EACH ROW EXECUTE FUNCTION course_revisions_guard_submission();

CREATE TRIGGER zz_guard_course_submission_audit_append_only
    BEFORE UPDATE OR DELETE ON course_submission_audit
    FOR EACH ROW EXECUTE FUNCTION review_audit_append_only();
CREATE TRIGGER zz_guard_course_submission_audit_no_truncate
    BEFORE TRUNCATE ON course_submission_audit
    FOR EACH STATEMENT EXECUTE FUNCTION review_audit_append_only();

-- ---- review decisions (the pattern of 0006) ----

CREATE TABLE course_review_decisions (
    id          bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    course_id   bigint      NOT NULL REFERENCES courses (id),
    revision_id bigint      NOT NULL,
    reviewer_id bigint      NOT NULL REFERENCES users (id),
    action      text        NOT NULL,
    comment     text,
    decided_at  timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT course_review_decisions_action CHECK (action IN ('approve', 'request_changes')),
    CONSTRAINT course_review_decisions_comment_not_blank CHECK (comment IS NULL OR comment ~ '[^[:space:]]'),
    CONSTRAINT course_review_decisions_comment_required CHECK (action <> 'request_changes' OR comment IS NOT NULL),
    CONSTRAINT course_review_decisions_comment_length CHECK (char_length(comment) <= 5000),
    CONSTRAINT course_review_decisions_revision_key UNIQUE (revision_id),
    CONSTRAINT course_review_decisions_revision_fkey FOREIGN KEY (course_id, revision_id)
        REFERENCES course_revisions (course_id, id)
);

CREATE INDEX course_review_decisions_reviewer_idx ON course_review_decisions (reviewer_id);

-- Lock order: the course FOR SHARE, then the revision FOR UPDATE (its
-- status changes next), then the reviewer's assignment and account rows
-- (the application has share-locked them already). Accepts only a
-- non-withdrawn revision in review and a reviewer course_reviewer_eligible
-- accepts.
CREATE FUNCTION course_review_decisions_guard_insert() RETURNS trigger
LANGUAGE plpgsql SET search_path = pg_catalog, pg_temp AS $$
DECLARE
    course    bigint;
    subject   bigint;
    st        text;
    withdrawn boolean;
    eligible  boolean;
BEGIN
    IF NEW.decided_at IS DISTINCT FROM now() THEN
        RAISE EXCEPTION 'decided_at must be the transaction time' USING ERRCODE = 'LRN01';
    END IF;
    EXECUTE format('SELECT course_id FROM %I.course_revisions WHERE id = $1', TG_TABLE_SCHEMA)
       INTO course USING NEW.revision_id;
    IF course IS NULL OR course IS DISTINCT FROM NEW.course_id THEN
        RAISE EXCEPTION 'revision % is not a revision of course %', NEW.revision_id, NEW.course_id USING ERRCODE = 'LRN01';
    END IF;
    EXECUTE format('SELECT 1 FROM %I.courses WHERE id = $1 FOR SHARE', TG_TABLE_SCHEMA) USING course;
    EXECUTE format('SELECT subject_id, status, withdrawn_at IS NOT NULL FROM %I.course_revisions WHERE id = $1 FOR UPDATE', TG_TABLE_SCHEMA)
       INTO subject, st, withdrawn USING NEW.revision_id;
    IF st IS DISTINCT FROM 'in_review' THEN
        RAISE EXCEPTION 'course revision % is not in review', NEW.revision_id USING ERRCODE = 'LRN01';
    END IF;
    IF withdrawn THEN
        RAISE EXCEPTION 'course revision % is withdrawn', NEW.revision_id USING ERRCODE = 'LRN01';
    END IF;
    EXECUTE format('SELECT 1 FROM %1$I.reviewer_assignments r JOIN %1$I.users u ON u.id = r.user_id
                     WHERE r.user_id = $1 AND r.subject_id = $2 FOR SHARE OF r, u', TG_TABLE_SCHEMA)
       USING NEW.reviewer_id, subject;
    EXECUTE format('SELECT %I.course_reviewer_eligible($1, $2)', TG_TABLE_SCHEMA)
       INTO eligible USING NEW.reviewer_id, NEW.revision_id;
    IF eligible IS NOT TRUE THEN
        RAISE EXCEPTION 'user % is not an eligible reviewer of course revision %', NEW.reviewer_id, NEW.revision_id
            USING ERRCODE = 'LRN01';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER zz_guard_course_review_decisions_insert
    BEFORE INSERT ON course_review_decisions
    FOR EACH ROW EXECUTE FUNCTION course_review_decisions_guard_insert();

CREATE FUNCTION course_review_decisions_check_applied() RETURNS trigger
LANGUAGE plpgsql SET search_path = pg_catalog, pg_temp AS $$
DECLARE
    st       text;
    expected text;
BEGIN
    expected := CASE NEW.action WHEN 'approve' THEN 'approved' ELSE 'changes_requested' END;
    EXECUTE format('SELECT status FROM %I.course_revisions WHERE id = $1', TG_TABLE_SCHEMA)
       INTO st USING NEW.revision_id;
    IF st IS DISTINCT FROM expected THEN
        RAISE EXCEPTION 'review decision on course revision % was not applied to its status', NEW.revision_id
            USING ERRCODE = 'LRN01';
    END IF;
    RETURN NULL;
END;
$$;

CREATE CONSTRAINT TRIGGER zz_guard_course_review_decisions_applied
    AFTER INSERT ON course_review_decisions
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION course_review_decisions_check_applied();

CREATE FUNCTION course_revisions_guard_decision() RETURNS trigger
LANGUAGE plpgsql SET search_path = pg_catalog, pg_temp AS $$
DECLARE
    expected text;
    act      text;
BEGIN
    IF OLD.status <> 'in_review' OR NEW.status IS NOT DISTINCT FROM OLD.status THEN
        RETURN NEW;
    END IF;
    expected := CASE NEW.status WHEN 'approved' THEN 'approve' WHEN 'changes_requested' THEN 'request_changes' END;
    EXECUTE format('SELECT action FROM %I.course_review_decisions WHERE revision_id = $1', TG_TABLE_SCHEMA)
       INTO act USING OLD.id;
    IF expected IS NULL OR act IS DISTINCT FROM expected THEN
        RAISE EXCEPTION 'course revision % leaves review only with a recorded matching review decision', OLD.id
            USING ERRCODE = 'LRN01';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER zz_guard_course_revisions_decided
    BEFORE UPDATE ON course_revisions
    FOR EACH ROW EXECUTE FUNCTION course_revisions_guard_decision();

CREATE TRIGGER zz_guard_course_review_decisions_append_only
    BEFORE UPDATE OR DELETE ON course_review_decisions
    FOR EACH ROW EXECUTE FUNCTION review_audit_append_only();
CREATE TRIGGER zz_guard_course_review_decisions_no_truncate
    BEFORE TRUNCATE ON course_review_decisions
    FOR EACH STATEMENT EXECUTE FUNCTION review_audit_append_only();

-- ---- revision withdrawal ----

CREATE TABLE course_revision_withdrawals (
    id          bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    course_id   bigint      NOT NULL REFERENCES courses (id),
    revision_id bigint      NOT NULL,
    actor_id    bigint      NOT NULL REFERENCES users (id),
    reason      text        NOT NULL,
    occurred_at timestamptz NOT NULL DEFAULT now(),
    xact_id     xid8        NOT NULL DEFAULT pg_current_xact_id(),
    CONSTRAINT course_revision_withdrawals_reason_not_blank CHECK (reason ~ '[^[:space:]]'),
    CONSTRAINT course_revision_withdrawals_reason_length CHECK (char_length(reason) <= 2000),
    CONSTRAINT course_revision_withdrawals_revision_key UNIQUE (revision_id),
    CONSTRAINT course_revision_withdrawals_revision_fkey FOREIGN KEY (course_id, revision_id)
        REFERENCES course_revisions (course_id, id)
);

CREATE INDEX course_revision_withdrawals_course_idx ON course_revision_withdrawals (course_id, id);

-- Course FOR UPDATE (serializes with publication, restoration and
-- transfer), then the revision FOR UPDATE (the trigger below sets
-- withdrawn_at). In review or approved, not the published revision
-- (suspension is the emergency path), by an active administrator.
CREATE FUNCTION course_revision_withdrawals_guard_insert() RETURNS trigger
LANGUAGE plpgsql SET search_path = pg_catalog, pg_temp AS $$
DECLARE
    found_course boolean;
    current_ptr  bigint;
    course       bigint;
    st           text;
    allowed      boolean;
BEGIN
    IF NEW.occurred_at IS DISTINCT FROM now() OR NEW.xact_id IS DISTINCT FROM pg_current_xact_id() THEN
        RAISE EXCEPTION 'a course withdrawal records its own transaction and its time' USING ERRCODE = 'LRN01';
    END IF;
    EXECUTE format('SELECT true, published_revision_id FROM %I.courses WHERE id = $1 FOR UPDATE', TG_TABLE_SCHEMA)
       INTO found_course, current_ptr USING NEW.course_id;
    IF found_course IS NULL THEN
        RAISE EXCEPTION 'course % does not exist', NEW.course_id USING ERRCODE = 'LRN01';
    END IF;
    EXECUTE format('SELECT course_id, status FROM %I.course_revisions WHERE id = $1 FOR UPDATE', TG_TABLE_SCHEMA)
       INTO course, st USING NEW.revision_id;
    IF course IS DISTINCT FROM NEW.course_id OR st NOT IN ('in_review', 'approved') THEN
        RAISE EXCEPTION 'withdrawal: revision % is not a course revision in review or approved of course %', NEW.revision_id, NEW.course_id
            USING ERRCODE = 'LRN01';
    END IF;
    IF current_ptr IS NOT DISTINCT FROM NEW.revision_id THEN
        RAISE EXCEPTION 'course revision % is published; suspend the course instead', NEW.revision_id USING ERRCODE = 'LRN01';
    END IF;
    EXECUTE format('SELECT EXISTS (
                        SELECT 1 FROM %1$I.user_permissions p JOIN %1$I.users u ON u.id = p.user_id
                         WHERE p.user_id = $1 AND p.permission = ''administrator'' AND u.disabled_at IS NULL
                           FOR SHARE OF p, u)', TG_TABLE_SCHEMA)
       INTO allowed USING NEW.actor_id;
    IF NOT allowed THEN
        RAISE EXCEPTION 'user % is not an active administrator', NEW.actor_id USING ERRCODE = 'LRN01';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER zz_guard_course_revision_withdrawals_insert
    BEFORE INSERT ON course_revision_withdrawals
    FOR EACH ROW EXECUTE FUNCTION course_revision_withdrawals_guard_insert();

-- Applies an accepted withdrawal: sets withdrawn_at, which the active
-- candidate predicate excludes (the slot is freed) and which every decision,
-- publication and restoration guard refuses. SECURITY DEFINER with the
-- hardening of revision_withdrawals_close_candidate (0007):
-- course_revisions_guard_update (0014) accepts withdrawn_at only from inside
-- a trigger running as the owner of course_revisions.
CREATE FUNCTION course_revision_withdrawals_apply() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $$
BEGIN
    IF TG_WHEN <> 'AFTER' OR TG_LEVEL <> 'ROW' OR TG_OP <> 'INSERT' OR TG_TABLE_NAME <> 'course_revision_withdrawals'
       OR NOT EXISTS (SELECT 1 FROM pg_catalog.pg_class c
                       WHERE c.oid = TG_RELID
                         AND c.relpersistence = 'p'
                         AND c.relowner = (SELECT r.oid FROM pg_catalog.pg_roles r WHERE r.rolname = current_user)) THEN
        RAISE EXCEPTION 'course_revision_withdrawals_apply may only fire on the course_revision_withdrawals table' USING ERRCODE = 'LRN01';
    END IF;
    EXECUTE format('UPDATE %I.course_revisions SET withdrawn_at = pg_catalog.now() WHERE id = $1 AND course_id = $2', TG_TABLE_SCHEMA)
      USING NEW.revision_id, NEW.course_id;
    RETURN NULL;
END;
$$;

REVOKE EXECUTE ON FUNCTION course_revision_withdrawals_apply() FROM PUBLIC;

CREATE TRIGGER course_revision_withdrawals_apply
    AFTER INSERT ON course_revision_withdrawals
    FOR EACH ROW EXECUTE FUNCTION course_revision_withdrawals_apply();

CREATE TRIGGER zz_guard_course_revision_withdrawals_append_only
    BEFORE UPDATE OR DELETE ON course_revision_withdrawals
    FOR EACH ROW EXECUTE FUNCTION review_audit_append_only();
CREATE TRIGGER zz_guard_course_revision_withdrawals_no_truncate
    BEFORE TRUNCATE ON course_revision_withdrawals
    FOR EACH STATEMENT EXECUTE FUNCTION review_audit_append_only();

-- ---- publication audit and the pointer ----

-- Locks, in contract §2 order, for publish and restore: the editions of the
-- revision's placements, the lessons of its steps, their pinned revisions
-- (FOR SHARE, ascending), then the course FOR UPDATE, then the revision FOR
-- SHARE. The revision is submitted, so its children no longer change.
--   publish  an approved, never published, non-withdrawn revision of the
--            course whose base IS NOT DISTINCT FROM the pointer, approved by
--            a recorded decision, every step eligible, placement valid in
--            publish mode, course not archived, by an active publisher;
--   restore  an approved, non-withdrawn revision of the course with an
--            earlier publish row, every step eligible now (placement is not
--            rechecked: contract §3), course not archived, by an active
--            publisher or administrator;
--   suspend  the pointer to NULL, by an active administrator;
--   archive  the pointer to NULL with archived_at set in the same
--            transaction, by an active administrator.
CREATE FUNCTION course_publication_audit_guard_insert() RETURNS trigger
LANGUAGE plpgsql SET search_path = pg_catalog, pg_temp AS $$
DECLARE
    found_course boolean;
    current_ptr  bigint;
    archived     boolean;
    course       bigint;
    st           text;
    base         bigint;
    closed       boolean;
    withdrawn    boolean;
    flag         boolean;
    ineligible   bigint;
    perms        text[];
BEGIN
    IF NEW.occurred_at IS DISTINCT FROM now() OR NEW.xact_id IS DISTINCT FROM pg_current_xact_id() THEN
        RAISE EXCEPTION 'a course publication audit row records its own transaction and its time' USING ERRCODE = 'LRN01';
    END IF;
    IF NEW.action IN ('publish', 'restore') THEN
        IF pg_catalog.current_setting('transaction_isolation') <> 'read committed' THEN
            RAISE EXCEPTION 'course publication and restoration require READ COMMITTED' USING ERRCODE = 'LRN01';
        END IF;
        EXECUTE format('SELECT course_id, status FROM %I.course_revisions WHERE id = $1', TG_TABLE_SCHEMA)
           INTO course, st USING NEW.revision_id;
        IF course IS DISTINCT FROM NEW.course_id OR st IS DISTINCT FROM 'approved' THEN
            RAISE EXCEPTION 'course publication audit: revision % is not an approved revision of course %', NEW.revision_id, NEW.course_id
                USING ERRCODE = 'LRN01';
        END IF;
        EXECUTE format('SELECT 1 FROM %1$I.curriculum_editions WHERE id IN (
                            SELECT a.edition_id FROM %1$I.course_revision_assignments cra
                              JOIN %1$I.curriculum_assignments a ON a.id = cra.assignment_id
                             WHERE cra.course_revision_id = $1) ORDER BY id FOR SHARE', TG_TABLE_SCHEMA) USING NEW.revision_id;
        EXECUTE format('SELECT 1 FROM %1$I.lessons WHERE id IN (SELECT lesson_id FROM %1$I.course_steps
                         WHERE course_revision_id = $1) ORDER BY id FOR SHARE', TG_TABLE_SCHEMA) USING NEW.revision_id;
        EXECUTE format('SELECT 1 FROM %1$I.lesson_revisions WHERE id IN (SELECT lesson_revision_id FROM %1$I.course_steps
                         WHERE course_revision_id = $1) ORDER BY id FOR SHARE', TG_TABLE_SCHEMA) USING NEW.revision_id;
    END IF;
    EXECUTE format('SELECT true, published_revision_id, archived_at IS NOT NULL FROM %I.courses WHERE id = $1 FOR UPDATE', TG_TABLE_SCHEMA)
       INTO found_course, current_ptr, archived USING NEW.course_id;
    IF found_course IS NULL THEN
        RAISE EXCEPTION 'course % does not exist', NEW.course_id USING ERRCODE = 'LRN01';
    END IF;
    IF NEW.previous_revision_id IS DISTINCT FROM current_ptr THEN
        RAISE EXCEPTION 'previous_revision_id must be the course''s current published revision' USING ERRCODE = 'LRN01';
    END IF;

    IF NEW.action IN ('suspend', 'archive') THEN
        perms := ARRAY['administrator'];
    ELSE
        IF archived THEN
            RAISE EXCEPTION 'course % is archived', NEW.course_id USING ERRCODE = 'LRN01';
        END IF;
        EXECUTE format('SELECT base_revision_id, candidate_closed_at IS NOT NULL, withdrawn_at IS NOT NULL
                          FROM %I.course_revisions WHERE id = $1 FOR SHARE', TG_TABLE_SCHEMA)
           INTO base, closed, withdrawn USING NEW.revision_id;
        IF withdrawn THEN
            RAISE EXCEPTION 'course revision % is withdrawn and can never be published or restored', NEW.revision_id USING ERRCODE = 'LRN01';
        END IF;
        EXECUTE format('SELECT lesson_id FROM %1$I.course_steps
                         WHERE course_revision_id = $1 AND NOT %1$I.course_step_revision_eligible(lesson_id, lesson_revision_id)
                         ORDER BY lesson_id LIMIT 1', TG_TABLE_SCHEMA)
           INTO ineligible USING NEW.revision_id;
        IF ineligible IS NOT NULL THEN
            RAISE EXCEPTION 'course revision %: the step of lesson % is not eligible', NEW.revision_id, ineligible USING ERRCODE = 'LRN01';
        END IF;
        IF NEW.action = 'publish' THEN
            IF closed THEN
                RAISE EXCEPTION 'course revision % was already published; re-pointing to it is restoration', NEW.revision_id
                    USING ERRCODE = 'LRN01';
            END IF;
            IF base IS DISTINCT FROM current_ptr THEN
                RAISE EXCEPTION 'course revision % was drafted against another published revision (stale base)', NEW.revision_id
                    USING ERRCODE = 'LRN01';
            END IF;
            EXECUTE format('SELECT EXISTS (SELECT 1 FROM %I.course_review_decisions WHERE revision_id = $1 AND action = ''approve'')',
                           TG_TABLE_SCHEMA) INTO flag USING NEW.revision_id;
            IF NOT flag THEN
                RAISE EXCEPTION 'course revision % has no recorded approval', NEW.revision_id USING ERRCODE = 'LRN01';
            END IF;
            EXECUTE format('SELECT %I.course_revision_placement_valid($1, ''publish'')', TG_TABLE_SCHEMA)
               INTO flag USING NEW.revision_id;
            IF flag IS NOT TRUE THEN
                RAISE EXCEPTION 'course revision %: placement invalid for publication', NEW.revision_id USING ERRCODE = 'LRN01';
            END IF;
            perms := ARRAY['publisher'];
        ELSE
            EXECUTE format('SELECT EXISTS (SELECT 1 FROM %I.course_publication_audit
                                             WHERE revision_id = $1 AND course_id = $2 AND action = ''publish'')', TG_TABLE_SCHEMA)
               INTO flag USING NEW.revision_id, NEW.course_id;
            IF NOT flag THEN
                RAISE EXCEPTION 'course revision % was never published and cannot be restored', NEW.revision_id USING ERRCODE = 'LRN01';
            END IF;
            perms := ARRAY['publisher', 'administrator'];
        END IF;
    END IF;
    EXECUTE format('SELECT EXISTS (
                        SELECT 1 FROM %1$I.user_permissions p JOIN %1$I.users u ON u.id = p.user_id
                         WHERE p.user_id = $1 AND p.permission = ANY ($2) AND u.disabled_at IS NULL
                           FOR SHARE OF p, u)', TG_TABLE_SCHEMA)
       INTO flag USING NEW.actor_id, perms;
    IF NOT flag THEN
        RAISE EXCEPTION 'user % may not % courses', NEW.actor_id, NEW.action USING ERRCODE = 'LRN01';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER zz_guard_course_publication_audit_insert
    BEFORE INSERT ON course_publication_audit
    FOR EACH ROW EXECUTE FUNCTION course_publication_audit_guard_insert();

-- At commit, the course points at the audited revision (NULL for suspend
-- and archive), and an archive row archived it.
CREATE FUNCTION course_publication_audit_check_applied() RETURNS trigger
LANGUAGE plpgsql SET search_path = pg_catalog, pg_temp AS $$
DECLARE
    current_ptr bigint;
    archived    boolean;
BEGIN
    EXECUTE format('SELECT published_revision_id, archived_at IS NOT NULL FROM %I.courses WHERE id = $1', TG_TABLE_SCHEMA)
       INTO current_ptr, archived USING NEW.course_id;
    IF current_ptr IS DISTINCT FROM NEW.revision_id OR (NEW.action = 'archive' AND archived IS NOT TRUE) THEN
        RAISE EXCEPTION 'course publication audit row % (%) was not applied to course %', NEW.id, NEW.action, NEW.course_id
            USING ERRCODE = 'LRN01';
    END IF;
    RETURN NULL;
END;
$$;

CREATE CONSTRAINT TRIGGER zz_guard_course_publication_audit_applied
    AFTER INSERT ON course_publication_audit
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION course_publication_audit_check_applied();

CREATE TRIGGER zz_guard_course_publication_audit_append_only
    BEFORE UPDATE OR DELETE ON course_publication_audit
    FOR EACH ROW EXECUTE FUNCTION review_audit_append_only();
CREATE TRIGGER zz_guard_course_publication_audit_no_truncate
    BEFORE TRUNCATE ON course_publication_audit
    FOR EACH STATEMENT EXECUTE FUNCTION review_audit_append_only();

-- Every change of the pointer requires an audit row of this transaction
-- recording exactly that change, and the pointer names only an approved,
-- non-withdrawn revision of the course.
CREATE FUNCTION courses_guard_published() RETURNS trigger
LANGUAGE plpgsql SET search_path = pg_catalog, pg_temp AS $$
DECLARE
    audited   boolean;
    st        text;
    withdrawn boolean;
BEGIN
    IF NEW.published_revision_id IS NOT DISTINCT FROM OLD.published_revision_id THEN
        RETURN NEW;
    END IF;
    EXECUTE format('SELECT EXISTS (
                        SELECT 1 FROM %I.course_publication_audit
                         WHERE course_id = $1 AND revision_id IS NOT DISTINCT FROM $2
                           AND previous_revision_id IS NOT DISTINCT FROM $3
                           AND xact_id = pg_catalog.pg_current_xact_id())', TG_TABLE_SCHEMA)
       INTO audited USING NEW.id, NEW.published_revision_id, OLD.published_revision_id;
    IF NOT audited THEN
        RAISE EXCEPTION 'changing the published revision of course % requires a course publication audit row in the same transaction', NEW.id
            USING ERRCODE = 'LRN01';
    END IF;
    IF NEW.published_revision_id IS NOT NULL THEN
        EXECUTE format('SELECT status, withdrawn_at IS NOT NULL FROM %I.course_revisions WHERE id = $1 AND course_id = $2', TG_TABLE_SCHEMA)
           INTO st, withdrawn USING NEW.published_revision_id, NEW.id;
        IF st IS DISTINCT FROM 'approved' OR withdrawn THEN
            RAISE EXCEPTION 'the published pointer of course % can name only an approved, non-withdrawn revision', NEW.id
                USING ERRCODE = 'LRN01';
        END IF;
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER zz_guard_courses_published_audited
    BEFORE UPDATE ON courses
    FOR EACH ROW EXECUTE FUNCTION courses_guard_published();

-- Publication closes the revision as the course's candidate
-- (candidate_closed_at, 0004 meaning). SECURITY DEFINER with the hardening of
-- lessons_close_published_candidate (0004).
CREATE FUNCTION courses_close_published_candidate() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $$
BEGIN
    IF TG_WHEN <> 'AFTER' OR TG_LEVEL <> 'ROW' OR TG_OP <> 'UPDATE' OR TG_TABLE_NAME <> 'courses'
       OR NOT EXISTS (SELECT 1 FROM pg_catalog.pg_class c
                       WHERE c.oid = TG_RELID
                         AND c.relpersistence = 'p'
                         AND c.relowner = (SELECT r.oid FROM pg_catalog.pg_roles r WHERE r.rolname = current_user)) THEN
        RAISE EXCEPTION 'courses_close_published_candidate may only fire on the courses table' USING ERRCODE = 'LRN01';
    END IF;
    IF NEW.published_revision_id IS NULL OR NEW.published_revision_id IS NOT DISTINCT FROM OLD.published_revision_id THEN
        RETURN NULL;
    END IF;
    EXECUTE format('UPDATE %I.course_revisions SET candidate_closed_at = pg_catalog.now()
                     WHERE id = $1 AND course_id = $2 AND candidate_closed_at IS NULL', TG_TABLE_SCHEMA)
      USING NEW.published_revision_id, NEW.id;
    RETURN NULL;
END;
$$;

REVOKE EXECUTE ON FUNCTION courses_close_published_candidate() FROM PUBLIC;

CREATE TRIGGER courses_close_published_candidate
    AFTER UPDATE OF published_revision_id ON courses
    FOR EACH ROW EXECUTE FUNCTION courses_close_published_candidate();

-- ---- lifecycle audit: archive requests, archive without a pointer,
-- unarchive, administrator draft discard, transfer ----

CREATE TABLE course_lifecycle_audit (
    id            bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    course_id     bigint      NOT NULL REFERENCES courses (id),
    actor_id      bigint      NOT NULL REFERENCES users (id),
    action        text        NOT NULL,
    reason        text        NOT NULL,
    -- discard_draft: the discarded draft (deleted, so no foreign key).
    revision_id   bigint,
    from_owner_id bigint      REFERENCES users (id),
    to_owner_id   bigint      REFERENCES users (id),
    occurred_at   timestamptz NOT NULL DEFAULT now(),
    xact_id       xid8        NOT NULL DEFAULT pg_current_xact_id(),
    CONSTRAINT course_lifecycle_audit_action CHECK (action IN ('archive_request', 'archive', 'unarchive', 'discard_draft', 'transfer')),
    CONSTRAINT course_lifecycle_audit_revision CHECK ((action = 'discard_draft') = (revision_id IS NOT NULL)),
    CONSTRAINT course_lifecycle_audit_owners CHECK (
        (action = 'transfer' AND from_owner_id IS NOT NULL AND to_owner_id IS NOT NULL AND from_owner_id <> to_owner_id)
        OR (action <> 'transfer' AND from_owner_id IS NULL AND to_owner_id IS NULL)),
    CONSTRAINT course_lifecycle_audit_reason_not_blank CHECK (reason ~ '[^[:space:]]'),
    CONSTRAINT course_lifecycle_audit_reason_length CHECK (char_length(reason) <= 2000)
);

CREATE INDEX course_lifecycle_audit_course_idx ON course_lifecycle_audit (course_id, id);
CREATE INDEX course_lifecycle_audit_requests_idx ON course_lifecycle_audit (id) WHERE action = 'archive_request';

-- Course FOR UPDATE (FOR SHARE for a request), then the checks per action;
-- archive_request is the owner's (an active author), every other action an
-- active administrator's.
CREATE FUNCTION course_lifecycle_audit_guard_insert() RETURNS trigger
LANGUAGE plpgsql SET search_path = pg_catalog, pg_temp AS $$
DECLARE
    found_course boolean;
    owner        bigint;
    current_ptr  bigint;
    archived     boolean;
    flag         boolean;
    course       bigint;
    st           text;
BEGIN
    IF NEW.occurred_at IS DISTINCT FROM now() OR NEW.xact_id IS DISTINCT FROM pg_current_xact_id() THEN
        RAISE EXCEPTION 'a course lifecycle audit row records its own transaction and its time' USING ERRCODE = 'LRN01';
    END IF;
    EXECUTE format('SELECT true, owner_id, published_revision_id, archived_at IS NOT NULL FROM %I.courses WHERE id = $1 '
                   || CASE WHEN NEW.action = 'archive_request' THEN 'FOR SHARE' ELSE 'FOR UPDATE' END, TG_TABLE_SCHEMA)
       INTO found_course, owner, current_ptr, archived USING NEW.course_id;
    IF found_course IS NULL THEN
        RAISE EXCEPTION 'course % does not exist', NEW.course_id USING ERRCODE = 'LRN01';
    END IF;
    IF NEW.action = 'archive_request' THEN
        IF NEW.actor_id IS DISTINCT FROM owner OR archived THEN
            RAISE EXCEPTION 'only the owner of course % can request its archival, once it is not archived', NEW.course_id USING ERRCODE = 'LRN01';
        END IF;
    END IF;
    EXECUTE format('SELECT EXISTS (
                        SELECT 1 FROM %1$I.user_permissions p JOIN %1$I.users u ON u.id = p.user_id
                         WHERE p.user_id = $1 AND p.permission = $2 AND u.disabled_at IS NULL
                           FOR SHARE OF p, u)', TG_TABLE_SCHEMA)
       INTO flag USING NEW.actor_id, CASE WHEN NEW.action = 'archive_request' THEN 'author' ELSE 'administrator' END;
    IF NOT flag THEN
        RAISE EXCEPTION 'user % may not record % for courses', NEW.actor_id, NEW.action USING ERRCODE = 'LRN01';
    END IF;
    IF NEW.action = 'archive' AND (archived OR current_ptr IS NOT NULL) THEN
        RAISE EXCEPTION 'course % is archived or published (archive a published course with a course_publication_audit row)', NEW.course_id
            USING ERRCODE = 'LRN01';
    ELSIF NEW.action = 'unarchive' AND NOT archived THEN
        RAISE EXCEPTION 'course % is not archived', NEW.course_id USING ERRCODE = 'LRN01';
    ELSIF NEW.action = 'transfer' THEN
        IF NEW.from_owner_id IS DISTINCT FROM owner THEN
            RAISE EXCEPTION 'from_owner_id must be the current owner of course %', NEW.course_id USING ERRCODE = 'LRN01';
        END IF;
        EXECUTE format('SELECT EXISTS (
                            SELECT 1 FROM %1$I.user_permissions p JOIN %1$I.users u ON u.id = p.user_id
                             WHERE p.user_id = $1 AND p.permission = ''author'' AND u.disabled_at IS NULL
                               FOR SHARE OF p, u)', TG_TABLE_SCHEMA)
           INTO flag USING NEW.to_owner_id;
        IF NOT flag THEN
            RAISE EXCEPTION 'user % is not an active author', NEW.to_owner_id USING ERRCODE = 'LRN01';
        END IF;
        EXECUTE format('SELECT EXISTS (SELECT 1 FROM %I.course_revisions
                         WHERE course_id = $1 AND withdrawn_at IS NULL
                           AND (status IN (''draft'', ''in_review'') OR (status = ''approved'' AND candidate_closed_at IS NULL)))',
                       TG_TABLE_SCHEMA) INTO flag USING NEW.course_id;
        IF flag THEN
            RAISE EXCEPTION 'course % has an active candidate; discard or withdraw it before a transfer', NEW.course_id USING ERRCODE = 'LRN01';
        END IF;
    ELSIF NEW.action = 'discard_draft' THEN
        EXECUTE format('SELECT course_id, status FROM %I.course_revisions WHERE id = $1 FOR UPDATE', TG_TABLE_SCHEMA)
           INTO course, st USING NEW.revision_id;
        IF course IS DISTINCT FROM NEW.course_id OR st IS DISTINCT FROM 'draft' THEN
            RAISE EXCEPTION 'revision % is not a draft of course %', NEW.revision_id, NEW.course_id USING ERRCODE = 'LRN01';
        END IF;
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER zz_guard_course_lifecycle_audit_insert
    BEFORE INSERT ON course_lifecycle_audit
    FOR EACH ROW EXECUTE FUNCTION course_lifecycle_audit_guard_insert();

-- At commit, every row was applied: archive archived the course, unarchive
-- unarchived it, transfer gave it to to_owner_id, discard_draft removed the
-- draft.
CREATE FUNCTION course_lifecycle_audit_check_applied() RETURNS trigger
LANGUAGE plpgsql SET search_path = pg_catalog, pg_temp AS $$
DECLARE
    owner    bigint;
    archived boolean;
    present  boolean;
BEGIN
    EXECUTE format('SELECT owner_id, archived_at IS NOT NULL FROM %I.courses WHERE id = $1', TG_TABLE_SCHEMA)
       INTO owner, archived USING NEW.course_id;
    EXECUTE format('SELECT EXISTS (SELECT 1 FROM %I.course_revisions WHERE id = $1)', TG_TABLE_SCHEMA)
       INTO present USING NEW.revision_id;
    IF (NEW.action = 'archive' AND archived IS NOT TRUE)
       OR (NEW.action = 'unarchive' AND archived IS NOT FALSE)
       OR (NEW.action = 'transfer' AND owner IS DISTINCT FROM NEW.to_owner_id)
       OR (NEW.action = 'discard_draft' AND present) THEN
        RAISE EXCEPTION 'course lifecycle audit row % (%) was not applied to course %', NEW.id, NEW.action, NEW.course_id
            USING ERRCODE = 'LRN01';
    END IF;
    RETURN NULL;
END;
$$;

CREATE CONSTRAINT TRIGGER zz_guard_course_lifecycle_audit_applied
    AFTER INSERT ON course_lifecycle_audit
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION course_lifecycle_audit_check_applied();

CREATE TRIGGER zz_guard_course_lifecycle_audit_append_only
    BEFORE UPDATE OR DELETE ON course_lifecycle_audit
    FOR EACH ROW EXECUTE FUNCTION review_audit_append_only();
CREATE TRIGGER zz_guard_course_lifecycle_audit_no_truncate
    BEFORE TRUNCATE ON course_lifecycle_audit
    FOR EACH STATEMENT EXECUTE FUNCTION review_audit_append_only();

-- ---- courses: replaced guard ----

-- Replaces 0014's courses_guard. Identity and last_revision_number as
-- before. owner_id changes only with a transfer row of this transaction
-- naming exactly that change; archived_at is set (to the transaction time)
-- only with an archive row of this transaction (course_publication_audit
-- when the pointer is cleared, course_lifecycle_audit otherwise) and
-- cleared only with an unarchive row; dependency_count grows only from
-- inside a trigger running as the table owner (the dependency trigger),
-- alone. The pointer has its own guard (courses_guard_published). Delete:
-- never once a revision was submitted, the course was published or it has
-- a lifecycle row.
CREATE OR REPLACE FUNCTION courses_guard() RETURNS trigger
LANGUAGE plpgsql SET search_path = pg_catalog, pg_temp AS $$
DECLARE
    history boolean;
    top     integer;
    audited boolean;
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
        IF NEW.last_revision_number > OLD.last_revision_number THEN
            EXECUTE format('SELECT max(revision_number) FROM %I.course_revisions WHERE course_id = $1', TG_TABLE_SCHEMA)
               INTO top USING OLD.id;
            IF NEW.last_revision_number IS DISTINCT FROM top THEN
                RAISE EXCEPTION 'course % last_revision_number can only become its highest revision number', OLD.id
                    USING ERRCODE = 'LRN01';
            END IF;
        END IF;
        IF NEW.dependency_count IS DISTINCT FROM OLD.dependency_count THEN
            IF pg_trigger_depth() < 2
               OR NOT EXISTS (SELECT 1 FROM pg_catalog.pg_class c
                               WHERE c.oid = TG_RELID
                                 AND c.relowner = (SELECT r.oid FROM pg_catalog.pg_roles r WHERE r.rolname = current_user)) THEN
                RAISE EXCEPTION 'course % dependency_count changes only through recorded dependency events', OLD.id USING ERRCODE = 'LRN01';
            END IF;
            IF NEW.dependency_count <= OLD.dependency_count
               OR to_jsonb(NEW) - 'dependency_count' IS DISTINCT FROM to_jsonb(OLD) - 'dependency_count' THEN
                RAISE EXCEPTION 'course % dependency_count only grows, alone', OLD.id USING ERRCODE = 'LRN01';
            END IF;
            RETURN NEW;
        END IF;
        IF NEW.owner_id IS DISTINCT FROM OLD.owner_id THEN
            EXECUTE format('SELECT EXISTS (SELECT 1 FROM %I.course_lifecycle_audit
                             WHERE course_id = $1 AND action = ''transfer'' AND from_owner_id = $2 AND to_owner_id = $3
                               AND xact_id = pg_catalog.pg_current_xact_id())', TG_TABLE_SCHEMA)
               INTO audited USING OLD.id, OLD.owner_id, NEW.owner_id;
            IF NOT audited THEN
                RAISE EXCEPTION 'course % owner changes only through an audited transfer', OLD.id USING ERRCODE = 'LRN01';
            END IF;
        END IF;
        IF NEW.archived_at IS DISTINCT FROM OLD.archived_at THEN
            IF OLD.archived_at IS NULL THEN
                IF NEW.archived_at IS DISTINCT FROM now() THEN
                    RAISE EXCEPTION 'course % archived_at is the transaction time', OLD.id USING ERRCODE = 'LRN01';
                END IF;
                EXECUTE format('SELECT EXISTS (SELECT 1 FROM %1$I.course_publication_audit
                                 WHERE course_id = $1 AND action = ''archive'' AND xact_id = pg_catalog.pg_current_xact_id())
                                OR EXISTS (SELECT 1 FROM %1$I.course_lifecycle_audit
                                 WHERE course_id = $1 AND action = ''archive'' AND xact_id = pg_catalog.pg_current_xact_id())',
                               TG_TABLE_SCHEMA) INTO audited USING OLD.id;
            ELSIF NEW.archived_at IS NULL THEN
                EXECUTE format('SELECT EXISTS (SELECT 1 FROM %I.course_lifecycle_audit
                                 WHERE course_id = $1 AND action = ''unarchive'' AND xact_id = pg_catalog.pg_current_xact_id())',
                               TG_TABLE_SCHEMA) INTO audited USING OLD.id;
            ELSE
                audited := false;
            END IF;
            IF NOT audited THEN
                RAISE EXCEPTION 'course % archive state changes only through an audited archive or unarchive', OLD.id USING ERRCODE = 'LRN01';
            END IF;
        END IF;
        RETURN NEW;
    END IF;
    EXECUTE format('SELECT EXISTS (SELECT 1 FROM %1$I.course_revisions WHERE course_id = $1 AND status <> ''draft'')
                        OR EXISTS (SELECT 1 FROM %1$I.course_lifecycle_audit WHERE course_id = $1)', TG_TABLE_SCHEMA)
       INTO history USING OLD.id;
    IF history OR OLD.published_revision_id IS NOT NULL THEN
        RAISE EXCEPTION 'course % has submitted history and is never deleted', OLD.id USING ERRCODE = 'LRN01';
    END IF;
    RETURN OLD;
END;
$$;

-- ---- dependency effects (contract §7) ----

CREATE TABLE course_dependency_events (
    id                 bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    course_id          bigint      NOT NULL REFERENCES courses (id),
    -- The course's published revision and its affected step.
    course_revision_id bigint      NOT NULL,
    lesson_id          bigint      NOT NULL REFERENCES lessons (id),
    lesson_revision_id bigint      NOT NULL,
    cause              text        NOT NULL,
    occurred_at        timestamptz NOT NULL DEFAULT now(),
    xact_id            xid8        NOT NULL DEFAULT pg_current_xact_id(),
    CONSTRAINT course_dependency_events_cause CHECK (cause IN ('revision_withdrawn', 'lesson_unpublished', 'lesson_archived')),
    CONSTRAINT course_dependency_events_revision_fkey FOREIGN KEY (course_id, course_revision_id)
        REFERENCES course_revisions (course_id, id),
    CONSTRAINT course_dependency_events_step_fkey FOREIGN KEY (course_revision_id, lesson_id)
        REFERENCES course_steps (course_revision_id, lesson_id)
);

CREATE INDEX course_dependency_events_course_idx ON course_dependency_events (course_id, id);

-- Rows are written only by the owner-run dependency trigger, at the
-- transaction time.
CREATE FUNCTION course_dependency_events_guard_insert() RETURNS trigger
LANGUAGE plpgsql SET search_path = pg_catalog, pg_temp AS $$
BEGIN
    IF pg_trigger_depth() < 2
       OR NOT EXISTS (SELECT 1 FROM pg_catalog.pg_class c
                       WHERE c.oid = TG_RELID
                         AND c.relowner = (SELECT r.oid FROM pg_catalog.pg_roles r WHERE r.rolname = current_user)) THEN
        RAISE EXCEPTION 'course dependency events are recorded only by their triggers' USING ERRCODE = 'LRN01';
    END IF;
    IF NEW.occurred_at IS DISTINCT FROM now() OR NEW.xact_id IS DISTINCT FROM pg_current_xact_id() THEN
        RAISE EXCEPTION 'a course dependency event records its own transaction and its time' USING ERRCODE = 'LRN01';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER zz_guard_course_dependency_events_insert
    BEFORE INSERT ON course_dependency_events
    FOR EACH ROW EXECUTE FUNCTION course_dependency_events_guard_insert();
CREATE TRIGGER zz_guard_course_dependency_events_append_only
    BEFORE UPDATE OR DELETE ON course_dependency_events
    FOR EACH ROW EXECUTE FUNCTION review_audit_append_only();
CREATE TRIGGER zz_guard_course_dependency_events_no_truncate
    BEFORE TRUNCATE ON course_dependency_events
    FOR EACH STATEMENT EXECUTE FUNCTION review_audit_append_only();

-- Records the effect of a lesson revision withdrawal, a lesson
-- unpublication or a lesson archive on published courses: one event per
-- affected course and step, and dependency_count of each affected course
-- increased by its number of events. Refuses outside READ COMMITTED
-- whether or not a course is affected, so a stale snapshot can never miss a
-- course published concurrently. Runs under the lesson lock its writer holds
-- (the withdrawal and audit guards lock the lesson FOR UPDATE; an archive
-- updates the lesson row), so no publication or restoration naming the
-- lesson can commit meanwhile (they share-lock it); then locks the affected
-- courses FOR UPDATE in ascending ID and re-reads them after the lock.
--
-- SECURITY DEFINER with the hardening of 0004/0007: dependency_count and
-- course_dependency_events accept writes only from inside a trigger running
-- as their owner. It refuses to run except AFTER INSERT/UPDATE FOR EACH ROW
-- on the owner's permanent revision_withdrawals, publication_audit or
-- lessons, and is not executable by PUBLIC.
CREATE FUNCTION course_dependency_effects() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $$
DECLARE
    lesson      bigint;
    lrev        bigint;
    why         text;
    affected    bigint[];
BEGIN
    IF TG_WHEN <> 'AFTER' OR TG_LEVEL <> 'ROW'
       OR TG_TABLE_NAME NOT IN ('revision_withdrawals', 'publication_audit', 'lessons')
       OR (TG_TABLE_NAME = 'lessons') <> (TG_OP = 'UPDATE')
       OR (TG_TABLE_NAME <> 'lessons') <> (TG_OP = 'INSERT')
       OR NOT EXISTS (SELECT 1 FROM pg_catalog.pg_class c
                       WHERE c.oid = TG_RELID
                         AND c.relpersistence = 'p'
                         AND c.relowner = (SELECT r.oid FROM pg_catalog.pg_roles r WHERE r.rolname = current_user)) THEN
        RAISE EXCEPTION 'course_dependency_effects may only fire on revision_withdrawals, publication_audit or lessons' USING ERRCODE = 'LRN01';
    END IF;
    IF TG_TABLE_NAME = 'revision_withdrawals' THEN
        lesson := NEW.lesson_id;
        lrev := NEW.revision_id;
        why := 'revision_withdrawn';
    ELSIF TG_TABLE_NAME = 'publication_audit' THEN
        IF NEW.action <> 'unpublish' THEN
            RETURN NULL;
        END IF;
        lesson := NEW.lesson_id;
        why := 'lesson_unpublished';
    ELSE
        IF OLD.archived_at IS NOT NULL OR NEW.archived_at IS NULL THEN
            RETURN NULL;
        END IF;
        lesson := NEW.id;
        why := 'lesson_archived';
    END IF;
    IF pg_catalog.current_setting('transaction_isolation') <> 'read committed' THEN
        RAISE EXCEPTION 'lesson withdrawal, unpublication and archive require READ COMMITTED (course dependency effects)'
            USING ERRCODE = 'LRN01';
    END IF;
    EXECUTE format('SELECT coalesce(array_agg(id ORDER BY id), ''{}'') FROM (
                        SELECT c.id FROM %1$I.courses c
                         WHERE EXISTS (SELECT 1 FROM %1$I.course_steps s
                                        WHERE s.course_revision_id = c.published_revision_id AND s.lesson_id = $1
                                          AND ($2::bigint IS NULL OR s.lesson_revision_id = $2))
                         ORDER BY c.id FOR UPDATE) locked', TG_TABLE_SCHEMA)
       INTO affected USING lesson, lrev;
    IF cardinality(affected) = 0 THEN
        RETURN NULL;
    END IF;
    -- Re-read after the locks (a fresh snapshot at READ COMMITTED).
    EXECUTE format('WITH ins AS (
                        INSERT INTO %1$I.course_dependency_events (course_id, course_revision_id, lesson_id, lesson_revision_id, cause)
                        SELECT c.id, c.published_revision_id, s.lesson_id, s.lesson_revision_id, $3
                          FROM %1$I.courses c
                          JOIN %1$I.course_steps s ON s.course_revision_id = c.published_revision_id
                         WHERE c.id = ANY ($4) AND s.lesson_id = $1 AND ($2::bigint IS NULL OR s.lesson_revision_id = $2)
                         ORDER BY c.id
                        RETURNING course_id)
                    UPDATE %1$I.courses c SET dependency_count = c.dependency_count + e.n
                      FROM (SELECT course_id, count(*)::integer AS n FROM ins GROUP BY course_id) e
                     WHERE c.id = e.course_id', TG_TABLE_SCHEMA)
      USING lesson, lrev, why, affected;
    RETURN NULL;
END;
$$;

REVOKE EXECUTE ON FUNCTION course_dependency_effects() FROM PUBLIC;

CREATE TRIGGER course_dependency_on_withdrawal
    AFTER INSERT ON revision_withdrawals
    FOR EACH ROW EXECUTE FUNCTION course_dependency_effects();
CREATE TRIGGER course_dependency_on_unpublish
    AFTER INSERT ON publication_audit
    FOR EACH ROW WHEN (NEW.action = 'unpublish') EXECUTE FUNCTION course_dependency_effects();
CREATE TRIGGER course_dependency_on_lesson_archive
    AFTER UPDATE OF archived_at ON lessons
    FOR EACH ROW WHEN (OLD.archived_at IS NULL AND NEW.archived_at IS NOT NULL) EXECUTE FUNCTION course_dependency_effects();

-- ---- reader views (contract §5; used by B08) ----

-- A course is readable when its pointer is set, it is not archived, and the
-- published revision is approved and not withdrawn. security_invoker: read
-- with the caller's privileges.
CREATE VIEW reader_published_courses WITH (security_invoker = true) AS
SELECT c.id          AS course_id,
       c.public_id,
       c.subject_id,
       c.language,
       c.owner_id,
       r.id          AS revision_id,
       r.revision_number,
       r.title,
       r.summary,
       r.objectives,
       r.prerequisites,
       r.cover_media_id,
       r.cover_alt
  FROM courses c
  JOIN course_revisions r ON r.id = c.published_revision_id AND r.course_id = c.id
 WHERE c.archived_at IS NULL
   AND r.status = 'approved'
   AND r.withdrawn_at IS NULL;

-- Every step of every readable course, in order, with readable = the
-- step's pinned revision is eligible now. The lesson revision's title and
-- content are joined only for a readable step: an unreadable step exposes
-- only its course-authored title.
CREATE VIEW reader_course_steps WITH (security_invoker = true) AS
SELECT pc.course_id,
       pc.public_id         AS course_public_id,
       pc.revision_id       AS course_revision_id,
       m.module_key,
       m.position           AS module_position,
       m.title              AS module_title,
       s.position,
       s.title,
       s.lesson_id,
       l.public_id          AS lesson_public_id,
       s.lesson_revision_id,
       e.readable,
       lr.title             AS lesson_title,
       lr.content
  FROM reader_published_courses pc
  JOIN course_modules m ON m.course_revision_id = pc.revision_id
  JOIN course_steps s ON s.course_revision_id = pc.revision_id AND s.module_key = m.module_key
  JOIN lessons l ON l.id = s.lesson_id
  CROSS JOIN LATERAL (SELECT course_step_revision_eligible(s.lesson_id, s.lesson_revision_id) AS readable) e
  LEFT JOIN lesson_revisions lr ON e.readable AND lr.id = s.lesson_revision_id;
