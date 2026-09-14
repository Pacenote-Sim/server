package clientbuild

// Signing is how a built client is signed, which is the one question on the
// build page an operator has to think about.
//
// There are two, and an operator has to be shown both: the difference between
// them is the difference between a driver seeing a Windows warning and not.
//
// A third once lived here — a paid service that built and signed on our own
// infrastructure. It has been taken out rather than left disabled. A choice an
// operator can read, understand and never use is a choice that wastes their
// attention every time they visit the page, and offering to sell something that
// does not exist is worse than not mentioning it.
type Signing string

// The two routes.
const (
	// SigningNone is a stamped copy of the prebuilt client, handed over as it
	// is. Free, immediate, and every driver sees Windows warn them.
	SigningNone Signing = "unsigned"
	// SigningOwnCertificate is the operator signing with a certificate of
	// their own, which this server holds.
	SigningOwnCertificate Signing = "own-certificate"
)

// SigningRoutes is the two in the order the page offers them.
func SigningRoutes() []Signing {
	return []Signing{SigningNone, SigningOwnCertificate}
}

// Valid reports whether s is one of the two.
func (s Signing) Valid() bool {
	switch s {
	case SigningNone, SigningOwnCertificate:
		return true
	default:
		return false
	}
}

// Available reports whether this server can take this route right now.
//
// It takes an argument rather than reading anything because the answer changes
// while the server runs: an operator uploads a certificate on the same page
// they build from, and the route goes from unavailable to available without a
// restart.
func (s Signing) Available(certificate bool) bool {
	switch s {
	case SigningNone:
		return true
	case SigningOwnCertificate:
		return certificate
	default:
		return false
	}
}

// Label is the route's name on the page and in the build history.
func (s Signing) Label() string {
	switch s {
	case SigningNone:
		return "Unsigned"
	case SigningOwnCertificate:
		return "Your own certificate"
	default:
		return string(s)
	}
}

// Unavailable is why a route cannot be taken, phrased for the operator who
// just chose it. It is empty for a route that can be.
func (s Signing) Unavailable(certificate bool) string {
	switch {
	case s.Available(certificate):
		return ""
	case s == SigningOwnCertificate:
		return "This server has no certificate to sign with — add one on this page first."
	default:
		return "That is not one of the ways a client can be signed."
	}
}
