package api

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pacenote-sim/plugin"
	"github.com/pacenote-sim/protocol/wire"
)

// What a client is told about the plugins here. The rule is one line — running,
// and reachable by a client — and every case below is one way of failing it.

// runningHost is a plugin host under the test's control: it is told what runs.
type runningHost struct{ manifests []plugin.Manifest }

func (h *runningHost) Notify(context.Context, plugin.Event) {}
func (h *runningHost) Running() []plugin.Manifest           { return h.manifests }

func manifestWith(name, title string, routes ...plugin.Route) plugin.Manifest {
	m := plugin.Manifest{Name: name, Version: "1.2.3", Author: "a", Description: "d", InterfaceVersion: plugin.InterfaceVersion}
	if title != "" || len(routes) > 0 {
		m.Capabilities.HTTP = &plugin.HTTPCapability{Title: title, Routes: routes}
	}
	return m
}

func TestInstalledIsWhatAClientCouldTalkTo(t *testing.T) {
	t.Parallel()

	t.Run("a running plugin, with the operator's pages left out", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)

		a := &API{deps: Deps{Plugins: &runningHost{manifests: []plugin.Manifest{
			manifestWith("engineer", "Engineer",
				plugin.Route{Path: "/", Access: plugin.AccessAdmin},
				plugin.Route{Path: "/agent", Access: plugin.AccessAdmin},
				plugin.Route{Path: "/me", Access: plugin.AccessDriver, Reason: "their own debriefs"},
				plugin.Route{Path: "/telemetry", Access: plugin.AccessCustom, Reason: "its own token"},
			),
		}}}}
		r.Equal([]wire.PluginInfo{{
			Name: "engineer", Title: "Engineer", Version: "1.2.3",
			Routes: []wire.PluginRoute{{Path: "/me", Access: "driver"}, {Path: "/telemetry", Access: "custom"}},
		}}, a.installed())
	})

	t.Run("a plugin with only operator pages is not mentioned", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)

		a := &API{deps: Deps{Plugins: &runningHost{manifests: []plugin.Manifest{
			manifestWith("results", "Results", plugin.Route{Path: "/", Access: plugin.AccessAdmin}),
		}}}}
		r.Empty(a.installed(), "there is nothing a client could reach")
	})

	t.Run("a plugin with no routes at all is not mentioned", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)

		a := &API{deps: Deps{Plugins: &runningHost{manifests: []plugin.Manifest{manifestWith("quiet", "")}}}}
		r.Empty(a.installed())
	})

	t.Run("the title falls back to the name", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)

		a := &API{deps: Deps{Plugins: &runningHost{manifests: []plugin.Manifest{
			manifestWith("payments", "", plugin.Route{Path: "/webhook", Access: plugin.AccessPublic}),
		}}}}
		got := a.installed()
		r.Len(got, 1)
		r.Equal("payments", got[0].Title, "a client shows the title, and a blank one is a blank button")
	})

	t.Run("in name order, whatever order the host answers in", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)

		a := &API{deps: Deps{Plugins: &runningHost{manifests: []plugin.Manifest{
			manifestWith("zulu", "Z", plugin.Route{Path: "/", Access: plugin.AccessPublic}),
			manifestWith("alpha", "A", plugin.Route{Path: "/", Access: plugin.AccessPublic}),
		}}}}
		got := a.installed()
		r.Equal([]string{"alpha", "zulu"}, []string{got[0].Name, got[1].Name})
	})

	t.Run("a server with no host has none, and the key is still there", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)

		for name, a := range map[string]*API{
			"no host at all":            {},
			"a host that only receives": {deps: Deps{Plugins: &recordingSink{}}},
		} {
			got := a.installed()
			r.NotNil(got, name)
			r.Empty(got, name)

			encoded, err := json.Marshal(wire.Me{Plugins: got})
			r.NoError(err)
			r.Contains(string(encoded), `"plugins":[]`, "%s: the key is present and empty, never null", name)
		}
	})
}
