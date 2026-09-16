package api

import (
	"sort"

	"github.com/pacenote-sim/plugin"
	"github.com/pacenote-sim/protocol/wire"
)

// Running is a plugin host that can say what is running. It is the second thing
// the API asks of [Deps.Plugins], beside receiving events, and it is optional
// for the same reason [EventSink] is an interface: this package must not depend
// on how plugins are run, and a server built without a host has nothing running
// and says so.
type Running interface {
	// Running is the manifest of every plugin running right now.
	Running() []plugin.Manifest
}

// installed is what GET /me tells a client about the plugins here: each one
// that is running and has an address a client could reach, by name, with its
// routes. It is never nil, so the document always carries the key and a client
// author sees it in the fixture.
func (a *API) installed() []wire.PluginInfo {
	out := []wire.PluginInfo{}
	if a.deps.Plugins == nil {
		return out
	}
	host, ok := a.deps.Plugins.(Running)
	if !ok {
		return out
	}
	for _, m := range host.Running() {
		if info, ok := advertise(m); ok {
			out = append(out, info)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// advertise is one plugin as a client is told about it, or false when there is
// nothing to tell.
//
// A client is told about the routes it could reach: public ones, ones for a
// signed-in driver, and ones the plugin guards itself. An operator's pages are
// left out — a client has no way to open them, and a list of addresses that
// answer "sign in as an administrator" is noise. A plugin with no route a
// client could reach is left out altogether: there is nothing to talk to.
//
// The title falls back to the name, because a client shows one and an empty
// string is a blank button.
func advertise(m plugin.Manifest) (wire.PluginInfo, bool) {
	if m.Capabilities.HTTP == nil {
		return wire.PluginInfo{}, false
	}
	routes := make([]wire.PluginRoute, 0, len(m.Capabilities.HTTP.Routes))
	for _, r := range m.Capabilities.HTTP.Routes {
		if r.Access == plugin.AccessAdmin {
			continue
		}
		routes = append(routes, wire.PluginRoute{Path: r.Path, Access: string(r.Access)})
	}
	if len(routes) == 0 {
		return wire.PluginInfo{}, false
	}
	title := m.Capabilities.HTTP.Title
	if title == "" {
		title = m.Name
	}
	return wire.PluginInfo{Name: m.Name, Title: title, Version: m.Version, Routes: routes}, true
}
