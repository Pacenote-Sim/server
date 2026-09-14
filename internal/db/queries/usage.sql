-- What the language-model features have spent. One row per call, summed over a
-- window for the settings page and for the operator's daily cap.

-- name: RecordTokenUsage :exec
INSERT INTO llm_usage (job, model, driver_id, input_tokens, output_tokens)
VALUES ($1, $2, $3, $4, $5);

-- name: TokensSince :one
SELECT
    coalesce(sum(input_tokens), 0)::bigint  AS input_tokens,
    coalesce(sum(output_tokens), 0)::bigint AS output_tokens,
    count(*)::bigint                        AS calls
FROM llm_usage WHERE at >= $1;
