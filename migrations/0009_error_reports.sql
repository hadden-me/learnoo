-- LRN-015: student error reports on published lessons and their audited
-- resolution. (Numbered 0009 on a stacked branch; renumbered at merge if
-- needed.)
--
-- Conventions of 0004_lessons.sql and later apply: guards raise SQLSTATE
-- LRN01 and are named zz_guard_* so they fire after any trigger that changes
-- NEW; every function pins search_path to pg_catalog, pg_temp and reaches
-- other tables through the schema of the table that fired it
-- (TG_TABLE_SCHEMA, EXECUTE format('%I...') with bound parameters), so a
-- temporary table of the calling role cannot shadow them. No function is
-- SECURITY DEFINER; the guards bind the application's least-privilege role,
-- not a superuser or table owner (docs/DECISIONS.md "Lesson persistence",
-- threat model).
--
-- How the rules are kept in the database:
--
--   * A report is accepted only against the lesson's current published
--     revision (the one a student can read: lesson not archived, pointer
--     naming the revision, revision not withdrawn), from an active account,
--     at the transaction time, open. The insert guard share-locks the lesson
--     row first, so the pointer cannot change until the report commits.
--   * A report's content never changes and it is never deleted. Its status
--     changes once, from open to resolved or dismissed, only together with
--     an error_report_resolutions row of the same transaction recording the
--     matching action.
--   * A resolution is accepted only for an open report (locked FOR UPDATE),
--     with a required note, from an active administrator or an active
--     reviewer of the lesson's subject, at the transaction time; a deferred
--     check requires the report to carry the matching status by commit. A
--     report has at most one resolution (unique report_id), so of a
--     concurrent resolve and dismiss exactly one applies.
--   * Resolutions are append-only: update, delete and truncate are rejected.
--
-- Reports never change lesson content: nothing here writes to lessons or
-- lesson_revisions.
--
-- Snapshots. The insert guard reads the lesson row FOR SHARE; every change
-- that makes a revision unreadable (pointer change, withdrawal, archival)
-- updates that row, so a writer at REPEATABLE READ or SERIALIZABLE whose
-- snapshot predates it fails with 40001 instead of reporting against it. At
-- READ COMMITTED the locked row is re-read after the lock is granted. The
-- resolution guard locks the report FOR UPDATE, which a concurrent
-- resolution updates; reviewer grants inserted after a snapshot are
-- invisible (fail-safe) and revocation or disablement change share-locked
-- rows (conflict).
--
-- Personal data: a report stores the reporting account and the free text
-- the student wrote. Retention and deletion are not decided
-- (docs/DECISIONS.md "Privacy, deletion and retention"); nothing here
-- deletes rows, and users with reports cannot be deleted.

CREATE TABLE error_reports (
    id          bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    lesson_id   bigint      NOT NULL REFERENCES lessons (id),
    -- The published revision the student was reading (insert guard).
    revision_id bigint      NOT NULL,
    reporter_id bigint      NOT NULL REFERENCES users (id),
    message     text        NOT NULL,
    status      text        NOT NULL DEFAULT 'open',
    -- The transaction time (insert guard).
    created_at  timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT error_reports_status CHECK (status IN ('open', 'resolved', 'dismissed')),
    CONSTRAINT error_reports_message_not_blank CHECK (message ~ '[^[:space:]]'),
    CONSTRAINT error_reports_message_length CHECK (char_length(message) <= 2000),
    -- Line feeds are the only control character; bidirectional formatting
    -- characters and Unicode line/paragraph separators are refused, as for
    -- display names (internal/reports.NormalizeText).
    CONSTRAINT error_reports_message_characters CHECK (
        message !~ '[\x01-\x09\x0B-\x1F\x7F-\x9F؜‎‏‪-‮⁦-⁩  ]'),
    CONSTRAINT error_reports_revision_fkey FOREIGN KEY (lesson_id, revision_id)
        REFERENCES lesson_revisions (lesson_id, id)
);

CREATE INDEX error_reports_lesson_idx ON error_reports (lesson_id, id);
CREATE INDEX error_reports_reporter_idx ON error_reports (reporter_id);
CREATE INDEX error_reports_open_idx ON error_reports (created_at) WHERE status = 'open';

CREATE TABLE error_report_resolutions (
    id          bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    report_id   bigint      NOT NULL REFERENCES error_reports (id),
    actor_id    bigint      NOT NULL REFERENCES users (id),
    action      text        NOT NULL,
    note        text        NOT NULL,
    -- The transaction time and the top-level transaction (insert guard);
    -- the status guard on error_reports requires a row of its own
    -- transaction.
    occurred_at timestamptz NOT NULL DEFAULT now(),
    xact_id     xid8        NOT NULL DEFAULT pg_current_xact_id(),
    CONSTRAINT error_report_resolutions_action CHECK (action IN ('resolve', 'dismiss')),
    CONSTRAINT error_report_resolutions_note_not_blank CHECK (note ~ '[^[:space:]]'),
    CONSTRAINT error_report_resolutions_note_length CHECK (char_length(note) <= 2000),
    CONSTRAINT error_report_resolutions_note_characters CHECK (
        note !~ '[\x01-\x09\x0B-\x1F\x7F-\x9F؜‎‏‪-‮⁦-⁩  ]'),
    -- A report is resolved or dismissed once, for good.
    CONSTRAINT error_report_resolutions_report_key UNIQUE (report_id)
);

CREATE INDEX error_report_resolutions_actor_idx ON error_report_resolutions (actor_id);

-- Insert: an open report of an active account, at the transaction time,
-- against the current published revision of a readable lesson. Lock order:
-- the lesson row (FOR SHARE), then the reporter's account row (FOR SHARE).
-- Update: only status, once, from open, with a resolution of this
-- transaction recording the matching action. Delete and truncate: never.
CREATE FUNCTION error_reports_guard() RETURNS trigger
LANGUAGE plpgsql SET search_path = pg_catalog, pg_temp AS $$
DECLARE
    found_lesson boolean;
    current_ptr  bigint;
    archived     boolean;
    withdrawn    boolean;
    active       boolean;
    act          text;
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
        IF archived OR current_ptr IS DISTINCT FROM NEW.revision_id THEN
            RAISE EXCEPTION 'an error report must name the current published revision of lesson %', NEW.lesson_id
                USING ERRCODE = 'LRN01';
        END IF;
        -- 0007 already keeps withdrawn revisions out of the pointer; repeated
        -- here as defence in depth, like reader_published_lessons (0008).
        EXECUTE format('SELECT EXISTS (SELECT 1 FROM %I.revision_withdrawals WHERE revision_id = $1)', TG_TABLE_SCHEMA)
           INTO withdrawn USING NEW.revision_id;
        IF withdrawn THEN
            RAISE EXCEPTION 'revision % is withdrawn and cannot be reported', NEW.revision_id USING ERRCODE = 'LRN01';
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

CREATE TRIGGER zz_guard_error_reports
    BEFORE INSERT OR UPDATE OR DELETE ON error_reports
    FOR EACH ROW EXECUTE FUNCTION error_reports_guard();

CREATE TRIGGER zz_guard_error_reports_no_truncate
    BEFORE TRUNCATE ON error_reports
    FOR EACH STATEMENT EXECUTE FUNCTION error_reports_guard();

-- A resolution is accepted only for an open report, locked FOR UPDATE (the
-- same transaction then updates its status, so a share lock would have to
-- be upgraded and two concurrent resolvers would deadlock), from an active
-- administrator or an active reviewer of the report's lesson subject.
CREATE FUNCTION error_report_resolutions_guard_insert() RETURNS trigger
LANGUAGE plpgsql SET search_path = pg_catalog, pg_temp AS $$
DECLARE
    lesson  bigint;
    st      text;
    subject bigint;
    allowed boolean;
BEGIN
    IF NEW.occurred_at IS DISTINCT FROM now() OR NEW.xact_id IS DISTINCT FROM pg_current_xact_id() THEN
        RAISE EXCEPTION 'a resolution records its own transaction and its time' USING ERRCODE = 'LRN01';
    END IF;
    EXECUTE format('SELECT lesson_id, status FROM %I.error_reports WHERE id = $1 FOR UPDATE', TG_TABLE_SCHEMA)
       INTO lesson, st USING NEW.report_id;
    IF st IS NULL THEN
        RAISE EXCEPTION 'error report % does not exist', NEW.report_id USING ERRCODE = 'LRN01';
    END IF;
    IF st <> 'open' THEN
        RAISE EXCEPTION 'error report % is not open', NEW.report_id USING ERRCODE = 'LRN01';
    END IF;
    -- A lesson's subject never changes (0004).
    EXECUTE format('SELECT subject_id FROM %I.lessons WHERE id = $1', TG_TABLE_SCHEMA)
       INTO subject USING lesson;
    EXECUTE format('SELECT EXISTS (
                        SELECT 1 FROM %1$I.user_permissions p JOIN %1$I.users u ON u.id = p.user_id
                         WHERE p.user_id = $1 AND p.permission = ''administrator'' AND u.disabled_at IS NULL
                           FOR SHARE OF p, u)
                    OR EXISTS (
                        SELECT 1 FROM %1$I.reviewer_assignments r JOIN %1$I.users u ON u.id = r.user_id
                         WHERE r.user_id = $1 AND r.subject_id = $2 AND u.disabled_at IS NULL
                           FOR SHARE OF r, u)', TG_TABLE_SCHEMA)
       INTO allowed USING NEW.actor_id, subject;
    IF NOT allowed THEN
        RAISE EXCEPTION 'user % is neither an active administrator nor an active reviewer of subject %', NEW.actor_id, subject
            USING ERRCODE = 'LRN01';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER zz_guard_error_report_resolutions_insert
    BEFORE INSERT ON error_report_resolutions
    FOR EACH ROW EXECUTE FUNCTION error_report_resolutions_guard_insert();

-- At commit, the resolved report must carry the resolution's status.
CREATE FUNCTION error_report_resolutions_check_applied() RETURNS trigger
LANGUAGE plpgsql SET search_path = pg_catalog, pg_temp AS $$
DECLARE
    st text;
BEGIN
    EXECUTE format('SELECT status FROM %I.error_reports WHERE id = $1', TG_TABLE_SCHEMA)
       INTO st USING NEW.report_id;
    IF st IS DISTINCT FROM (CASE NEW.action WHEN 'resolve' THEN 'resolved' ELSE 'dismissed' END) THEN
        RAISE EXCEPTION 'resolution of error report % was not applied to its status', NEW.report_id USING ERRCODE = 'LRN01';
    END IF;
    RETURN NULL;
END;
$$;

CREATE CONSTRAINT TRIGGER zz_guard_error_report_resolutions_applied
    AFTER INSERT ON error_report_resolutions
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION error_report_resolutions_check_applied();

CREATE TRIGGER zz_guard_error_report_resolutions_append_only
    BEFORE UPDATE OR DELETE ON error_report_resolutions
    FOR EACH ROW EXECUTE FUNCTION review_audit_append_only();

CREATE TRIGGER zz_guard_error_report_resolutions_no_truncate
    BEFORE TRUNCATE ON error_report_resolutions
    FOR EACH STATEMENT EXECUTE FUNCTION review_audit_append_only();
