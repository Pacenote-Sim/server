-- Plugins: what is installed, what the operator configured, and what each one
-- spent. The host in internal/plugins is the only caller.

-- name: UpsertPlugin :exec
INSERT INTO plugins (
    name, version, author, description, interface_version,
    capabilities, declared_settings, directory, state, updated_at
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, now())
ON CONFLICT (name) DO UPDATE SET
    version           = EXCLUDED.version,
    author            = EXCLUDED.author,
    description       = EXCLUDED.description,
    interface_version = EXCLUDED.interface_version,
    capabilities      = EXCLUDED.capabilities,
    directory         = EXCLUDED.directory,
    state             = EXCLUDED.state,
    updated_at        = now();

-- name: SetPluginSettings :exec
UPDATE plugins SET declared_settings = $2, updated_at = now() WHERE name = $1;

-- name: SetPluginState :exec
UPDATE plugins
SET state = $2, last_error = $3, last_output = $4, restarts = $5, updated_at = now()
WHERE name = $1;

-- name: SetPluginEnabled :exec
UPDATE plugins SET enabled = $2, updated_at = now() WHERE name = $1;

-- name: ListPlugins :many
SELECT
    name, version, author, description, interface_version, capabilities,
    declared_settings, directory, enabled, state, last_error, last_output,
    restarts, first_seen_at, updated_at
FROM plugins ORDER BY name;

-- name: GetPlugin :one
SELECT
    name, version, author, description, interface_version, capabilities,
    declared_settings, directory, enabled, state, last_error, last_output,
    restarts, first_seen_at, updated_at
FROM plugins WHERE name = $1;

-- name: DeletePlugin :exec
DELETE FROM plugins WHERE name = $1;

-- name: ListPluginSettings :many
SELECT name, value, sealed FROM plugin_settings WHERE plugin_name = $1 ORDER BY name;

-- name: UpsertPluginSetting :exec
INSERT INTO plugin_settings (plugin_name, name, value, sealed, updated_at)
VALUES ($1, $2, $3, $4, now())
ON CONFLICT (plugin_name, name) DO UPDATE SET
    value      = EXCLUDED.value,
    sealed     = EXCLUDED.sealed,
    updated_at = now();

-- name: DeletePluginSetting :exec
DELETE FROM plugin_settings WHERE plugin_name = $1 AND name = $2;

-- name: RecordPluginUsage :exec
INSERT INTO plugin_usage (plugin_name, job, model, driver_id, input_tokens, output_tokens, cached)
VALUES ($1, $2, $3, $4, $5, $6, $7);

-- name: PluginTokensSince :one
SELECT
    coalesce(sum(input_tokens), 0)::bigint  AS input_tokens,
    coalesce(sum(output_tokens), 0)::bigint AS output_tokens,
    count(*)::bigint                        AS calls
FROM plugin_usage WHERE at >= $1;

-- name: PluginTokensSinceFor :one
SELECT
    coalesce(sum(input_tokens), 0)::bigint  AS input_tokens,
    coalesce(sum(output_tokens), 0)::bigint AS output_tokens,
    count(*)::bigint                        AS calls
FROM plugin_usage WHERE plugin_name = $1 AND at >= $2;

-- The plugin's own database. Separate queries rather than columns on the ones
-- above: every reader of a plugin row wants its name and state, and almost none
-- of them want a sealed password. Keeping it out of the common path means it is
-- not accidentally logged by something that dumps a struct.

-- name: GetPluginDatabase :one
SELECT db_password_sealed, db_provisioned, db_migration
FROM plugins WHERE name = $1;

-- name: SetPluginProvisioned :exec
UPDATE plugins
SET db_password_sealed = $2,
    db_provisioned     = true,
    db_migration       = $3,
    updated_at         = now()
WHERE name = $1;


-- name: ClearPluginDatabase :exec
UPDATE plugins
SET db_password_sealed = NULL,
    db_provisioned     = false,
    db_migration       = '',
    updated_at         = now()
WHERE name = $1;

