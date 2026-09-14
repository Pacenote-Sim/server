-- +goose Up

-- The schema this server was designed around, with the indexes it needs.
--
-- Three tables are here that the sketch does not list, and each is here for a
-- reason worth stating:
--
--   setup_state     one row, written in the same transaction as the first
--                   administrator. It is what makes setup a one-time event:
--                   the database decides whether the wizard may run, not the
--                   presence of a directory on disk.
--   admins          the sketch has no administrator table because D-2 is about
--                   drivers, who have no passwords. The admin panel does need
--                   an account, so it gets its own table and its own hashing.
--   admin_sessions  panel sessions, stored as digests like every other token.
--
-- Two shapes differ from the sketch, both noted where they occur: laps carries
-- the three reference-lookup columns so the partial index the sketch asks for
-- can exist, and reference_laps keys the "self" scope by driver.

-- --------------------------------------------------------------------------
-- Setup and the admin panel
-- --------------------------------------------------------------------------

-- A single row, and the primary key is what enforces that: id is a boolean
-- constrained to true, so a second INSERT cannot succeed. Two operators who
-- press "finish" at the same moment therefore contend on this key, one wins,
-- and the loser's whole transaction — including its administrator — rolls back.
CREATE TABLE setup_state (
    id            boolean     PRIMARY KEY DEFAULT true CHECK (id),
    completed_at  timestamptz NOT NULL DEFAULT now(),
    completed_by  text        NOT NULL,
    server_version text       NOT NULL DEFAULT ''
);

CREATE TABLE admins (
    id            bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    email         text        NOT NULL,
    password_hash text        NOT NULL,
    created_at    timestamptz NOT NULL DEFAULT now(),
    last_login_at timestamptz
);

-- Sign-in is by lower-cased email, so uniqueness has to be too: otherwise
-- "Ana@example.com" and "ana@example.com" are two accounts and one of them can
-- never sign in.
CREATE UNIQUE INDEX admins_email_key ON admins (lower(email));

CREATE TABLE admin_sessions (
    id           bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    admin_id     bigint      NOT NULL REFERENCES admins (id) ON DELETE CASCADE,
    token_sha256 bytea       NOT NULL UNIQUE,
    created_at   timestamptz NOT NULL DEFAULT now(),
    last_seen_at timestamptz NOT NULL DEFAULT now(),
    expires_at   timestamptz NOT NULL,
    user_agent   text        NOT NULL DEFAULT ''
);

CREATE INDEX admin_sessions_expires_at ON admin_sessions (expires_at);

-- key/value, per the sketch: branding, features, limits and the encrypted API
-- key. jsonb rather than text so a setting can be a document without a second
-- table appearing every time one grows.
CREATE TABLE settings (
    key        text        PRIMARY KEY,
    value      jsonb       NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- --------------------------------------------------------------------------
-- Drivers, their machines, and how a machine is paired
-- --------------------------------------------------------------------------

CREATE TABLE drivers (
    id          bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    external_id text        NOT NULL DEFAULT '',
    name        text        NOT NULL,
    slug        text        NOT NULL UNIQUE,
    class       text        NOT NULL DEFAULT '',
    avatar_url  text        NOT NULL DEFAULT '',
    created_at  timestamptz NOT NULL DEFAULT now()
);

-- D-2: only the digest is stored, with a short clear-text prefix so a lookup is
-- an index seek. A dump of this table contains no usable credential.
CREATE TABLE devices (
    id           bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    driver_id    bigint      NOT NULL REFERENCES drivers (id) ON DELETE CASCADE,
    token_sha256 bytea       NOT NULL UNIQUE,
    token_prefix text        NOT NULL,
    label        text        NOT NULL DEFAULT '',
    created_at   timestamptz NOT NULL DEFAULT now(),
    last_used_at timestamptz,
    revoked_at   timestamptz
);

CREATE INDEX devices_token_prefix ON devices (token_prefix);
CREATE INDEX devices_driver_id ON devices (driver_id);

-- The device code is a secret the client polls with, so it is stored the same
-- way a device token is. The user code is the short one the driver reads aloud.
CREATE TABLE pairings (
    id                 bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    device_code_sha256 bytea       NOT NULL UNIQUE,
    user_code          text        NOT NULL,
    status             text        NOT NULL DEFAULT 'pending',
    driver_id          bigint      REFERENCES drivers (id) ON DELETE SET NULL,
    created_at         timestamptz NOT NULL DEFAULT now(),
    expires_at         timestamptz NOT NULL,
    CONSTRAINT pairings_status_check
        CHECK (status IN ('pending', 'approved', 'denied', 'expired'))
);

-- The sketch asks for this index partial on "unexpired". A predicate over now()
-- is not immutable and Postgres will not index it, so the partial predicate is
-- status = 'pending' instead: the same rows in practice, because a poll marks
-- an expired pairing expired, and a lookup still reads one index entry.
CREATE INDEX pairings_pending ON pairings (user_code) WHERE status = 'pending';

-- --------------------------------------------------------------------------
-- Driving data
-- --------------------------------------------------------------------------

CREATE TABLE stints (
    id           uuid        PRIMARY KEY,
    driver_id    bigint      NOT NULL REFERENCES drivers (id) ON DELETE CASCADE,
    sim          text        NOT NULL,
    track_id     text        NOT NULL,
    track        text        NOT NULL,
    car          text        NOT NULL,
    car_class    text        NOT NULL,
    session_type text        NOT NULL,
    sectors      real[]      NOT NULL DEFAULT '{}',
    started_at   timestamptz NOT NULL,
    finished_at  timestamptz,
    created_at   timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT stints_session_type_check
        CHECK (session_type IN ('practice', 'qualifying', 'race', 'testing'))
);

CREATE INDEX stints_driver_started ON stints (driver_id, started_at DESC);

-- trace is the compressed binary blob of D-1, written with binary parameters.
-- trace_codec is its format version, in a neighbouring smallint so the codec can
-- change without a migration.
--
-- track_id, car and car_class are copied down from the stint. They are the one
-- denormalisation on this table and they exist so the reference index below can
-- be a single partial index on laps rather than a join with stints on every
-- lookup — a query that runs once per lap while a driver waits to hear a cue.
CREATE TABLE laps (
    id             bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    stint_id       uuid        NOT NULL REFERENCES stints (id) ON DELETE CASCADE,
    number         integer     NOT NULL,
    lap_ms         integer     NOT NULL,
    kind           text        NOT NULL,
    started_at     timestamptz NOT NULL,
    track_id       text        NOT NULL,
    car            text        NOT NULL,
    car_class      text        NOT NULL,
    driver_id      bigint      NOT NULL REFERENCES drivers (id) ON DELETE CASCADE,
    content_sha256 bytea       NOT NULL,
    trace_codec    smallint    NOT NULL,
    trace          bytea       NOT NULL,
    created_at     timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT laps_kind_check CHECK (kind IN ('clean', 'in', 'out', 'invalid')),
    CONSTRAINT laps_lap_ms_check CHECK (lap_ms > 0),
    CONSTRAINT laps_number_check CHECK (number >= 0),
    UNIQUE (stint_id, number)
);

-- The sketch's reference index. UNIQUE (stint_id, number) already provides the
-- laps (stint_id, number) index it also names.
CREATE INDEX laps_reference ON laps (track_id, car_class, lap_ms) WHERE kind = 'clean';
CREATE INDEX laps_driver_reference ON laps (driver_id, track_id, car, lap_ms) WHERE kind = 'clean';

CREATE TABLE stint_summaries (
    stint_id        uuid        PRIMARY KEY REFERENCES stints (id) ON DELETE CASCADE,
    laps            integer     NOT NULL DEFAULT 0,
    incidents       integer     NOT NULL DEFAULT 0,
    best_lap_ms     integer     NOT NULL DEFAULT 0,
    avg_lap_ms      integer     NOT NULL DEFAULT 0,
    consistency_pct integer     NOT NULL DEFAULT 0,
    top_speed_kmh   integer     NOT NULL DEFAULT 0,
    conditions      jsonb       NOT NULL DEFAULT '{}',
    car_state       jsonb       NOT NULL DEFAULT '{}',
    best_lap_id     bigint      REFERENCES laps (id) ON DELETE SET NULL,
    finished_at     timestamptz,
    updated_at      timestamptz NOT NULL DEFAULT now()
);

-- The one denormalisation of the sketch: the best lap per bucket, maintained on
-- lap insert, so the coach's request is a key lookup rather than a sort over
-- every lap ever driven.
--
-- The sketch keys this (scope, track_id, car, car_class). That is right for the
-- "car" and "class" scopes and wrong for "self", where there is one best per
-- driver rather than one overall — so driver_id is part of the key for that
-- scope and null for the others. Two partial unique indexes rather than one
-- primary key is what expresses that; either way a lookup reads one index entry.
CREATE TABLE reference_laps (
    scope      text        NOT NULL,
    track_id   text        NOT NULL,
    car        text        NOT NULL,
    car_class  text        NOT NULL,
    driver_id  bigint      REFERENCES drivers (id) ON DELETE CASCADE,
    lap_id     bigint      NOT NULL REFERENCES laps (id) ON DELETE CASCADE,
    lap_ms     integer     NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT reference_laps_scope_check CHECK (scope IN ('self', 'car', 'class')),
    CONSTRAINT reference_laps_self_has_driver
        CHECK ((scope = 'self') = (driver_id IS NOT NULL))
);

CREATE UNIQUE INDEX reference_laps_bucket
    ON reference_laps (scope, track_id, car, car_class) WHERE driver_id IS NULL;
CREATE UNIQUE INDEX reference_laps_driver_bucket
    ON reference_laps (driver_id, scope, track_id, car, car_class) WHERE driver_id IS NOT NULL;

-- D-6: idempotency is a table. The first request inserts and executes in one
-- transaction, a repeat with the same hash replays the stored response, and a
-- different hash is a conflict.
CREATE TABLE idempotency_keys (
    key           text        PRIMARY KEY,
    request_hash  bytea       NOT NULL,
    status        integer     NOT NULL,
    response_body bytea       NOT NULL,
    created_at    timestamptz NOT NULL DEFAULT now(),
    expires_at    timestamptz NOT NULL
);

CREATE INDEX idempotency_keys_expires_at ON idempotency_keys (expires_at);

CREATE TABLE audit_log (
    id      bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    at      timestamptz NOT NULL DEFAULT now(),
    actor   text        NOT NULL,
    action  text        NOT NULL,
    subject text        NOT NULL DEFAULT '',
    detail  jsonb       NOT NULL DEFAULT '{}'
);

CREATE INDEX audit_log_at ON audit_log (at DESC);


-- +goose Down
DROP TABLE IF EXISTS audit_log;
DROP TABLE IF EXISTS idempotency_keys;
DROP TABLE IF EXISTS reference_laps;
DROP TABLE IF EXISTS stint_summaries;
DROP TABLE IF EXISTS laps;
DROP TABLE IF EXISTS stints;
DROP TABLE IF EXISTS pairings;
DROP TABLE IF EXISTS devices;
DROP TABLE IF EXISTS drivers;
DROP TABLE IF EXISTS settings;
DROP TABLE IF EXISTS admin_sessions;
DROP TABLE IF EXISTS admins;
DROP TABLE IF EXISTS setup_state;
