-- +goose Up

-- The read contract a plugin sees.
--
-- A plugin keeps its own tables in its own schema and joins them to core's data.
-- That join has to read something, and the something must not be the core tables
-- themselves. Two reasons, and the second is the one that matters in a year:
--
--   1. Core tables carry credentials. devices.token_sha256, admins.password_hash,
--      admin_sessions.token_sha256 and settings.value — which holds every sealed
--      key — are all in the same database as the laps. A plugin granted SELECT on
--      the tables would be granted those too.
--   2. Core would never be able to change its own schema again. Every installed
--      plugin, including ones written by people we will never meet against a
--      version we no longer ship, would be reading columns by name. Renaming a
--      column would break them silently at the next upgrade.
--
-- So core publishes views and grants on those. The views are a contract with the
-- same weight as the wire protocol: a column here may be added, and may not be
-- renamed or removed without a plugin interface version. What is behind them can
-- change freely, which is the entire point.
--
-- The views are unprefixed inside a schema that is already named for what it is.
-- A plugin runs with search_path = plugin_<name>, core_read, so its SQL reads
-- "JOIN stints USING (id)" exactly the way core's own queries do. A v_ prefix
-- would restate the schema name in every identifier.
--
-- Nothing is granted here. The schema and the views exist; a plugin is granted
-- USAGE and SELECT at install time, one role per plugin, and that grant is the
-- host's to give and to take away.

CREATE SCHEMA IF NOT EXISTS core_read;

COMMENT ON SCHEMA core_read IS
    'Read-only view of core data for plugins. A contract: columns may be added, '
    'not renamed or removed without a plugin interface version.';

-- Every view below says security_invoker = false, which is also the default.
-- It is stated anyway because it is the property the whole design rests on: a
-- view without it runs the underlying reads as the view's owner — this server's
-- role — so granting a plugin SELECT on the view grants it nothing at all on the
-- table behind it. A default that changed, or a reader who assumed the other
-- behaviour, would turn the plugin boundary into a hole quietly. Stating it costs
-- one clause and makes the intent reviewable.

-- --------------------------------------------------------------------------
-- Who drove
-- --------------------------------------------------------------------------

-- Every column. A driver row holds no credential — the tokens are on devices,
-- which is not published at all — and external_id is the operator's own
-- identifier for the person, which is the column a league plugin needs in order
-- to match a driver to an account it already has.
CREATE VIEW core_read.drivers WITH (security_invoker = false) AS
SELECT id,
       external_id,
       name,
       slug,
       class,
       avatar_url,
       created_at
FROM drivers;

-- --------------------------------------------------------------------------
-- A sitting
-- --------------------------------------------------------------------------

-- Every column, including setup. The setup is null when the simulator published
-- none, and that null is load-bearing: it says we were not told, which is not the
-- same as a car with nothing changed.
CREATE VIEW core_read.stints WITH (security_invoker = false) AS
SELECT id,
       driver_id,
       sim,
       track_id,
       track,
       car,
       car_class,
       session_type,
       sectors,
       setup,
       started_at,
       finished_at,
       created_at
FROM stints;

-- --------------------------------------------------------------------------
-- A lap
-- --------------------------------------------------------------------------

-- Without the trace. trace is a compressed blob whose layout is a codec version
-- in a neighbouring column, and handing it to a plugin would make every plugin
-- that touched it depend on a codec core intends to keep changing. A plugin that
-- needs a trace asks the host, which decodes it and hands over points.
--
-- trace_bytes stays, because it answers the only question a plugin has about a
-- trace it cannot read: whether there is one. Zero means the lap arrived without.
--
-- content_sha256 is gone. It is how the server decides two uploads of lap 7 are
-- the same lap, which is core's business and means nothing outside it.
--
-- corners stays, and is most of the reason this view exists. It is the lap's
-- analysis: where the time went, per corner, already computed.
CREATE VIEW core_read.laps WITH (security_invoker = false) AS
SELECT id,
       stint_id,
       driver_id,
       number,
       lap_ms,
       kind,
       sim,
       track_id,
       car,
       car_class,
       corners,
       trace_bytes,
       started_at,
       created_at
FROM laps;

-- --------------------------------------------------------------------------
-- How the sitting went
-- --------------------------------------------------------------------------

-- Without best_trace, for the reason the lap view has no trace.
CREATE VIEW core_read.stint_summaries WITH (security_invoker = false) AS
SELECT stint_id,
       laps,
       incidents,
       best_lap_ms,
       avg_lap_ms,
       consistency_pct,
       top_speed_kmh,
       conditions,
       car_state,
       best_lap_id,
       finished_at,
       updated_at
FROM stint_summaries;

-- --------------------------------------------------------------------------
-- What a lap is measured against
-- --------------------------------------------------------------------------

-- Every column. A plugin comparing a driver to the reference needs the same row
-- the coach uses, or it will quietly disagree with the cue the driver heard.
CREATE VIEW core_read.reference_laps WITH (security_invoker = false) AS
SELECT scope,
       sim,
       track_id,
       car,
       car_class,
       driver_id,
       lap_id,
       lap_ms,
       updated_at
FROM reference_laps;

-- --------------------------------------------------------------------------
-- Nothing else is published
-- --------------------------------------------------------------------------

-- Named so that leaving one out later is a decision rather than an oversight:
--
--   admins, admin_sessions  password hashes and session tokens
--   devices, pairings       device tokens and pairing codes
--   settings                every sealed credential the server holds
--   plugins, plugin_*       another plugin's settings, including its secrets
--   audit_log               who did what, which is the operator's record and not
--                           a plugin's to read
--   idempotency_keys        core's own bookkeeping
--   client_builds           build artefacts and their signing state
--   llm_usage, setup_state  core's own bookkeeping

-- --------------------------------------------------------------------------
-- The public schema stops being writable by everybody
-- --------------------------------------------------------------------------

-- PostgreSQL 15 stopped granting CREATE on the public schema to PUBLIC, and 15
-- is this server's floor, so on a database created by a modern PostgreSQL this
-- line changes nothing.
--
-- It is here for the database that was created under PostgreSQL 14 and then
-- pg_upgraded. pg_upgrade preserves the public schema's existing privileges, so
-- such a database still grants CREATE to PUBLIC however new the server reporting
-- its version now is — and every plugin role would be able to create tables in
-- the public schema beside core's, where a DROP SCHEMA on uninstall would never
-- find them.
--
-- Core connects as the owner of its own objects and does not reach them through
-- PUBLIC, so nothing of ours depends on this grant.
REVOKE CREATE ON SCHEMA public FROM PUBLIC;

-- +goose Down
DROP VIEW IF EXISTS core_read.reference_laps;
DROP VIEW IF EXISTS core_read.stint_summaries;
DROP VIEW IF EXISTS core_read.laps;
DROP VIEW IF EXISTS core_read.stints;
DROP VIEW IF EXISTS core_read.drivers;
DROP SCHEMA IF EXISTS core_read;
-- The public-schema grant is deliberately not restored. Putting CREATE back for
-- every role in the database, on the way down from a migration, would be a
-- security regression performed in the name of tidiness.
