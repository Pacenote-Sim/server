-- Device tokens: the row holds only a SHA-256 and a
-- short clear-text prefix for the lookup index. The pairing endpoints that mint
-- these are not built yet; the table and the queries are.

-- name: CreateDevice :one
INSERT INTO devices (driver_id, token_sha256, token_prefix, label)
VALUES ($1, $2, $3, $4)
RETURNING id, driver_id, token_prefix, label, created_at;

-- name: DevicesByTokenPrefix :many
SELECT id, driver_id, token_sha256, token_prefix, label, created_at, last_used_at, revoked_at
FROM devices
WHERE token_prefix = $1 AND revoked_at IS NULL;

-- name: TouchDevice :exec
UPDATE devices SET last_used_at = now() WHERE id = $1;

-- name: RevokeDevice :execrows
UPDATE devices SET revoked_at = now() WHERE id = $1 AND revoked_at IS NULL;

-- name: ListDevicesForDriver :many
SELECT id, driver_id, token_prefix, label, created_at, last_used_at, revoked_at
FROM devices WHERE driver_id = $1 ORDER BY created_at DESC;

-- name: CreateDriver :one
INSERT INTO drivers (external_id, name, slug, class, avatar_url)
VALUES ($1, $2, $3, $4, $5)
RETURNING id, name, slug, class, avatar_url, created_at;

-- name: RevokeAllDevices :execrows
UPDATE devices SET revoked_at = now() WHERE revoked_at IS NULL;
