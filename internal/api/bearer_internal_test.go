package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pacenote-sim/server/internal/auth"
	"github.com/pacenote-sim/server/internal/db"
)

// devices is a store under the test's control: one device, one driver.
type devices struct {
	device  db.Device
	driver  db.Driver
	fail    error
	touched []int64
}

func (d *devices) DeviceByToken(_ context.Context, prefix string, _ []byte) (db.Device, error) {
	if d.fail != nil {
		return db.Device{}, d.fail
	}
	if prefix != d.device.TokenPrefix {
		return db.Device{}, db.ErrNotFound
	}
	return d.device, nil
}

func (d *devices) DriverByID(_ context.Context, id int64) (db.Driver, error) {
	if id != d.driver.ID {
		return db.Driver{}, db.ErrNotFound
	}
	return d.driver, nil
}

func (d *devices) touch(_ context.Context, id int64) { d.touched = append(d.touched, id) }

func (d *devices) TouchDevice(_ context.Context, id int64) error {
	d.touched = append(d.touched, id)
	return nil
}

// apiOver is an API whose token path runs against the store given, and nothing
// else: what the plugin routes call, without a database.
func apiOver(s *devices) *API {
	return &API{
		deps:    Deps{Now: time.Now, Log: slog.New(slog.DiscardHandler)},
		devices: s,
		touched: map[int64]time.Time{},
	}
}

func withBearer(t *testing.T, value string) *http.Request {
	t.Helper()
	r, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "/plugin/engineer/me", http.NoBody)
	require.NoError(t, err)
	if value != "" {
		r.Header.Set("Authorization", value)
	}
	return r
}

// TestATokenBecomesADriverOneWay covers the one path a token takes, and every
// way it fails — with the failure named, because the API says three different
// things and a plugin route says nothing at all.
func TestATokenBecomesADriverOneWay(t *testing.T) {
	t.Parallel()

	token, err := auth.NewDeviceToken()
	require.NoError(t, err)
	store := &devices{
		device: db.Device{ID: 42, DriverID: 7, TokenPrefix: token.Prefix},
		driver: db.Driver{ID: 7, Slug: "ana-ruiz", Name: "Ana Ruiz"},
	}

	t.Run("a token this server issued is its driver, and the device is marked used", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)

		sum, prefix, err := presented(withBearer(t, "Bearer "+token.Plain))
		r.NoError(err)
		r.Equal(token.Prefix, prefix)
		r.Equal(token.Sum, sum)

		s := *store
		device, driver, err := resolve(t.Context(), &s, sum, prefix, s.touch)
		r.NoError(err)
		r.Equal(int64(42), device.ID)
		r.Equal("ana-ruiz", driver.Slug)
		r.Equal([]int64{42}, s.touched)
	})

	t.Run("no credential at all is nobody, not a mistake", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		_, _, err := presented(withBearer(t, ""))
		r.ErrorIs(err, ErrNoToken)
		_, _, err = presented(withBearer(t, "Basic YW5hOnNlY3JldA=="))
		r.ErrorIs(err, ErrNoToken, "a Basic credential is not a Bearer one")
	})

	t.Run("a bearer that is not shaped like ours", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		_, _, err := presented(withBearer(t, "Bearer not-a-device-token"))
		r.ErrorIs(err, ErrBadToken)
	})

	t.Run("a token shaped like ours that nobody holds reads the same as a malformed one", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		other, err := auth.NewDeviceToken()
		r.NoError(err)
		s := *store
		_, _, err = resolve(t.Context(), &s, other.Sum, other.Prefix, s.touch)
		r.ErrorIs(err, ErrBadToken)
		r.Empty(s.touched, "nothing was marked as used")
	})

	t.Run("a token the operator took back", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		s := *store
		when := time.Now()
		s.device.RevokedAt = &when
		_, _, err := resolve(t.Context(), &s, token.Sum, token.Prefix, s.touch)
		r.ErrorIs(err, ErrRevokedToken)
		r.Empty(s.touched)
	})

	t.Run("the store failing is not the client's fault", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		s := *store
		s.fail = errors.New("the database went away")
		_, _, err := resolve(t.Context(), &s, token.Sum, token.Prefix, s.touch)
		r.Error(err)
		r.NotErrorIs(err, ErrBadToken)
		r.NotErrorIs(err, ErrRevokedToken)
		r.ErrorContains(err, "went away")
	})

	t.Run("DriverByBearer with nothing presented never reaches the store", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		a := &API{}
		_, err := a.DriverByBearer(t.Context(), withBearer(t, ""))
		r.ErrorIs(err, ErrNoToken)
		_, err = a.DriverByBearer(t.Context(), withBearer(t, "Bearer nope"))
		r.ErrorIs(err, ErrBadToken)
	})

	t.Run("DriverByBearer, end to end: the plugin routes' question", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)

		s := *store
		driver, err := apiOver(&s).DriverByBearer(t.Context(), withBearer(t, "Bearer "+token.Plain))
		r.NoError(err)
		r.Equal("ana-ruiz", driver.Slug)
		r.Equal("Ana Ruiz", driver.Name)
		r.Equal([]int64{42}, s.touched, "the device is marked used, as it is for an upload")

		revoked := *store
		when := time.Now()
		revoked.device.RevokedAt = &when
		_, err = apiOver(&revoked).DriverByBearer(t.Context(), withBearer(t, "Bearer "+token.Plain))
		r.ErrorIs(err, ErrRevokedToken)
		r.Empty(revoked.touched)
	})
}
