package plugins

import (
	"strings"
	"sync"

	"github.com/pacenote-sim/plugin"
)

// minSecretLength is the shortest credential worth scrubbing for. Below it the
// value is more likely to appear in ordinary prose than to be the secret, and
// replacing every "a" in a plugin's output would make the output useless
// without making anything safer. An operator who sets a three-character API key
// has a different problem.
const minSecretLength = 6

// scrubber removes the operator's credentials from anything a plugin says.
//
// This is the mechanism behind the promise that a secret never reaches a log
// line. The contract's [plugin.Secret] type stops one leaking by accident
// inside this process; it can do nothing about the other process, which is free
// to print the key it was lent straight to standard error. So the host keeps
// the values it has lent out and scrubs everything coming back — error
// messages, captured output, anything about to be logged or stored.
//
// It is a blunt instrument and that is the right shape for it. A credential
// split across two writes survives, which is why the buffer it guards is
// line-oriented; a credential printed backwards survives, which is not a threat
// model anybody has. What it catches is the ordinary case, which is the case
// that actually happens: a plugin author logging the request they were given.
type scrubber struct {
	mu     sync.RWMutex
	values []string
}

// learn adds credentials to scrub for. They accumulate rather than replace: a
// key the operator changed five minutes ago may still be in a buffer, and
// forgetting it would be forgetting to scrub it.
func (s *scrubber) learn(secrets plugin.Secrets) {
	if len(secrets) == 0 {
		return
	}
	values := make([]string, 0, len(secrets))
	for _, name := range secrets.Names() {
		values = append(values, secrets[name].Value())
	}
	s.learnValues(values...)
}

// learnValues is learn for credentials that are not settings — a plugin's own
// database password and the connection string carrying it. They are generated
// by the host rather than entered by the operator, and a plugin that prints its
// own connection string must not be able to write it into last_output, where
// the panel would show it.
func (s *scrubber) learnValues(values ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, v := range values {
		if len(v) < minSecretLength {
			continue
		}
		if !contains(s.values, v) {
			s.values = append(s.values, v)
		}
	}
}

// clean replaces every credential it knows about with [plugin.Redacted].
func (s *scrubber) clean(text string) string {
	if text == "" {
		return text
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, v := range s.values {
		text = strings.ReplaceAll(text, v, plugin.Redacted)
	}
	return text
}

// cleanError is clean for an error, which is where a leaked credential most
// often travels: a vendor library putting the request it failed on into the
// message it returns.
func (s *scrubber) cleanError(err error) string {
	if err == nil {
		return ""
	}
	return s.clean(err.Error())
}

func contains(values []string, v string) bool {
	for _, existing := range values {
		if existing == v {
			return true
		}
	}
	return false
}

// tail is the last of what a plugin printed to standard error, kept so the
// panel can show an operator why a plugin will not start.
//
// It is bounded, because a plugin in a crash loop can print without limit, and
// it is scrubbed on the way in rather than on the way out, so that a credential
// is never in this process's memory in a form something else could log.
type tail struct {
	mu    sync.Mutex
	limit int
	buf   []byte
	scrub *scrubber
}

// newTail keeps at most limit bytes.
func newTail(limit int, scrub *scrubber) *tail {
	return &tail{limit: limit, scrub: scrub}
}

// Write takes a line of the plugin's standard error.
func (t *tail) Write(p []byte) (int, error) {
	n := len(p)
	cleaned := []byte(t.scrub.clean(string(p)))

	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, cleaned...)
	if !strings.HasSuffix(string(cleaned), "\n") {
		t.buf = append(t.buf, '\n')
	}
	if len(t.buf) > t.limit {
		t.buf = t.buf[len(t.buf)-t.limit:]
	}
	// The caller is go-plugin copying the plugin's output; it is told the
	// whole write landed, because a short write would make it retry something
	// that was deliberately dropped.
	return n, nil
}

// String is what was kept, oldest first.
func (t *tail) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return strings.TrimRight(string(t.buf), "\n")
}
