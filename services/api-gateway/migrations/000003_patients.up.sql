-- Synthetic patients. Deletion is a soft delete so that jobs and results
-- keep a valid reference; the status/deleted_at pair is kept consistent.
CREATE TABLE patients (
    id                 uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    external_reference text        NOT NULL,
    date_of_birth      date        NOT NULL,
    sex                text        NOT NULL,
    status             text        NOT NULL DEFAULT 'ACTIVE',
    created_at         timestamptz NOT NULL DEFAULT now(),
    updated_at         timestamptz NOT NULL DEFAULT now(),
    deleted_at         timestamptz,
    CONSTRAINT patients_external_reference_check CHECK (external_reference <> '' AND length(external_reference) <= 128),
    CONSTRAINT patients_sex_check                CHECK (sex IN ('FEMALE', 'MALE', 'OTHER', 'UNKNOWN')),
    CONSTRAINT patients_status_check             CHECK (status IN ('ACTIVE', 'INACTIVE', 'DELETED')),
    CONSTRAINT patients_deleted_at_check         CHECK ((status = 'DELETED') = (deleted_at IS NOT NULL)),
    CONSTRAINT patients_date_of_birth_check      CHECK (date_of_birth >= DATE '1900-01-01')
);

CREATE UNIQUE INDEX patients_external_reference_key ON patients (external_reference);

-- Keyset pagination over the collection endpoint.
CREATE INDEX patients_created_at_id_idx ON patients (created_at, id);

CREATE TRIGGER patients_set_updated_at
    BEFORE UPDATE ON patients
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();
