-- Processing jobs and their lifecycle. The status determines which
-- timestamps must be present, timestamps are monotonic, and a trigger
-- rejects any status transition outside the specified state machine.
CREATE TABLE processing_jobs (
    id                uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    patient_id        uuid        NOT NULL REFERENCES patients (id),
    status            text        NOT NULL DEFAULT 'PENDING',
    parameters        jsonb       NOT NULL,
    algorithm_version text        NOT NULL,
    requested_at      timestamptz NOT NULL DEFAULT now(),
    started_at        timestamptz,
    completed_at      timestamptz,
    failed_at         timestamptz,
    cancelled_at      timestamptz,
    error_code        text,
    error_message     text,
    attempt_count     integer     NOT NULL DEFAULT 0,
    created_by        uuid        NOT NULL REFERENCES users (id),
    service_version   text,
    request_id        text,
    trace_id          text,
    lease_expires_at  timestamptz,
    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT processing_jobs_status_check            CHECK (status IN ('PENDING', 'PROCESSING', 'COMPLETED', 'FAILED', 'CANCELLED')),
    CONSTRAINT processing_jobs_parameters_check        CHECK (jsonb_typeof(parameters) = 'object'),
    CONSTRAINT processing_jobs_algorithm_version_check CHECK (algorithm_version <> ''),
    CONSTRAINT processing_jobs_attempt_count_check     CHECK (attempt_count >= 0),
    CONSTRAINT processing_jobs_pending_check
        CHECK (status <> 'PENDING' OR (started_at IS NULL AND completed_at IS NULL AND failed_at IS NULL AND cancelled_at IS NULL)),
    CONSTRAINT processing_jobs_processing_check
        CHECK (status <> 'PROCESSING' OR (started_at IS NOT NULL AND completed_at IS NULL AND failed_at IS NULL AND cancelled_at IS NULL)),
    CONSTRAINT processing_jobs_completed_check
        CHECK (status <> 'COMPLETED' OR (started_at IS NOT NULL AND completed_at IS NOT NULL)),
    CONSTRAINT processing_jobs_failed_check
        CHECK (status <> 'FAILED' OR (started_at IS NOT NULL AND failed_at IS NOT NULL AND error_code IS NOT NULL)),
    CONSTRAINT processing_jobs_cancelled_check
        CHECK (status <> 'CANCELLED' OR cancelled_at IS NOT NULL),
    CONSTRAINT processing_jobs_timestamps_check
        CHECK ((started_at IS NULL OR started_at >= requested_at)
           AND (completed_at IS NULL OR completed_at >= started_at)
           AND (failed_at IS NULL OR failed_at >= started_at)
           AND (cancelled_at IS NULL OR cancelled_at >= requested_at))
);

CREATE INDEX processing_jobs_patient_id_created_at_idx ON processing_jobs (patient_id, created_at);
CREATE INDEX processing_jobs_status_created_at_idx     ON processing_jobs (status, created_at);

CREATE TRIGGER processing_jobs_set_updated_at
    BEFORE UPDATE ON processing_jobs
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE FUNCTION processing_jobs_enforce_transition() RETURNS trigger
    LANGUAGE plpgsql
AS $$
BEGIN
    IF NEW.status = OLD.status THEN
        RETURN NEW;
    END IF;
    IF (OLD.status, NEW.status) IN (
        ('PENDING', 'PROCESSING'),
        ('PENDING', 'CANCELLED'),
        ('PROCESSING', 'COMPLETED'),
        ('PROCESSING', 'FAILED'),
        ('PROCESSING', 'CANCELLED')
    ) THEN
        RETURN NEW;
    END IF;
    RAISE EXCEPTION 'invalid job transition from % to %', OLD.status, NEW.status
        USING ERRCODE = 'check_violation', CONSTRAINT = 'processing_jobs_transition_check';
END;
$$;

CREATE TRIGGER processing_jobs_enforce_transition
    BEFORE UPDATE OF status ON processing_jobs
    FOR EACH ROW EXECUTE FUNCTION processing_jobs_enforce_transition();
