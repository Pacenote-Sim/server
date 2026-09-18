-- The queries API v1 runs. Everything here is a single statement against an
-- index; the parts of the ingest path that cannot be written this way — the
-- temporary table a COPY lands in, which sqlc cannot see because it exists only
-- inside one transaction — are in laps.go beside the code that runs them.

-- --------------------------------------------------------------------------
-- Pairing
-- --------------------------------------------------------------------------

-- name: CreatePairing :one
INSERT INTO pairings (device_code_sha256, user_code, expires_at)
VALUES ($1, $2, $3)
RETURNING id, user_code, created_at, expires_at;

-- name: PairingByDeviceCode :one
SELECT p.id, p.user_code, p.status, p.driver_id, p.device_id, p.created_at, p.expires_at
FROM pairings p
WHERE p.device_code_sha256 = $1;

-- name: ListPendingPairings :many
SELECT id, user_code, created_at, expires_at
FROM pairings
WHERE status = 'pending' AND expires_at > now()
ORDER BY created_at;

-- name: DecidePairing :execrows
UPDATE pairings
SET status = $2, driver_id = $3, decided_at = now(), decided_by = $4
WHERE id = $1 AND status = 'pending' AND expires_at > now();

-- name: ExpirePairing :exec
UPDATE pairings SET status = 'expired' WHERE id = $1 AND status = 'pending';

-- name: AttachPairingDevice :exec
UPDATE pairings SET device_id = $2 WHERE id = $1 AND device_id IS NULL;

-- name: DeleteFinishedPairings :execrows
DELETE FROM pairings WHERE expires_at < $1;

-- --------------------------------------------------------------------------
-- Drivers
-- --------------------------------------------------------------------------

-- name: DriverByID :one
SELECT id, external_id, name, slug, class, avatar_url, created_at
FROM drivers WHERE id = $1;

-- name: DriverBySlug :one
SELECT id, external_id, name, slug, class, avatar_url, created_at
FROM drivers WHERE slug = $1;

-- name: ListDrivers :many
SELECT id, external_id, name, slug, class, avatar_url, created_at
FROM drivers ORDER BY name;

-- --------------------------------------------------------------------------
-- Stints
-- --------------------------------------------------------------------------

-- The DO UPDATE carries a WHERE on the owner, so a stint id that already belongs
-- to another driver updates nothing and returns no row. The caller reads that as
-- "no such stint", which is the right answer: a client must not learn that
-- somebody else's identifier exists.
--
-- setup is updated like every other mutable field, and a client that sends the
-- stint again without one clears it. That is the right way round: the setup a
-- stint was driven on is whatever the client last said it was, and a client
-- that has stopped seeing one is telling us something.
-- name: UpsertStint :one
INSERT INTO stints (id, driver_id, sim, track_id, track, car, car_class, session_type, sectors, started_at, setup)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
ON CONFLICT (id) DO UPDATE SET
    sim          = EXCLUDED.sim,
    track_id     = EXCLUDED.track_id,
    track        = EXCLUDED.track,
    car          = EXCLUDED.car,
    car_class    = EXCLUDED.car_class,
    session_type = EXCLUDED.session_type,
    sectors      = EXCLUDED.sectors,
    started_at   = EXCLUDED.started_at,
    setup        = EXCLUDED.setup
WHERE stints.driver_id = EXCLUDED.driver_id
RETURNING id, driver_id;

-- The sim comes back with the rest of the identity because every lap written
-- against this stint copies it down, and because the reference lookup the coach
-- runs is scoped by it.
--
-- The setup comes back with it because the facts a finished stint produces
-- carry it, and that read happens inside the same transaction as the write it
-- belongs to.
-- name: StintForDriver :one
SELECT id, driver_id, sim, track_id, track, car, car_class, session_type, started_at, finished_at, setup
FROM stints WHERE id = $1 AND driver_id = $2;

-- name: BestCleanLapMs :one
SELECT COALESCE(min(lap_ms), 0)::int FROM laps WHERE stint_id = $1 AND kind = 'clean';

-- name: CountLapsForStint :one
SELECT count(*) FROM laps WHERE stint_id = $1;

-- --------------------------------------------------------------------------
-- Summaries
-- --------------------------------------------------------------------------

-- Whole-document replace, so a retry can never merge two halves of two
-- different states: every column is in the update list.
-- name: ReplaceStintSummary :exec
INSERT INTO stint_summaries (
    stint_id, laps, incidents, best_lap_ms, avg_lap_ms, consistency_pct,
    top_speed_kmh, conditions, car_state, best_trace, best_trace_codec,
    finished_at, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, now())
ON CONFLICT (stint_id) DO UPDATE SET
    laps             = EXCLUDED.laps,
    incidents        = EXCLUDED.incidents,
    best_lap_ms      = EXCLUDED.best_lap_ms,
    avg_lap_ms       = EXCLUDED.avg_lap_ms,
    consistency_pct  = EXCLUDED.consistency_pct,
    top_speed_kmh    = EXCLUDED.top_speed_kmh,
    conditions       = EXCLUDED.conditions,
    car_state        = EXCLUDED.car_state,
    best_trace       = EXCLUDED.best_trace,
    best_trace_codec = EXCLUDED.best_trace_codec,
    finished_at      = EXCLUDED.finished_at,
    updated_at       = now();

-- name: FinishStint :exec
UPDATE stints SET finished_at = $2 WHERE id = $1;

-- --------------------------------------------------------------------------
-- Reference laps
-- --------------------------------------------------------------------------

-- Three index lookups and a sort of at most three rows. Each branch reads one
-- entry of one of the two partial unique indexes on reference_laps, so the cost
-- does not grow with the number of laps ever driven — which is the whole reason
-- the table is maintained on insert.
--
-- The preference array decides the winner: the requested scope first, then the
-- narrower ones, so a server with no class data still answers with the driver's
-- own best rather than a 404.
--
-- Every branch is scoped by the simulator, and that is not an optimisation.
-- Two simulators agree about the slug of a circuit and the name of a car and
-- agree about nothing underneath, so a lap from one is not a reference for a
-- lap from the other and a lookup that crossed that line would answer with a
-- time nobody driving this simulator has ever set.
-- name: ReferenceLap :one
WITH candidates AS (
    SELECT 'self'::text AS scope, r.lap_id, r.lap_ms FROM reference_laps r
     WHERE r.scope = 'self' AND r.driver_id = @driver_id::bigint AND r.sim = @sim::text
       AND r.track_id = @track_id::text AND r.car = @car::text AND r.car_class = @car_class::text
    UNION ALL
    SELECT 'car'::text, r.lap_id, r.lap_ms FROM reference_laps r
     WHERE r.scope = 'car' AND r.driver_id IS NULL AND r.sim = @sim::text
       AND r.track_id = @track_id::text AND r.car = @car::text AND r.car_class = ''
    UNION ALL
    SELECT 'class'::text, r.lap_id, r.lap_ms FROM reference_laps r
     WHERE r.scope = 'class' AND r.driver_id IS NULL AND r.sim = @sim::text
       AND r.track_id = @track_id::text AND r.car = '' AND r.car_class = @car_class::text
),
pick AS (
    SELECT c.scope, c.lap_id, c.lap_ms FROM candidates c
     WHERE c.scope = ANY(@preference::text[])
     ORDER BY array_position(@preference::text[], c.scope)
     LIMIT 1
)
SELECT pick.scope, pick.lap_ms, l.driver_id, d.name AS driver_name,
       l.trace_codec, l.trace
FROM pick
JOIN laps l ON l.id = pick.lap_id
JOIN drivers d ON d.id = l.driver_id;

-- --------------------------------------------------------------------------
-- Idempotency
-- --------------------------------------------------------------------------

-- name: ClaimIdempotencyKey :exec
INSERT INTO idempotency_keys (device_id, key, request_hash, status, response_body, expires_at)
VALUES ($1, $2, $3, 0, '', $4);

-- name: IdempotencyKey :one
SELECT device_id, key, request_hash, status, response_body, created_at, expires_at
FROM idempotency_keys WHERE device_id = $1 AND key = $2;

-- name: FinishIdempotencyKey :exec
UPDATE idempotency_keys SET status = $3, response_body = $4
WHERE device_id = $1 AND key = $2;

-- name: DeleteExpiredIdempotencyKeys :execrows
DELETE FROM idempotency_keys WHERE expires_at < $1;
