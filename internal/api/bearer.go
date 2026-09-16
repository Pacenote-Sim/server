package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/pacenote-sim/server/internal/auth"
	"github.com/pacenote-sim/server/internal/db"
	"github.com/pacenote-sim/server/internal/httpx"
)

// A device token, from the header to the driver holding it.
//
// This is the one path a token takes to become a driver, and two things walk
// it: every authenticated API route, and a plugin's own routes when a client
// with no browser calls one. Keeping it in one place is what makes the two
// agree — a token the API refuses is a token a plugin never hears about.

// The ways a token fails, for a caller that has to say which.
var (
	// ErrNoToken is a request with no Bearer credential at all. On the API it
	// is a refusal; on a plugin route it is the common case — a browser — and
	// not a failure.
	ErrNoToken = errors.New("api: no device token")
	// ErrBadToken is a token this server did not issue: not shaped like one,
	// or shaped like one and held by nobody. The two are one answer on
	// purpose, so the shape of the refusal tells a guesser nothing.
	ErrBadToken = errors.New("api: that token is not valid on this server")
	// ErrRevokedToken is a token the operator took back.
	ErrRevokedToken = errors.New("api: that device has been removed")
)

// deviceLookup is the part of the store a token is resolved through.
type deviceLookup interface {
	DeviceByToken(ctx context.Context, prefix string, sum []byte) (db.Device, error)
	DriverByID(ctx context.Context, id int64) (db.Driver, error)
	TouchDevice(ctx context.Context, id int64) error
}

// presented is the token a request carries, split into what the database is
// queried with. The rate limiter sits between this and [resolve] on the API,
// keyed by the prefix, which is why the two are separate steps.
func presented(r *http.Request) (sum []byte, prefix string, err error) {
	token, ok := httpx.BearerToken(r)
	if !ok {
		return nil, "", ErrNoToken
	}
	sum, prefix, err = auth.SplitDeviceToken(token)
	if err != nil {
		return nil, "", ErrBadToken
	}
	return sum, prefix, nil
}

// resolve turns a split token into the device and the driver holding it, and
// records that the device was used. Anything that is not one of the sentinels
// above is the store failing, and is not the client's fault.
func resolve(ctx context.Context, store deviceLookup, sum []byte, prefix string, touch func(context.Context, int64)) (db.Device, db.Driver, error) {
	device, err := store.DeviceByToken(ctx, prefix, sum)
	switch {
	case errors.Is(err, db.ErrNotFound):
		return db.Device{}, db.Driver{}, ErrBadToken
	case err != nil:
		return db.Device{}, db.Driver{}, fmt.Errorf("the device could not be read: %w", err)
	case device.Revoked():
		return db.Device{}, db.Driver{}, ErrRevokedToken
	}
	driver, err := store.DriverByID(ctx, device.DriverID)
	if err != nil {
		return db.Device{}, db.Driver{}, fmt.Errorf("the driver could not be read: %w", err)
	}
	touch(ctx, device.ID)
	return device, driver, nil
}

// DriverByBearer is the driver whose device token the request carries. It is
// for the plugin routes: a client that talks to a plugin's own address
// authenticates the way it does to this API, the plugin is told who, and the
// token itself never reaches the plugin. The device is recorded as used, as it
// is for an upload.
//
// [ErrNoToken] is a request with no Bearer credential — a browser, usually —
// and the caller treats it as nobody rather than as an error.
func (a *API) DriverByBearer(ctx context.Context, r *http.Request) (db.Driver, error) {
	sum, prefix, err := presented(r)
	if err != nil {
		return db.Driver{}, err
	}
	_, driver, err := resolve(ctx, a.devices, sum, prefix, a.touch)
	return driver, err
}
