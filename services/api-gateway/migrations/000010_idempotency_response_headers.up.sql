-- A replay must reproduce the first response, not merely its status and
-- body. A client that followed Location on the original 201 has to be able
-- to follow it on the replay, so the headers that describe the response are
-- stored with it.
--
-- Only an allow-list is ever written here (see internal/httpapi/middleware),
-- so nothing per-request or sensitive is persisted: no credentials, no
-- cookies, no request identifiers.
ALTER TABLE idempotency_keys
    ADD COLUMN response_headers jsonb NOT NULL DEFAULT '{}'::jsonb;
