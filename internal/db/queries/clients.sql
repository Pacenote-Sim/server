-- The build history. Keyset pagination, newest first, over the index the
-- migration creates: page fifty costs what page one costs, and a build made
-- while an operator is reading cannot shuffle a row onto two pages or off
-- both.

-- name: InsertClientBuild :one
INSERT INTO client_builds (actor, address, signing, reference, file_name, file_bytes, sha256, client_version, signed_by)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
RETURNING *;

-- name: ClientBuildsPage :many
SELECT * FROM client_builds
 WHERE sqlc.arg(first)::boolean
    OR (at, id) < (sqlc.arg(after_at)::timestamptz, sqlc.arg(after_id)::bigint)
 ORDER BY at DESC, id DESC
 LIMIT sqlc.arg(lim);

-- name: ClientBuildByReference :one
SELECT * FROM client_builds WHERE reference = $1;

-- name: CountClientBuilds :one
SELECT count(*)::bigint FROM client_builds;
