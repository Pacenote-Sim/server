// Package api is version 1 of the sim telemetry API: the contract a telemetry
// client talks to, exactly as docs/API-V1.md defines it.
//
// The shapes on the wire are [github.com/pacenote-sim/protocol/wire]'s and the trace codec is
// [github.com/pacenote-sim/protocol/trace]'s. Nothing is redefined here — a type that crosses
// between the client and this server lives in one module that both are built
// against, which is what keeps two programs written in separate repositories
// interoperable.
//
// # The rules that hold on every route
//
//   - Everything except discovery and pairing carries a bearer device token.
//     The lookup is by the token's clear-text prefix and the decision is a
//     constant-time comparison of digests.
//   - Every non-2xx response is the [wire.ErrorEnvelope], with one of the eight
//     stable codes. The message inside is shown to the driver exactly as
//     written, so it is a complete sentence with no jargon and no identifier.
//   - Every authenticated POST and PUT carries an Idempotency-Key. Three of
//     them are backed by the idempotency table and replay their stored answer;
//     see [API.Routes] for which, and why the others are not.
//   - Rate limiting is a token bucket per device token under a global ceiling,
//     built from the same [wire.Limits] that discovery serves, so the client's
//     intervals and the server's limiter cannot drift apart (D-7).
//   - Live samples and field reports are held in memory and never written to
//     the database (D-5). A sample two seconds old is worthless, and persisting
//     it would cost a write per second per driver for nothing.
//
// # Degrade, do not fail
//
// An optional feature that this installation does not have is named in neither
// [wire.Discovery.Features] nor [wire.Me.Features], and its endpoint answers
// [wire.CodeForbidden] rather than 404. The difference matters to a client: a
// 404 is a server it does not understand, and a forbidden feature is a button
// it turns off. The features are the core's own and follow nothing an operator
// configures; what is installed beside the core is not a feature at all but a
// list of plugins and their routes on GET /me, and a client decides what it
// knows how to talk to.
//
// # Golden fixtures
//
// testdata/golden holds one request and one response document for every
// endpoint and every error code. They are committed so that the capture client,
// which is built in another repository by people who cannot see this code, can
// assert against the same bytes this server produces. Regenerate them with
//
//	go test ./internal/api -update
//
// and read what that means before doing it: a fixture that changes is a change
// to the contract.
package api
