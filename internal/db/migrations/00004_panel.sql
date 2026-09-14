-- +goose Up

-- What the drivers, devices and data pages need on top of the settings
-- migration. One stored column and four indexes, and every one of them is here
-- because a page an operator opens would otherwise scan a table that grows with
-- every lap ever driven.

-- --------------------------------------------------------------------------
-- How big a trace is, kept beside it
-- --------------------------------------------------------------------------

-- "Storage used, per driver" and "what would retention delete today" are both
-- sums of length(trace) over a set of laps. Computed from the blob, that is a
-- heap read per lap, and the blob is the whole reason the table is large.
-- Stored, it is four bytes an index can carry, so both questions are answered
-- from an index and neither one fetches a trace.
--
-- GENERATED ALWAYS rather than a column the writer maintains: it cannot drift
-- from the blob beside it, and clearing a trace zeroes it in the same statement
-- without anyone remembering to. The COPY that ingests laps names its columns,
-- so it is unaffected.
--
-- Adding it rewrites the table once, on the laps an installation already holds.
ALTER TABLE laps
    ADD COLUMN trace_bytes integer NOT NULL GENERATED ALWAYS AS (length(trace)) STORED;

-- The roster page's numbers: laps, bytes and the last upload, per driver. With
-- trace_bytes and created_at carried in the index this is one index-only scan
-- and no heap access at all, which is what keeps a driver with ten thousand
-- laps from being ten thousand random reads when an operator opens the page.
CREATE INDEX laps_driver_rollup ON laps (driver_id) INCLUDE (trace_bytes, created_at);

-- The retention preview and the prune read exactly the same set: laps older
-- than a cutoff that still hold a trace. Partial on trace_bytes > 0 so the
-- index is the work still to do — a pruned lap leaves it, and the prune's next
-- batch does not walk past the ones it has already cleared.
CREATE INDEX laps_unpruned_started ON laps (started_at) WHERE trace_bytes > 0;

-- --------------------------------------------------------------------------
-- Ordering the two smaller pages
-- --------------------------------------------------------------------------

-- The devices page is newest first and pages by keyset, so the ordering it asks
-- for is the ordering the index holds.
CREATE INDEX devices_created_at ON devices (created_at DESC, id DESC);

-- The roster's default ordering. drivers is small, but the keyset page reads
-- the first few rows of this order and an index means it reads only those.
CREATE INDEX drivers_name ON drivers (name, id);

-- --------------------------------------------------------------------------
-- The roster, as one row per driver
-- --------------------------------------------------------------------------

-- Every number the drivers page and the data page show about a driver comes
-- from here, and it is a view rather than three copies of the same subqueries
-- because the roster is read three ways — by name, by last seen, by laps — and
-- the only thing that differs between them is the ORDER BY.
--
-- The aggregates are grouped rather than correlated: one pass per source table
-- for the whole roster, not one lookup per driver. That is the difference
-- between a page that costs the same whether a league has five drivers or fifty
-- and one that does not.
--
-- seen_key exists beside last_seen_at because a driver who has never connected
-- has no last-seen moment, and a keyset page cannot compare against null.
-- last_seen_at is what the page prints; seen_key is what it orders and pages by.
CREATE VIEW driver_roster AS
SELECT d.id,
       d.name,
       d.slug,
       d.class,
       d.created_at,
       coalesce(l.laps, 0)::bigint                    AS laps,
       coalesce(l.trace_bytes, 0)::bigint             AS trace_bytes,
       l.last_upload_at::timestamptz                  AS last_upload_at,
       coalesce(s.stints, 0)::bigint                  AS stints,
       coalesce(v.devices, 0)::bigint                 AS devices,
       coalesce(v.revoked, 0)::bigint                 AS revoked_devices,
       v.last_seen_at::timestamptz                    AS last_seen_at,
       coalesce(v.last_seen_at, 'epoch'::timestamptz)::timestamptz AS seen_key
  FROM drivers d
  LEFT JOIN (
       SELECT driver_id,
              count(*)                AS laps,
              sum(trace_bytes)::bigint AS trace_bytes,
              max(created_at)         AS last_upload_at
         FROM laps GROUP BY driver_id) l ON l.driver_id = d.id
  LEFT JOIN (
       SELECT driver_id, count(*) AS stints
         FROM stints GROUP BY driver_id) s ON s.driver_id = d.id
  LEFT JOIN (
       SELECT driver_id,
              count(*) FILTER (WHERE revoked_at IS NULL) AS devices,
              count(*) FILTER (WHERE revoked_at IS NOT NULL) AS revoked,
              max(last_used_at) AS last_seen_at
         FROM devices GROUP BY driver_id) v ON v.driver_id = d.id;

-- +goose Down
DROP VIEW IF EXISTS driver_roster;
DROP INDEX IF EXISTS drivers_name;
DROP INDEX IF EXISTS devices_created_at;
DROP INDEX IF EXISTS laps_unpruned_started;
DROP INDEX IF EXISTS laps_driver_rollup;
ALTER TABLE laps DROP COLUMN IF EXISTS trace_bytes;
