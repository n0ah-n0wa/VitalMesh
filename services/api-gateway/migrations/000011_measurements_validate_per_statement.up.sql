-- The reading validation trigger, per statement instead of per row.
--
-- The row trigger ran a plpgsql function with a catalogue lookup for every
-- reading. Measured on a 1000-reading batch it cost 35 to 50 ms of the
-- statement's 120 ms (docs/PERFORMANCE_OPTIMIZATIONS.md); the same check as
-- one set-based query over the statement's rows costs under a millisecond.
-- The rules, the error codes and the constraint names are unchanged: an
-- unknown type is a foreign_key_violation on measurements_type_fkey, a
-- wrong unit a check_violation on measurements_unit_check, a value outside
-- the technical range a check_violation on measurements_value_range_check.
-- The per-reading insert path still gets one statement per reading, so a
-- batch that is refused is still attributed to the reading that broke the
-- rule.
DROP TRIGGER measurements_validate ON measurements;
DROP FUNCTION measurements_validate();

CREATE FUNCTION measurements_validate() RETURNS trigger
    LANGUAGE plpgsql
AS $$
DECLARE
    bad RECORD;
BEGIN
    SELECT n.type, n.value, n.unit, mt.code, mt.canonical_unit, mt.min_value, mt.max_value
    INTO bad
    FROM new_rows n
    LEFT JOIN measurement_types mt ON mt.code = n.type
    WHERE mt.code IS NULL
       OR n.unit <> mt.canonical_unit
       OR n.value < mt.min_value
       OR n.value > mt.max_value
    LIMIT 1;
    IF NOT FOUND THEN
        RETURN NULL;
    END IF;
    IF bad.code IS NULL THEN
        RAISE EXCEPTION 'unknown measurement type %', bad.type
            USING ERRCODE = 'foreign_key_violation', CONSTRAINT = 'measurements_type_fkey';
    END IF;
    IF bad.unit <> bad.canonical_unit THEN
        RAISE EXCEPTION 'measurement type % requires unit %, got %', bad.type, bad.canonical_unit, bad.unit
            USING ERRCODE = 'check_violation', CONSTRAINT = 'measurements_unit_check';
    END IF;
    RAISE EXCEPTION 'measurement type % value % is outside the technical range % to %',
        bad.type, bad.value, bad.min_value, bad.max_value
        USING ERRCODE = 'check_violation', CONSTRAINT = 'measurements_value_range_check';
END;
$$;

-- A transition table can be named for one event per trigger, so the insert
-- and the update each get one; both run the same function.
CREATE TRIGGER measurements_validate
    AFTER INSERT ON measurements
    REFERENCING NEW TABLE AS new_rows
    FOR EACH STATEMENT EXECUTE FUNCTION measurements_validate();

CREATE TRIGGER measurements_validate_update
    AFTER UPDATE ON measurements
    REFERENCING NEW TABLE AS new_rows
    FOR EACH STATEMENT EXECUTE FUNCTION measurements_validate();
