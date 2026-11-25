-- LRN-008: invited student accounts, bookmarks and the reader's view of
-- published lessons. (Numbered 0008 on a stacked branch; renumbered at merge
-- if needed.)
--
-- Conventions of 0004_lessons.sql apply: guards raise SQLSTATE LRN01 and are
-- named zz_guard_* so they fire after any trigger that changes NEW; every
-- function pins search_path to pg_catalog, pg_temp and reaches other tables
-- through the schema of the table that fired it (TG_TABLE_SCHEMA, EXECUTE
-- format('%I...') with bound parameters). No function is SECURITY DEFINER;
-- the guards bind the application's least-privilege role, not a superuser or
-- table owner (docs/DECISIONS.md "Lesson persistence", threat model).
--
-- Personal data: an invitation stores only the invited email address. The
-- account created from it stores email, display name and password hash
-- (0001). Retention and deletion are not decided (docs/DECISIONS.md
-- "Privacy, deletion and retention"); nothing here deletes rows.

-- A single-use, expiring invitation to create a Student account (no
-- permissions). Only the SHA-256 of the random token is stored. Created by a
-- local operator tool, recorded with a self-asserted operator label.
CREATE TABLE student_invitations (
    id               bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    token_hash       bytea       NOT NULL CHECK (octet_length(token_hash) = 32),
    email            text        NOT NULL,
    operator_label   text        NOT NULL,
    created_at       timestamptz NOT NULL,
    expires_at       timestamptz NOT NULL,
    -- Set once, together, at acceptance (guard below).
    accepted_at      timestamptz,
    accepted_user_id bigint      REFERENCES users (id),
    CONSTRAINT student_invitations_token_key UNIQUE (token_hash),
    CONSTRAINT student_invitations_email_normalized CHECK (email = lower(btrim(email)) AND email <> '' AND char_length(email) <= 254),
    CONSTRAINT student_invitations_operator_label CHECK (
        operator_label = btrim(operator_label) AND char_length(operator_label) BETWEEN 1 AND 200
        -- The rule of permission_audit and account_status_audit (0002,
        -- 0003): no control (Cc), format (Cf: invisible and bidirectional
        -- controls) or line/paragraph separator (Zl, Zp) characters, as of
        -- Unicode 15, listed by code point so it does not depend on the locale.
        AND operator_label !~ E'[\\u0001-\\u001F\\u007F-\\u009F\\u00AD\\u0600-\\u0605\\u061C\\u06DD\\u070F\\u0890-\\u0891\\u08E2\\u180E\\u200B-\\u200F\\u2028-\\u202E\\u2060-\\u2064\\u2066-\\u206F\\uFEFF\\uFFF9-\\uFFFB\\U000110BD\\U000110CD\\U00013430-\\U0001343F\\U0001BCA0-\\U0001BCA3\\U0001D173-\\U0001D17A\\U000E0001\\U000E0020-\\U000E007F]'),
    CONSTRAINT student_invitations_lifetime CHECK (expires_at > created_at AND expires_at <= created_at + interval '30 days'),
    CONSTRAINT student_invitations_accepted CHECK ((accepted_at IS NULL) = (accepted_user_id IS NULL)),
    CONSTRAINT student_invitations_accepted_in_time CHECK (accepted_at IS NULL OR accepted_at < expires_at)
);

CREATE INDEX student_invitations_email_idx ON student_invitations (email);

-- Append-only record of issuing and accepting invitations, written only by
-- the triggers on student_invitations so the application cannot forget it.
CREATE TABLE student_invitation_audit (
    id             bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    invitation_id  bigint      NOT NULL REFERENCES student_invitations (id),
    action         text        NOT NULL CHECK (action IN ('issue', 'accept')),
    occurred_at    timestamptz NOT NULL DEFAULT now(),
    -- issue: the operator label; accept: the created account.
    operator_label text,
    user_id        bigint      REFERENCES users (id),
    CONSTRAINT student_invitation_audit_actor CHECK (
        (action = 'issue' AND operator_label IS NOT NULL AND user_id IS NULL)
        OR (action = 'accept' AND operator_label IS NULL AND user_id IS NOT NULL))
);

CREATE INDEX student_invitation_audit_invitation_idx ON student_invitation_audit (invitation_id);

-- Insert: never pre-accepted. Update: the only change is acceptance, once,
-- at the transaction time, before expiry, by an account with the invited
-- email. Delete and truncate are rejected (retention is undecided).
CREATE FUNCTION student_invitations_guard() RETURNS trigger
LANGUAGE plpgsql SET search_path = pg_catalog, pg_temp AS $$
DECLARE
    account_email text;
BEGIN
    IF TG_OP = 'INSERT' THEN
        IF NEW.accepted_at IS NOT NULL OR NEW.accepted_user_id IS NOT NULL THEN
            RAISE EXCEPTION 'an invitation is created unaccepted' USING ERRCODE = 'LRN01';
        END IF;
        RETURN NEW;
    END IF;
    IF TG_OP <> 'UPDATE' THEN
        RAISE EXCEPTION 'student invitations are never deleted' USING ERRCODE = 'LRN01';
    END IF;
    IF (NEW.id, NEW.token_hash, NEW.email, NEW.operator_label, NEW.created_at, NEW.expires_at)
       IS DISTINCT FROM (OLD.id, OLD.token_hash, OLD.email, OLD.operator_label, OLD.created_at, OLD.expires_at) THEN
        RAISE EXCEPTION 'invitation identity is immutable' USING ERRCODE = 'LRN01';
    END IF;
    IF OLD.accepted_at IS NOT NULL THEN
        RAISE EXCEPTION 'invitation % was already accepted', OLD.id USING ERRCODE = 'LRN01';
    END IF;
    IF NEW.accepted_at IS DISTINCT FROM now() OR now() >= OLD.expires_at THEN
        RAISE EXCEPTION 'invitation % is accepted only before it expires, at the transaction time', OLD.id USING ERRCODE = 'LRN01';
    END IF;
    EXECUTE format('SELECT email FROM %I.users WHERE id = $1', TG_TABLE_SCHEMA)
       INTO account_email USING NEW.accepted_user_id;
    IF account_email IS DISTINCT FROM OLD.email THEN
        RAISE EXCEPTION 'invitation % must be accepted by an account with its email', OLD.id USING ERRCODE = 'LRN01';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER zz_guard_student_invitations
    BEFORE INSERT OR UPDATE OR DELETE ON student_invitations
    FOR EACH ROW EXECUTE FUNCTION student_invitations_guard();

CREATE TRIGGER zz_guard_student_invitations_no_truncate
    BEFORE TRUNCATE ON student_invitations
    FOR EACH STATEMENT EXECUTE FUNCTION student_invitations_guard();

CREATE FUNCTION student_invitations_audit() RETURNS trigger
LANGUAGE plpgsql SET search_path = pg_catalog, pg_temp AS $$
BEGIN
    IF TG_OP = 'INSERT' THEN
        EXECUTE format('INSERT INTO %I.student_invitation_audit (invitation_id, action, operator_label) VALUES ($1, $2, $3)', TG_TABLE_SCHEMA)
           USING NEW.id, 'issue', NEW.operator_label;
    ELSIF OLD.accepted_at IS NULL AND NEW.accepted_at IS NOT NULL THEN
        EXECUTE format('INSERT INTO %I.student_invitation_audit (invitation_id, action, user_id) VALUES ($1, $2, $3)', TG_TABLE_SCHEMA)
           USING NEW.id, 'accept', NEW.accepted_user_id;
    END IF;
    RETURN NULL;
END;
$$;

CREATE TRIGGER student_invitations_audit
    AFTER INSERT OR UPDATE ON student_invitations
    FOR EACH ROW EXECUTE FUNCTION student_invitations_audit();

CREATE FUNCTION student_invitation_audit_guard() RETURNS trigger
LANGUAGE plpgsql SET search_path = pg_catalog, pg_temp AS $$
BEGIN
    IF TG_OP = 'INSERT' THEN
        IF NEW.occurred_at IS DISTINCT FROM now() THEN
            RAISE EXCEPTION 'occurred_at must be the transaction time' USING ERRCODE = 'LRN01';
        END IF;
        RETURN NEW;
    END IF;
    RAISE EXCEPTION '% is append-only', TG_TABLE_NAME USING ERRCODE = 'LRN01';
END;
$$;

CREATE TRIGGER zz_guard_student_invitation_audit
    BEFORE INSERT OR UPDATE OR DELETE ON student_invitation_audit
    FOR EACH ROW EXECUTE FUNCTION student_invitation_audit_guard();

CREATE TRIGGER zz_guard_student_invitation_audit_no_truncate
    BEFORE TRUNCATE ON student_invitation_audit
    FOR EACH STATEMENT EXECUTE FUNCTION student_invitation_audit_guard();

-- Stable bookmarks: keyed by the lesson, which the reader addresses by its
-- random public_id, never by a revision or title. A bookmark survives new
-- published revisions; if the lesson stops being published the dashboard
-- shows it as unavailable.
CREATE TABLE bookmarks (
    user_id    bigint      NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    lesson_id  bigint      NOT NULL REFERENCES lessons (id),
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, lesson_id)
);

CREATE INDEX bookmarks_lesson_idx ON bookmarks (lesson_id);

-- The single definition of what a student may read: a lesson that is not
-- archived and whose published pointer names an approved, non-withdrawn
-- revision of that lesson. 0007 already guarantees that the pointer never
-- names a withdrawn revision (withdrawing the published revision clears the
-- pointer in the same transaction); the NOT EXISTS repeats that rule here as
-- defence in depth, so the reader cannot show withdrawn content even if a
-- later change weakened the guard.
-- security_invoker: the view is read with the caller's privileges.
CREATE VIEW reader_published_lessons WITH (security_invoker = true) AS
SELECT l.id          AS lesson_id,
       l.public_id,
       l.language,
       l.subject_id,
       r.id          AS revision_id,
       r.title,
       r.objectives,
       r.chapter_id,
       r.author_id,
       r.content
  FROM lessons l
  JOIN lesson_revisions r ON r.id = l.published_revision_id AND r.lesson_id = l.id
 WHERE l.archived_at IS NULL
   AND r.status = 'approved'
   AND NOT EXISTS (SELECT 1 FROM revision_withdrawals w WHERE w.revision_id = r.id);

-- Navigation lists published lessons per subject.
CREATE INDEX lessons_published_subject_idx ON lessons (subject_id)
    WHERE published_revision_id IS NOT NULL AND archived_at IS NULL;

-- Title search key: Unicode NFD, combining marks removed (Latin accents,
-- Arabic harakat, hamza and madda marks, tatweel), lower-cased, with the
-- French ligatures expanded. Used on both sides of a substring match
-- (strpos), so the search needs no LIKE pattern and no escaping.
--
-- The result must not depend on the database locale (issue #67): lower()
-- folds non-ASCII letters only under a locale that knows them, so with
-- LC_CTYPE=C "Œuvre" would not fold to "œuvre". normalize() and the regular
-- expression work on code points whatever the locale, so after removing the
-- marks the function lowers ASCII with lower(... COLLATE "C"), which is
-- ASCII-only under every database locale, and the remaining upper-case
-- letters with an explicit translate() table: those of Latin-1 and Latin
-- Extended-A that have no canonical decomposition (Æ Ð Ø Þ Đ Ħ Ĳ Ŀ Ł Ŋ Œ Ŧ;
-- every other one, such as É or Ÿ, has already lost its mark) and the
-- basic Greek capitals used in mathematics, with final sigma folded to
-- sigma. Other scripts with case are left as they are, under every locale.
CREATE FUNCTION reader_fold(t text) RETURNS text
LANGUAGE sql IMMUTABLE PARALLEL SAFE STRICT
SET search_path = pg_catalog, pg_temp
RETURN replace(replace(translate(
    lower(regexp_replace(normalize(t, NFD), '[̀-ͯؐ-ًؚ-ٰٟـ]', '', 'g') COLLATE "C"),
    'ÆÐØÞĐĦĲĿŁŊŒŦΑΒΓΔΕΖΗΘΙΚΛΜΝΞΟΠΡΣΤΥΦΧΨΩς',
    'æðøþđħĳŀłŋœŧαβγδεζηθικλμνξοπρστυφχψωσ'),
    'œ', 'oe'), 'æ', 'ae');
