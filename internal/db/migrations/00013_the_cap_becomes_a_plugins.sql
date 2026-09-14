-- +goose Up

-- What a plugin may spend moves onto the plugin.
--
-- The cap was a server setting because the server used to do the spending. It
-- does not any more: every token this installation costs is spent by a plugin,
-- on a credential the plugin holds, against an account the operator opened for
-- it. A budget for that sitting in the server's settings was a number filed
-- under the wrong thing, and the page saying so — "coaching is done by a plugin"
-- above a field controlling what coaching may spend — was the giveaway.
--
-- It is now one cap per plugin, in plugin_settings, under a name beginning with
-- an underscore. That prefix is what separates it from the settings a plugin
-- declares for itself: the plugin interface refuses a setting name that starts
-- with one, so a plugin cannot declare a name that collides with it however
-- hard it tries, and it is never sent this value or asked about it.
--
-- The host still enforces it, and that has not moved and must not. A plugin that
-- held its own spending limit could simply not apply it, and a limit somebody
-- else's code may ignore is a preference rather than a limit.
--
-- The old row goes. An operator who had set a cap sets it again on the page of
-- whichever plugin they want capped, which is also the first time that question
-- has had a sensible answer: one number for the whole server could not say that
-- the coach may spend and the voice may not.
DELETE FROM settings WHERE key = 'coaching';

-- +goose Down
-- Deliberately empty. Putting a server-wide cap back would put back a number
-- nothing reads.
