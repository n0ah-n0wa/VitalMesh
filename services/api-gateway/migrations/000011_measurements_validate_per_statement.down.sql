-- Back to the per-row form of the validation trigger (000005).
DROP TRIGGER measurements_validate ON measurements;
DROP TRIGGER measurements_validate_update ON measurements;
DROP FUNCTION measurements_validate();

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
