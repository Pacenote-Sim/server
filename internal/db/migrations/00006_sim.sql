-- +goose Up

-- Laps from two simulators stop being comparable.
--
-- stints has carried a sim since the first migration, but laps did not and
-- reference_laps did not, and the bucket a reference lap lives in was keyed on
-- (scope, track_id, car, car_class). A track_id is a slug the client derives
-- from the name the simulator prints, so two simulators agree on "spa" and on
-- "Ferrari 296 GT3" and disagree about everything underneath: the tyre model,
-- the fuel burn, the kerbs, and therefore the lap time. Keyed without the
-- simulator, an Assetto Corsa Competizione lap at Spa was the reference served
-- for an iRacing lap at Spa, and the coach told a driver they were seconds off
-- a time nobody in their simulator had ever set.
--
-- The simulator is therefore part of every identity the server compares on.
-- Comparing across simulators is a thing somebody may build deliberately one
-- day; until then it is not something to inherit by accident.

-- --------------------------------------------------------------------------
-- laps
-- --------------------------------------------------------------------------

-- Copied down from the stint, for the same reason track_id, car, car_class and
-- driver_id are: the reference index is a single partial index on laps rather
-- than a join with stints on every lookup, and a query that runs once per lap
-- while a driver waits to hear a cue does not get to join.
--
-- The default exists only for the length of the backfill. Dropped afterwards,
-- the column is NOT NULL with nothing to fall back on, so a writer that forgets
-- the simulator fails loudly rather than filing its laps under the empty one.
ALTER TABLE laps ADD COLUMN sim text NOT NULL DEFAULT '';

UPDATE laps l SET sim = s.sim FROM stints s WHERE s.id = l.stint_id;

ALTER TABLE laps ALTER COLUMN sim DROP DEFAULT;

-- The two reference indexes, rebuilt with the simulator in front of the columns
-- that follow it. Leading with sim rather than appending it keeps every lookup
-- an equality prefix, which is what the planner needs to seek rather than scan.
DROP INDEX laps_reference;
DROP INDEX laps_driver_reference;

CREATE INDEX laps_reference ON laps (sim, track_id, car_class, lap_ms) WHERE kind = 'clean';
CREATE INDEX laps_driver_reference ON laps (driver_id, sim, track_id, car, lap_ms) WHERE kind = 'clean';

-- --------------------------------------------------------------------------
-- reference_laps
-- --------------------------------------------------------------------------

-- Backfilled from the lap the row already points at, which is the simulator
-- that lap was driven in.
ALTER TABLE reference_laps ADD COLUMN sim text NOT NULL DEFAULT '';

UPDATE reference_laps r SET sim = l.sim FROM laps l WHERE l.id = r.lap_id;

ALTER TABLE reference_laps ALTER COLUMN sim DROP DEFAULT;

-- The bucket keys. Widening a unique key can only remove collisions, never
-- create them, so every row already here survives the rebuild.
DROP INDEX reference_laps_bucket;
DROP INDEX reference_laps_driver_bucket;

CREATE UNIQUE INDEX reference_laps_bucket
    ON reference_laps (scope, sim, track_id, car, car_class) WHERE driver_id IS NULL;
CREATE UNIQUE INDEX reference_laps_driver_bucket
    ON reference_laps (driver_id, scope, sim, track_id, car, car_class) WHERE driver_id IS NOT NULL;

-- What the backfill above cannot do on its own: the rows that are here are the
-- best lap across all simulators, so labelling one with its own simulator is
-- right, but the buckets of every other simulator are now missing. A driver
-- whose quickest Spa lap was in ACC would have no iRacing reference at all
-- until they drove another iRacing lap, even though the server holds one.
--
-- These two statements are refreshReference in internal/db/laps.go with its
-- WHERE widened from one batch to every lap the installation holds. They insert
-- the buckets that are missing and lower the ones a quicker lap in the same
-- simulator belongs in; a row that is already the best is left where it is, and
-- a scope this edition does not maintain is left alone entirely.
--
-- Laps whose trace has been pruned are excluded: a reference lap is fetched
-- whole and decoded, so a bucket pointing at a cleared blob would turn the next
-- coaching request into a server error. An empty bucket is an ordinary answer
-- and the next upload refills it.
INSERT INTO reference_laps (scope, sim, track_id, car, car_class, driver_id, lap_id, lap_ms)
SELECT DISTINCT ON (l.driver_id, l.sim, l.track_id, l.car, l.car_class)
       'self', l.sim, l.track_id, l.car, l.car_class, l.driver_id, l.id, l.lap_ms
  FROM laps l
 WHERE l.kind = 'clean' AND l.trace_bytes > 0
 ORDER BY l.driver_id, l.sim, l.track_id, l.car, l.car_class, l.lap_ms, l.id
    ON CONFLICT (driver_id, scope, sim, track_id, car, car_class) WHERE driver_id IS NOT NULL
    DO UPDATE SET lap_id = EXCLUDED.lap_id, lap_ms = EXCLUDED.lap_ms, updated_at = now()
     WHERE reference_laps.lap_ms > EXCLUDED.lap_ms;

INSERT INTO reference_laps (scope, sim, track_id, car, car_class, driver_id, lap_id, lap_ms)
SELECT DISTINCT ON (l.sim, l.track_id, l.car)
       'car', l.sim, l.track_id, l.car, '', NULL::bigint, l.id, l.lap_ms
  FROM laps l
 WHERE l.kind = 'clean' AND l.trace_bytes > 0
 ORDER BY l.sim, l.track_id, l.car, l.lap_ms, l.id
    ON CONFLICT (scope, sim, track_id, car, car_class) WHERE driver_id IS NULL
    DO UPDATE SET lap_id = EXCLUDED.lap_id, lap_ms = EXCLUDED.lap_ms, updated_at = now()
     WHERE reference_laps.lap_ms > EXCLUDED.lap_ms;

-- +goose Down

-- Going back narrows the bucket key, so the rows two simulators now hold side
-- by side collide on it. The quickest of each old bucket is the one the old key
-- would have kept, so the rest go before the index that forbids them returns.
DROP INDEX reference_laps_bucket;
DROP INDEX reference_laps_driver_bucket;

DELETE FROM reference_laps r
 USING reference_laps o
 WHERE o.scope = r.scope
   AND o.track_id = r.track_id
   AND o.car = r.car
   AND o.car_class = r.car_class
   AND o.driver_id IS NOT DISTINCT FROM r.driver_id
   AND (o.lap_ms, o.lap_id) < (r.lap_ms, r.lap_id);

ALTER TABLE reference_laps DROP COLUMN sim;

CREATE UNIQUE INDEX reference_laps_bucket
    ON reference_laps (scope, track_id, car, car_class) WHERE driver_id IS NULL;
CREATE UNIQUE INDEX reference_laps_driver_bucket
    ON reference_laps (driver_id, scope, track_id, car, car_class) WHERE driver_id IS NOT NULL;

DROP INDEX laps_reference;
DROP INDEX laps_driver_reference;

ALTER TABLE laps DROP COLUMN sim;

CREATE INDEX laps_reference ON laps (track_id, car_class, lap_ms) WHERE kind = 'clean';
CREATE INDEX laps_driver_reference ON laps (driver_id, track_id, car, lap_ms) WHERE kind = 'clean';
