-- Synthetic readings. The unit and value range are enforced against
-- measurement_types by a trigger, so an invalid reading cannot be stored no
-- matter which code path writes it.
CREATE TABLE measurements (
    id          uuid             PRIMARY KEY DEFAULT gen_random_uuid(),
    patient_id  uuid             NOT NULL REFERENCES patients (id),
    type        text             NOT NULL REFERENCES measurement_types (code),
    value       double precision NOT NULL,
    unit        text             NOT NULL,
    recorded_at timestamptz      NOT NULL,
    created_at  timestamptz      NOT NULL DEFAULT now(),
    source      text             NOT NULL,
    metadata    jsonb            NOT NULL DEFAULT '{}'::jsonb,
    CONSTRAINT measurements_value_finite_check
        CHECK (value <> 'NaN'::float8 AND value <> 'Infinity'::float8 AND value <> '-Infinity'::float8),
    CONSTRAINT measurements_source_check          CHECK (source <> '' AND length(source) <= 64),
    CONSTRAINT measurements_metadata_object_check CHECK (jsonb_typeof(metadata) = 'object'),
    CONSTRAINT measurements_metadata_size_check   CHECK (pg_column_size(metadata) <= 4096),
    -- The same reading submitted twice under different requests is a conflict.
    CONSTRAINT measurements_patient_type_time_source_key UNIQUE (patient_id, type, recorded_at, source)
);

-- Access paths required by the specification: (patient_id, recorded_at) and
-- (patient_id, type, recorded_at). The second is the leading prefix of the
-- unique constraint's index above, which serves it; a separate index would
-- only add write cost on the hottest table.
CREATE INDEX measurements_patient_id_recorded_at_idx ON measurements (patient_id, recorded_at);

CREATE FUNCTION measurements_validate() RETURNS trigger
    LANGUAGE plpgsql
AS $$
DECLARE
    mt measurement_types%ROWTYPE;
BEGIN
    SELECT * INTO mt FROM measurement_types WHERE code = NEW.type;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'unknown measurement type %', NEW.type
            USING ERRCODE = 'foreign_key_violation', CONSTRAINT = 'measurements_type_fkey';
    END IF;
    IF NEW.unit <> mt.canonical_unit THEN
        RAISE EXCEPTION 'measurement type % requires unit %, got %', NEW.type, mt.canonical_unit, NEW.unit
            USING ERRCODE = 'check_violation', CONSTRAINT = 'measurements_unit_check';
    END IF;
    IF NEW.value < mt.min_value OR NEW.value > mt.max_value THEN
        RAISE EXCEPTION 'measurement type % value % is outside the technical range % to %',
            NEW.type, NEW.value, mt.min_value, mt.max_value
            USING ERRCODE = 'check_violation', CONSTRAINT = 'measurements_value_range_check';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER measurements_validate
    BEFORE INSERT OR UPDATE ON measurements
    FOR EACH ROW EXECUTE FUNCTION measurements_validate();
