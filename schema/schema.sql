BEGIN TRANSACTION;

--
-- drop
--

DROP TABLE IF EXISTS record;
DROP TABLE IF EXISTS zone;

DROP TYPE IF EXISTS record_type;

--
-- raise
--

CREATE TYPE record_type AS ENUM (
    'CNAME'
);

CREATE TABLE zone (
    id              bigserial           PRIMARY KEY,
    created_at      timestamptz         NOT NULL DEFAULT NOW(),
    name            varchar(100)        NOT NULL UNIQUE,
    updated_at      timestamptz
);

CREATE TABLE record (
    id              bigserial           PRIMARY KEY,
    created_at      timestamptz         NOT NULL DEFAULT NOW(),
    name            varchar(100)        NOT NULL,
    record_type     record_type         NOT NULL,
    updated_at      timestamptz,
    zone_id         bigint              NOT NULL REFERENCES zone (id)
);

CREATE UNIQUE INDEX record_name_record_type_zone_id
    ON record (name, record_type, zone_id);

COMMIT TRANSACTION;
