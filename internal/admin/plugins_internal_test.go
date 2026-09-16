package admin

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pacenote-sim/plugin"
	"github.com/pacenote-sim/server/internal/plugins"
)

// A plugin's own pages, as links.
//
// The capability list already said a plugin serves an address. This is how an
// operator gets to it: engineer installs a page for editing what the coach is,
// and before this the only way to find it was to read the manifest.
func TestAPluginsPagesAreLinkedFromItsPage(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	pages := pagesOf(plugins.Status{
		Name: "engineer",
		Capabilities: plugin.Capabilities{HTTP: &plugin.HTTPCapability{Routes: []plugin.Route{
			{Path: "/", Access: plugin.AccessAdmin},
			{Path: "/agent", Access: plugin.AccessAdmin},
			{Path: "/me", Access: plugin.AccessDriver, Reason: "a driver reads their own"},
			{Path: "/hook", Access: plugin.AccessPublic},
			{Path: "/own", Access: plugin.AccessCustom, Reason: "it checks a signature"},
		}}},
	})
	r.Len(pages, 5)

	by := map[string]pluginPage{}
	for _, p := range pages {
		by[p.Path] = p
	}

	// The address is the plugin's mount plus its own path, and it is a link an
	// operator can actually follow.
	r.Equal("/plugin/engineer/agent", by["/agent"].Href)
	r.True(by["/agent"].Yours)
	r.Equal("you", by["/agent"].Reach)

	// A driver's page is listed so the operator knows it exists and can say
	// where it is — but not offered as a link, because following it as an
	// administrator produces a refusal and looks like a broken page.
	r.False(by["/me"].Yours)
	r.Equal("a signed-in driver", by["/me"].Reach)
	r.Equal("a driver reads their own", by["/me"].Reason)

	r.True(by["/hook"].Yours)
	r.Equal("anyone", by["/hook"].Reach)
	r.Equal("whoever the plugin decides", by["/own"].Reach)

	// Sorted, so the list does not reorder itself between page loads.
	r.Equal([]string{"/", "/agent", "/hook", "/me", "/own"},
		[]string{pages[0].Path, pages[1].Path, pages[2].Path, pages[3].Path, pages[4].Path})

	// A plugin that serves nothing offers nothing.
	r.Empty(pagesOf(plugins.Status{Name: "quiet"}))
}

// An access this build does not understand is not offered as a link, because
// the server will refuse it and the operator would only find out by clicking.
func TestAPageThisServerDoesNotUnderstand(t *testing.T) {
	t.Parallel()

	pages := pagesOf(plugins.Status{
		Name: "future",
		Capabilities: plugin.Capabilities{HTTP: &plugin.HTTPCapability{Routes: []plugin.Route{
			{Path: "/x", Access: plugin.Access("from-the-future")},
		}}},
	})
	require.Len(t, pages, 1)
	require.False(t, pages[0].Yours)
	require.Contains(t, pages[0].Reach, "nobody")
}
