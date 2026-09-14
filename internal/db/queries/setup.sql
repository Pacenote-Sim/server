-- Setup runs once, and this is where that is decided. The row in setup_state is
-- written in the same transaction as the first administrator, so a half-finished
-- setup leaves neither behind.

-- name: SetupCompletedAt :one
SELECT completed_at FROM setup_state WHERE id = true;

-- name: MarkSetupComplete :one
INSERT INTO setup_state (id, completed_by, server_version)
VALUES (true, $1, $2)
RETURNING completed_at;

-- name: CreateAdmin :one
INSERT INTO admins (email, password_hash)
VALUES ($1, $2)
RETURNING id, email, created_at;

-- name: CountAdmins :one
SELECT count(*) FROM admins;
