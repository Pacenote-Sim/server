//go:build postgres

package admin_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pacenote-sim/server/internal/admin"
	"github.com/pacenote-sim/server/internal/auth"
	"github.com/pacenote-sim/server/internal/config"
	"github.com/pacenote-sim/server/internal/db"
	"github.com/pacenote-sim/server/internal/db/dbtest"
	"github.com/pacenote-sim/server/internal/logging"
)

func TestAuthenticate(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	ctx := context.Background()

	store, err := db.Open(ctx, dbtest.URL(t), logging.Discard())
	r.NoError(err)
	t.Cleanup(store.Close)
	r.NoError(store.Migrate(ctx))

	const password = "correct horse battery staple"
	hash, err := auth.HashPassword(password)
	r.NoError(err)
	_, err = store.CompleteSetup(ctx, db.SetupRequest{
		AdminEmail:        "ana@example.com",
		AdminPasswordHash: hash,
		Settings:          config.DefaultSettings("A league", "pacenote.example.com", config.TLSProxy),
	})
	r.NoError(err)

	cases := []struct {
		name     string
		email    string
		password string
		want     bool
	}{
		{"the right pair", "ana@example.com", password, true},
		{"the address in a different case", "ANA@example.com", password, true},
		{"the wrong password", "ana@example.com", "not it", false},
		{"an address with no account", "nobody@example.com", password, false},
		{"an empty password", "ana@example.com", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := require.New(t)
			_, ok, err := admin.Authenticate(ctx, store, tc.email, tc.password)
			r.NoError(err, "a wrong password is an answer, not a failure")
			r.Equal(tc.want, ok)
		})
	}
}
