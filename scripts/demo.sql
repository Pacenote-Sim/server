-- Demo data, for looking at a server that nobody has driven on yet.
--
-- It writes three drivers, a stint each and a handful of laps with the corner
-- analysis a real client would have sent, plus a debrief in the engineer
-- plugin's own schema if that plugin is installed.
--
-- Everything it writes is prefixed demo- or named in the list at the bottom, so
-- the last statement in this file removes all of it and nothing else. Run it
-- against a server you are trying out, never one with real driving on it.
--
--   psql -d pacenote_try -f scripts/demo.sql
--
BEGIN;

-- Idempotent: running it twice replaces the demo rather than doubling it.
DELETE FROM drivers WHERE slug LIKE 'demo-%';

INSERT INTO drivers (name, slug, class) VALUES
    ('Ana López',    'demo-ana',  'GT3'),
    ('Bea Ortiz',    'demo-bea',  'GT3'),
    ('Caro Iglesias','demo-caro', 'GT4');

-- One stint each. The setup sheet is what a debrief and a setup request read.
INSERT INTO stints (id, driver_id, sim, track_id, track, car, car_class, session_type,
                    sectors, setup, started_at, finished_at)
SELECT
    gen_random_uuid(), d.id, 'iracing', 'spa', 'Spa-Francorchamps',
    CASE WHEN d.class = 'GT4' THEN 'Porsche 718 Cayman GT4' ELSE 'Ferrari 296 GT3' END,
    d.class,
    CASE WHEN d.slug = 'demo-caro' THEN 'practice' ELSE 'race' END,
    ARRAY[31.2, 52.9, 33.4]::real[],
    jsonb_build_object(
        'tyre_pressure_lf_cold', 172, 'tyre_pressure_rf_cold', 172,
        'tyre_pressure_lr_cold', 168, 'tyre_pressure_rr_cold', 168,
        'rear_wing', 7, 'front_arb', 4, 'rear_arb', 6,
        'fuel_l', 68, 'brake_bias_pct', 54.5),
    now() - interval '2 hours',
    now() - interval '1 hour'
FROM drivers d WHERE d.slug LIKE 'demo-%';

-- Laps. content_sha256 is the client's idempotency digest; here it is derived
-- from the lap itself so two runs of this file produce the same rows. There is
-- no trace: trace_bytes is generated from it, so an empty one is a lap that
-- arrived without, which is allowed and is what keeps this file small.
INSERT INTO laps (stint_id, driver_id, number, lap_ms, kind, started_at, sim,
                  track_id, car, car_class, content_sha256, corners,
                  trace_codec, trace)
SELECT
    s.id, s.driver_id, n,
    -- A quick one, then a scrappy one, then the best of the set.
    CASE n WHEN 1 THEN 138900 WHEN 2 THEN 141250 WHEN 3 THEN 137980 ELSE 138420 END
        + (s.driver_id % 3) * 640,
    CASE n WHEN 2 THEN 'invalid' ELSE 'clean' END,
    s.started_at + (n * interval '3 minutes'),
    s.sim, s.track_id, s.car, s.car_class,
    sha256((s.id::text || n::text)::bytea),
    jsonb_build_array(
        jsonb_build_object('turn', 1,  'apex_pct', 4,  'apex_kmh', 86,
                           'ref_apex_kmh', 91, 'deficit_kmh', 5, 'brake_at_apex', 12,
                           'pattern', 'early_apex'),
        jsonb_build_object('turn', 5,  'apex_pct', 31, 'apex_kmh', 174,
                           'ref_apex_kmh', 176, 'deficit_kmh', 2),
        jsonb_build_object('turn', 8,  'apex_pct', 52, 'apex_kmh', 118,
                           'ref_apex_kmh', 127, 'deficit_kmh', 9, 'throttle_lag', 14,
                           'pattern', 'slow_exit'),
        jsonb_build_object('turn', 15, 'apex_pct', 84, 'apex_kmh', 96,
                           'ref_apex_kmh', 98, 'deficit_kmh', 2)),
    1, ''::bytea
FROM stints s
CROSS JOIN generate_series(1, 4) AS n
WHERE s.driver_id IN (SELECT id FROM drivers WHERE slug LIKE 'demo-%');

-- A debrief each, in the engineer plugin's own schema, if it is installed. The
-- schema name carries a digest of the database it is in, so it is looked up
-- rather than spelled out.
DO $$
DECLARE
    engineer_schema text;
BEGIN
    SELECT nspname INTO engineer_schema
    FROM pg_namespace WHERE nspname LIKE 'plugin\_engineer\_%' LIMIT 1;

    IF engineer_schema IS NULL THEN
        RAISE NOTICE 'engineer is not installed, so no debriefs were written';
        RETURN;
    END IF;

    EXECUTE format('DELETE FROM %I.debriefs WHERE driver_slug LIKE %L', engineer_schema, 'demo-%');
    EXECUTE format($f$
        INSERT INTO %I.debriefs
            (driver_slug, driver_name, sim, track, car, summary, findings,
             model, prompt_version, written_at)
        SELECT d.slug, d.name, 'iracing', 'Spa-Francorchamps',
               CASE WHEN d.class = 'GT4' THEN 'Porsche 718 Cayman GT4' ELSE 'Ferrari 296 GT3' END,
               'Quick where it matters and losing it in the same two places. Your best lap was '
               || 'two tenths off what the corner data says you had available.',
               %L::jsonb, 'claude-sonnet-5', '1', now() - interval '55 minutes'
        FROM drivers d WHERE d.slug LIKE %L
    $f$, engineer_schema,
    '[{"area":"Turn 8","observation":"Nine kilometres per hour down at the apex and the throttle is late by about a tenth.","drill":"Brake four metres earlier and get the car straight sooner; the exit is what pays here."},
      {"area":"Turn 1","observation":"You are still on the brake at the apex on most laps.","drill":"Release the brake before you turn in. The car will rotate without it."},
      {"area":"Consistency","observation":"Three clean laps inside four tenths of each other.","drill":"Nothing to fix. Keep the same reference points."}]',
    'demo-%');
END $$;

COMMIT;

-- To remove all of it:
--   DELETE FROM drivers WHERE slug LIKE 'demo-%';
-- Stints and laps go with them; the debriefs are removed by re-running the DO
-- block above, or by dropping the plugin.
