-- A driver signed in to a browser. The lookup enforces expiry itself, so there
-- is no window in which an old cookie works.

-- name: CreateDriverSession :exec
INSERT INTO driver_sessions (driver_id, token_sha256, expires_at, user_agent, signed_in_by)
VALUES ($1, $2, $3, $4, $5);

-- name: DriverSessionByToken :one
SELECT s.id, s.driver_id, s.created_at, s.last_seen_at, s.expires_at, s.user_agent, s.signed_in_by,
       d.slug, d.name
  FROM driver_sessions s
  JOIN drivers d ON d.id = s.driver_id
 WHERE s.token_sha256 = $1 AND s.expires_at > now();

-- name: TouchDriverSession :exec
UPDATE driver_sessions SET last_seen_at = now(), expires_at = $2 WHERE token_sha256 = $1;

-- name: DeleteDriverSession :exec
DELETE FROM driver_sessions WHERE token_sha256 = $1;

-- name: DeleteDriverSessionsForDriver :execrows
DELETE FROM driver_sessions WHERE driver_id = $1;

-- name: DeleteDriverSessionByID :execrows
DELETE FROM driver_sessions WHERE id = $1;

-- name: DriverSessionsForDriver :many
SELECT id, driver_id, created_at, last_seen_at, expires_at, user_agent, signed_in_by
  FROM driver_sessions
 WHERE driver_id = $1 AND expires_at > now()
 ORDER BY last_seen_at DESC, id DESC;

-- name: DeleteExpiredDriverSessions :execrows
DELETE FROM driver_sessions WHERE expires_at <= now();
