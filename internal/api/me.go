package api

import (
	"net/http"

	"github.com/pacenote-sim/protocol/wire"
)

// getMe answers who this token belongs to and what they may do.
//
// The features here are the intersection of the server's with this driver's
// entitlements, and they are the list the client uses once it is paired.
// Discovery's list is what it read before it had a token; this one is what it
// acts on.
func (a *API) getMe(w http.ResponseWriter, _ *http.Request, s session) {
	a.ok(w, wire.Me{
		Driver:   driverOf(s.driver),
		Team:     nil, // Teams are an enterprise feature; a community driver drives alone.
		Features: s.features,
	})
}
