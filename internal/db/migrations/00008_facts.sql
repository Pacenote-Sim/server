-- +goose Up

-- The two things the capture client already knew and had nowhere to put.
--
-- Both are derived facts rather than trace data. The corner detector lives in
-- the client and ranks a lap's corners by the apex speed each one lost; the car
-- setup is the sheet the simulator publishes, reduced to measurements. Neither
-- can be recomputed here — the corner analysis is against a reference lap the
-- client chose, and the setup is not in the telemetry stream at all — so both
-- are stored as sent, and a plugin is given them as facts.

-- --------------------------------------------------------------------------
-- A lap carries its corner analysis
-- --------------------------------------------------------------------------

-- jsonb and not a corners table. Nothing queries inside this: it is written
-- once with the lap, read back whole to build the facts a plugin receives, and
-- never joined, filtered or aggregated on. A table would buy an index nobody
-- reads and cost a second write on the path a driver waits on — the ingest
-- budget is fifty milliseconds for a forty-lap batch — and a
-- simulator that starts reporting a new channel per corner would need a
-- migration instead of a client release.
--
-- It is part of the lap's content, so it is covered by content_sha256 like
-- every other field: two uploads of lap 7 that disagree about its corners are a
-- conflict, which is what makes the idempotency honest.
--
-- Default '[]' and NOT NULL rather than nullable. A lap with no corners and a
-- lap whose corners were never sent are the same thing to every reader here —
-- there is nothing to say about where the time went — and one empty array is
-- less to get wrong than a null and an empty array that mean the same.
ALTER TABLE laps ADD COLUMN corners jsonb NOT NULL DEFAULT '[]'::jsonb;

-- --------------------------------------------------------------------------
-- A stint carries the car it was driven on
-- --------------------------------------------------------------------------

-- On the stint and not on the lap: a setup is one per sitting and does not
-- change from lap to lap, and putting it on the lap would store it three
-- hundred times a session to say one thing.
--
-- Nullable here, where the corners are not. The distinction is real and worth
-- the null: a simulator that publishes no setup, and a series that locks the
-- setup and hides it, have told us nothing, and that is different from a car
-- whose setup is empty. A plugin reading null knows it was not told; a plugin
-- reading an empty document would think it had been.
ALTER TABLE stints ADD COLUMN setup jsonb;

-- +goose Down
ALTER TABLE stints DROP COLUMN IF EXISTS setup;
ALTER TABLE laps DROP COLUMN IF EXISTS corners;
