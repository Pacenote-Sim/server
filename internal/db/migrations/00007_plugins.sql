-- +goose Up

-- Where plugins live in the database.
--
-- A plugin is a separate process the server starts, so almost everything about
-- one is on disk: the binary, the manifest, its own files. Three things are
-- not, and they are here.
--
-- What the server knows about it, so the panel can show a plugin that is not
-- running — including one that failed to start, which is exactly the one an
-- operator goes looking for and exactly the one an in-memory list has nothing
-- to say about after a restart.
--
-- What the operator configured, because a plugin declaring "I need an API key"
-- and then storing it in a file beside its binary is the arrangement this
-- design exists to avoid. The core holds the credential, sealed with the same
-- data key the operator's own keys are sealed with, and lends it one call at a
-- time.
--
-- What it spent, because the daily cap belongs to the core. A plugin deciding
-- how much of an operator's money to spend is backwards, and a cap the core
-- enforces needs rows the core wrote.

-- --------------------------------------------------------------------------
-- Installed plugins
-- --------------------------------------------------------------------------

-- One row per plugin directory the server has seen. The name is the key
-- everywhere — the directory, the settings, the metering — and renaming one is
-- installing a different plugin, which is why it is the primary key rather
-- than a surrogate with a unique index over it.
CREATE TABLE plugins (
    name              text        PRIMARY KEY,
    -- What the manifest said, last time it was read. Kept so the panel can
    -- describe a plugin whose files have since gone, which is what an operator
    -- is looking at when they wonder where it went.
    version           text        NOT NULL,
    author            text        NOT NULL,
    description       text        NOT NULL,
    -- The interface version the plugin was built against. It must equal the
    -- host's exactly; a mismatch is a plugin that never starts, and the row
    -- exists so the panel can say which version it wanted.
    interface_version integer     NOT NULL,
    -- The capability declaration, whole, as the manifest carried it. It is
    -- jsonb rather than columns because it is shown to the operator and never
    -- queried on, and because a declaration that grows a field should not need
    -- a migration to be displayable.
    capabilities      jsonb       NOT NULL DEFAULT '{}'::jsonb,
    -- The settings the plugin declared, as a JSON array. Stored so the panel
    -- can render the form for a plugin that is not running: the alternative is
    -- an operator unable to fix the credential that is stopping it from
    -- starting.
    declared_settings jsonb       NOT NULL DEFAULT '[]'::jsonb,
    -- The directory it was found in, relative to the plugin directory.
    directory         text        NOT NULL,
    -- Whether the operator wants it running. Separate from state: disabled is
    -- a choice and stopped is a consequence.
    enabled           boolean     NOT NULL DEFAULT true,
    -- What it is doing. See the Go constants in internal/plugins; the check
    -- here is deliberately a list rather than an enum type, because adding a
    -- state should be a migration and not a type rewrite.
    state             text        NOT NULL DEFAULT 'discovered',
    -- Why it is not running, in the words an operator reads, and the last of
    -- what it printed to standard error before it stopped. Both are scrubbed
    -- of credentials before they are written: a plugin that prints the
    -- operator's key must not be able to store it here in clear.
    last_error        text        NOT NULL DEFAULT '',
    last_output       text        NOT NULL DEFAULT '',
    -- How many times it has been restarted since it last ran cleanly, so the
    -- backoff survives a reload of the page and an operator can see that a
    -- plugin is flapping rather than running.
    restarts          integer     NOT NULL DEFAULT 0,
    first_seen_at     timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT plugins_state_check CHECK (state IN ('discovered', 'starting', 'running', 'stopped', 'disabled', 'failed'))
);

-- --------------------------------------------------------------------------
-- What the operator configured
-- --------------------------------------------------------------------------

-- One row per setting per plugin. A plain value goes in value; a credential
-- goes in sealed, encrypted with the data key from the configuration file, so
-- that a database dump holds ciphertext and nothing that opens it.
--
-- Never both, which the check enforces: a credential that is also readable is
-- not a credential.
CREATE TABLE plugin_settings (
    plugin_name text        NOT NULL REFERENCES plugins (name) ON DELETE CASCADE,
    name        text        NOT NULL,
    value       text,
    sealed      bytea,
    updated_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (plugin_name, name),
    CONSTRAINT plugin_settings_one_of CHECK (num_nonnulls(value, sealed) <= 1)
);

-- --------------------------------------------------------------------------
-- What it spent
-- --------------------------------------------------------------------------

-- One row per call that cost anything, which is the same shape llm_usage has
-- and for the same reason: it answers both questions an operator asks — what
-- did today cost, and what is spending it — without a counter a crash can lose.
--
-- plugin_name is not a foreign key. Removing a plugin must not erase what it
-- already spent, because the money was the organisation's and the row is the
-- record of it.
CREATE TABLE plugin_usage (
    id            bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    at            timestamptz NOT NULL DEFAULT now(),
    plugin_name   text        NOT NULL,
    job           text        NOT NULL,
    model         text        NOT NULL DEFAULT '',
    driver_id     bigint      REFERENCES drivers (id) ON DELETE SET NULL,
    input_tokens  bigint      NOT NULL DEFAULT 0,
    output_tokens bigint      NOT NULL DEFAULT 0,
    cached        boolean     NOT NULL DEFAULT false,
    CONSTRAINT plugin_usage_tokens_check CHECK (input_tokens >= 0 AND output_tokens >= 0)
);

-- The cap is read as "everything since midnight", over every plugin and over
-- one, so both are a range scan.
CREATE INDEX plugin_usage_at ON plugin_usage (at DESC);
CREATE INDEX plugin_usage_plugin_at ON plugin_usage (plugin_name, at DESC);

-- +goose Down
DROP TABLE IF EXISTS plugin_usage;
DROP TABLE IF EXISTS plugin_settings;
DROP TABLE IF EXISTS plugins;
