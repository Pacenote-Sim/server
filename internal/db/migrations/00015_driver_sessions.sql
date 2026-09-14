-- +goose Up

-- A driver signed in to a browser.
--
-- Drivers have had no way to sign in at all until now: a driver's *machine*
-- holds a token, issued when the operator approved its pairing, and that token
-- is the client's rather than the person's. This is the other thing — the
-- person, in a browser, reading something about themselves.
--
-- It is here rather than in the plugin that authenticates them, and that is the
-- decision this table represents. A plugin's cookies are namespaced to that
-- plugin so that one plugin cannot read another's, which means a login plugin
-- holding the session privately would be the only thing that knew who was
-- signed in: not this server, and not the results plugin next to it. Identity
-- two things have to agree on has to live where both can see it.
--
-- What is *not* here is how they proved it. Discord, an emailed link, a
-- password, a code handed out at a race — all of that is the plugin's, and this
-- server never learns which. The plugin says who; this server says for how long.
--
-- Shaped like admin_sessions on purpose. Only the digest of the token is
-- stored, so a dump of this table cannot be used to sign in as anybody, and one
-- row per browser means an operator can sign out the laptop that was stolen
-- rather than every machine that driver owns.
CREATE TABLE driver_sessions (
    id           bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    driver_id    bigint      NOT NULL REFERENCES drivers (id) ON DELETE CASCADE,
    token_sha256 bytea       NOT NULL UNIQUE,
    created_at   timestamptz NOT NULL DEFAULT now(),
    last_seen_at timestamptz NOT NULL DEFAULT now(),
    expires_at   timestamptz NOT NULL,
    user_agent   text        NOT NULL DEFAULT '',
    -- Which plugin signed this driver in. An operator looking at a session
    -- their driver did not expect needs to know which plugin vouched for it,
    -- and removing a login plugin should take its sessions with it.
    signed_in_by text        NOT NULL DEFAULT ''
);

CREATE INDEX driver_sessions_expires_at ON driver_sessions (expires_at);
CREATE INDEX driver_sessions_driver_id ON driver_sessions (driver_id);

-- +goose Down
DROP INDEX IF EXISTS driver_sessions_driver_id;
DROP INDEX IF EXISTS driver_sessions_expires_at;
DROP TABLE IF EXISTS driver_sessions;
