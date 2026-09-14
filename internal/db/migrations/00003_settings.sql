-- +goose Up

-- What the settings page needs on top of the API migration: a place to count
-- what the language-model features spend, so the operator's daily cap
-- is a number the server can enforce and an operator can see rather than a
-- promise in a document.

-- --------------------------------------------------------------------------
-- Language-model usage
-- --------------------------------------------------------------------------

-- One row per call. D-8 says "every call records tokens used, visible to the
-- operator", and a row per call is the only shape that answers both questions
-- an operator asks — what did today cost, and what is spending it — without
-- keeping a counter that a crash can lose.
--
-- The driver is nullable and ON DELETE SET NULL: removing a driver must not
-- erase the spend their calls already cost, because the cap is the
-- organisation's and not theirs.
CREATE TABLE llm_usage (
    id            bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    at            timestamptz NOT NULL DEFAULT now(),
    job           text        NOT NULL,
    model         text        NOT NULL,
    driver_id     bigint      REFERENCES drivers (id) ON DELETE SET NULL,
    input_tokens  bigint      NOT NULL DEFAULT 0,
    output_tokens bigint      NOT NULL DEFAULT 0,
    CONSTRAINT llm_usage_tokens_check CHECK (input_tokens >= 0 AND output_tokens >= 0)
);

-- Every read of this table is "since a moment", which is one range scan.
CREATE INDEX llm_usage_at ON llm_usage (at DESC);

-- +goose Down
DROP TABLE IF EXISTS llm_usage;
