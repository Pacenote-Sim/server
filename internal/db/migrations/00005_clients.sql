-- +goose Up

-- What the build client page remembers. One row per client an operator built:
-- when, who pressed it, which address went into the binary, which prebuilt
-- client it was stamped from, and how it was signed.
--
-- It is a table rather than a reading of the audit trail because these are
-- facts about a file that is still on the disk and still being downloaded by
-- drivers — its name, its size, its digest — and because the audit trail
-- deliberately records only which fields changed and never their values.
CREATE TABLE client_builds (
    id bigserial PRIMARY KEY,
    at timestamptz NOT NULL DEFAULT now(),
    -- The administrator's email address, as the audit trail records an actor.
    actor text NOT NULL,
    -- The server address written into the binary, shown back on the page so a
    -- mistake is visible before the file is handed to a league.
    address text NOT NULL,
    -- Which of the three signing routes was taken.
    signing text NOT NULL,
    -- The build's reference: the file's name on the disk, the last part of its
    -- download address, and the identifier stamped inside the binary itself,
    -- so a client a driver is running can be traced back to this row.
    reference text NOT NULL UNIQUE,
    -- What a driver downloads, and what they can check it against.
    file_name text NOT NULL,
    file_bytes bigint NOT NULL,
    sha256 text NOT NULL,
    -- The version of the prebuilt client this copy was stamped from, as the Go
    -- toolchain recorded it inside that binary.
    client_version text NOT NULL
);

-- The history is read newest first and pages by keyset, so the ordering it
-- asks for is the ordering the index holds.
CREATE INDEX client_builds_at ON client_builds (at DESC, id DESC);

-- +goose Down
DROP INDEX IF EXISTS client_builds_at;
DROP TABLE IF EXISTS client_builds;
