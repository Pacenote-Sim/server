package admin

import (
	"net/http"
	"strings"
)

// PairPath is the page a driver is sent to.
//
// It is the one address in this product that a person is told out loud. The
// client prints it on the machine being paired — "open this and enter 5DH-EHY" —
// and it is published in the discovery document as
// [wire.Discovery.PairURI], so it has to exist and it has to be at exactly this
// path. It answered 404 for a while, which meant the first thing a new driver
// was asked to do failed.
const PairPath = "/pair"

// pairForm is the page.
type pairForm struct {
	// Code is what the driver typed or was linked with, echoed back so they can
	// see it is the one on the other screen. It is not checked against anything
	// here and it grants nothing: this page starts no pairing and completes
	// none.
	Code string
	// Organisation is whose server this is, so a driver pairing with two
	// teams can tell which one they are looking at.
	Organisation string
}

// getPair is the page a driver opens with the code their client printed.
//
// It deliberately does very little. Approving a pairing is the operator's, and
// it happens in the panel behind a sign-in; this page exists so that the address
// a driver is given leads somewhere that explains what to do next, rather than
// to a 404 that looks like a broken server.
//
// It needs no session and takes no action, which is why it is the one page here
// outside [Panel.requireSession]. A pairing code is useless without an
// administrator, so showing one to whoever opens the page gives nothing away.
func (p *Panel) getPair(w http.ResponseWriter, r *http.Request) {
	code := strings.ToUpper(strings.TrimSpace(r.URL.Query().Get("code")))
	if len(code) > 32 {
		// Not a code, so not echoed. A page that reflected an arbitrary string
		// back would be a page worth probing.
		code = ""
	}
	p.render(w, r, "pair", pageData{
		Title: "Pair a machine",
		Form: pairForm{
			Code:         code,
			Organisation: p.organisationName(r.Context()),
		},
	})
}
