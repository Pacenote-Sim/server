-- What the admin panel's overview page shows. Every one of these is a single
-- statement, because the page is loaded by someone waiting for it.

-- name: DatabaseSizeBytes :one
SELECT pg_database_size(current_database())::bigint AS bytes;

-- name: CountDrivers :one
SELECT count(*) AS count FROM drivers;

-- name: CountActiveDevices :one
SELECT count(*) AS count FROM devices WHERE revoked_at IS NULL;

-- name: CountStints :one
SELECT count(*) AS count FROM stints;

-- name: CountLaps :one
SELECT count(*) AS count FROM laps;

-- name: TraceBytes :one
SELECT coalesce(sum(length(trace)), 0)::bigint AS bytes FROM laps;
