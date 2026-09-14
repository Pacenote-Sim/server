-- The operator's code-signing certificate. One row, replaced whole: uploading a
-- certificate replaces whatever was there, because a server signs with one.

-- name: SaveSigningCertificate :exec
INSERT INTO signing_certificate (
    id, pfx_sealed, password_sealed, subject, issuer, not_before, not_after,
    thumbprint, algorithm, self_signed, code_signing, uploaded_at, uploaded_by)
VALUES (1, $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, now(), $11)
ON CONFLICT (id) DO UPDATE SET
    pfx_sealed = excluded.pfx_sealed,
    password_sealed = excluded.password_sealed,
    subject = excluded.subject,
    issuer = excluded.issuer,
    not_before = excluded.not_before,
    not_after = excluded.not_after,
    thumbprint = excluded.thumbprint,
    algorithm = excluded.algorithm,
    self_signed = excluded.self_signed,
    code_signing = excluded.code_signing,
    uploaded_at = now(),
    uploaded_by = excluded.uploaded_by;

-- name: SigningCertificate :one
SELECT * FROM signing_certificate WHERE id = 1;

-- name: DeleteSigningCertificate :exec
DELETE FROM signing_certificate WHERE id = 1;
