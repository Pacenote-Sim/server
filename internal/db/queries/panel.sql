-- What the drivers, devices and data pages read. Three rules hold across all of
-- them, because an admin page is loaded by a person waiting for it:
--
--   * one query per page, never one per row. Every count, sum and "last seen"
--     on a page comes back from a single statement, so the cost of the page
--     does not grow with the number of rows on it;
--   * keyset pagination, never OFFSET. A cursor is the last row's sort key, so
--     page fifty costs what page one costs and a row inserted mid-read cannot
--     shuffle a driver onto two pages or off both;
--   * the first page says so with a boolean rather than with a sentinel value,
--     because there is no value of a name or a timestamp that reliably sorts
--     before every real one.

-- --------------------------------------------------------------------------
-- The roster
-- --------------------------------------------------------------------------

-- Three orderings of one view, spelled out rather than assembled from a sort
-- parameter: the ORDER BY and the keyset predicate have to agree on direction
-- and on tie-break, and a CASE that got that wrong would page silently past
-- rows rather than fail.

-- name: DriversByName :many
SELECT r.* FROM driver_roster r
 WHERE sqlc.arg(first)::boolean
    OR (r.name, r.id) > (sqlc.arg(after_name)::text, sqlc.arg(after_id)::bigint)
 ORDER BY r.name, r.id
 LIMIT sqlc.arg(lim);

-- name: DriversByLastSeen :many
SELECT r.* FROM driver_roster r
 WHERE sqlc.arg(first)::boolean
    OR (r.seen_key, r.id) < (sqlc.arg(after_seen)::timestamptz, sqlc.arg(after_id)::bigint)
 ORDER BY r.seen_key DESC, r.id DESC
 LIMIT sqlc.arg(lim);

-- name: DriversByLaps :many
SELECT r.* FROM driver_roster r
 WHERE sqlc.arg(first)::boolean
    OR (r.laps, r.id) < (sqlc.arg(after_laps)::bigint, sqlc.arg(after_id)::bigint)
 ORDER BY r.laps DESC, r.id DESC
 LIMIT sqlc.arg(lim);

-- name: DriverRosterByID :one
SELECT r.* FROM driver_roster r WHERE r.id = $1;

-- --------------------------------------------------------------------------
-- A driver's stints, and a stint's laps
-- --------------------------------------------------------------------------

-- The lateral is per stint on the page and not per stint in the table: the
-- keyset above it has already cut the set to one page, so this reads the laps
-- of twenty-odd stints through laps (stint_id, number) and stops.

-- name: StintsForDriver :many
SELECT s.id, s.track, s.car, s.car_class, s.session_type, s.started_at, s.finished_at,
       coalesce(l.laps, 0)::bigint        AS laps,
       coalesce(l.best_ms, 0)::int        AS best_lap_ms,
       coalesce(l.trace_bytes, 0)::bigint AS trace_bytes
  FROM stints s
  LEFT JOIN LATERAL (
       SELECT count(*)                                   AS laps,
              min(lap_ms) FILTER (WHERE kind = 'clean')  AS best_ms,
              sum(trace_bytes)                           AS trace_bytes
         FROM laps WHERE stint_id = s.id) l ON true
 WHERE s.driver_id = sqlc.arg(driver_id)
   AND (sqlc.arg(first)::boolean
        OR (s.started_at, s.id) < (sqlc.arg(after_started)::timestamptz, sqlc.arg(after_id)::uuid))
 ORDER BY s.started_at DESC, s.id DESC
 LIMIT sqlc.arg(lim);

-- name: StintForPanel :one
SELECT s.id, s.driver_id, d.name AS driver_name, s.sim, s.track, s.track_id,
       s.car, s.car_class, s.session_type, s.started_at, s.finished_at,
       coalesce(l.laps, 0)::bigint        AS laps,
       coalesce(l.best_ms, 0)::int        AS best_lap_ms,
       coalesce(l.trace_bytes, 0)::bigint AS trace_bytes
  FROM stints s
  JOIN drivers d ON d.id = s.driver_id
  LEFT JOIN LATERAL (
       SELECT count(*)                                   AS laps,
              min(lap_ms) FILTER (WHERE kind = 'clean')  AS best_ms,
              sum(trace_bytes)                           AS trace_bytes
         FROM laps WHERE stint_id = s.id) l ON true
 WHERE s.id = $1;

-- Lap numbers are unique within a stint and start at zero, so the cursor is the
-- last number rendered and -1 is "from the beginning". No boolean is needed
-- here: there is a value below every real one, which is the case the roster
-- queries above do not have.
-- name: LapsForStint :many
SELECT id, number, lap_ms, kind, started_at, trace_bytes, created_at
  FROM laps
 WHERE stint_id = sqlc.arg(stint_id) AND number > sqlc.arg(after_number)::int
 ORDER BY number
 LIMIT sqlc.arg(lim);

-- --------------------------------------------------------------------------
-- Devices
-- --------------------------------------------------------------------------

-- name: DevicesPage :many
SELECT v.id, v.driver_id, d.name AS driver_name, v.token_prefix, v.label,
       v.created_at, v.last_used_at, v.revoked_at
  FROM devices v
  JOIN drivers d ON d.id = v.driver_id
 WHERE sqlc.arg(first)::boolean
    OR (v.created_at, v.id) < (sqlc.arg(after_created)::timestamptz, sqlc.arg(after_id)::bigint)
 ORDER BY v.created_at DESC, v.id DESC
 LIMIT sqlc.arg(lim);

-- name: DeviceForPanel :one
SELECT v.id, v.driver_id, d.name AS driver_name, v.token_prefix, v.label,
       v.created_at, v.last_used_at, v.revoked_at
  FROM devices v
  JOIN drivers d ON d.id = v.driver_id
 WHERE v.id = $1;

-- name: DevicesForDriverPage :many
SELECT v.id, v.driver_id, d.name AS driver_name, v.token_prefix, v.label,
       v.created_at, v.last_used_at, v.revoked_at
  FROM devices v
  JOIN drivers d ON d.id = v.driver_id
 WHERE v.driver_id = sqlc.arg(driver_id)
 ORDER BY v.created_at DESC, v.id DESC
 LIMIT sqlc.arg(lim);

-- Revoking is scoped to one driver and touches nothing else, which is the whole
-- point of the lost-laptop action: it is not the danger zone's "revoke
-- everything", and no other driver stops uploading because of it.
-- name: RevokeDevicesForDriver :execrows
UPDATE devices SET revoked_at = now() WHERE driver_id = $1 AND revoked_at IS NULL;

-- name: CountLiveDevicesForDriver :one
SELECT count(*)::bigint FROM devices WHERE driver_id = $1 AND revoked_at IS NULL;

-- name: CountAllDevices :one
SELECT count(*)::bigint AS total,
       (count(*) FILTER (WHERE revoked_at IS NULL))::bigint AS live
  FROM devices;

-- --------------------------------------------------------------------------
-- What is stored, and what it costs
-- --------------------------------------------------------------------------

-- Every figure on the data page, in one statement. The three scalar subqueries
-- are each a single aggregate over a small table; the rest is one pass over
-- laps, which is the table the page is actually about.
-- name: DataTotals :one
SELECT pg_database_size(current_database())::bigint            AS database_bytes,
       (SELECT count(*) FROM drivers)::bigint                  AS drivers,
       (SELECT count(*) FROM stints)::bigint                   AS stints,
       (SELECT min(started_at) FROM stints)::timestamptz       AS oldest_stint_at,
       count(*)::bigint                                        AS laps,
       (count(*) FILTER (WHERE trace_bytes > 0))::bigint       AS traces,
       coalesce(sum(trace_bytes), 0)::bigint                   AS trace_bytes,
       min(started_at)::timestamptz                            AS oldest_lap_at
  FROM laps;

-- name: StorageByDriver :many
SELECT r.id, r.name, r.laps, r.trace_bytes
  FROM driver_roster r
 ORDER BY r.trace_bytes DESC, r.id
 LIMIT $1;

-- The retention preview and the prune read one set through one index: laps
-- older than the cutoff that still hold a trace. They must agree exactly, or
-- the page would promise one number and the job would do another.
-- name: TracesOlderThan :one
SELECT count(*)::bigint                  AS laps,
       coalesce(sum(trace_bytes), 0)::bigint AS trace_bytes,
       min(started_at)::timestamptz      AS oldest_at,
       max(started_at)::timestamptz      AS newest_at
  FROM laps
 WHERE started_at < $1 AND trace_bytes > 0;

-- One batch of the prune. The lap row stays and so does its time: only the blob
-- goes, and trace_codec goes to zero because no codec wrote the empty trace —
-- version one is the lowest a real one carries.
--
-- The reference rows for those laps are deleted in the same statement. A
-- reference lap is fetched whole and decoded, so leaving one pointing at a
-- cleared blob would turn a driver's next coaching request into a server error;
-- deleting it makes the bucket empty, which is an ordinary answer the next
-- upload refills.
-- name: PruneTracesOlderThan :execrows
WITH doomed AS (
    SELECT id FROM laps
     WHERE started_at < sqlc.arg(before)::timestamptz AND trace_bytes > 0
     ORDER BY started_at
     LIMIT sqlc.arg(lim)
),
dropped_references AS (
    DELETE FROM reference_laps r USING doomed WHERE r.lap_id = doomed.id
)
UPDATE laps SET trace = '', trace_codec = 0
  FROM doomed
 WHERE laps.id = doomed.id;
