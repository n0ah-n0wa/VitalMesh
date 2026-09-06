-- Accounts that authenticate against the gateway. Emails are stored in
-- lower case so that the unique index is case-insensitive by construction.
CREATE TABLE users (
    id            uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    email         text        NOT NULL,
    password_hash text        NOT NULL,
    role          text        NOT NULL,
    status        text        NOT NULL DEFAULT 'ACTIVE',
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT users_email_check  CHECK (email <> '' AND email = lower(email) AND length(email) <= 320),
    CONSTRAINT users_role_check   CHECK (role IN ('ADMIN', 'OPERATOR', 'USER')),
    CONSTRAINT users_status_check CHECK (status IN ('ACTIVE', 'DISABLED'))
);

CREATE UNIQUE INDEX users_email_key ON users (email);

-- Keyset pagination over the user list.
CREATE INDEX users_created_at_id_idx ON users (created_at, id);

CREATE TRIGGER users_set_updated_at
    BEFORE UPDATE ON users
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();
