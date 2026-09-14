-- The admin panel's accounts and sessions. A session cookie is stored as its
-- SHA-256, like every other token here, so a database dump cannot be used to
-- sign in as anyone.

-- name: AdminByEmail :one
SELECT id, email, password_hash, created_at, last_login_at
FROM admins WHERE lower(email) = lower($1);

-- name: AdminByID :one
SELECT id, email, password_hash, created_at, last_login_at
FROM admins WHERE id = $1;

-- name: TouchAdminLogin :exec
UPDATE admins SET last_login_at = now() WHERE id = $1;

-- name: SetAdminPasswordHash :exec
UPDATE admins SET password_hash = $2 WHERE id = $1;

-- name: CreateAdminSession :one
INSERT INTO admin_sessions (admin_id, token_sha256, expires_at, user_agent)
VALUES ($1, $2, $3, $4)
RETURNING id, created_at, expires_at;

-- name: AdminSessionByToken :one
SELECT s.id, s.admin_id, s.created_at, s.last_seen_at, s.expires_at, a.email
FROM admin_sessions s
JOIN admins a ON a.id = s.admin_id
WHERE s.token_sha256 = $1 AND s.expires_at > now();

-- name: TouchAdminSession :exec
UPDATE admin_sessions SET last_seen_at = now(), expires_at = $2 WHERE token_sha256 = $1;

-- name: DeleteAdminSession :exec
DELETE FROM admin_sessions WHERE token_sha256 = $1;

-- name: DeleteAdminSessionsForAdmin :execrows
DELETE FROM admin_sessions WHERE admin_id = $1;

-- name: DeleteExpiredAdminSessions :execrows
DELETE FROM admin_sessions WHERE expires_at <= now();
