package plugins_test

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pacenote-sim/plugin"
	"github.com/pacenote-sim/server/internal/plugins"
)

// One plugin asking another, through this host, with two real processes: alpha
// and beta are both the example plugin, installed under two names. Alpha's
// ".ask" relays whatever it is told to ask; the broker in between is what these
// tests are about.

// pair installs alpha and beta and waits for both to run.
func pair(t *testing.T) *harness {
	t.Helper()
	h := newHarness(t)
	install(t, h.dir, "alpha", manifestFor("alpha"))
	install(t, h.dir, "beta", manifestFor("beta"))
	require.NoError(t, h.host.Discover(t.Context()))
	eventually(t, "both plugins to start", func() bool {
		return stateOf(h.host, "alpha") == plugins.StateRunning && stateOf(h.host, "beta") == plugins.StateRunning
	})
	return h
}

// relay is the question that makes alpha ask something.
func relay(kind, payload string) plugin.Request {
	return plugin.Request{
		ID: "r-1", Kind: "alpha.ask", From: "test",
		Payload: json.RawMessage(`{"kind":"` + kind + `","payload":` + payload + `}`),
	}
}

func TestOnePluginAsksAnotherThroughTheHost(t *testing.T) {
	t.Parallel()
	h := pair(t)

	t.Run("the question reaches the other plugin, stamped with who asked", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)

		res, err := h.host.Ask(t.Context(), "alpha", relay("beta.echo", `{"hello":1}`))
		r.NoError(err)

		var got struct {
			From string          `json:"from"`
			Hops int             `json:"hops"`
			Echo json.RawMessage `json:"echo"`
		}
		r.NoError(json.Unmarshal(res.Payload, &got))
		r.Equal("alpha", got.From, "the host stamped the asker; alpha did not name itself")
		r.Equal(1, got.Hops, "one plugin passed through")
		r.JSONEq(`{"hello":1}`, string(got.Echo), "the payload arrived as it was asked")
	})

	t.Run("a plugin the asker did not declare is refused before it is asked", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)

		_, err := h.host.Ask(t.Context(), "alpha", relay("gamma.echo", `{}`))
		r.ErrorIs(err, plugin.ErrNotAllowed)
		r.ErrorContains(err, "did not declare that it asks gamma")
	})

	t.Run("a plugin that is declared and not there is not available", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)

		_, err := h.host.Ask(t.Context(), "alpha", relay("otherplugin.echo", `{}`))
		r.ErrorIs(err, plugin.ErrUnavailable)
	})

	t.Run("a kind the other plugin never said it answers", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)

		_, err := h.host.Ask(t.Context(), "alpha", relay("beta.nothing", `{}`))
		r.ErrorIs(err, plugin.ErrUnsupported)
	})

	t.Run("a kind that is not spelled like one", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)

		_, err := h.host.Ask(t.Context(), "alpha", relay("nodot", `{}`))
		r.ErrorIs(err, plugin.ErrInvalid)
	})

	t.Run("two plugins that ask each other are stopped", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)

		// alpha.chain asks beta.chain asks alpha.chain ... until the host
		// refuses the question that would pass through a fourth plugin. The
		// plugin that met the refusal answers with how deep it got, and that
		// answer comes all the way back.
		res, err := h.host.Ask(t.Context(), "alpha", plugin.Request{
			ID: "r-1", Kind: "alpha.chain", From: "test", Payload: json.RawMessage(`{"partner":"beta"}`),
		})
		r.NoError(err)
		r.JSONEq(`{"reached":3}`, string(res.Payload))
	})
}

func TestAskingIsMeteredAgainstThePluginThatAnswered(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	h := pair(t)
	h.store.setValueFor("beta", "budget", "40")

	_, err := h.host.Ask(t.Context(), "alpha", relay("beta.echo", `{}`))
	r.NoError(err)

	var metered int
	for _, u := range h.store.calls() {
		if u.Plugin == "beta" && u.Job == "beta.echo" {
			metered++
			r.Nil(u.DriverID, "a question is not about a driver")
			r.Equal(int64(40), u.Input+u.Output)
		}
		r.NotEqual("alpha", u.Plugin, "the asker spent nothing; the answerer did the work")
	}
	r.Equal(1, metered, "the answer was metered against beta, once")

	// And when beta has spent its day, alpha is told it is not available —
	// the cap is beta's and beta's operator's, not something alpha can see
	// past.
	r.NoError(h.host.SetDailyCap(t.Context(), "beta", 10))
	_, err = h.host.Ask(t.Context(), "alpha", relay("beta.echo", `{}`))
	r.ErrorIs(err, plugin.ErrUnavailable)
	r.ErrorContains(err, "daily cap")
}

func TestAPluginThatIsSwitchedOffCannotBeAsked(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	h := pair(t)
	r.NoError(h.host.Disable(t.Context(), "beta"))

	_, err := h.host.Ask(t.Context(), "alpha", relay("beta.echo", `{}`))
	r.ErrorIs(err, plugin.ErrUnavailable, "switched off reads as not there, which is what the operator asked for")
}
