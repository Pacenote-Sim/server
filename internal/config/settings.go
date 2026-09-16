package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"

	"github.com/pacenote-sim/protocol/wire"
)

// TLSMode is how the server is reached from the internet. The operator chooses
// once, in the wizard, and can change it later in the admin panel.
type TLSMode string

// The two ways a server is reached.
const (
	// TLSProxy is plain HTTP on a port, with a reverse proxy in front doing
	// the certificate. The operator already has nginx, Caddy or a load
	// balancer and wants this to stay out of the way.
	TLSProxy TLSMode = "proxy"
	// TLSAuto is an automatic Let's Encrypt certificate for [Settings.PublicHost].
	// It is what lets a bare binary on a fresh server talk to a real client,
	// which needs HTTPS, and it needs ports 80 and 443.
	TLSAuto TLSMode = "auto"
)

// Valid reports whether m is one of the two modes.
func (m TLSMode) Valid() bool { return m == TLSProxy || m == TLSAuto }

// Settings are the organisation's own configuration. They live in the database
// rather than in the file beside the binary, because they are what makes this
// deployment itself: delete the data directory and these survive.
type Settings struct {
	// Organisation is what drivers see when their client connects. It is
	// wire.Discovery.Name.
	Organisation string `json:"organisation"`
	// PublicHost is the host name drivers' clients reach, with no scheme and
	// no port. Pairing links are built from it, and it is the name on the
	// certificate when TLSMode is [TLSAuto].
	PublicHost string `json:"public_host"`
	// TLSMode is how that host is served.
	TLSMode TLSMode `json:"tls_mode"`
	// ShortName, Logo and Accent are the rest of the white-label branding a
	// client wears once it is paired. ShortName empty means the initials of
	// Organisation; Logo is an absolute URL; Accent is a CSS hex colour, and
	// empty means the client keeps its own.
	ShortName string `json:"short_name,omitempty"`
	Logo      string `json:"logo,omitempty"`
	Accent    string `json:"accent,omitempty"`
	// Limits are served in discovery and enforced by the server.
	Limits wire.Limits `json:"limits"`
	// Retention is how long lap traces are kept. It is a setting rather than a
	// constant because what a team can afford to store is the operator's
	// answer and not this server's.
	Retention Retention `json:"retention"`
}

// MaxRetentionMonths is the longest retention an operator may set. Ten years is
// past the point where the setting means anything — a team that wants its
// traces for ever says so with [KeepTracesForever] rather than with a number
// nobody will be around to see expire.
const MaxRetentionMonths = 120

// KeepTracesForever is the [Retention.TraceMonths] that deletes nothing. It is
// zero so that an installation that has never opened the data page keeps
// everything, which is the only safe thing for a default to do.
const KeepTracesForever = 0

// Retention is how long this server keeps the compressed trace of a lap.
//
// It applies to traces and not to laps. A lap's number, its time and the stint
// it belongs to are the driver's record and are never deleted here; the trace
// is the shape of the lap, it is the bulk of the data, and it is the part an
// operator runs out of disk for.
type Retention struct {
	// TraceMonths is how many months of traces to keep, counted from the lap's
	// own start. [KeepTracesForever] keeps them all.
	TraceMonths int `json:"trace_months"`
}

// Keeps reports whether this setting deletes anything at all.
func (r Retention) Keeps() bool { return r.TraceMonths > KeepTracesForever }

// Cutoff is the instant a trace has to be older than to be deleted. The second
// result is false when nothing is deleted, which the caller must check before
// using the first: a zero-month setting would otherwise read as a cutoff of now
// and delete everything.
func (r Retention) Cutoff(now time.Time) (time.Time, bool) {
	if !r.Keeps() {
		return time.Time{}, false
	}
	return now.AddDate(0, -r.TraceMonths, 0), true
}

// ValidateRetention reports what is wrong with a retention setting, phrased for
// the operator who typed it.
func ValidateRetention(r Retention) error {
	if r.TraceMonths < 0 {
		return errors.New("config: a retention of fewer than zero months is not a length of time")
	}
	if r.TraceMonths > MaxRetentionMonths {
		return fmt.Errorf("config: the retention is %d months, and the longest this server sets is %d",
			r.TraceMonths, MaxRetentionMonths)
	}
	return nil
}

// APIBase is where version 1 is mounted, and DiscoveryPath the one document a
// client fetches before anything else. They are constants here rather than in
// the API package because the discovery document names the first and is served
// from the second, and two spellings of either would be a client that cannot
// find the server it was pointed at.
const (
	APIBase       = "/api/v1"
	DiscoveryPath = "/.well-known/sim-telemetry.json"
)

// DefaultSettings are the settings of an organisation that has said nothing but
// its name, the host drivers reach it on, and how that host is served. Every
// other field has an answer that is right for most installations, and the wizard
// asks for none of them.
func DefaultSettings(organisation, host string, mode TLSMode) Settings {
	return Settings{
		Organisation: organisation,
		PublicHost:   host,
		TLSMode:      mode,
		ShortName:    shortName(organisation),
		Limits:       DefaultLimits(),
		Retention:    Retention{TraceMonths: KeepTracesForever},
	}
}

// Scheme is how a driver's client reaches this server: https everywhere except
// a development run on localhost, which has no certificate and does not need
// one.
func (s Settings) Scheme() string {
	if s.PublicHost == "localhost" || strings.HasPrefix(s.PublicHost, "localhost:") ||
		strings.HasPrefix(s.PublicHost, "127.0.0.1") {
		return "http"
	}
	return "https"
}

// BaseURL is the address everything a driver is sent to is built from: the
// pairing link, the privacy notice, the admin panel printed at startup.
func (s Settings) BaseURL() string { return s.Scheme() + "://" + s.PublicHost }

// Discovery is the document a client fetches before anything else.
//
// The features are passed in rather than read from here because what an
// installation has depends on what is running rather than on what is stored —
// a plugin answering a request kind, not a key somebody pasted — and this
// package knows nothing about plugins.
func (s Settings) Discovery(features []wire.Feature, minClient string) wire.Discovery {
	name := s.ShortName
	if name == "" {
		name = shortName(s.Organisation)
	}
	return wire.Discovery{
		API:       APIBase,
		Name:      s.Organisation,
		ShortName: name,
		Logo:      s.Logo,
		Accent:    s.Accent,
		Features:  features,
		Limits:    s.Limits,
		MinClient: minClient,
		PairURI:   s.BaseURL() + "/pair",
	}
}

// Validate reports the first thing wrong with these settings, phrased for the
// operator who has to fix it.
func (s Settings) Validate() error {
	switch {
	case strings.TrimSpace(s.Organisation) == "":
		return errors.New("config: the organisation name is empty")
	case strings.TrimSpace(s.PublicHost) == "":
		return errors.New("config: the public host name is empty")
	case !s.TLSMode.Valid():
		return fmt.Errorf("config: %q is not a way of serving this host — it is either behind a proxy or getting its own certificate", s.TLSMode)
	}
	if err := ValidateHost(s.PublicHost); err != nil {
		return err
	}
	if err := ValidateLimits(s.Limits); err != nil {
		return err
	}
	if err := ValidateAccent(s.Accent); err != nil {
		return err
	}
	if s.Logo != "" {
		if err := ValidateURL("logo", s.Logo); err != nil {
			return err
		}
	}
	return ValidateRetention(s.Retention)
}

// ValidateHost refuses anything that is not a bare host name.
//
// It is strict on purpose. This name goes into the pairing link a driver opens,
// into the certificate when the server gets its own, and into every URL the
// client is told to use — so a scheme, a port or a path pasted in here is a
// mistake that surfaces later, somewhere else, as something that looks unrelated.
func ValidateHost(host string) error {
	switch {
	case strings.TrimSpace(host) == "":
		return errors.New("config: the public host name is empty")
	case strings.Contains(host, "://"):
		return errors.New("config: that carries a scheme. Leave the scheme off — the host name on its own, with no https:// in front of it")
	case strings.Contains(host, "/"):
		return errors.New("config: that carries a path. Give the host name on its own")
	case strings.Contains(host, ":"):
		return errors.New("config: that carries a port. Leave the port off — it goes in the listen address, not in the name drivers reach")
	}
	if net.ParseIP(host) != nil {
		return errors.New("config: that is an IP address — give a host name rather than an IP, because a pairing link and a certificate both need a name")
	}
	labels := strings.Split(host, ".")
	if len(labels) == 1 && host != "localhost" {
		return errors.New("config: that host name needs a domain — pacenote.example.com rather than pacenote")
	}
	for _, l := range labels {
		if err := validLabel(l); err != nil {
			return err
		}
	}
	return nil
}

// The bounds on the limits an operator may publish. They are wide — this is a
// self-hosted server and the operator is the one paying for the traffic — but
// they are not absent: a zero interval is a client told to send continuously,
// and a body cap of a gigabyte is a memory limit written in a text field.
const (
	// MinTracePoints is the fewest samples a lap trace may be held to.
	MinTracePoints = 10
	// MaxTracePoints is the most, which is storage on every lap ever driven.
	MaxTracePoints = 10_000
	// MinLapsPerRequest is the fewest laps one upload may carry.
	MinLapsPerRequest = 1
	// MaxLapsPerRequest is the most, which bounds one offline queue drain.
	MaxLapsPerRequest = 1_000
	// MinIntervalMs is the shortest published interval, ten a second.
	MinIntervalMs = 100
	// MaxIntervalMs is the longest, ten minutes.
	MaxIntervalMs = 600_000
	// MinBodyBytes is the smallest body cap that still fits a trace.
	MinBodyBytes = 64 << 10
	// MaxBodyBytes is the largest, which is a memory limit as much as a size.
	MaxBodyBytes = 64 << 20
)

// ValidateLimits reports the first limit that is out of range, phrased for the
// operator who typed it.
//
// These are the numbers discovery publishes and the rate limiter enforces, so
// they are checked once, here, and both sides read the result. A client that
// obeys the document must never be refused by the limiter, which is only true
// while there is one set of numbers rather than two.
func ValidateLimits(l wire.Limits) error {
	checks := []struct {
		name     string
		value    int
		min, max int
	}{
		{"trace points", l.TracePoints, MinTracePoints, MaxTracePoints},
		{"laps per request", l.LapsPerRequest, MinLapsPerRequest, MaxLapsPerRequest},
		{"live interval", l.LiveIntervalMs, MinIntervalMs, MaxIntervalMs},
		{"field interval", l.FieldIntervalMs, MinIntervalMs, MaxIntervalMs},
		{"summary interval", l.SummaryIntervalMs, MinIntervalMs, MaxIntervalMs},
		{"largest body", l.MaxBodyBytes, MinBodyBytes, MaxBodyBytes},
	}
	for _, c := range checks {
		if c.value < c.min || c.value > c.max {
			return fmt.Errorf("config: the %s is %d, and it has to be between %d and %d",
				c.name, c.value, c.min, c.max)
		}
	}
	return nil
}

// ValidateAccent checks the accent colour. Empty is allowed and means the
// client keeps its own; anything else is a CSS hex colour, because that is
// what the client puts straight into a stylesheet.
func ValidateAccent(accent string) error {
	a := strings.TrimSpace(accent)
	if a == "" {
		return nil
	}
	if a[0] != '#' || (len(a) != 4 && len(a) != 7) {
		return errors.New("config: the accent colour is a hex colour like \"#C6F24B\", or empty to leave the client's own")
	}
	for i := 1; i < len(a); i++ {
		c := a[i]
		hex := (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
		if !hex {
			return errors.New("config: the accent colour is a hex colour like \"#C6F24B\", or empty to leave the client's own")
		}
	}
	return nil
}

// ValidateURL checks an absolute URL an operator types — the logo. Empty is
// allowed and means none; anything else has to be http or https, because it is
// fetched.
func ValidateURL(kind, raw string) error {
	v := strings.TrimSpace(raw)
	if v == "" {
		return nil
	}
	u, err := url.Parse(v)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return fmt.Errorf("config: the %s has to be a full address starting with https://", kind)
	}
	return nil
}

// shortName is the abbreviation a client shows where the full organisation name
// does not fit. It is the initial of each word, except that a word already
// written in capitals — "GT", "SRO" — is an abbreviation already and is kept
// whole: "Iberian GT Championship" is IGTC, not IGC.
//
// It is a starting value the operator can change in settings, not a rule.
func shortName(organisation string) string {
	var b strings.Builder
	for _, f := range strings.Fields(organisation) {
		r := []rune(f)
		if len(r) == 0 {
			continue
		}
		if len(r) <= 3 && isAllCaps(r) {
			b.WriteString(string(r))
		} else {
			b.WriteRune(upper(r[0]))
		}
		if b.Len() >= 5 {
			break
		}
	}
	if b.Len() == 0 {
		return organisation
	}
	return b.String()
}

func isAllCaps(r []rune) bool {
	letters := 0
	for _, c := range r {
		switch {
		case c >= 'A' && c <= 'Z':
			letters++
		case c >= 'a' && c <= 'z':
			return false
		}
	}
	return letters > 0
}

func upper(c rune) rune {
	if c >= 'a' && c <= 'z' {
		return c - ('a' - 'A')
	}
	return c
}

// validLabel checks one dot-separated part of a host name. It is the check that
// stops a settings field from becoming a name nothing will resolve.
func validLabel(label string) error {
	if label == "" || len(label) > 63 {
		return errors.New("config: that public address has an empty or over-long part between its dots")
	}
	for i := range len(label) {
		c := label[i]
		ok := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-'
		if !ok {
			return fmt.Errorf("config: %q is not a host name — it may hold letters, digits, hyphens and dots", label)
		}
	}
	if label[0] == '-' || label[len(label)-1] == '-' {
		return errors.New("config: a part of a host name cannot start or end with a hyphen")
	}
	return nil
}
