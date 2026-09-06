-- Durable record of idempotent write requests. A key is scoped to the
-- user and the operation; a replay with the same fingerprint returns the
-- stored response, a replay with a different one is a conflict.
CREATE TABLE idempotency_keys (
    id                  uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id             uuid        NOT NULL REFERENCES users (id),
    method              text        NOT NULL,
    path                text        NOT NULL,
    key                 text        NOT NULL,
    request_fingerprint text        NOT NULL,
    status              text        NOT NULL DEFAULT 'IN_PROGRESS',
    response_status     integer,
    response_body       jsonb,
    created_at          timestamptz NOT NULL DEFAULT now(),
    expires_at          timestamptz NOT NULL,
    CONSTRAINT idempotency_keys_method_check          CHECK (method <> ''),
    CONSTRAINT idempotency_keys_path_check            CHECK (path <> ''),
    CONSTRAINT idempotency_keys_key_check             CHECK (key <> '' AND length(key) <= 255),
    CONSTRAINT idempotency_keys_fingerprint_check     CHECK (request_fingerprint <> ''),
    CONSTRAINT idempotency_keys_status_check          CHECK (status IN ('IN_PROGRESS', 'COMPLETED')),
    CONSTRAINT idempotency_keys_completed_check       CHECK ((status = 'COMPLETED') = (response_status IS NOT NULL)),
    CONSTRAINT idempotency_keys_response_status_check CHECK (response_status IS NULL OR response_status BETWEEN 100 AND 599),
    CONSTRAINT idempotency_keys_expires_check         CHECK (expires_at > created_at),
    CONSTRAINT idempotency_keys_user_method_path_key_key UNIQUE (user_id, method, path, key)
);

-- Expiry sweep by the retention job.
CREATE INDEX idempotency_keys_expires_at_idx ON idempotency_keys (expires_at);
