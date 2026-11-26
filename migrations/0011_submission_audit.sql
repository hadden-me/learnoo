-- Submission audit: who submitted which revision for review, and when
-- (CONTENT_AND_EDITORIAL.md: "Record actor, revision, action, and timestamp
-- for submission, decisions, publication, ..."). Decisions (0006),
-- publication, unpublication, restoration and withdrawal (0006, 0007) were
-- already audited; until now a submission was recorded only by
-- lesson_revisions.submitted_at and author_id.
--
-- Conventions of 0004_lessons.sql and later apply: guards raise SQLSTATE
-- LRN01 and are named zz_guard_* so they fire after any trigger that changes
-- NEW; every function pins search_path to pg_catalog, pg_temp and reaches
-- other tables only through the schema of the table that fired it
-- (TG_TABLE_SCHEMA, EXECUTE format('%I...') with bound parameters), so a
-- temporary table of the calling role cannot shadow them. No function is
-- SECURITY DEFINER; the guards bind the application's least-privilege role,
-- not a superuser or table owner (docs/DECISIONS.md "Lesson persistence",
-- threat model). Lock order stays: lesson row first, then its revision rows.
--
-- How the two sides are kept consistent (as decisions and their status in
-- 0006):
--
--   * A revision leaves draft (for in_review, the only transition the 0004
--     guard allows) only together with a submission_audit row for it written
--     by the same top-level transaction (xact_id), and its submitted_at must
--     be the transaction time, which is also the audit row's submitted_at.
--   * An audit row is accepted only for a draft (locked FOR UPDATE after its
--     lesson, FOR SHARE) of a lesson that is not archived, with actor_id
--     equal to the revision's author, who must be active and hold the
--     author permission (grant and account rows share-locked), at the
--     transaction time, in its own transaction; a deferred check requires the
--     revision to have left draft by commit. One row per revision (unique
--     revision_id), and a revision leaves draft for good, so of two
--     concurrent submissions of one draft exactly one applies.
--   * The table is append-only: update, delete and truncate are refused.
--
-- Snapshots. A writer at REPEATABLE READ or SERIALIZABLE whose snapshot
-- predates a change that would deny the submission cannot submit: a
-- concurrent submission updates the revision row this guard locks FOR
-- UPDATE, archival updates the lesson row it share-locks, revocation deletes
-- and disablement updates the grant and account rows it share-locks (all
-- 40001); a grant committed after the snapshot is invisible, which denies.
--
-- Backfill. Revisions submitted before this migration have no row. Each is
-- given one here, before the guards exist: actor_id = author_id (the only
-- user who could submit, both in the store and, since 0004, for any writer
-- the ownership guard binds), submitted_at = the revision's recorded
-- submitted_at, xact_id = this migration's transaction and backfilled =
-- true. Backfilled rows are reconstructed, not observed at submission; the
-- insert guard refuses backfilled = true, so only this migration (or a
-- restore, which loads data before creating triggers) can hold them.

CREATE TABLE submission_audit (
    id           bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    lesson_id    bigint      NOT NULL REFERENCES lessons (id),
    revision_id  bigint      NOT NULL,
    -- The submitting user: always the revision's author (insert guard).
    actor_id     bigint      NOT NULL REFERENCES users (id),
    -- The transaction time and the top-level transaction that wrote the row
    -- (insert guard), except for backfilled rows (see above).
    submitted_at timestamptz NOT NULL DEFAULT now(),
    xact_id      xid8        NOT NULL DEFAULT pg_current_xact_id(),
    -- True only for rows reconstructed by this migration.
    backfilled   boolean     NOT NULL DEFAULT false,
    -- A revision is submitted at most once (it never returns to draft).
    CONSTRAINT submission_audit_revision_key UNIQUE (revision_id),
    CONSTRAINT submission_audit_revision_fkey FOREIGN KEY (lesson_id, revision_id)
        REFERENCES lesson_revisions (lesson_id, id)
);

CREATE INDEX submission_audit_lesson_idx ON submission_audit (lesson_id, id);
CREATE INDEX submission_audit_actor_idx ON submission_audit (actor_id);

-- The backfill runs before any trigger exists on this table.
INSERT INTO submission_audit (lesson_id, revision_id, actor_id, submitted_at, xact_id, backfilled)
SELECT lesson_id, id, author_id, submitted_at, pg_current_xact_id(), true
  FROM lesson_revisions
 WHERE status <> 'draft'
 ORDER BY submitted_at, id;

-- Lock order: the lesson (FOR SHARE, so it cannot be archived meanwhile),
-- then the revision FOR UPDATE, because the same transaction then updates
-- its status (as Submit already does, and as the decision guard of 0006).
CREATE FUNCTION submission_audit_guard_insert() RETURNS trigger
LANGUAGE plpgsql SET search_path = pg_catalog, pg_temp AS $$
DECLARE
    lesson   bigint;
    archived boolean;
    author   bigint;
    st       text;
    allowed  boolean;
BEGIN
    IF NEW.backfilled THEN
        RAISE EXCEPTION 'submission audit rows are backfilled only by their migration' USING ERRCODE = 'LRN01';
    END IF;
    IF NEW.submitted_at IS DISTINCT FROM now() OR NEW.xact_id IS DISTINCT FROM pg_current_xact_id() THEN
        RAISE EXCEPTION 'a submission audit row records its own transaction and its time' USING ERRCODE = 'LRN01';
    END IF;
    -- EXECUTE does not set FOUND; a missing row leaves the targets NULL.
    EXECUTE format('SELECT lesson_id FROM %I.lesson_revisions WHERE id = $1', TG_TABLE_SCHEMA)
       INTO lesson USING NEW.revision_id;
    IF lesson IS NULL OR lesson IS DISTINCT FROM NEW.lesson_id THEN
        RAISE EXCEPTION 'revision % is not a revision of lesson %', NEW.revision_id, NEW.lesson_id USING ERRCODE = 'LRN01';
    END IF;
    EXECUTE format('SELECT archived_at IS NOT NULL FROM %I.lessons WHERE id = $1 FOR SHARE', TG_TABLE_SCHEMA)
       INTO archived USING lesson;
    IF archived IS DISTINCT FROM false THEN
        RAISE EXCEPTION 'lesson % is archived', lesson USING ERRCODE = 'LRN01';
    END IF;
    EXECUTE format('SELECT author_id, status FROM %I.lesson_revisions WHERE id = $1 FOR UPDATE', TG_TABLE_SCHEMA)
       INTO author, st USING NEW.revision_id;
    IF st IS DISTINCT FROM 'draft' THEN
        RAISE EXCEPTION 'revision % is not a draft', NEW.revision_id USING ERRCODE = 'LRN01';
    END IF;
    IF NEW.actor_id IS DISTINCT FROM author THEN
        RAISE EXCEPTION 'only the author of revision % can submit it', NEW.revision_id USING ERRCODE = 'LRN01';
    END IF;
    EXECUTE format('SELECT EXISTS (
                        SELECT 1 FROM %1$I.user_permissions p JOIN %1$I.users u ON u.id = p.user_id
                         WHERE p.user_id = $1 AND p.permission = ''author'' AND u.disabled_at IS NULL
                           FOR SHARE OF p, u)', TG_TABLE_SCHEMA)
       INTO allowed USING NEW.actor_id;
    IF NOT allowed THEN
        RAISE EXCEPTION 'user % is not an active author', NEW.actor_id USING ERRCODE = 'LRN01';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER zz_guard_submission_audit_insert
    BEFORE INSERT ON submission_audit
    FOR EACH ROW EXECUTE FUNCTION submission_audit_guard_insert();

-- At commit, the audited revision must have left draft, at the audited time.
CREATE FUNCTION submission_audit_check_applied() RETURNS trigger
LANGUAGE plpgsql SET search_path = pg_catalog, pg_temp AS $$
DECLARE
    st        text;
    submitted timestamptz;
BEGIN
    EXECUTE format('SELECT status, submitted_at FROM %I.lesson_revisions WHERE id = $1', TG_TABLE_SCHEMA)
       INTO st, submitted USING NEW.revision_id;
    IF st IS NULL OR st = 'draft' OR submitted IS DISTINCT FROM NEW.submitted_at THEN
        RAISE EXCEPTION 'submission of revision % was audited but not applied', NEW.revision_id
            USING ERRCODE = 'LRN01';
    END IF;
    RETURN NULL;
END;
$$;

CREATE CONSTRAINT TRIGGER zz_guard_submission_audit_applied
    AFTER INSERT ON submission_audit
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION submission_audit_check_applied();

-- A revision leaves draft only with a submission audit row of this
-- transaction, at the transaction time. The audit row can exist only while
-- the revision is a draft and must be applied by its own transaction
-- (deferred check above); matching xact_id rules out a row of an earlier
-- transaction in any case.
CREATE FUNCTION lesson_revisions_guard_submission() RETURNS trigger
LANGUAGE plpgsql SET search_path = pg_catalog, pg_temp AS $$
DECLARE
    audited boolean;
BEGIN
    IF OLD.status <> 'draft' OR NEW.status IS NOT DISTINCT FROM OLD.status THEN
        RETURN NEW;
    END IF;
    IF NEW.submitted_at IS DISTINCT FROM now() THEN
        RAISE EXCEPTION 'revision % submitted_at must be the transaction time', OLD.id USING ERRCODE = 'LRN01';
    END IF;
    EXECUTE format('SELECT EXISTS (
                        SELECT 1 FROM %I.submission_audit
                         WHERE revision_id = $1 AND lesson_id = $2 AND actor_id = $3 AND NOT backfilled
                           AND submitted_at = $4 AND xact_id = pg_catalog.pg_current_xact_id())', TG_TABLE_SCHEMA)
       INTO audited USING OLD.id, OLD.lesson_id, OLD.author_id, NEW.submitted_at;
    IF NOT audited THEN
        RAISE EXCEPTION 'revision % leaves draft only with a submission audit row in the same transaction', OLD.id
            USING ERRCODE = 'LRN01';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER zz_guard_lesson_revisions_submission_audited
    BEFORE UPDATE ON lesson_revisions
    FOR EACH ROW EXECUTE FUNCTION lesson_revisions_guard_submission();

-- Append-only, with 0006's function (it names the table in its message).
CREATE TRIGGER zz_guard_submission_audit_append_only
    BEFORE UPDATE OR DELETE ON submission_audit
    FOR EACH ROW EXECUTE FUNCTION review_audit_append_only();

CREATE TRIGGER zz_guard_submission_audit_no_truncate
    BEFORE TRUNCATE ON submission_audit
    FOR EACH STATEMENT EXECUTE FUNCTION review_audit_append_only();
