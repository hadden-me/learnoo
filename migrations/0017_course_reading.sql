-- LMS-B08 (issue #98): student home preference, course bookmarks and
-- course-context error reports (docs/LMS_CONTRACT.md §9, docs/LMS_PRODUCT_SPEC.md
-- §5 and §7). 0016 is reserved by the administration slice; the runner
-- applies migrations in lexical order and accepts the gap.
--
-- Conventions of 0004 and later apply: guards raise SQLSTATE LRN01 and are
-- named zz_guard_*; trigger functions pin search_path to pg_catalog, pg_temp
-- and reach other tables only through the schema of the table that fired
-- them (TG_TABLE_SCHEMA, EXECUTE format('%I...') with bound parameters), so
-- a temporary table of the calling role cannot shadow them. No function is
-- SECURITY DEFINER; the guards bind the application's least-privilege role.
--
-- Nothing here changes an existing row: the new table and columns start
-- empty or NULL, and every existing ID, bookmark, report and audit row is
-- kept.
--
-- Personal data: the preference stores the account and a curriculum level
-- and optional section (no school, phone or address); course bookmarks
-- store the account and the course. Both are deleted with their account,
-- like lesson bookmarks (0008). Retention and deletion are not otherwise
-- decided (docs/DECISIONS.md "Privacy, deletion and retention").

-- ---- course bookmarks ----

-- Keyed by the course, which the reader addresses by its random public_id,
-- never by a revision. A bookmark survives new published revisions,
-- suspension and archive; the student home shows it as unavailable while
-- the course is not readable (reader_published_courses, 0015).
CREATE TABLE course_bookmarks (
    user_id    bigint      NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    course_id  bigint      NOT NULL REFERENCES courses (id),
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, course_id)
);

CREATE INDEX course_bookmarks_course_idx ON course_bookmarks (course_id);

-- ---- student preference ----

-- The optional level and section a student browses by default on /learn.
-- A section, when set, belongs to the level (composite foreign key). It is
-- skippable: no row means "browse everything".
ALTER TABLE academic_sections ADD CONSTRAINT academic_sections_id_level_key UNIQUE (id, level_id);

CREATE TABLE student_preferences (
    user_id    bigint      PRIMARY KEY REFERENCES users (id) ON DELETE CASCADE,
    level_id   bigint      NOT NULL REFERENCES education_levels (id),
    section_id bigint,
    updated_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT student_preferences_section_fkey FOREIGN KEY (section_id, level_id)
        REFERENCES academic_sections (id, level_id)
);

CREATE INDEX student_preferences_level_idx ON student_preferences (level_id);
CREATE INDEX student_preferences_section_idx ON student_preferences (section_id) WHERE section_id IS NOT NULL;

-- ---- course-context error reports ----

-- A report filed while reading a course step names the published course
-- revision it was displayed in. The composite foreign key keeps the report's
-- lesson revision equal to that step's pinned revision; course steps of a
-- submitted revision never change (0014), so the key never blocks a
-- legitimate write.
ALTER TABLE course_steps
    ADD CONSTRAINT course_steps_revision_lesson_pin_key UNIQUE (course_revision_id, lesson_id, lesson_revision_id);

ALTER TABLE error_reports
    ADD COLUMN course_revision_id bigint,
    ADD CONSTRAINT error_reports_course_step_fkey FOREIGN KEY (course_revision_id, lesson_id, revision_id)
        REFERENCES course_steps (course_revision_id, lesson_id, lesson_revision_id);

CREATE INDEX error_reports_course_revision_idx ON error_reports (course_revision_id)
    WHERE course_revision_id IS NOT NULL;

-- Replaces the 0009 guard (its triggers stay). Insert: an open report of an
-- active account, at the transaction time, and either
--
--   * without course_revision_id, against the lesson's current published
--     revision of a readable lesson (unchanged from 0009); or
--   * with course_revision_id, against the pinned revision of a readable
--     step of that course revision, which must be the course's readable
--     published revision: course pointer equal to it, course not archived,
--     revision approved and not withdrawn, and the step's pinned revision
--     eligible now (course_step_revision_eligible, 0014: approved, not
--     withdrawn, published once, lesson published and not archived).
--
-- Lock order (contract §2): the lesson row (FOR SHARE), then the course row
-- (FOR SHARE), then the reporter's account row (FOR SHARE). Every change
-- that makes a step unreadable updates one of the first two rows (lesson
-- withdrawal count, pointer or archive; course pointer or archive), so the
-- check cannot race it. Like every guard that evaluates step eligibility
-- (0015), the course branch refuses to run outside READ COMMITTED.
-- Update and delete: unchanged from 0009.
CREATE OR REPLACE FUNCTION error_reports_guard() RETURNS trigger
LANGUAGE plpgsql SET search_path = pg_catalog, pg_temp AS $$
DECLARE
    found_lesson boolean;
    current_ptr  bigint;
    archived     boolean;
    withdrawn    boolean;
    active       boolean;
    act          text;
    course       bigint;
    course_ptr   bigint;
    readable     boolean;
BEGIN
    IF TG_OP = 'INSERT' THEN
        IF NEW.created_at IS DISTINCT FROM now() THEN
            RAISE EXCEPTION 'an error report records the transaction time' USING ERRCODE = 'LRN01';
        END IF;
        IF NEW.status IS DISTINCT FROM 'open' THEN
            RAISE EXCEPTION 'an error report is created open' USING ERRCODE = 'LRN01';
        END IF;
        EXECUTE format('SELECT true, published_revision_id, archived_at IS NOT NULL
                          FROM %I.lessons WHERE id = $1 FOR SHARE', TG_TABLE_SCHEMA)
           INTO found_lesson, current_ptr, archived USING NEW.lesson_id;
        IF found_lesson IS NULL THEN
            RAISE EXCEPTION 'lesson % does not exist', NEW.lesson_id USING ERRCODE = 'LRN01';
        END IF;
        IF NEW.course_revision_id IS NULL THEN
            IF archived OR current_ptr IS DISTINCT FROM NEW.revision_id THEN
                RAISE EXCEPTION 'an error report must name the current published revision of lesson %', NEW.lesson_id
                    USING ERRCODE = 'LRN01';
            END IF;
            -- 0007 already keeps withdrawn revisions out of the pointer;
            -- repeated here as defence in depth, like
            -- reader_published_lessons (0008).
            EXECUTE format('SELECT EXISTS (SELECT 1 FROM %I.revision_withdrawals WHERE revision_id = $1)', TG_TABLE_SCHEMA)
               INTO withdrawn USING NEW.revision_id;
            IF withdrawn THEN
                RAISE EXCEPTION 'revision % is withdrawn and cannot be reported', NEW.revision_id USING ERRCODE = 'LRN01';
            END IF;
        ELSE
            IF pg_catalog.current_setting('transaction_isolation') <> 'read committed' THEN
                RAISE EXCEPTION 'a course-context error report is filed only at READ COMMITTED' USING ERRCODE = 'LRN01';
            END IF;
            -- A revision's course never changes (0014).
            EXECUTE format('SELECT course_id FROM %I.course_revisions WHERE id = $1', TG_TABLE_SCHEMA)
               INTO course USING NEW.course_revision_id;
            IF course IS NOT NULL THEN
                EXECUTE format('SELECT published_revision_id FROM %I.courses
                                 WHERE id = $1 AND archived_at IS NULL FOR SHARE', TG_TABLE_SCHEMA)
                   INTO course_ptr USING course;
            END IF;
            EXECUTE format('SELECT EXISTS (
                                SELECT 1
                                  FROM %1$I.course_revisions r
                                  JOIN %1$I.course_steps s ON s.course_revision_id = r.id
                                 WHERE r.id = $1 AND r.course_id = $2
                                   AND r.status = ''approved'' AND r.withdrawn_at IS NULL
                                   AND s.lesson_id = $3 AND s.lesson_revision_id = $4
                                   AND %1$I.course_step_revision_eligible(s.lesson_id, s.lesson_revision_id))', TG_TABLE_SCHEMA)
               INTO readable USING NEW.course_revision_id, course, NEW.lesson_id, NEW.revision_id;
            IF course_ptr IS NULL OR course_ptr IS DISTINCT FROM NEW.course_revision_id OR NOT readable THEN
                RAISE EXCEPTION 'a course-context error report must name the pinned revision of a readable step of the published revision of its course (lesson %)', NEW.lesson_id
                    USING ERRCODE = 'LRN01';
            END IF;
        END IF;
        EXECUTE format('SELECT EXISTS (SELECT 1 FROM %I.users WHERE id = $1 AND disabled_at IS NULL FOR SHARE)', TG_TABLE_SCHEMA)
           INTO active USING NEW.reporter_id;
        IF NOT active THEN
            RAISE EXCEPTION 'user % is not an active account', NEW.reporter_id USING ERRCODE = 'LRN01';
        END IF;
        RETURN NEW;
    END IF;
    IF TG_OP <> 'UPDATE' THEN
        RAISE EXCEPTION 'error reports are never deleted' USING ERRCODE = 'LRN01';
    END IF;
    IF to_jsonb(NEW) - 'status' IS DISTINCT FROM to_jsonb(OLD) - 'status' THEN
        RAISE EXCEPTION 'error report % is immutable; only its status changes, by resolution', OLD.id USING ERRCODE = 'LRN01';
    END IF;
    IF NEW.status IS NOT DISTINCT FROM OLD.status THEN
        RETURN NEW;
    END IF;
    IF OLD.status <> 'open' THEN
        RAISE EXCEPTION 'error report % was already closed', OLD.id USING ERRCODE = 'LRN01';
    END IF;
    EXECUTE format('SELECT action FROM %I.error_report_resolutions
                     WHERE report_id = $1 AND xact_id = pg_catalog.pg_current_xact_id()', TG_TABLE_SCHEMA)
       INTO act USING OLD.id;
    IF act IS NULL
       OR NEW.status IS DISTINCT FROM (CASE act WHEN 'resolve' THEN 'resolved' WHEN 'dismiss' THEN 'dismissed' END) THEN
        RAISE EXCEPTION 'error report % changes status only with a matching resolution of the same transaction', OLD.id
            USING ERRCODE = 'LRN01';
    END IF;
    RETURN NEW;
END;
$$;
