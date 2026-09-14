package api

import (
	"github.com/pacenote-sim/protocol/wire"
	"github.com/pacenote-sim/server/internal/db"
)

// driverOf renders a stored driver as the contract's identity. It is the one
// place the two shapes meet, so a column that is renamed cannot quietly change
// what a client is told.
func driverOf(d db.Driver) wire.Driver {
	return wire.Driver{
		ID:     d.Slug,
		Name:   d.Name,
		Slug:   d.Slug,
		Class:  d.Class,
		Avatar: d.AvatarURL,
	}
}
