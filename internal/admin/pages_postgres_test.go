//go:build postgres

package admin_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pacenote-sim/server/internal/admin"
	"github.com/pacenote-sim/server/internal/auth"
	"github.com/pacenote-sim/server/internal/config"
)

// signedOut is the panel as somebody who has not signed in sees it: the same
// server, a cookie jar of its own. The harness signs in when it is built, and
// these tests are about the door rather than what is behind it.
func signedOut(t *testing.T, p *panel) *panel {
	t.Helper()
	r := require.New(t)
	jar, err := cookiejar.New(nil)
	r.NoError(err)
	out := *p
	out.client = &http.Client{
		Jar:           jar,
		Timeout:       30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	return &out
}

// Signing in, and the four ways it goes wrong.
//
// Three of them answer with the same sentence on purpose — a wrong password, an
// address nobody has an account at, and an account that does not exist are one
// answer, because saying which would tell somebody guessing which half of the
// pair they got right. The fourth, too many attempts, is worth saying: it is
// the one an operator locked out of their own panel needs to understand.

func TestTheSignInPageSendsASignedInOperatorOn(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	p := newPanel(t)

	// The panel harness signs in when it is built, so this is the operator who
	// bookmarked the sign-in page and came back to it a week later.
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet,
		p.server.URL+"/admin/login", http.NoBody)
	r.NoError(err)
	res, err := p.client.Do(req)
	r.NoError(err)
	defer func() { _ = res.Body.Close() }()

	r.Equal(http.StatusSeeOther, res.StatusCode, "a signed-in operator was shown the sign-in form again")
	r.Equal("/admin/overview", res.Header.Get("Location"))
}

func TestSignInRefusesWhatItCannotRead(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	p := newPanel(t)

	// A body that is not a form. It answers rather than letting the parse
	// failure out as something else.
	for _, path := range []string{"/admin/login", "/admin/logout"} {
		req, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
			p.server.URL+path, strings.NewReader("%zz&x=1"))
		r.NoError(err)
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		res, err := p.client.Do(req)
		r.NoError(err)
		_ = res.Body.Close()
		r.Equal(http.StatusBadRequest, res.StatusCode, "%s read a body that is not a form", path)
	}
}

// Guessing is slowed down per address. The limiter is the only thing standing
// between a panel on the open internet and somebody working through a word
// list, so it has to fire without an operator having configured anything.
func TestSignInStopsSomebodyGuessing(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	p := newPanel(t)

	out := signedOut(t, p)
	var limited bool
	for range 20 {
		page := out.get("/admin/login")
		code := out.post("/admin/login", url.Values{
			"csrf": {csrfOf(t, page)}, "email": {panelEmail}, "password": {"not the password"},
		})
		if code == http.StatusTooManyRequests {
			limited = true
			break
		}
		r.Equal(http.StatusUnauthorized, code)
		r.Contains(string(out.lastBody), "do not match an account here")
	}
	r.True(limited, "twenty wrong passwords from one address were all answered in full")
	r.Contains(string(out.lastBody), "Wait a minute")

	// The password is not echoed back into the page, because a page that
	// reflected it would be a page that put it in somebody's browser history.
	r.NotContains(string(out.lastBody), "not the password")
}

// A password stored with weaker parameters than today's is rewritten at the one
// moment it can be: while the operator has just typed it. Nothing else on this
// server ever holds the password, so a hash left alone here is left alone
// forever.
func TestSigningInUpgradesAnOldPasswordHash(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	ctx := context.Background()
	p := newPanel(t)

	weak := auth.DefaultParams
	weak.Memory /= 2
	old, err := auth.HashPasswordWith(weak, panelPassword)
	r.NoError(err)
	r.True(auth.NeedsRehash(old), "the fixture hash is not actually weaker than today's")

	account, err := p.store.AdminByEmail(ctx, panelEmail)
	r.NoError(err)
	r.NoError(p.store.SetAdminPasswordHash(ctx, account.ID, old))

	out := signedOut(t, p)
	page := out.get("/admin/login")
	r.Equal(http.StatusSeeOther, out.post("/admin/login", url.Values{
		"csrf": {csrfOf(t, page)}, "email": {panelEmail}, "password": {panelPassword},
	}))

	account, err = p.store.AdminByEmail(ctx, panelEmail)
	r.NoError(err)
	r.NotEqual(old, account.PasswordHash, "the weaker hash was left in the database")
	r.False(auth.NeedsRehash(account.PasswordHash))

	// And the password still works, which is the thing a rewrite could break.
	ok, err := auth.VerifyPassword(account.PasswordHash, panelPassword)
	r.NoError(err)
	r.True(ok)
}

// The overview page when the settings behind it will not read.
//
// It is the page an operator opens when something is wrong, so it has to render
// when something is. Each piece that could not be read is named and the page
// still comes back — a 500 here would tell them nothing they did not already
// know, and would hide the parts that are fine.
func TestTheOverviewSaysWhichPartsAreMissing(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	p := newPanel(t, func(d *admin.Deps) {
		d.Settings = func(context.Context) (config.Settings, error) {
			return config.Settings{}, errors.New("the database did not answer")
		}
	})

	body := p.get("/admin/overview")
	r.Contains(body, "Overview", "the page did not render at all")
	r.Contains(body, "Some of this page is missing")
	r.Contains(body, "the settings for this organisation could not be read")
}

// What the page says about how this server is reached. The two modes are two
// different things for an operator to check when a client cannot connect, so
// they are two different sentences.
func TestTheOverviewSaysHowThisServerIsReached(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	ctx := context.Background()
	p := newPanel(t)

	r.Contains(p.get("/admin/overview"), "your own proxy in front")

	settings := config.DefaultSettings("Iberian GT Championship", "pacenote.example.com", config.TLSAuto)
	r.NoError(p.store.SaveSettings(ctx, settings))
	r.Contains(p.get("/admin/overview"), "holds its own certificate")
}
