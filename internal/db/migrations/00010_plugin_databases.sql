-- +goose Up

-- What the server has to remember about a plugin's own database.
--
-- The database itself is not here: it is a PostgreSQL role and a schema, and
-- PostgreSQL is where those live. What is here is the one thing the server
-- cannot look up again — the password it generated for that role — and enough
-- state to know whether a plugin has been provisioned without asking the
-- catalogue on every start.

-- --------------------------------------------------------------------------
-- The role's password
-- --------------------------------------------------------------------------

-- Sealed with the data key from the configuration file, like every other
-- credential this server keeps, so a database dump carries ciphertext and
-- nothing that opens it. That the plaintext is a password to the very database
-- holding it is not the joke it first looks like: an attacker who can read this
-- column already has the server's own connection, which is strictly more than
-- this role can do.
--
-- What it buys is the other direction. The value never appears in the panel, in
-- a log line, in an error message or in a support dump, and the plugin is the
-- only thing that is ever told it.
--
-- Null means a plugin with no database of its own, which is most of them.
ALTER TABLE plugins ADD COLUMN db_password_sealed bytea;

-- --------------------------------------------------------------------------
-- Whether it has been set up
-- --------------------------------------------------------------------------

-- The role and schema exist, the migrations have run, and the connection string
-- can be handed over. It is a column rather than a query against pg_roles
-- because the two can disagree, and when they do this is the one that is wrong
-- in the safe direction: a server that thinks a plugin is not provisioned will
-- try to provision it again, and every statement it runs to do so tolerates
-- already existing.
ALTER TABLE plugins ADD COLUMN db_provisioned boolean NOT NULL DEFAULT false;

-- The highest migration file the plugin has applied, by file name. Empty means
-- none have run. It is a mirror of the plugin's own bookkeeping table, kept
-- here so the panel can show what a plugin is at without connecting as it.
ALTER TABLE plugins ADD COLUMN db_migration text NOT NULL DEFAULT '';

-- A plugin cannot be half provisioned: if there is a password there is a role,
-- and if it is marked provisioned there is a password to reach it with. This is
-- the invariant the provisioning code is written against, so it is worth having
-- the database refuse the states that would break it rather than discovering one
-- in a year through a plugin that will not start.
ALTER TABLE plugins ADD CONSTRAINT plugins_db_consistent
    CHECK (NOT db_provisioned OR db_password_sealed IS NOT NULL);

-- +goose Down
ALTER TABLE plugins DROP CONSTRAINT IF EXISTS plugins_db_consistent;
ALTER TABLE plugins DROP COLUMN IF EXISTS db_migration;
ALTER TABLE plugins DROP COLUMN IF EXISTS db_provisioned;
ALTER TABLE plugins DROP COLUMN IF EXISTS db_password_sealed;
