-- One row per job, measurement type and window instance. Results follow
-- their job when it is removed by retention.
--
-- patient_id is denormalised from the job (set by trigger, never by the
-- application) so that the results-of-a-patient access path is one index
-- range scan without joining jobs.
CREATE TABLE processing_results (
    id                uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    job_id            uuid        NOT NULL REFERENCES processing_jobs (id) ON DELETE CASCADE,
    patient_id        uuid        NOT NULL REFERENCES patients (id),
    measurement_type  text        NOT NULL REFERENCES measurement_types (code),
    "window"          text        NOT NULL,
    window_start      timestamptz NOT NULL,
    statistics        jsonb       NOT NULL,
    anomalies         jsonb       NOT NULL DEFAULT '[]'::jsonb,
    algorithm_version text        NOT NULL,
    service_version   text        NOT NULL,
    created_at        timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT processing_results_window_check     CHECK ("window" IN ('1m', '5m', '15m', '1h', '6h', '24h', '7d')),
    CONSTRAINT processing_results_statistics_check CHECK (jsonb_typeof(statistics) = 'object'),
    CONSTRAINT processing_results_anomalies_check  CHECK (jsonb_typeof(anomalies) = 'array'),
    CONSTRAINT processing_results_versions_check   CHECK (algorithm_version <> '' AND service_version <> ''),
    -- Also the results-of-a-job access path required by the specification.
    CONSTRAINT processing_results_job_type_window_key UNIQUE (job_id, measurement_type, "window", window_start)
);

-- Results of a patient, in type/window/time order with job_id as the
-- tie-break (two jobs can produce the same window).
CREATE INDEX processing_results_patient_type_window_idx
    ON processing_results (patient_id, measurement_type, "window", window_start, job_id);

CREATE FUNCTION processing_results_set_patient() RETURNS trigger
    LANGUAGE plpgsql
AS $$
BEGIN
    SELECT patient_id INTO NEW.patient_id FROM processing_jobs WHERE id = NEW.job_id;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'unknown job %', NEW.job_id
            USING ERRCODE = 'foreign_key_violation', CONSTRAINT = 'processing_results_job_id_fkey';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER processing_results_set_patient
    BEFORE INSERT OR UPDATE OF job_id, patient_id ON processing_results
    FOR EACH ROW EXECUTE FUNCTION processing_results_set_patient();
