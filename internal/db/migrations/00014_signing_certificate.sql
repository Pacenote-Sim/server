-- +goose Up

-- The operator's own code-signing certificate.
--
-- One row, and the primary key says so: a server signs the clients it builds
-- with one certificate or with none, and a table that could hold two would need
-- a rule somewhere else about which one wins.
--
-- The certificate and its password are sealed with the data key, which lives in
-- the configuration file beside the binary and not in here. A dump of this
-- database therefore carries a private key nobody can open, which is the whole
-- reason the key is kept where it is. An operator who loses the data directory
-- loses the ability to sign and uploads the file again — the same bargain every
-- other sealed value in this server makes, and the page says so plainly rather
-- than failing at the moment somebody presses build.
--
-- Everything a person reads about the certificate is stored beside it in the
-- clear. None of it is secret — it is all printed on the certificate, which
-- travels inside every client signed with it — and keeping it readable means
-- the page can still say which certificate is installed and when it expires on
-- a server whose data key is gone. It also means rendering the page does not
-- have to open a PKCS#12 file, which is deliberately slow to open.
CREATE TABLE signing_certificate (
    id smallint PRIMARY KEY DEFAULT 1 CHECK (id = 1),
    -- The PKCS#12 file exactly as the operator uploaded it, sealed, and the
    -- password that opens it, sealed separately. Both are needed to sign and
    -- neither is any use without the data key.
    pfx_sealed bytea NOT NULL,
    password_sealed bytea,
    -- Who the certificate says signed, and who vouched for them.
    subject text NOT NULL,
    issuer text NOT NULL,
    -- The window a signature can be made inside.
    not_before timestamptz NOT NULL,
    not_after timestamptz NOT NULL,
    -- The SHA-256 of the certificate, which is what an operator compares
    -- against what their authority issued them.
    thumbprint text NOT NULL,
    -- The key in words, and the two facts that decide what a driver will see:
    -- whether the operator issued it to themselves, and whether it is marked
    -- for signing software at all.
    algorithm text NOT NULL,
    self_signed boolean NOT NULL,
    code_signing boolean NOT NULL,
    -- When it was uploaded and by which administrator, as the audit trail
    -- records an actor.
    uploaded_at timestamptz NOT NULL DEFAULT now(),
    uploaded_by text NOT NULL
);

-- Which certificate signed a build. The route is already recorded and is not
-- enough on its own: an operator who replaces an expiring certificate wants to
-- know which of the two signed the file a driver is asking about, and the
-- answer has to survive the certificate being replaced.
--
-- Empty for an unsigned build, and for every build made before this column
-- existed. That is the honest value: this server does not know, rather than a
-- guess that it was unsigned.
ALTER TABLE client_builds ADD COLUMN signed_by text NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE client_builds DROP COLUMN IF EXISTS signed_by;
DROP TABLE IF EXISTS signing_certificate;
