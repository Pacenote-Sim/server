-- The audit trail. Every change an administrator makes in the panel writes one
-- row here: who, what, and when. The detail column names the fields that
-- changed and never carries their values, because a settings change can be a
-- change to an API key.

-- name: InsertAuditEntry :one
INSERT INTO audit_log (actor, action, subject, detail)
VALUES ($1, $2, $3, $4)
RETURNING id, at, actor, action, subject, detail;

-- name: RecentAuditEntries :many
SELECT id, at, actor, action, subject, detail
FROM audit_log ORDER BY at DESC, id DESC LIMIT $1;

-- name: AuditEntriesForAction :many
SELECT id, at, actor, action, subject, detail
FROM audit_log WHERE action = $1 ORDER BY at DESC, id DESC LIMIT $2;
