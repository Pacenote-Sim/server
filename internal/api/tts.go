package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/pacenote-sim/plugin"
	"github.com/pacenote-sim/protocol/wire"
	"github.com/pacenote-sim/server/internal/httpx"
)

// ttsBodyLimit caps the request. The text is one sentence.
const ttsBodyLimit int64 = 8 << 10

// postTTS speaks a coaching cue.
//
// This server has no voice of its own, names no engine and holds no credential
// for one. It hands the text to whichever plugin answers [plugin.RequestSpeak]
// and relays the bytes that come back, so an operator points their installation
// at whatever service they pay for by installing a plugin for it.
//
// The route stays here rather than moving with the rest of it because it is in
// the client's API contract: a client that met a 404 would decide it was talking
// to a server it does not understand, where one that meets "the operator has not
// installed a voice plugin" turns a button off.
//
// The call carries an idempotency key like every other write, and it is not
// replayed: the client caches audio by text already, so a stored copy here would
// be a second cache of the same bytes with a worse eviction rule.
func (a *API) postTTS(w http.ResponseWriter, r *http.Request, s session) {
	if !s.has(wire.FeatureTTS) {
		a.fail(w, r, wire.CodeForbidden, msgTTSUnavailable)
		return
	}
	if _, ok := a.idempotencyKey(w, r); !ok {
		return
	}

	var body wire.TTSRequest
	if _, err := httpx.DecodeJSONFingerprint(w, r, &body, ttsBodyLimit); err != nil {
		a.invalid(w, r, err)
		return
	}
	switch {
	case blank(body.Text):
		a.failDetail(w, r, wire.CodeInvalid,
			"That request had nothing to say — no audio was made.",
			map[string]any{"field": "text"})
		return
	case len(body.Text) > MaxTTSTextLen:
		a.failDetail(w, r, wire.CodeInvalid,
			"That cue is longer than this server speaks — no audio was made.",
			map[string]any{"field": "text", "max_bytes": MaxTTSTextLen})
		return
	}

	res, err := a.speak(r.Context(), s, body)
	if err != nil {
		if errors.Is(err, plugin.ErrNotConfigured) {
			a.fail(w, r, wire.CodeForbidden, msgTTSUnconfigured)
			return
		}
		// The plugin's credential is never in this line. It is not put in, and
		// the host's scrubber would take it out if it were.
		a.deps.Log.LogAttrs(r.Context(), slog.LevelWarn, "nothing spoke the cue",
			slog.Any("error", err))
		a.fail(w, r, wire.CodeServerError, msgTTSFailed)
		return
	}

	w.Header().Set("Content-Type", res.AudioType)
	w.Header().Set("Cache-Control", "private, max-age=86400")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(res.Audio)
}

// speak asks the plugins for audio and checks what comes back before any of it
// reaches a client.
//
// Only the two media types the contract names are relayed. A plugin answering
// with something else — an HTML error page it did not notice, most likely — is a
// failure and not a cue, and passing it through would have the client try to
// play it.
func (a *API) speak(ctx context.Context, s session, body wire.TTSRequest) (plugin.Response, error) {
	host := a.asking()
	if host == nil {
		return plugin.Response{}, plugin.ErrNotConfigured
	}
	res, err := host.Ask(ctx, plugin.Request{
		Kind:   plugin.RequestSpeak,
		Driver: plugin.Driver{Slug: s.driver.Slug, Name: s.driver.Name},
		Text:   body.Text,
		Voice:  voiceOf(body),
	})
	if err != nil {
		return plugin.Response{}, err
	}
	if len(res.Audio) == 0 {
		return plugin.Response{}, errors.New("api: the voice plugin answered with no audio")
	}
	if !playable(res.AudioType) {
		return plugin.Response{}, errors.New("api: the voice plugin answered with " + res.AudioType + ", which a client cannot play")
	}
	return res, nil
}

// voiceOf is the voice a caller asked for, or empty for the operator's default.
// The field is a pointer on the wire so that "use the default" and "use this
// one" are distinguishable, and nothing downstream wants a pointer.
func voiceOf(body wire.TTSRequest) string {
	if body.Voice == nil {
		return ""
	}
	return *body.Voice
}

// playable reports whether a media type is one the contract names. Anything
// else is refused rather than relayed.
func playable(contentType string) bool {
	switch contentType {
	case "audio/wav", "audio/mpeg":
		return true
	}
	return false
}
