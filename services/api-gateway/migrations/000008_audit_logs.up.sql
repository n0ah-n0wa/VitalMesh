-- Append-only record of security-sensitive operations. Rows can be inserted
-- and read but never updated or deleted; a trigger enforces this for every
-- role. Users are never deleted, so actor references stay valid.
CREATE TABLE audit_logs (
    id            uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    actor_id      uuid        REFERENCES users (id),
    actor_type    text        NOT NULL DEFAULT 'USER',
    action        text        NOT NULL,
    resource_type text        NOT NULL,
    resource_id   uuid,
    request_id    text        NOT NULL,
    metadata      jsonb       NOT NULL DEFAULT '{}'::jsonb,
    created_at    timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT audit_logs_actor_type_check    CHECK (actor_type IN ('USER', 'SYSTEM')),
    CONSTRAINT audit_logs_actor_check         CHECK ((actor_type = 'USER') = (actor_id IS NOT NULL)),
    CONSTRAINT audit_logs_action_check        CHECK (action IN (
        'LOGIN', 'LOGOUT', 'USER_CREATED', 'ROLE_CHANGED',
        'PATIENT_CREATED', 'PATIENT_DELETED',
        'MEASUREMENT_CREATED', 'MEASUREMENT_DELETED',
        'PROCESSING_JOB_CREATED', 'PROCESSING_JOB_CANCELLED',
        'RETENTION_RUN')),
    CONSTRAINT audit_logs_resource_type_check CHECK (resource_type <> ''),
    CONSTRAINT audit_logs_request_id_check    CHECK (request_id <> ''),
    CONSTRAINT audit_logs_metadata_check      CHECK (jsonb_typeof(metadata) = 'object')
);

CREATE INDEX audit_logs_resource_id_created_at_idx ON audit_logs (resource_id, created_at);

CREATE FUNCTION audit_logs_reject_change() RETURNS trigger
    LANGUAGE plpgsql
AS $$
BEGIN
    RAISE EXCEPTION 'audit_logs is append-only'
        USING ERRCODE = 'insufficient_privilege';
END;
$$;

CREATE TRIGGER audit_logs_append_only
    BEFORE UPDATE OR DELETE ON audit_logs
    FOR EACH ROW EXECUTE FUNCTION audit_logs_reject_change();
