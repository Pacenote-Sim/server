package db

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/pacenote-sim/server/internal/db/gen"
)

// The operator's code-signing certificate.
//
// It is one row and it is read on every visit to the build page, so what a page
// needs is stored in the clear beside what must not be: the certificate's own
// facts are printed on the certificate and travel inside every client signed
// with it, and keeping them readable means the page still says which
// certificate is installed on a server whose data key has been lost.

// SigningCertificate is the stored certificate, as this package holds it. The
// sealed fields are ciphertext and stay that way here — opening them needs the
// data key, which this package never sees.
type SigningCertificate struct {
	// PfxSealed is the PKCS#12 file the operator uploaded, sealed.
	PfxSealed []byte
	// PasswordSealed is the password that opens it, sealed. Empty is a file
	// with no password, which is unusual and allowed.
	PasswordSealed []byte
	// Subject and Issuer are who the certificate says signed, and who vouched.
	Subject, Issuer string
	// NotBefore and NotAfter are the window a signature is made inside.
	NotBefore, NotAfter time.Time
	// Thumbprint is the certificate's own SHA-256.
	Thumbprint string
	// Algorithm is the key in words.
	Algorithm string
	// SelfSigned and CodeSigning are what decide what a driver will see.
	SelfSigned, CodeSigning bool
	// UploadedAt and UploadedBy are when it arrived and which administrator
	// put it there.
	UploadedAt time.Time
	UploadedBy string
}

// NewSigningCertificate is a certificate to store. It is a separate type from
// the one that is read back so that a caller cannot accidentally set the fields
// this package fills in.
type NewSigningCertificate struct {
	PfxSealed           []byte
	PasswordSealed      []byte
	Subject, Issuer     string
	NotBefore, NotAfter time.Time
	Thumbprint          string
	Algorithm           string
	SelfSigned          bool
	CodeSigning         bool
	UploadedBy          string
}

// SaveSigningCertificate stores the operator's certificate, replacing whatever
// was there. A server signs with one certificate, so uploading is replacing.
func (s *Store) SaveSigningCertificate(ctx context.Context, c NewSigningCertificate) error {
	err := s.q.SaveSigningCertificate(ctx, gen.SaveSigningCertificateParams{
		PfxSealed:      c.PfxSealed,
		PasswordSealed: c.PasswordSealed,
		Subject:        c.Subject,
		Issuer:         c.Issuer,
		NotBefore:      pgtype.Timestamptz{Time: c.NotBefore, Valid: true},
		NotAfter:       pgtype.Timestamptz{Time: c.NotAfter, Valid: true},
		Thumbprint:     c.Thumbprint,
		Algorithm:      c.Algorithm,
		SelfSigned:     c.SelfSigned,
		CodeSigning:    c.CodeSigning,
		UploadedBy:     c.UploadedBy,
	})
	if err != nil {
		return fmt.Errorf("db: cannot store the signing certificate: %w", err)
	}
	return nil
}

// SigningCertificate is the operator's certificate, or [ErrNotFound] when this
// server has not been given one. That is the ordinary state of a fresh
// installation rather than a failure, and every caller treats it as such.
func (s *Store) SigningCertificate(ctx context.Context) (SigningCertificate, error) {
	row, err := s.q.SigningCertificate(ctx)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return SigningCertificate{}, ErrNotFound
	case err != nil:
		return SigningCertificate{}, fmt.Errorf("db: cannot read the signing certificate: %w", err)
	}
	return SigningCertificate{
		PfxSealed:      row.PfxSealed,
		PasswordSealed: row.PasswordSealed,
		Subject:        row.Subject,
		Issuer:         row.Issuer,
		NotBefore:      row.NotBefore.Time,
		NotAfter:       row.NotAfter.Time,
		Thumbprint:     row.Thumbprint,
		Algorithm:      row.Algorithm,
		SelfSigned:     row.SelfSigned,
		CodeSigning:    row.CodeSigning,
		UploadedAt:     row.UploadedAt.Time,
		UploadedBy:     row.UploadedBy,
	}, nil
}

// DeleteSigningCertificate removes it. Removing one that is not there is not an
// error: the operator asked for a server with no certificate and that is what
// they have.
func (s *Store) DeleteSigningCertificate(ctx context.Context) error {
	if err := s.q.DeleteSigningCertificate(ctx); err != nil {
		return fmt.Errorf("db: cannot remove the signing certificate: %w", err)
	}
	return nil
}
