-- LRN-013 account status: append-only record of every account disable and
-- enable. users.disabled_at (0001) holds the current status. Like
-- permission_audit (0002), the append-only triggers bind only roles that
-- neither own the table nor are superusers, and occurred_at defaults to
-- now(), the transaction time, so it equals the disabled_at it records.

CREATE TABLE account_status_audit (
    id             bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    occurred_at    timestamptz NOT NULL DEFAULT now(),
    -- Exactly one of actor_id (a signed-in administrator) and operator_label
    -- (a self-asserted local operator identity, e.g. user@host) is set, as
    -- in permission_audit.
    actor_id       bigint      REFERENCES users (id),
    operator_label text,
    target_user_id bigint      NOT NULL REFERENCES users (id),
    action         text        NOT NULL CHECK (action IN ('disable', 'enable')),
    CONSTRAINT account_status_audit_actor CHECK ((actor_id IS NULL) = (operator_label IS NOT NULL)),
    CONSTRAINT account_status_audit_operator_label CHECK (
        char_length(operator_label) BETWEEN 1 AND 200
        AND operator_label = btrim(operator_label)
        -- The same rule as permission_audit_operator_label: no control (Cc),
        -- format (Cf: invisible and bidirectional controls) or
        -- line/paragraph separator (Zl, Zp) characters, as of Unicode 15,
        -- listed by code point so the check does not depend on the locale.
        -- OperatorAs in internal/permissions rejects at least these.
        AND operator_label !~ E'[\\u0001-\\u001F\\u007F-\\u009F\\u00AD\\u0600-\\u0605\\u061C\\u06DD\\u070F\\u0890-\\u0891\\u08E2\\u180E\\u200B-\\u200F\\u2028-\\u202E\\u2060-\\u2064\\u2066-\\u206F\\uFEFF\\uFFF9-\\uFFFB\\U000110BD\\U000110CD\\U00013430-\\U0001343F\\U0001BCA0-\\U0001BCA3\\U0001D173-\\U0001D17A\\U000E0001\\U000E0020-\\U000E007F]')
);

CREATE INDEX account_status_audit_target_idx ON account_status_audit (target_user_id, occurred_at);

CREATE FUNCTION account_status_audit_append_only() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'account_status_audit is append-only';
END;
$$;

CREATE TRIGGER account_status_audit_no_update_delete
    BEFORE UPDATE OR DELETE ON account_status_audit
    FOR EACH ROW EXECUTE FUNCTION account_status_audit_append_only();

CREATE TRIGGER account_status_audit_no_truncate
    BEFORE TRUNCATE ON account_status_audit
    FOR EACH STATEMENT EXECUTE FUNCTION account_status_audit_append_only();
