-- +goose Up

-- The operator's Anthropic key leaves the server, because the server never used
-- it.
--
-- It was stored here from the beginning and read by nothing: the coaching was
-- always going to be a plugin, and now it is one. The plugin holds the key, in
-- plugin_settings, sealed with the same data key and lent to it one call at a
-- time.
--
-- Deleting the row rather than leaving it is the point. A credential nobody uses
-- is a credential nobody rotates, and one sitting in a table that nothing reads
-- is one an operator will not think to revoke when they should. An operator
-- upgrading past this has to enter their key once more on the plugin's page,
-- which is a minute of work and the last time it will move.
DELETE FROM settings WHERE key = 'anthropic_key';

-- And which model does which job goes with it. That was three model ids on a
-- form, chosen by an operator who had no way to tell what the difference would
-- be, for jobs the server did not run. The plugin that runs them offers the
-- choice, with what each model costs said out loud beside it.
--
-- The cap stays. It bounds a mistake made with the operator's money, and a
-- plugin choosing its own spending limit is backwards.
UPDATE settings
SET value = value - 'models', updated_at = now()
WHERE key = 'coaching' AND value ? 'models';

-- +goose Down
-- Deliberately empty. There is nothing to put back: the key was deleted rather
-- than moved, and a down migration that recreated an empty row would be
-- pretending the credential came with it.
