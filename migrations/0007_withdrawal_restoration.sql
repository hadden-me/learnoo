-- LRN-009: withdrawal, emergency unpublication and restoration.
--
-- Conventions of 0004_lessons.sql and 0006_review_publication.sql apply:
-- guards raise SQLSTATE LRN01 and are named zz_guard_* so they fire after any
-- trigger that changes NEW; every function pins search_path to
-- pg_catalog, pg_temp and reaches other tables through the schema of the
-- table that fired it (TG_TABLE_SCHEMA, EXECUTE format('%I...') with bound
-- parameters), so a temporary table of the calling role cannot shadow them.
-- Lock order stays: lesson row first, then its revision rows. The guards
-- bind the application's least-privilege role, not a superuser or table
-- owner (docs/DECISIONS.md "Lesson persistence", threat model).
--
-- 0006 is extended here rather than edited, so its history stays as
-- reviewed. Two of its functions are replaced (CREATE OR REPLACE keeps their
-- triggers):
--
--   * publication_audit records every change of lessons.published_revision_id:
--     'publish' (a never-published approved revision whose base is the
--     current pointer), 'restore' (a previously published, approved,
--     non-withdrawn revision; no base check) and 'unpublish' (the pointer to
--     NULL, with a reason). revision_id is the new pointer, NULL only for
--     'unpublish'; previous_revision_id is the pointer it replaced.
--   * The pointer guard on lessons now requires such an audit row of the
--     same transaction for every change, including to NULL.
--
-- revision_withdrawals records withdrawals (administrator, reason). A
-- withdrawn revision can never again be named by the pointer (a guard on
-- lessons, the audit insert guard and a commit-time check on the withdrawal),
-- so withdrawing the published revision must unpublish it in the same
-- transaction. Withdrawing a pending approved candidate closes it
-- (candidate_closed_at) through an AFTER INSERT trigger that is SECURITY
-- DEFINER and owned by the table owner, hardened like
-- lessons_close_published_candidate (0004); lesson_revisions_guard_update
-- already accepts that close from an owner-run trigger and is unchanged.
--
-- "Was published" is the existence of a 'publish' audit row for the
-- revision. candidate_closed_at cannot answer it: withdrawal sets it too.
--
-- Snapshots. The guards read facts that are only inserted (a withdrawal
-- row). A writer at REPEATABLE READ or SERIALIZABLE whose snapshot predates
-- a committed withdrawal would not see it. Every withdrawal therefore also
-- updates its lesson row (lessons.withdrawal_count, owner-run trigger
-- below); every pointer change locks and updates that row, so such a stale
-- writer fails with a serialization error (40001) instead of acting on a
-- withdrawn revision. At READ COMMITTED each guard statement takes a new
-- snapshot after the lesson lock is granted and sees the withdrawal.

ALTER TABLE publication_audit
    DROP CONSTRAINT publication_audit_action,
    ALTER COLUMN revision_id DROP NOT NULL,
    ADD COLUMN reason text,
    ADD CONSTRAINT publication_audit_action CHECK (action IN ('publish', 'restore', 'unpublish')),
    -- Only unpublication clears the pointer, and it needs one to clear.
    ADD CONSTRAINT publication_audit_unpublish_target CHECK ((action = 'unpublish') = (revision_id IS NULL)),
    ADD CONSTRAINT publication_audit_unpublish_previous CHECK (action <> 'unpublish' OR previous_revision_id IS NOT NULL),
    -- A reason is required for unpublication and recorded for nothing else.
    ADD CONSTRAINT publication_audit_reason CHECK ((action = 'unpublish') = (reason IS NOT NULL)),
    ADD CONSTRAINT publication_audit_reason_not_blank CHECK (reason IS NULL OR reason ~ '[^[:space:]]'),
    ADD CONSTRAINT publication_audit_reason_length CHECK (char_length(reason) <= 2000);

-- "Was this revision ever published?" (restoration) looks up publish rows.
CREATE INDEX publication_audit_revision_idx ON publication_audit (revision_id) WHERE action = 'publish';

CREATE TABLE revision_withdrawals (
    id          bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    lesson_id   bigint      NOT NULL REFERENCES lessons (id),
    revision_id bigint      NOT NULL,
    actor_id    bigint      NOT NULL REFERENCES users (id),
    reason      text        NOT NULL,
    -- The transaction time and the top-level transaction (insert guard).
    occurred_at timestamptz NOT NULL DEFAULT now(),
    xact_id     xid8        NOT NULL DEFAULT pg_current_xact_id(),
    CONSTRAINT revision_withdrawals_reason_not_blank CHECK (reason ~ '[^[:space:]]'),
    CONSTRAINT revision_withdrawals_reason_length CHECK (char_length(reason) <= 2000),
    -- Withdrawal is final: a revision is withdrawn at most once.
    CONSTRAINT revision_withdrawals_revision_key UNIQUE (revision_id),
    CONSTRAINT revision_withdrawals_revision_fkey FOREIGN KEY (lesson_id, revision_id)
        REFERENCES lesson_revisions (lesson_id, id)
);

-- Number of withdrawals of the lesson's revisions. Changed only by the
-- withdrawal trigger below, by exactly one, so that a withdrawal modifies
-- the lesson row (see "Snapshots" above).
ALTER TABLE lessons ADD COLUMN withdrawal_count integer NOT NULL DEFAULT 0;

-- withdrawal_count starts at 0 and changes only from inside a trigger
-- running as the owner of lessons (the SECURITY DEFINER withdrawal trigger),
-- by one, alone. As for candidate_closed_at (0004), the depth alone proves
-- nothing (any role may create temporary triggers); the owner check does.
CREATE FUNCTION lessons_guard_withdrawal_count() RETURNS trigger
LANGUAGE plpgsql SET search_path = pg_catalog, pg_temp AS $$
BEGIN
    IF TG_OP = 'INSERT' THEN
        IF NEW.withdrawal_count <> 0 THEN
            RAISE EXCEPTION 'a lesson starts with no withdrawals' USING ERRCODE = 'LRN01';
        END IF;
        RETURN NEW;
    END IF;
    IF NEW.withdrawal_count IS NOT DISTINCT FROM OLD.withdrawal_count THEN
        RETURN NEW;
    END IF;
    IF pg_trigger_depth() < 2
       OR NOT EXISTS (SELECT 1 FROM pg_catalog.pg_class c
                       WHERE c.oid = TG_RELID
                         AND c.relowner = (SELECT r.oid FROM pg_catalog.pg_roles r WHERE r.rolname = current_user)) THEN
        RAISE EXCEPTION 'lesson % withdrawal_count is changed only by an audited withdrawal', OLD.id USING ERRCODE = 'LRN01';
    END IF;
    IF NEW.withdrawal_count <> OLD.withdrawal_count + 1
       OR to_jsonb(NEW) - 'withdrawal_count' IS DISTINCT FROM to_jsonb(OLD) - 'withdrawal_count' THEN
        RAISE EXCEPTION 'lesson % withdrawal_count only grows by one, alone', OLD.id USING ERRCODE = 'LRN01';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER zz_guard_lessons_withdrawal_count
    BEFORE INSERT OR UPDATE ON lessons
    FOR EACH ROW EXECUTE FUNCTION lessons_guard_withdrawal_count();

CREATE INDEX revision_withdrawals_lesson_idx ON revision_withdrawals (lesson_id, id);
CREATE INDEX revision_withdrawals_actor_idx ON revision_withdrawals (actor_id);

-- Replaces 0006's guard. Locks the lesson FOR UPDATE first (the row the
-- pointer update then changes), then share-locks the revision.
CREATE OR REPLACE FUNCTION publication_audit_guard_insert() RETURNS trigger
LANGUAGE plpgsql SET search_path = pg_catalog, pg_temp AS $$
DECLARE
    found_lesson boolean;
    current_ptr  bigint;
    st           text;
    base         bigint;
    closed       boolean;
    withdrawn    boolean;
    published    boolean;
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

    IF NEW.action = 'unpublish' THEN
        EXECUTE format('SELECT EXISTS (
                            SELECT 1 FROM %1$I.user_permissions p JOIN %1$I.users u ON u.id = p.user_id
                             WHERE p.user_id = $1 AND p.permission = ''administrator'' AND u.disabled_at IS NULL
                               FOR SHARE OF p, u)', TG_TABLE_SCHEMA)
           INTO allowed USING NEW.actor_id;
        IF NOT allowed THEN
            RAISE EXCEPTION 'user % is not an active administrator', NEW.actor_id USING ERRCODE = 'LRN01';
        END IF;
        RETURN NEW;
    END IF;

    EXECUTE format('SELECT status, base_revision_id, candidate_closed_at IS NOT NULL
                      FROM %I.lesson_revisions WHERE id = $1 AND lesson_id = $2 FOR SHARE', TG_TABLE_SCHEMA)
       INTO st, base, closed USING NEW.revision_id, NEW.lesson_id;
    IF st IS DISTINCT FROM 'approved' THEN
        RAISE EXCEPTION 'publication audit: revision % is not an approved revision of lesson %', NEW.revision_id, NEW.lesson_id
            USING ERRCODE = 'LRN01';
    END IF;
    EXECUTE format('SELECT EXISTS (SELECT 1 FROM %I.revision_withdrawals WHERE revision_id = $1)', TG_TABLE_SCHEMA)
       INTO withdrawn USING NEW.revision_id;
    IF withdrawn THEN
        RAISE EXCEPTION 'revision % is withdrawn and can never be published or restored', NEW.revision_id USING ERRCODE = 'LRN01';
    END IF;

    IF NEW.action = 'publish' THEN
        -- Publication takes the lesson's pending candidate: never published
        -- (still open) and drafted against the current pointer.
        IF closed THEN
            RAISE EXCEPTION 'revision % was already published; re-pointing to it is restoration', NEW.revision_id
                USING ERRCODE = 'LRN01';
        END IF;
        IF base IS DISTINCT FROM current_ptr THEN
            RAISE EXCEPTION 'revision % was drafted against another published revision (stale base)', NEW.revision_id
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
    END IF;

    -- 'restore': a revision published before, without the historical base check.
    EXECUTE format('SELECT EXISTS (SELECT 1 FROM %I.publication_audit
                                     WHERE revision_id = $1 AND lesson_id = $2 AND action = ''publish'')', TG_TABLE_SCHEMA)
       INTO published USING NEW.revision_id, NEW.lesson_id;
    IF NOT published THEN
        RAISE EXCEPTION 'revision % was never published and cannot be restored', NEW.revision_id USING ERRCODE = 'LRN01';
    END IF;
    EXECUTE format('SELECT EXISTS (
                        SELECT 1 FROM %1$I.user_permissions p JOIN %1$I.users u ON u.id = p.user_id
                         WHERE p.user_id = $1 AND p.permission IN (''publisher'', ''administrator'') AND u.disabled_at IS NULL
                           FOR SHARE OF p, u)', TG_TABLE_SCHEMA)
       INTO allowed USING NEW.actor_id;
    IF NOT allowed THEN
        RAISE EXCEPTION 'user % is not an active publisher or administrator', NEW.actor_id USING ERRCODE = 'LRN01';
    END IF;
    RETURN NEW;
END;
$$;

-- Replaces 0006's guard: every change of the pointer, including to NULL,
-- requires an audit row of this transaction recording exactly that change.
CREATE OR REPLACE FUNCTION lessons_guard_published_audited() RETURNS trigger
LANGUAGE plpgsql SET search_path = pg_catalog, pg_temp AS $$
DECLARE
    previous bigint;
    audited  boolean;
BEGIN
    IF (TG_OP = 'INSERT' AND NEW.published_revision_id IS NULL)
       OR (TG_OP = 'UPDATE' AND NEW.published_revision_id IS NOT DISTINCT FROM OLD.published_revision_id) THEN
        RETURN NEW;
    END IF;
    IF TG_OP = 'UPDATE' THEN
        previous := OLD.published_revision_id;
    END IF;
    EXECUTE format('SELECT EXISTS (
                        SELECT 1 FROM %I.publication_audit
                         WHERE lesson_id = $1 AND revision_id IS NOT DISTINCT FROM $2
                           AND previous_revision_id IS NOT DISTINCT FROM $3
                           AND xact_id = pg_catalog.pg_current_xact_id())', TG_TABLE_SCHEMA)
       INTO audited USING NEW.id, NEW.published_revision_id, previous;
    IF NOT audited THEN
        RAISE EXCEPTION 'changing the published revision of lesson % to % requires a publication audit row in the same transaction',
            NEW.id, coalesce(NEW.published_revision_id::text, 'NULL')
            USING ERRCODE = 'LRN01';
    END IF;
    RETURN NEW;
END;
$$;

-- The pointer never names a withdrawn revision. Sorts after the approved-only
-- and audit guards, so those report first.
CREATE FUNCTION lessons_guard_published_not_withdrawn() RETURNS trigger
LANGUAGE plpgsql SET search_path = pg_catalog, pg_temp AS $$
DECLARE
    withdrawn boolean;
BEGIN
    IF NEW.published_revision_id IS NULL
       OR (TG_OP = 'UPDATE' AND NEW.published_revision_id IS NOT DISTINCT FROM OLD.published_revision_id) THEN
        RETURN NEW;
    END IF;
    EXECUTE format('SELECT EXISTS (SELECT 1 FROM %I.revision_withdrawals WHERE revision_id = $1)', TG_TABLE_SCHEMA)
       INTO withdrawn USING NEW.published_revision_id;
    IF withdrawn THEN
        RAISE EXCEPTION 'the published pointer cannot name withdrawn revision %', NEW.published_revision_id
            USING ERRCODE = 'LRN01';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER zz_guard_lessons_published_not_withdrawn
    BEFORE INSERT OR UPDATE ON lessons
    FOR EACH ROW EXECUTE FUNCTION lessons_guard_published_not_withdrawn();

-- A withdrawal is accepted only for an approved revision of the lesson, from
-- an active administrator, at the transaction time. Locks the lesson FOR
-- UPDATE first (so it serializes with publication and restoration), then
-- share-locks the revision, as the pointer triggers do.
CREATE FUNCTION revision_withdrawals_guard_insert() RETURNS trigger
LANGUAGE plpgsql SET search_path = pg_catalog, pg_temp AS $$
DECLARE
    found_lesson boolean;
    st           text;
    allowed      boolean;
BEGIN
    IF NEW.occurred_at IS DISTINCT FROM now() OR NEW.xact_id IS DISTINCT FROM pg_current_xact_id() THEN
        RAISE EXCEPTION 'a withdrawal records its own transaction and its time' USING ERRCODE = 'LRN01';
    END IF;
    EXECUTE format('SELECT true FROM %I.lessons WHERE id = $1 FOR UPDATE', TG_TABLE_SCHEMA)
       INTO found_lesson USING NEW.lesson_id;
    IF found_lesson IS NULL THEN
        RAISE EXCEPTION 'lesson % does not exist', NEW.lesson_id USING ERRCODE = 'LRN01';
    END IF;
    EXECUTE format('SELECT status FROM %I.lesson_revisions WHERE id = $1 AND lesson_id = $2 FOR SHARE', TG_TABLE_SCHEMA)
       INTO st USING NEW.revision_id, NEW.lesson_id;
    IF st IS DISTINCT FROM 'approved' THEN
        RAISE EXCEPTION 'withdrawal: revision % is not an approved revision of lesson %', NEW.revision_id, NEW.lesson_id
            USING ERRCODE = 'LRN01';
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

CREATE TRIGGER zz_guard_revision_withdrawals_insert
    BEFORE INSERT ON revision_withdrawals
    FOR EACH ROW EXECUTE FUNCTION revision_withdrawals_guard_insert();

-- At commit, the withdrawn revision must not be the published one: withdrawing
-- the published revision unpublishes it in the same transaction.
CREATE FUNCTION revision_withdrawals_check_unpublished() RETURNS trigger
LANGUAGE plpgsql SET search_path = pg_catalog, pg_temp AS $$
DECLARE
    current_ptr bigint;
BEGIN
    EXECUTE format('SELECT published_revision_id FROM %I.lessons WHERE id = $1', TG_TABLE_SCHEMA)
       INTO current_ptr USING NEW.lesson_id;
    IF current_ptr IS NOT DISTINCT FROM NEW.revision_id THEN
        RAISE EXCEPTION 'withdrawn revision % is still published; unpublish it in the same transaction', NEW.revision_id
            USING ERRCODE = 'LRN01';
    END IF;
    RETURN NULL;
END;
$$;

CREATE CONSTRAINT TRIGGER zz_guard_revision_withdrawals_unpublished
    AFTER INSERT ON revision_withdrawals
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION revision_withdrawals_check_unpublished();

-- Applies an accepted withdrawal: increments the lesson's withdrawal_count
-- (so the withdrawal modifies the lesson row; see "Snapshots" above), then
-- closes the revision as the lesson's candidate, so a withdrawn pending
-- candidate no longer blocks new drafts. AFTER, so it runs only once the
-- guard above has accepted the withdrawal (lesson locked FOR UPDATE, revision
-- share-locked). Lock order stays lesson, then revision; the revision lock is
-- upgraded while the lesson row is held, as in
-- lessons_close_published_candidate. A revision already closed by
-- publication is left unchanged.
--
-- SECURITY DEFINER for the same reason and with the same hardening as
-- lessons_close_published_candidate (0004): lesson_revisions_guard_update
-- accepts a close only from inside a trigger running as the owner of
-- lesson_revisions. The function pins its search_path (pg_temp last), names
-- lesson_revisions through the schema of the firing table, refuses to run
-- for any relation its owner does not own or that is not permanent (such as
-- another role's temporary "revision_withdrawals" table), and is not
-- executable by PUBLIC, so no other role can attach it to a trigger.
CREATE FUNCTION revision_withdrawals_close_candidate() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $$
BEGIN
    IF TG_WHEN <> 'AFTER' OR TG_LEVEL <> 'ROW' OR TG_OP <> 'INSERT' OR TG_TABLE_NAME <> 'revision_withdrawals'
       OR NOT EXISTS (SELECT 1 FROM pg_catalog.pg_class c
                       WHERE c.oid = TG_RELID
                         AND c.relpersistence = 'p'
                         AND c.relowner = (SELECT r.oid FROM pg_catalog.pg_roles r WHERE r.rolname = current_user)) THEN
        RAISE EXCEPTION 'revision_withdrawals_close_candidate may only fire on the revision_withdrawals table' USING ERRCODE = 'LRN01';
    END IF;
    EXECUTE format('UPDATE %I.lessons SET withdrawal_count = withdrawal_count + 1 WHERE id = $1', TG_TABLE_SCHEMA)
      USING NEW.lesson_id;
    EXECUTE format('UPDATE %I.lesson_revisions SET candidate_closed_at = pg_catalog.now()
                     WHERE id = $1 AND lesson_id = $2 AND candidate_closed_at IS NULL', TG_TABLE_SCHEMA)
      USING NEW.revision_id, NEW.lesson_id;
    RETURN NULL;
END;
$$;

REVOKE EXECUTE ON FUNCTION revision_withdrawals_close_candidate() FROM PUBLIC;

CREATE TRIGGER revision_withdrawals_close_candidate
    AFTER INSERT ON revision_withdrawals
    FOR EACH ROW EXECUTE FUNCTION revision_withdrawals_close_candidate();

CREATE TRIGGER zz_guard_revision_withdrawals_append_only
    BEFORE UPDATE OR DELETE ON revision_withdrawals
    FOR EACH ROW EXECUTE FUNCTION review_audit_append_only();

CREATE TRIGGER zz_guard_revision_withdrawals_no_truncate
    BEFORE TRUNCATE ON revision_withdrawals
    FOR EACH STATEMENT EXECUTE FUNCTION review_audit_append_only();
