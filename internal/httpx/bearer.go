package httpx

import (
	"net/http"
	"strings"
)

// BearerToken pulls the token out of a request's Authorization header, or
// reports that there is no Bearer credential — a Basic one, or none at all.
func BearerToken(r *http.Request) (string, bool) {
	return ParseBearer(r.Header.Get("Authorization"))
}

// ParseBearer is [BearerToken] on the header's value. The scheme is matched
// case-insensitively because RFC 7235 says it is case-insensitive, and a client
// that sends "bearer" is not wrong.
func ParseBearer(header string) (string, bool) {
	const scheme = "bearer "
	if len(header) <= len(scheme) || !strings.EqualFold(header[:len(scheme)], scheme) {
		return "", false
	}
	token := strings.TrimSpace(header[len(scheme):])
	return token, token != ""
}
