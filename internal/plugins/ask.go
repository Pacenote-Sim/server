package plugins

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/pacenote-sim/plugin"
)

// One plugin asking another.
//
// The server is the broker and nothing more. It stamps who asked — the asker's
// word is never taken for who it is, because the channel a question arrives on
// belongs to exactly one plugin — checks that the asker declared the target and
// that the target declared the kind, applies the target's daily cap and the
// deadline, counts how many plugins the question has passed through, and hands
// the payload over without reading it. What is in the payload is between the
// two plugins.

// asker is this host as one plugin may address it: the [plugin.Host] the SDK
// hands that plugin, bound to who it is.
type asker struct {
	host *Host
	from string
}

// Ask is the one question a plugin can put to the server.
func (a *asker) Ask(ctx context.Context, kind string, payload json.RawMessage) (json.RawMessage, error) {
	return a.host.askFrom(ctx, a.from, plugin.RequestKind(kind), payload, plugin.Hops(ctx))
}

// askFrom is a question from one plugin to another, with every check the broker
// owes both of them.
//
// hops is how many plugins the question has already passed through, as the
// asker's side reported it: zero for a plugin asking on its own account, the
// depth of the question it is answering otherwise. The host counts the asker
// itself as one more, and refuses a question that would pass through more than
// [plugin.MaxHops], which is what stops two plugins that ask each other.
func (h *Host) askFrom(ctx context.Context, from string, kind plugin.RequestKind, payload json.RawMessage, hops int) (json.RawMessage, error) {
	if !kind.Valid() {
		return nil, fmt.Errorf("%w: %q is not a request kind — spell it <plugin>.<what>", plugin.ErrInvalid, kind)
	}
	target := kind.Plugin()

	h.mu.RLock()
	source, known := h.instances[from]
	h.mu.RUnlock()
	if !known {
		return nil, fmt.Errorf("%w: %s is not a plugin this server runs", plugin.ErrNotAllowed, from)
	}
	if !source.mayAsk(target) {
		return nil, fmt.Errorf("%w: %s did not declare that it asks %s", plugin.ErrNotAllowed, from, target)
	}
	if hops+1 > plugin.MaxHops {
		return nil, fmt.Errorf("%w: a question from %s has already passed through %d plugins", plugin.ErrNotAllowed, from, hops)
	}

	deadline := h.opts.Now().Add(h.opts.CallTimeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	res, err := h.Ask(ctx, target, plugin.Request{
		ID:       requestID(),
		Kind:     kind,
		From:     from,
		Hops:     hops + 1,
		Deadline: deadline,
		Payload:  payload,
	})
	if err != nil {
		return nil, askerError(target, err)
	}
	return res.Payload, nil
}

// askerError is the host's own failures as the asking plugin may branch on
// them. The target not being there, not running, switched off or over its cap
// are all "not available" to the asker: it is not the asker's fault and not
// something to retry in a loop. Everything else — the target's own sentinels,
// the deadline — passes through as itself.
func askerError(target string, err error) error {
	switch {
	case errors.Is(err, ErrNoPlugin), errors.Is(err, ErrUnavailable), errors.Is(err, ErrClosed):
		return fmt.Errorf("%w: %s is not running", plugin.ErrUnavailable, target)
	case errors.Is(err, ErrOverCap):
		return fmt.Errorf("%w: %s has spent its daily cap", plugin.ErrUnavailable, target)
	default:
		return err
	}
}

// requestID is the id a brokered question carries, for the answering plugin's
// own logging and cache. It is random rather than derived so that two identical
// questions are two questions.
func requestID() string {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		// No useful fallback and no reason to refuse the question over it.
		return "unidentified"
	}
	return hex.EncodeToString(b)
}
