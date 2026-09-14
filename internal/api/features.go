package api

import (
	"github.com/pacenote-sim/plugin"
	"github.com/pacenote-sim/protocol/wire"
	"github.com/pacenote-sim/server/internal/auth"
	"github.com/pacenote-sim/server/internal/config"
)

// MinClient is the oldest client version this build accepts, compared against
// the X-Client-Version header and published as [wire.Discovery.MinClient]. A
// client older than it is told [wire.CodeClientTooOld] and stops uploading.
const MinClient = "1.0.0"

// Keys are the operator's own credentials, already opened. They are passed in
// rather than read here because opening them needs the data key from the
// configuration file, and this package does not read files.
//
// An absent key is not an error anywhere: it is the feature being off, which
// the client sees as a button that is not there.
// It is empty, and that is the point: this server holds no vendor credential of
// any kind and makes no outbound call to one. The coaching key went with the
// coaching and the voice key went with the voice, and both are now a plugin's to
// hold, seal and spend.
//
// The type stays rather than being deleted because it is on [Deps.Features],
// which is the seam a build with more in it changes. An enterprise build with a
// credential of its own puts it here.
type Keys struct{}

// Answering reports whether any plugin will answer a kind of request. It is how
// the discovery document decides what this installation can do, and it replaced
// asking whether a key was stored.
//
// The difference matters. A stored key said an operator had once pasted
// something into a form; this says a program is running right now that answers
// the question. A plugin that was turned off, failed to start or is missing its
// own configuration stops advertising the feature, which is what a client wants
// to know — and a second plugin answering the same kind makes no difference to
// the answer, which is also right.
type Answering interface {
	Answering(kind plugin.RequestKind) []string
}

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

// Features is what this installation actually has, in the order a client reads
// them. It is the community edition's answer and the default for [Deps].
//
// Three of the eight are unconditional, because recording one driver's laps and
// comparing them against a reference is what this server is for and needs
// nothing configured.
//
// Everything else follows the plugins: a server advertises a feature when
// something is running that answers it, and stops when it is not. That is a
// stronger statement than the stored key it replaced — a key said an operator
// had once pasted something into a form, and this says a program is answering
// the question right now.
// The second parameter is [Keys], which this edition does not read: the answer
// is what is running, not what was stored. It stays in the signature because
// [Deps.Features] is the seam a build with more in it replaces.
func Features(answering Answering, _ Keys) []wire.Feature {
	out := []wire.Feature{
		wire.FeatureTelemetry,
		wire.FeatureReference,
		wire.FeatureLive,
	}
	if answers(answering, plugin.RequestCueRace, plugin.RequestCueTraining) {
		out = append(out, wire.FeatureCoach)
	}
	if answers(answering, plugin.RequestSetup) {
		out = append(out, wire.FeatureSetups)
	}
	if answers(answering, plugin.RequestSpeak) {
		out = append(out, wire.FeatureTTS)
	}
	return out
}

// Entitled narrows a server's features to what one driver may use. The
// community edition has one organisation and no paid tiers, so every driver is
// entitled to everything the server has and the intersection is the whole list.
//
// It exists as a seam rather than as an if: GET /me publishes the intersection
// and not the server's list, and a build that grows entitlements changes this
// function and nothing else.
func Entitled(server []wire.Feature, _ int64) []wire.Feature { return server }

// OpenKeys decrypts the operator's stored credentials with the data key from the
// configuration file. There are none left to decrypt, so it returns the zero
// value — and it stays as the seam a build with credentials of its own uses.
func OpenKeys(config.Settings, auth.SecretKey) Keys { return Keys{} }

// answers reports whether anything answers any of these kinds. A nil host is a
// server running without plugins, which has no coach and says so rather than
// advertising one nothing will deliver.
func answers(a Answering, kinds ...plugin.RequestKind) bool {
	if a == nil {
		return false
	}
	for _, k := range kinds {
		if len(a.Answering(k)) > 0 {
			return true
		}
	}
	return false
}
