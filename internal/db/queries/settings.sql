-- The settings table is the organisation's own configuration: what drivers see,
-- how the server is reached, and the encrypted API key. It outlives the data
-- directory, which is the point.

-- name: GetSetting :one
SELECT value FROM settings WHERE key = $1;

-- name: UpsertSetting :exec
INSERT INTO settings (key, value, updated_at)
VALUES ($1, $2, now())
ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_at = now();

-- name: ListSettings :many
SELECT key, value, updated_at FROM settings ORDER BY key;

-- name: DeleteSetting :exec
DELETE FROM settings WHERE key = $1;
