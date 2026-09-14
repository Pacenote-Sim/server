//go:build postgres

package admin_test

import (
	"bytes"
	"context"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pacenote-sim/server/internal/admin"
	"github.com/pacenote-sim/server/internal/auth"
	"github.com/pacenote-sim/server/internal/clientbuild"
	"github.com/pacenote-sim/server/internal/clientbuild/clientbuildtest"
	"github.com/pacenote-sim/server/internal/db"
)

// The certificate an operator uploads, from the page they upload it on.
//
// The thing these tests are really about is what the page says. A certificate
// that will sign and a certificate Windows will refuse are both stored, and the
// difference between them is a sentence — so a page that stored the second one
// silently would be a page that let an operator hand out a file believing
// something about it that is not true.

// certPanel is a panel with somewhere to build from and a data key to seal a
// certificate with.
type certPanel struct {
	*clientPanel
	keyring *auth.Keyring
}

func newCertPanel(t *testing.T) *certPanel {
	t.Helper()
	r := require.New(t)

	key, err := auth.NewSecretKey()
	r.NoError(err)
	keyring := auth.NewKeyring(key)

	p := newClientPanelWith(t, func(d *admin.Deps) { d.Keyring = keyring })
	return &certPanel{clientPanel: p, keyring: keyring}
}

// upload puts a certificate on the server the way the form does.
func (p *certPanel) upload(pfx []byte, password string) int {
	p.t.Helper()
	return p.uploadWithToken(pfx, password, csrfOf(p.t, p.get(admin.ClientsPath)))
}

// uploadWithToken is the same post with the form's token chosen by the caller,
// which is how the stale-form check is reached.
func (p *certPanel) uploadWithToken(pfx []byte, password, csrf string) int {
	p.t.Helper()
	r := require.New(p.t)

	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	r.NoError(form.WriteField("csrf", csrf))
	r.NoError(form.WriteField("password", password))
	if pfx != nil {
		file, err := form.CreateFormFile("certificate", "signing.pfx")
		r.NoError(err)
		_, err = file.Write(pfx)
		r.NoError(err)
	}
	r.NoError(form.Close())

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
		p.server.URL+admin.CertificatePath, &body)
	r.NoError(err)
	req.Header.Set("Content-Type", form.FormDataContentType())
	res, err := p.client.Do(req)
	r.NoError(err)
	defer func() { _ = res.Body.Close() }()
	p.lastBody, _ = io.ReadAll(res.Body)
	return res.StatusCode
}

// remove takes it off again.
func (p *certPanel) remove() int {
	p.t.Helper()
	return p.post(admin.CertificateRemovePath, url.Values{
		"csrf": {csrfOf(p.t, p.get(admin.ClientsPath))},
	})
}

func TestACertificateIsUploadedAndUsed(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	p := newCertPanel(t)

	// Before: the page says there is none, and the route is not on offer.
	body := p.get(admin.ClientsPath)
	r.Contains(body, "This server holds no certificate")
	r.Contains(body, "no certificate to sign with")
	r.Contains(body, `value="own-certificate" disabled`)

	pfx, _ := clientbuildtest.Certificate(t, clientbuildtest.CertificateOptions{
		CommonName: "Spain GT League", Password: "trackside",
	})
	r.Equal(http.StatusOK, p.upload(pfx, "trackside"))
	r.Contains(p.body(), "Stored")
	r.Contains(p.body(), "Spain GT League")

	// After: the certificate is described and the route is available.
	body = p.get(admin.ClientsPath)
	r.Contains(body, "Spain GT League")
	r.Contains(body, "you issued it to yourself")
	r.Contains(body, "RSA 2048")
	r.NotContains(body, "no certificate to sign with")
	r.NotContains(body, `value="own-certificate" disabled`)

	// And a client built down that route is signed with it.
	r.Equal(http.StatusOK, p.build("https://pacenote.example.com", clientbuild.SigningOwnCertificate))
	built, err := p.store.ClientBuilds(context.Background(), db.ClientBuildQuery{})
	r.NoError(err)
	r.Len(built, 1)
	r.Equal("own-certificate", built[0].Signing)
	r.Equal("Spain GT League", built[0].SignedBy,
		"the history does not say which certificate signed the file")
}

// The password never reaches the page, the log or the history — not on success
// and not on the failure where it is the thing that was wrong.
func TestTheCertificatePasswordIsNeverEchoed(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	p := newCertPanel(t)

	pfx, _ := clientbuildtest.Certificate(t, clientbuildtest.CertificateOptions{Password: "trackside"})

	r.Equal(http.StatusUnprocessableEntity, p.upload(pfx, "pitlane"))
	r.Contains(p.body(), "password does not open the file")
	r.NotContains(p.body(), "pitlane")

	r.Equal(http.StatusOK, p.upload(pfx, "trackside"))
	r.NotContains(p.body(), "trackside")
	r.NotContains(p.get(admin.ClientsPath), "trackside")
}

// A certificate that is not marked for signing software is stored, and the page
// says what will happen. Refusing it would take away a signature that still
// says who built the file; storing it quietly would let an operator believe
// they had solved a problem they had not.
func TestACertificateWindowsWillRefuseIsStoredAndExplained(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	p := newCertPanel(t)

	pfx, _ := clientbuildtest.Certificate(t, clientbuildtest.CertificateOptions{NoCodeSigning: true})
	r.Equal(http.StatusOK, p.upload(pfx, ""))
	r.Contains(p.body(), "not marked for signing software")
	r.Contains(p.get(admin.ClientsPath), "is one Windows refuses")
}

// An expired certificate is not stored at all. There is nothing to explain: it
// cannot sign, and a server holding one would offer a route that fails.
func TestAnExpiredCertificateIsRefused(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	p := newCertPanel(t)

	pfx, _ := clientbuildtest.Certificate(t, clientbuildtest.CertificateOptions{
		NotBefore: time.Now().Add(-48 * time.Hour), NotAfter: time.Now().Add(-time.Hour),
	})
	r.Equal(http.StatusUnprocessableEntity, p.upload(pfx, ""))
	r.Contains(p.body(), "cannot sign anything")

	_, err := p.store.SigningCertificate(context.Background())
	r.ErrorIs(err, db.ErrNotFound, "an expired certificate was stored anyway")
}

func TestAnUploadThatIsNotACertificate(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	p := newCertPanel(t)

	r.Equal(http.StatusUnprocessableEntity, p.upload(nil, ""))
	r.Contains(p.body(), "No certificate file was chosen")

	r.Equal(http.StatusUnprocessableEntity, p.upload([]byte{}, ""))
	r.Contains(p.body(), "empty")

	r.Equal(http.StatusUnprocessableEntity, p.upload([]byte("dear server, please sign things"), ""))
	r.Contains(p.body(), "not a certificate store")

	_, err := p.store.SigningCertificate(context.Background())
	r.ErrorIs(err, db.ErrNotFound)
}

// Uploading replaces. A server signs with one certificate, so the second upload
// is the operator rotating rather than adding.
func TestUploadingACertificateReplacesTheOne(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	p := newCertPanel(t)

	first, _ := clientbuildtest.Certificate(t, clientbuildtest.CertificateOptions{CommonName: "Last year"})
	r.Equal(http.StatusOK, p.upload(first, ""))
	second, _ := clientbuildtest.Certificate(t, clientbuildtest.CertificateOptions{CommonName: "This year"})
	r.Equal(http.StatusOK, p.upload(second, ""))

	body := p.get(admin.ClientsPath)
	r.Contains(body, "This year")
	r.NotContains(body, "Last year")
}

func TestACertificateIsRemoved(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	p := newCertPanel(t)

	pfx, _ := clientbuildtest.Certificate(t, clientbuildtest.CertificateOptions{CommonName: "Spain GT League"})
	r.Equal(http.StatusOK, p.upload(pfx, ""))
	r.Equal(http.StatusOK, p.remove())
	r.Contains(p.body(), "clients built from now on are unsigned")

	_, err := p.store.SigningCertificate(context.Background())
	r.ErrorIs(err, db.ErrNotFound)

	// The route goes away with it, and asking for it anyway is refused.
	body := p.get(admin.ClientsPath)
	r.Contains(body, "This server holds no certificate")
	r.Equal(http.StatusUnprocessableEntity,
		p.build("https://pacenote.example.com", clientbuild.SigningOwnCertificate))

	// Removing one that is not there is the state the operator asked for.
	r.Equal(http.StatusOK, p.remove())
}

// A data key that changed under a stored certificate. The page says so and the
// route is withdrawn, rather than the operator finding out by pressing build.
func TestACertificateThatWillNotOpenAnyMore(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	p := newCertPanel(t)

	pfx, _ := clientbuildtest.Certificate(t, clientbuildtest.CertificateOptions{CommonName: "Spain GT League"})
	r.Equal(http.StatusOK, p.upload(pfx, ""))

	replacement, err := auth.NewSecretKey()
	r.NoError(err)
	p.keyring.Replace(replacement)

	body := p.get(admin.ClientsPath)
	r.Contains(body, "Spain GT League", "the page forgot which certificate it is holding")
	r.Contains(body, "cannot open the certificate it is holding")
	r.Contains(body, `value="own-certificate" disabled`)
	r.Equal(http.StatusUnprocessableEntity,
		p.build("https://pacenote.example.com", clientbuild.SigningOwnCertificate))
}

// A server with no data key cannot store a certificate safely, and says so
// rather than writing a private key into the database in the clear.
func TestACertificateNeedsADataKey(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	p := &certPanel{clientPanel: newClientPanelWith(t, func(d *admin.Deps) {
		d.Keyring = auth.NewKeyring(nil)
	})}

	pfx, _ := clientbuildtest.Certificate(t, clientbuildtest.CertificateOptions{})
	r.Equal(http.StatusConflict, p.upload(pfx, ""))
	r.Contains(p.body(), "no data key")

	_, err := p.store.SigningCertificate(context.Background())
	r.ErrorIs(err, db.ErrNotFound)
}

// Both changes are in the audit trail, and neither records the file or the
// password — only that it happened and to which certificate.
func TestTheCertificateIsAudited(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	p := newCertPanel(t)

	pfx, _ := clientbuildtest.Certificate(t, clientbuildtest.CertificateOptions{
		CommonName: "Spain GT League", Password: "trackside",
	})
	r.Equal(http.StatusOK, p.upload(pfx, "trackside"))
	r.Equal(http.StatusOK, p.remove())

	entries, err := p.store.RecentAudit(context.Background(), 10)
	r.NoError(err)

	actions := make([]string, 0, len(entries))
	for _, e := range entries {
		actions = append(actions, e.Action)
		r.NotContains(string(e.Detail), "trackside")
	}
	r.Contains(actions, db.ActionCertificateStored)
	r.Contains(actions, db.ActionCertificateRemoved)
}

// A certificate far larger than a certificate is refused before any of it is
// stored. The cap is there so that the wrong file chosen by mistake — a video,
// a disk image — is a sentence on the page rather than a server holding it.
func TestAnUploadLargerThanACertificate(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	p := newCertPanel(t)

	huge := bytes.Repeat([]byte{0xAB}, int(admin.MaxCertificateBytes)+4096)
	r.Equal(http.StatusRequestEntityTooLarge, p.upload(huge, ""))
	r.Contains(p.body(), "larger than this server accepts")

	_, err := p.store.SigningCertificate(context.Background())
	r.ErrorIs(err, db.ErrNotFound)
}

// Both forms are behind the same stale-form check as every other post in the
// panel. Uploading a private key is the one place on this server where a form
// posted from somewhere else would matter most.
func TestTheCertificateFormsRefuseAStaleToken(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	p := newCertPanel(t)

	pfx, _ := clientbuildtest.Certificate(t, clientbuildtest.CertificateOptions{})
	r.Equal(http.StatusForbidden, p.uploadWithToken(pfx, "", "not-the-token"))
	_, err := p.store.SigningCertificate(context.Background())
	r.ErrorIs(err, db.ErrNotFound)

	r.Equal(http.StatusOK, p.upload(pfx, ""))
	r.Equal(http.StatusForbidden, p.post(admin.CertificateRemovePath, url.Values{"csrf": {"not-the-token"}}))
	_, err = p.store.SigningCertificate(context.Background())
	r.NoError(err, "a removal with a stale token removed the certificate anyway")
}
