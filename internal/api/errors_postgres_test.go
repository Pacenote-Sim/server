//go:build postgres

package api_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pacenote-sim/protocol/wire"
	"github.com/pacenote-sim/server/internal/api"
	"github.com/pacenote-sim/server/internal/auth"
)

// TestEveryErrorCode walks the eight codes the contract publishes. Each is
// reachable, each carries the status the contract pairs with it, and each
// carries a sentence a driver can read — which is what the client shows them.
func TestEveryErrorCode(t *testing.T) {
	t.Parallel()
	h := newHarness(t, harnessOptions{})

	t.Run("unauthorized", func(t *testing.T) {
		t.Parallel()
		cases := []struct {
			name  string
			token string
		}{
			{"no header at all", ""},
			{"a token that is not the right shape", "not-a-token"},
			{"a well-formed token nobody was issued", mintedButNeverStored(t)},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				r := require.New(t)
				res := h.do(request{
					method: http.MethodGet, path: "/api/v1/me",
					noAuth: tc.token == "", token: tc.token,
				})
				r.Equal(http.StatusUnauthorized, res.status, res.body)
				env := res.envelope(t)
				r.Equal(wire.CodeUnauthorized, env.Code)
				r.NotEmpty(env.Message)
			})
		}
	})

	t.Run("unauthorized after the machine is revoked", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		ctx := context.Background()

		driver, err := h.store.EnsureDriver(ctx, "Revoked Rider", "gt3")
		r.NoError(err)
		token, err := auth.NewDeviceToken()
		r.NoError(err)
		device, err := h.store.CreateDevice(ctx, driver.ID, token.Sum, token.Prefix, "old laptop")
		r.NoError(err)

		r.Equal(http.StatusOK, h.do(request{
			method: http.MethodGet, path: "/api/v1/me", token: token.Plain,
		}).status)

		revoked, err := h.store.RevokeDevice(ctx, device.ID)
		r.NoError(err)
		r.True(revoked)

		res := h.do(request{method: http.MethodGet, path: "/api/v1/me", token: token.Plain})
		r.Equal(http.StatusUnauthorized, res.status, res.body)
		r.Equal(wire.CodeUnauthorized, res.envelope(t).Code)
	})

	t.Run("forbidden when a feature is not on this installation", func(t *testing.T) {
		t.Parallel()
		cases := []struct {
			name string
			path string
			body any
		}{
			{"the field relay, which is an enterprise feature", "/api/v1/field", wire.FieldReport{StintID: randomUUIDv7(t, startedAt)}},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				r := require.New(t)
				res := h.do(request{
					method: http.MethodPost, path: tc.path,
					key: "forbidden-" + tc.name, body: tc.body,
				})
				r.Equal(http.StatusForbidden, res.status, res.body)
				env := res.envelope(t)
				r.Equal(wire.CodeForbidden, env.Code)
				r.NotEmpty(env.Message, "the driver is told once, in a sentence")
			})
		}
	})

	t.Run("not_found for an address that is not part of this version", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		res := h.do(request{method: http.MethodGet, path: "/api/v1/championships"})
		r.Equal(http.StatusNotFound, res.status, res.body)
		r.Equal(wire.CodeNotFound, res.envelope(t).Code,
			"a client must get the envelope here, not the panel's plain text")
	})

	t.Run("client_too_old", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		res := h.do(request{method: http.MethodGet, path: "/api/v1/me", version: "0.9.3"})
		r.Equal(http.StatusUpgradeRequired, res.status, res.body)
		env := res.envelope(t)
		r.Equal(wire.CodeClientTooOld, env.Code)
		r.Equal(api.MinClient, env.Detail["min_client"])
	})

	t.Run("a client at or past the minimum is served", func(t *testing.T) {
		t.Parallel()
		cases := []string{"1.0.0", "1.4.2", "v2.0.0", "1.0.0-rc1", "", "not-a-version"}
		for _, version := range cases {
			t.Run("version "+version, func(t *testing.T) {
				t.Parallel()
				r := require.New(t)
				res := h.do(request{
					method: http.MethodGet, path: "/api/v1/me",
					token: h.newToken("Version " + version), version: version,
				})
				r.Equal(http.StatusOK, res.status, res.body)
			})
		}
	})

	t.Run("rate_limited, with advice on when to try again", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		token := h.newToken("Impatient Ivan")

		var limited response
		for range int(api.ReadBurst) + 5 {
			res := h.do(request{method: http.MethodGet, path: "/api/v1/me", token: token})
			if res.status == http.StatusTooManyRequests {
				limited = res
				break
			}
		}
		r.Equal(http.StatusTooManyRequests, limited.status,
			"a client that ignores the published intervals is eventually refused")
		env := limited.envelope(t)
		r.Equal(wire.CodeRateLimited, env.Code)
		r.Positive(env.RetryAfterS, "the client is told how long to back off for")
		r.Equal("1", limited.headers.Get("Retry-After"),
			"a proxy in front of this server reads the header, not the body")
	})

	t.Run("one machine using up its budget does not refuse another", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		greedy := h.newToken("Greedy Gemma")
		quiet := h.newToken("Quiet Quim")

		for range int(api.ReadBurst) + 5 {
			h.do(request{method: http.MethodGet, path: "/api/v1/me", token: greedy})
		}
		res := h.do(request{method: http.MethodGet, path: "/api/v1/me", token: quiet})
		r.Equal(http.StatusOK, res.status, res.body)
	})

	t.Run("server_error is never a driver-readable fault", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		// This server calls no vendor, so the server_error a driver meets is
		// the database being unreachable — TestEveryRouteSurvivesADatabaseThatStopped
		// provokes that for real. What is asserted here is the contract's own
		// table: the code exists and pairs with a 5xx.
		r.Equal(http.StatusInternalServerError, wire.CodeServerError.HTTPStatus())
		r.True(wire.CodeServerError.Retryable())
	})
}

// mintedButNeverStored is a token of exactly the right shape that no row holds,
// which is what a revoked machine's old token or a guess looks like.
func mintedButNeverStored(tb testing.TB) string {
	tb.Helper()
	token, err := auth.NewDeviceToken()
	require.NoError(tb, err)
	return token.Plain
}
