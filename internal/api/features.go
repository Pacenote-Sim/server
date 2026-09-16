package api

import "github.com/pacenote-sim/protocol/wire"

// MinClient is the oldest client version this build accepts, compared against
// the X-Client-Version header and published as [wire.Discovery.MinClient]. A
// client older than it is told [wire.CodeClientTooOld] and stops uploading.
const MinClient = "1.0.0"

// EnterpriseFeatures are the ones this build does not have at all. They are
// named rather than merely absent so that the line is visible in the code:
// community is about one driver getting faster, and both of these are about a
// group being organised.
//
//   - field: the whole-session relay feeding a broadcast timing view.
//   - competition: championships, qualifying sessions, results.
//
// Their endpoints are built and gated rather than missing, so a client meets
// [wire.CodeForbidden] and turns a button off instead of meeting a 404 and
// deciding it is talking to a server it does not understand.
var EnterpriseFeatures = []wire.Feature{wire.FeatureField, wire.FeatureCompetition}

// Features is what this installation has, in the order a client reads them. It
// is the community edition's answer and the default for [Deps.Features].
//
// All three are unconditional, because recording one driver's laps and
// comparing them against a reference is what this server is for and needs
// nothing configured. Nothing here follows a plugin: a plugin is not a feature,
// and what is installed is a different question with a different answer —
// [wire.Me.Plugins], built by [API.installed]. The enterprise features are
// absent from this build rather than off.
func Features() []wire.Feature {
	return []wire.Feature{
		wire.FeatureTelemetry,
		wire.FeatureReference,
		wire.FeatureLive,
	}
}

// Entitled narrows a server's features to what one driver may use. The
// community edition has one organisation and no paid tiers, so every driver is
// entitled to everything the server has and the intersection is the whole list.
//
// It exists as a seam rather than as an if: GET /me publishes the intersection
// and not the server's list, and a build that grows entitlements changes this
// function and nothing else.
func Entitled(server []wire.Feature, _ int64) []wire.Feature { return server }
