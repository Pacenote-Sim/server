-- +goose Up

-- The voice service leaves the server, the way the coaching did before it.
--
-- This server no longer speaks to any vendor. It has no engine, no endpoint and
-- no credential for one: POST /v1/tts hands the line to whichever plugin answers
-- and relays the bytes back. An operator points their installation at whatever
-- service they pay for by installing a plugin for it, and the server knows
-- nothing about which one that is.
--
-- The rows go rather than being left for a reader that no longer looks at them.
-- A credential nobody uses is a credential nobody rotates, and one sitting in a
-- table that nothing reads is one an operator will not think to revoke when they
-- should.
--
-- An operator upgrading past this loses their spoken coach until they install a
-- voice plugin and give it the key again. That is a real regression and it is
-- deliberate: the alternative was a server that keeps a vendor integration in
-- its core for ever because moving it would be inconvenient once.
DELETE FROM settings WHERE key IN ('tts_endpoint', 'tts_key', 'voice');

-- +goose Down
-- Deliberately empty. The key was deleted rather than moved, and a down
-- migration that recreated an empty row would be pretending it came back.
