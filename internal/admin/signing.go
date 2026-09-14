package admin

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/pacenote-sim/server/internal/auth"
	"github.com/pacenote-sim/server/internal/clientbuild"
	"github.com/pacenote-sim/server/internal/db"
	"github.com/pacenote-sim/server/internal/httpx"
)

// The operator's own code-signing certificate: uploading one, removing it, and
// saying honestly what having one will and will not do for their drivers.
//
// It sits on the build page rather than in settings because it is the answer to
// the question that page asks — how should this client be signed — and because
// the one moment an operator thinks about it is the moment they are handing a
// file to a team.

// Where the certificate is uploaded and removed, both under the build page.
const (
	CertificatePath       = ClientsPath + "/certificate"
	CertificateRemovePath = CertificatePath + "/remove"
)

// The fields of the upload form, named here because the tests post to them.
const (
	certificateFormFile     = "certificate"
	certificateFormPassword = "password"
)

// MaxCertificateBytes is the largest PKCS#12 file this server will read. A
// certificate and a key and a chain is a few kilobytes; the cap is here so that
// a wrong file chosen by mistake is a sentence on the page rather than a server
// holding a gigabyte in memory.
const MaxCertificateBytes int64 = 256 << 10

// certificateForm is what the build page says about the stored certificate.
type certificateForm struct {
	// Stored is whether this server has one at all.
	Stored bool
	// Subject, Issuer, Algorithm and Thumbprint are the certificate's own
	// facts, as a person reads them.
	Subject, Issuer, Algorithm, Thumbprint string
	// Expires is when it runs out, and Expired and Expiring are whether that
	// has happened or is about to.
	Expires  moment
	Expired  bool
	Expiring bool
	// SelfSigned is whether the operator issued it to themselves, which is the
	// normal case here and decides what their drivers see.
	SelfSigned bool
	// CodeSigning is whether it is marked for signing software. One that is
	// not can still make a signature and Windows will not accept it.
	CodeSigning bool
	// Uploaded and Actor are when it arrived and who put it there.
	Uploaded moment
	Actor    string
	// Problem is why this server cannot sign with the certificate it holds —
	// a data key that has changed since it was stored, most likely. The page
	// says so rather than letting an operator find out by pressing build.
	Problem string
}

// certificateOf reads the stored certificate for the page.
func (p *Panel) certificateOf(ctx context.Context, now time.Time) certificateForm {
	row, err := p.deps.Store.SigningCertificate(ctx)
	switch {
	case errors.Is(err, db.ErrNotFound):
		return certificateForm{}
	case err != nil:
		// Said on the page rather than only logged, and said whether or not a
		// certificate turns out to be there. A server that cannot read the row
		// looks exactly like a server that has no certificate, and the
		// difference is whether the operator has something to fix.
		p.deps.Log.LogAttrs(ctx, slog.LevelError, "the signing certificate could not be read",
			slog.Any("error", err))
		return certificateForm{Problem: "The stored certificate could not be read — the database did not answer."}
	}

	form := certificateForm{
		Stored:      true,
		Subject:     row.Subject,
		Issuer:      row.Issuer,
		Algorithm:   row.Algorithm,
		Thumbprint:  row.Thumbprint,
		Expires:     momentAt(now, row.NotAfter),
		Expired:     now.After(row.NotAfter),
		SelfSigned:  row.SelfSigned,
		CodeSigning: row.CodeSigning,
		Uploaded:    momentAt(now, row.UploadedAt),
		Actor:       row.UploadedBy,
	}
	form.Expiring = !form.Expired && now.Add(30*24*time.Hour).After(row.NotAfter)
	if _, err := openCertificate(row, p.deps.Keyring.Key()); err != nil {
		form.Problem = "This server cannot open the certificate it is holding — " +
			"upload it again. " + strings.TrimPrefix(err.Error(), "clientbuild: ")
	}
	return form
}

// CanSign reports whether this server could sign a client right now, which is
// what decides whether the signing route is offered rather than explained.
func (f certificateForm) CanSign() bool { return f.Stored && f.Problem == "" && !f.Expired }

// openCertificate unseals a stored certificate. It is the only path from a
// database row to a usable private key, and it is exported to the one other
// caller that needs it — the builder, which asks for it at the moment somebody
// presses build.
func openCertificate(row db.SigningCertificate, key auth.SecretKey) (clientbuild.Certificate, error) {
	pfx, err := key.Open(row.PfxSealed)
	if err != nil {
		return clientbuild.Certificate{}, fmt.Errorf(
			"clientbuild: the stored certificate does not open with this server's data key")
	}
	password, err := key.Open(row.PasswordSealed)
	if err != nil {
		return clientbuild.Certificate{}, fmt.Errorf(
			"clientbuild: the stored certificate's password does not open with this server's data key")
	}
	return clientbuild.ParseCertificate([]byte(pfx), password)
}

// Certificate is the builder's way in: the stored certificate opened with the
// data key in force right now, or nil on a server that has not been given one.
//
// It is a function of the store and the keyring rather than a method so that
// wiring it into the builder does not mean handing the builder a whole panel.
func Certificate(store *db.Store, keyring *auth.Keyring) func(context.Context) (*clientbuild.Certificate, error) {
	return func(ctx context.Context) (*clientbuild.Certificate, error) {
		row, err := store.SigningCertificate(ctx)
		switch {
		case errors.Is(err, db.ErrNotFound):
			return nil, nil
		case err != nil:
			return nil, err
		}
		cert, err := openCertificate(row, keyring.Key())
		if err != nil {
			return nil, err
		}
		return &cert, nil
	}
}

// postCertificate stores the certificate an operator uploaded.
//
// The file is parsed before it is stored. A certificate that will not open, or
// opens and cannot sign, is refused here — because the alternative is a server
// that accepts a file, says nothing, and fails on the day somebody builds a
// client for a team.
func (p *Panel) postCertificate(w http.ResponseWriter, r *http.Request, sess db.AdminSession) {
	ctx := r.Context()
	if err := httpx.ParseUpload(w, r, MaxCertificateBytes); err != nil {
		p.renderClients(w, r, sess, clientsState{}, "",
			"That certificate could not be read — it may be larger than this server accepts.",
			http.StatusRequestEntityTooLarge)
		return
	}
	defer func() {
		if r.MultipartForm != nil {
			_ = r.MultipartForm.RemoveAll()
		}
	}()
	if !p.sessions.CheckCSRF(r) {
		httpx.Problem(w, r, http.StatusForbidden, "That form is stale. Load the page again and try once more.")
		return
	}

	pfx, problem := uploadedFile(r, certificateFormFile)
	if problem != "" {
		p.renderClients(w, r, sess, clientsState{}, "", problem, http.StatusUnprocessableEntity)
		return
	}
	password := r.PostFormValue(certificateFormPassword)

	cert, err := clientbuild.ParseCertificate(pfx, password)
	if err != nil {
		// The password is not in this message and is not logged. The operator
		// is told which of the two things went wrong and nothing else.
		p.renderClients(w, r, sess, clientsState{}, "",
			"That certificate was not stored — "+strings.TrimPrefix(err.Error(), "clientbuild: ")+".",
			http.StatusUnprocessableEntity)
		return
	}
	info := cert.Describe()
	if info.Expired(p.deps.Now()) {
		p.renderClients(w, r, sess, clientsState{}, "",
			"That certificate expired on "+info.NotAfter.Format("2 January 2006")+
				", so it cannot sign anything. Nothing was stored.",
			http.StatusUnprocessableEntity)
		return
	}

	key := p.deps.Keyring.Key()
	if len(key) != auth.SecretKeyBytes {
		p.renderClients(w, r, sess, clientsState{}, "",
			"This server has no data key, so a certificate cannot be stored safely. Nothing was saved.",
			http.StatusConflict)
		return
	}
	sealedPFX, err := key.Seal(string(pfx))
	if err == nil {
		var sealedPassword []byte
		if sealedPassword, err = key.Seal(password); err == nil {
			err = p.deps.Store.SaveSigningCertificate(ctx, db.NewSigningCertificate{
				PfxSealed:      sealedPFX,
				PasswordSealed: sealedPassword,
				Subject:        info.Subject,
				Issuer:         info.Issuer,
				NotBefore:      info.NotBefore,
				NotAfter:       info.NotAfter,
				Thumbprint:     info.Thumbprint,
				Algorithm:      info.Algorithm,
				SelfSigned:     info.SelfSigned,
				CodeSigning:    info.CodeSigning,
				UploadedBy:     sess.Email,
			})
		}
	}
	if err != nil {
		p.deps.Log.LogAttrs(ctx, slog.LevelError, "the signing certificate could not be stored",
			slog.Any("error", err))
		p.renderClients(w, r, sess, clientsState{}, "",
			"That certificate could not be stored. Nothing was saved.", http.StatusInternalServerError)
		return
	}

	if err := p.deps.Store.WriteAudit(ctx, sess.Email, db.ActionCertificateStored, info.Subject,
		[]string{"certificate"}); err != nil {
		p.deps.Log.LogAttrs(ctx, slog.LevelError, "the certificate could not be recorded in the audit trail",
			slog.Any("error", err))
	}
	p.deps.Log.LogAttrs(ctx, slog.LevelInfo, "signing certificate stored",
		slog.String("subject", info.Subject), slog.String("thumbprint", info.Thumbprint))

	p.renderClients(w, r, sess, clientsState{signing: clientbuild.SigningOwnCertificate}, notice(info),
		"", http.StatusOK)
}

// notice is what the page says after a certificate is stored: what it is, and
// the one thing about it the operator most needs to know.
func notice(info clientbuild.CertificateInfo) string {
	out := "Stored — clients built from now on can be signed as " + info.Subject + "."
	switch {
	case !info.CodeSigning:
		out += " This certificate is not marked for signing software, so Windows will refuse the signature." +
			" Clients will still build and drivers will still see the untrusted-app warning."
	case info.SelfSigned:
		out += " It is self-signed, so it stops the warning only on machines that already trust it."
	}
	return out
}

// postRemoveCertificate takes the certificate off this server.
func (p *Panel) postRemoveCertificate(w http.ResponseWriter, r *http.Request, sess db.AdminSession) {
	if !p.beginForm(w, r) {
		return
	}
	ctx := r.Context()
	if err := p.deps.Store.DeleteSigningCertificate(ctx); err != nil {
		p.deps.Log.LogAttrs(ctx, slog.LevelError, "the signing certificate could not be removed",
			slog.Any("error", err))
		p.renderClients(w, r, sess, clientsState{}, "",
			"That certificate could not be removed — the database did not answer.", http.StatusInternalServerError)
		return
	}
	if err := p.deps.Store.WriteAudit(ctx, sess.Email, db.ActionCertificateRemoved, "",
		[]string{"certificate"}); err != nil {
		p.deps.Log.LogAttrs(ctx, slog.LevelError, "the removal could not be recorded in the audit trail",
			slog.Any("error", err))
	}
	p.renderClients(w, r, sess, clientsState{},
		"Removed — this server holds no certificate, and clients built from now on are unsigned. "+
			"Clients already built keep the signature they were given.",
		"", http.StatusOK)
}

// uploadedFile reads one file off a multipart form, or says why it could not.
func uploadedFile(r *http.Request, field string) ([]byte, string) {
	file, header, err := r.FormFile(field)
	if err != nil {
		if errors.Is(err, http.ErrMissingFile) {
			return nil, "No certificate file was chosen, so nothing was stored."
		}
		return nil, "That certificate could not be read, so nothing was stored."
	}
	defer func() { _ = file.Close() }()

	if header != nil && header.Size > MaxCertificateBytes {
		return nil, fmt.Sprintf(
			"That file is %d bytes, which is far larger than a certificate — nothing was stored.", header.Size)
	}
	body, err := io.ReadAll(io.LimitReader(file, MaxCertificateBytes+1))
	switch {
	case err != nil:
		return nil, "That certificate could not be read, so nothing was stored."
	case len(body) == 0:
		return nil, "That file is empty, so nothing was stored."
	case int64(len(body)) > MaxCertificateBytes:
		return nil, "That file is far larger than a certificate, so nothing was stored."
	}
	return body, ""
}
