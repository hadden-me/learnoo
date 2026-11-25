-- LRN-007: review decisions and publication audit.
--
-- Conventions of 0004_lessons.sql apply: guards raise SQLSTATE LRN01 and are
-- named zz_guard_* so they fire after any trigger that changes NEW; every
-- function pins search_path to pg_catalog, pg_temp and reaches other tables
-- through the schema of the table that fired it (TG_TABLE_SCHEMA, EXECUTE
-- format('%I...') with bound parameters), so a temporary table of the
-- calling role cannot shadow them. Lock order stays: lesson row first, then
-- its revision rows. No function here is SECURITY DEFINER; the guards bind
-- the application's least-privilege role, not a superuser or table owner
-- (docs/DECISIONS.md "Lesson persistence", threat model).
--
-- How the two sides are kept consistent:
--
--   * A revision leaves in_review only together with its decision: the
--     status change requires a review_decisions row for the revision with the
--     matching action, and a deferred check requires every decision to have
--     moved its revision to the matching status by commit. A revision has at
--     most one decision (in_review is left for good), so concurrent decisions
--     cannot both apply.
--   * A decision is accepted only for a revision in review, from a reviewer
--     assigned to the revision's subject, whose account is active and who is
--     not the revision's author (administrators included).
--   * A publication audit row is accepted only for an approved revision of
--     the lesson, from an active publisher, naming the lesson's current
--     pointer as the previous revision; a deferred check requires the pointer
--     to name the audited revision by commit. Setting the pointer to a
--     revision requires an audit row written by the same transaction.
--     Clearing the pointer (unpublication) is LRN-009 and is not guarded here.
--
-- Both tables are append-only: update, delete and truncate are rejected.

CREATE TABLE review_decisions (
    id          bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    revision_id bigint      NOT NULL REFERENCES lesson_revisions (id),
    reviewer_id bigint      NOT NULL REFERENCES users (id),
    action      text        NOT NULL,
    -- NULL when an approval has no comment; never blank.
    comment     text,
    -- The transaction time (insert guard).
    decided_at  timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT review_decisions_action CHECK (action IN ('approve', 'request_changes')),
    CONSTRAINT review_decisions_comment_not_blank CHECK (comment IS NULL OR comment ~ '[^[:space:]]'),
    CONSTRAINT review_decisions_comment_required CHECK (action <> 'request_changes' OR comment IS NOT NULL),
    CONSTRAINT review_decisions_comment_length CHECK (char_length(comment) <= 5000),
    -- Deciding moves the revision out of in_review for good, so a revision
    -- has at most one decision.
    CONSTRAINT review_decisions_revision_key UNIQUE (revision_id)
);

CREATE INDEX review_decisions_reviewer_idx ON review_decisions (reviewer_id);

-- Lock order: the lesson (FOR SHARE, so it cannot be published meanwhile),
-- then the revision FOR UPDATE, because the same transaction then updates
-- its status; a share lock here would have to be upgraded and two
-- concurrent deciders would deadlock.
CREATE FUNCTION review_decisions_guard_insert() RETURNS trigger
LANGUAGE plpgsql SET search_path = pg_catalog, pg_temp AS $$
DECLARE
    lesson  bigint;
    author  bigint;
    subject bigint;
    st      text;
    allowed boolean;
BEGIN
    IF NEW.decided_at IS DISTINCT FROM now() THEN
        RAISE EXCEPTION 'decided_at must be the transaction time' USING ERRCODE = 'LRN01';
    END IF;
    -- EXECUTE does not set FOUND; a missing row leaves the targets NULL.
    EXECUTE format('SELECT lesson_id FROM %I.lesson_revisions WHERE id = $1', TG_TABLE_SCHEMA)
       INTO lesson USING NEW.revision_id;
    IF lesson IS NULL THEN
        RAISE EXCEPTION 'revision % does not exist', NEW.revision_id USING ERRCODE = 'LRN01';
    END IF;
    EXECUTE format('SELECT 1 FROM %I.lessons WHERE id = $1 FOR SHARE', TG_TABLE_SCHEMA) USING lesson;
    EXECUTE format('SELECT author_id, subject_id, status FROM %I.lesson_revisions WHERE id = $1 FOR UPDATE', TG_TABLE_SCHEMA)
       INTO author, subject, st USING NEW.revision_id;
    IF st IS DISTINCT FROM 'in_review' THEN
        RAISE EXCEPTION 'revision % is not in review', NEW.revision_id USING ERRCODE = 'LRN01';
    END IF;
    IF NEW.reviewer_id = author THEN
        RAISE EXCEPTION 'a reviewer cannot decide on their own revision' USING ERRCODE = 'LRN01';
    END IF;
    EXECUTE format('SELECT EXISTS (
                        SELECT 1 FROM %1$I.reviewer_assignments r JOIN %1$I.users u ON u.id = r.user_id
                         WHERE r.user_id = $1 AND r.subject_id = $2 AND u.disabled_at IS NULL
                           FOR SHARE OF r, u)', TG_TABLE_SCHEMA)
       INTO allowed USING NEW.reviewer_id, subject;
    IF NOT allowed THEN
        RAISE EXCEPTION 'user % is not an active reviewer of subject %', NEW.reviewer_id, subject USING ERRCODE = 'LRN01';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER zz_guard_review_decisions_insert
    BEFORE INSERT ON review_decisions
    FOR EACH ROW EXECUTE FUNCTION review_decisions_guard_insert();

-- At commit, the decided revision must carry the decision's status.
CREATE FUNCTION review_decisions_check_applied() RETURNS trigger
LANGUAGE plpgsql SET search_path = pg_catalog, pg_temp AS $$
DECLARE
    st       text;
    expected text;
BEGIN
    expected := CASE NEW.action WHEN 'approve' THEN 'approved' ELSE 'changes_requested' END;
    EXECUTE format('SELECT status FROM %I.lesson_revisions WHERE id = $1', TG_TABLE_SCHEMA)
       INTO st USING NEW.revision_id;
    IF st IS DISTINCT FROM expected THEN
        RAISE EXCEPTION 'review decision on revision % was not applied to its status', NEW.revision_id
            USING ERRCODE = 'LRN01';
    END IF;
    RETURN NULL;
END;
$$;

CREATE CONSTRAINT TRIGGER zz_guard_review_decisions_applied
    AFTER INSERT ON review_decisions
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION review_decisions_check_applied();

-- A revision leaves in_review only through its recorded decision. The
-- decision can exist only while the revision is in review and must be
-- applied by its own transaction (deferred check above), so a decision found
-- here for a revision still in review was written by this transaction.
CREATE FUNCTION lesson_revisions_guard_decision() RETURNS trigger
LANGUAGE plpgsql SET search_path = pg_catalog, pg_temp AS $$
DECLARE
    expected text;
    act      text;
BEGIN
    IF OLD.status <> 'in_review' OR NEW.status IS NOT DISTINCT FROM OLD.status THEN
        RETURN NEW;
    END IF;
    expected := CASE NEW.status WHEN 'approved' THEN 'approve' WHEN 'changes_requested' THEN 'request_changes' END;
    EXECUTE format('SELECT action FROM %I.review_decisions WHERE revision_id = $1', TG_TABLE_SCHEMA)
       INTO act USING OLD.id;
    IF expected IS NULL OR act IS DISTINCT FROM expected THEN
        RAISE EXCEPTION 'revision % leaves review only with a recorded matching review decision', OLD.id
            USING ERRCODE = 'LRN01';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER zz_guard_lesson_revisions_decided
    BEFORE UPDATE ON lesson_revisions
    FOR EACH ROW EXECUTE FUNCTION lesson_revisions_guard_decision();

CREATE TABLE publication_audit (
    id                   bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    lesson_id            bigint      NOT NULL REFERENCES lessons (id),
    revision_id          bigint      NOT NULL,
    -- The pointer this publication replaced (NULL for a first publication).
    previous_revision_id bigint,
    actor_id             bigint      NOT NULL REFERENCES users (id),
    -- LRN-009 adds unpublication and restoration.
    action               text        NOT NULL,
    -- The transaction time and the top-level transaction that wrote the row
    -- (insert guard). The pointer guard uses xact_id to require an audit row
    -- of its own transaction; transaction IDs of type xid8 never wrap around.
    occurred_at          timestamptz NOT NULL DEFAULT now(),
    xact_id              xid8        NOT NULL DEFAULT pg_current_xact_id(),
    CONSTRAINT publication_audit_action CHECK (action IN ('publish')),
    CONSTRAINT publication_audit_changes_pointer CHECK (previous_revision_id IS DISTINCT FROM revision_id),
    CONSTRAINT publication_audit_revision_fkey FOREIGN KEY (lesson_id, revision_id)
        REFERENCES lesson_revisions (lesson_id, id),
    CONSTRAINT publication_audit_previous_fkey FOREIGN KEY (lesson_id, previous_revision_id)
        REFERENCES lesson_revisions (lesson_id, id)
);

CREATE INDEX publication_audit_lesson_idx ON publication_audit (lesson_id, id);
CREATE INDEX publication_audit_actor_idx ON publication_audit (actor_id);

-- Locks the lesson FOR UPDATE first (the same row the pointer update then
-- changes), then share-locks the revision, as the pointer triggers do.
CREATE FUNCTION publication_audit_guard_insert() RETURNS trigger
LANGUAGE plpgsql SET search_path = pg_catalog, pg_temp AS $$
DECLARE
    found_lesson boolean;
    current_ptr  bigint;
    st           text;
    allowed      boolean;
BEGIN
    IF NEW.occurred_at IS DISTINCT FROM now() OR NEW.xact_id IS DISTINCT FROM pg_current_xact_id() THEN
        RAISE EXCEPTION 'a publication audit row records its own transaction and its time' USING ERRCODE = 'LRN01';
    END IF;
    EXECUTE format('SELECT true, published_revision_id FROM %I.lessons WHERE id = $1 FOR UPDATE', TG_TABLE_SCHEMA)
       INTO found_lesson, current_ptr USING NEW.lesson_id;
    IF found_lesson IS NULL THEN
        RAISE EXCEPTION 'lesson % does not exist', NEW.lesson_id USING ERRCODE = 'LRN01';
    END IF;
    IF NEW.previous_revision_id IS DISTINCT FROM current_ptr THEN
        RAISE EXCEPTION 'previous_revision_id must be the lesson''s current published revision' USING ERRCODE = 'LRN01';
    END IF;
    EXECUTE format('SELECT status FROM %I.lesson_revisions WHERE id = $1 AND lesson_id = $2 FOR SHARE', TG_TABLE_SCHEMA)
       INTO st USING NEW.revision_id, NEW.lesson_id;
    IF st IS DISTINCT FROM 'approved' THEN
        RAISE EXCEPTION 'publication audit: revision % is not an approved revision of lesson %', NEW.revision_id, NEW.lesson_id
            USING ERRCODE = 'LRN01';
    END IF;
    EXECUTE format('SELECT EXISTS (
                        SELECT 1 FROM %1$I.user_permissions p JOIN %1$I.users u ON u.id = p.user_id
                         WHERE p.user_id = $1 AND p.permission = ''publisher'' AND u.disabled_at IS NULL
                           FOR SHARE OF p, u)', TG_TABLE_SCHEMA)
       INTO allowed USING NEW.actor_id;
    IF NOT allowed THEN
        RAISE EXCEPTION 'user % is not an active publisher', NEW.actor_id USING ERRCODE = 'LRN01';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER zz_guard_publication_audit_insert
    BEFORE INSERT ON publication_audit
    FOR EACH ROW EXECUTE FUNCTION publication_audit_guard_insert();

-- At commit, the lesson must point at the audited revision.
CREATE FUNCTION publication_audit_check_applied() RETURNS trigger
LANGUAGE plpgsql SET search_path = pg_catalog, pg_temp AS $$
DECLARE
    current_ptr bigint;
BEGIN
    EXECUTE format('SELECT published_revision_id FROM %I.lessons WHERE id = $1', TG_TABLE_SCHEMA)
       INTO current_ptr USING NEW.lesson_id;
    IF current_ptr IS DISTINCT FROM NEW.revision_id THEN
        RAISE EXCEPTION 'publication of revision % was audited but the lesson does not point at it', NEW.revision_id
            USING ERRCODE = 'LRN01';
    END IF;
    RETURN NULL;
END;
$$;

CREATE CONSTRAINT TRIGGER zz_guard_publication_audit_applied
    AFTER INSERT ON publication_audit
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION publication_audit_check_applied();

-- Pointing the lesson at a revision requires an audit row of this
-- transaction recording exactly that change. Sorts after
-- zz_guard_lessons_published_approved, so an unapproved revision is still
-- reported as such.
CREATE FUNCTION lessons_guard_published_audited() RETURNS trigger
LANGUAGE plpgsql SET search_path = pg_catalog, pg_temp AS $$
DECLARE
    previous bigint;
    audited  boolean;
BEGIN
    IF NEW.published_revision_id IS NULL
       OR (TG_OP = 'UPDATE' AND NEW.published_revision_id IS NOT DISTINCT FROM OLD.published_revision_id) THEN
        RETURN NEW;
    END IF;
    IF TG_OP = 'UPDATE' THEN
        previous := OLD.published_revision_id;
    END IF;
    EXECUTE format('SELECT EXISTS (
                        SELECT 1 FROM %I.publication_audit
                         WHERE lesson_id = $1 AND revision_id = $2 AND previous_revision_id IS NOT DISTINCT FROM $3
                           AND xact_id = pg_catalog.pg_current_xact_id())', TG_TABLE_SCHEMA)
       INTO audited USING NEW.id, NEW.published_revision_id, previous;
    IF NOT audited THEN
        RAISE EXCEPTION 'publishing revision % requires a publication audit row in the same transaction', NEW.published_revision_id
            USING ERRCODE = 'LRN01';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER zz_guard_lessons_published_audited
    BEFORE INSERT OR UPDATE ON lessons
    FOR EACH ROW EXECUTE FUNCTION lessons_guard_published_audited();

CREATE FUNCTION review_audit_append_only() RETURNS trigger
LANGUAGE plpgsql SET search_path = pg_catalog, pg_temp AS $$
BEGIN
    RAISE EXCEPTION '% is append-only', TG_TABLE_NAME USING ERRCODE = 'LRN01';
END;
$$;

CREATE TRIGGER zz_guard_review_decisions_append_only
    BEFORE UPDATE OR DELETE ON review_decisions
    FOR EACH ROW EXECUTE FUNCTION review_audit_append_only();

CREATE TRIGGER zz_guard_review_decisions_no_truncate
    BEFORE TRUNCATE ON review_decisions
    FOR EACH STATEMENT EXECUTE FUNCTION review_audit_append_only();

CREATE TRIGGER zz_guard_publication_audit_append_only
    BEFORE UPDATE OR DELETE ON publication_audit
    FOR EACH ROW EXECUTE FUNCTION review_audit_append_only();

CREATE TRIGGER zz_guard_publication_audit_no_truncate
    BEFORE TRUNCATE ON publication_audit
    FOR EACH STATEMENT EXECUTE FUNCTION review_audit_append_only();
