-- The measurement types the platform accepts, each with its canonical unit
-- and technical plausibility range. The ranges reject values that cannot be
-- a reading at all; they are data validation, not medical thresholds.
-- Adding a type is a data migration, not a schema change.
CREATE TABLE measurement_types (
    code           text             PRIMARY KEY,
    canonical_unit text             NOT NULL,
    min_value      double precision NOT NULL,
    max_value      double precision NOT NULL,
    CONSTRAINT measurement_types_code_check  CHECK (code <> '' AND code = upper(code)),
    CONSTRAINT measurement_types_unit_check  CHECK (canonical_unit <> ''),
    CONSTRAINT measurement_types_range_check CHECK (min_value < max_value)
);

INSERT INTO measurement_types (code, canonical_unit, min_value, max_value) VALUES
    ('HEART_RATE',               'bpm',         0,  300),
    ('BLOOD_PRESSURE_SYSTOLIC',  'mmHg',        0,  300),
    ('BLOOD_PRESSURE_DIASTOLIC', 'mmHg',        0,  200),
    ('SPO2',                     '%',           0,  100),
    ('BODY_TEMPERATURE',         'C',          20,   45),
    ('BLOOD_GLUCOSE',            'mg/dL',       0, 1000),
    ('RESPIRATORY_RATE',         'breaths/min', 0,  100);
