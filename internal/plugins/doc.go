// Package plugins runs the server's plugins.
//
// A plugin is a separate process. The host in this package finds them, starts
// them, speaks to them over a local channel, restarts them when they crash and
// gives up when they keep crashing. It implements the contract in the
// github.com/pacenote-sim/plugin module and adds nothing to it: everything a plugin author
// sees is published there, and this package is one host among the two there
// will eventually be.
//
// # Why a separate process
//
// Three reasons, and the first is the whole argument on a server that is meant
// to run for a season: a crashing plugin does not take the server down. The
// second is that the same design works for a closed host, which the enterprise
// edition will be. The third is licensing — a plugin talking to a host over a
// local channel is not a derivative work of it, which is what makes a closed
// commercial plugin on top of this GPL-3 server possible at all.
//
// The cost is a process per plugin and a round trip per call, which is
// irrelevant at the rate a team generates events.
//
// # What this host provides, and what it does not
//
// Five things, which are the five the first demanding plugin needs:
//
//   - Events, fire and forget, carrying derived facts and never raw traces.
//   - Requests, with a deadline, where the caller is told plainly when there
//     was no answer so it can fall back.
//   - Sealed credentials, held by the core and lent one call at a time.
//   - Metering, with the daily cap enforced here and not in the plugin.
//   - Settings, declared by the plugin and configured in the panel.
//
// Serving an endpoint, adding a page, enriching data on the way in and the
// marketplace are all in the plan and none of them are here. They wait until
// something asks for them.
//
// # What it refuses to do
//
// It does not run a plugin built against a different interface version, and it
// does not offer a degraded mode for one. It does not retry an event. It does
// not let a plugin decide how much of the operator's money to spend. And it
// does not write a credential into a log line or a database column, which is
// enforced rather than promised: everything a plugin says passes through a
// scrubber that knows the credentials it was lent.
package plugins
