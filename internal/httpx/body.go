package httpx

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/pacenote-sim/protocol/wire"
)

// FormLimit is the body cap on an HTML form post. The wizard and the admin
// panel send a handful of short fields; sixty-four kilobytes is generous for
// that and nowhere near enough to be a memory problem.
const FormLimit int64 = 64 << 10

// ErrTooLarge is returned when a body is longer than the route's cap.
var ErrTooLarge = errors.New("httpx: the request body is too large")

// ErrMalformed is returned when a body is not what it claimed to be.
var ErrMalformed = errors.New("httpx: the request body is not readable")

// Limit caps the body of one route, per D-3: a cap belongs to an endpoint, not
// to the server, because the endpoint is the only place that knows what a
// reasonable body looks like.
func Limit(w http.ResponseWriter, r *http.Request, limit int64) {
	r.Body = http.MaxBytesReader(w, r.Body, limit)
}

// DecodeJSON reads one JSON document from the body into dst.
//
// Three things are true of it and none of them are the standard library's
// defaults: the body is capped before a byte is allocated, an unknown field is
// an error rather than a silent default, and trailing content after the
// document is an error rather than being ignored. The first is a memory limit,
// and the other two turn a client's typo into a 422 it can see in a log rather
// than into a value that quietly went missing.
func DecodeJSON(w http.ResponseWriter, r *http.Request, dst any, limit int64) error {
	if ct := r.Header.Get("Content-Type"); ct != "" && !isJSON(ct) {
		return fmt.Errorf("%w: it is not JSON", ErrMalformed)
	}
	Limit(w, r, limit)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return decodeError(err)
	}
	if err := dec.Decode(new(json.RawMessage)); !errors.Is(err, io.EOF) {
		return fmt.Errorf("%w: it carries more than one document", ErrMalformed)
	}
	return nil
}

// DecodeJSONFingerprint reads one JSON document into dst and returns a
// fingerprint of the request that carried it, for the idempotency table.
//
// The fingerprint covers the method, the path and the body bytes, so the same
// key presented on two different endpoints is a conflict rather than a replay
// of the wrong answer. It is computed as the body streams past the decoder, so
// no second copy of the payload is made and the cap is still enforced before
// anything is allocated.
func DecodeJSONFingerprint(w http.ResponseWriter, r *http.Request, dst any, limit int64) ([]byte, error) {
	if ct := r.Header.Get("Content-Type"); ct != "" && !isJSON(ct) {
		return nil, fmt.Errorf("%w: it is not JSON", ErrMalformed)
	}
	Limit(w, r, limit)
	sum := sha256.New()
	sum.Write([]byte(r.Method))
	sum.Write([]byte{0})
	sum.Write([]byte(r.URL.Path))
	sum.Write([]byte{0})

	dec := json.NewDecoder(io.TeeReader(r.Body, sum))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return nil, decodeError(err)
	}
	if err := dec.Decode(new(json.RawMessage)); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%w: it carries more than one document", ErrMalformed)
	}
	// The second Decode above answered io.EOF, which it can only do having
	// read the body to its end, so every byte the client sent has passed
	// through the tee and the fingerprint is complete.
	return sum.Sum(nil), nil
}

// ParseForm reads an HTML form body, capped at max. It is the form twin of
// [DecodeJSON] and exists so no handler calls r.ParseForm without a cap.
func ParseForm(w http.ResponseWriter, r *http.Request, limit int64) error {
	Limit(w, r, limit)
	if err := r.ParseForm(); err != nil {
		return decodeError(err)
	}
	return nil
}

// ParseUpload reads a form that carries a file.
//
// It is separate from [ParseForm] because the two need different caps. An
// ordinary form is a few text fields and is held to [FormLimit]; a form with a
// file on it is as large as the file, and the caller says how large that may
// be. The whole body is still capped, so an upload that claims to be small and
// keeps arriving is cut off rather than kept.
//
// Nothing is spooled to disk: the in-memory allowance is the same limit, so a
// file inside it never reaches the file system and a file outside it is refused
// before any of it does.
//
// which is the bound the rule is asking for; the analyser cannot see through
// the helper.
//
//nolint:gosec // G120: Limit caps the body before ParseMultipartForm reads it,
func ParseUpload(w http.ResponseWriter, r *http.Request, limit int64) error {
	Limit(w, r, limit)
	if err := r.ParseMultipartForm(limit); err != nil {
		return decodeError(err)
	}
	return nil
}

func decodeError(err error) error {
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		return fmt.Errorf("%w: at most %d bytes", ErrTooLarge, tooLarge.Limit)
	}
	var syntax *json.SyntaxError
	if errors.As(err, &syntax) {
		return fmt.Errorf("%w: it is not valid JSON", ErrMalformed)
	}
	var typeErr *json.UnmarshalTypeError
	if errors.As(err, &typeErr) {
		return fmt.Errorf("%w: %q is not a %s", ErrMalformed, typeErr.Field, typeErr.Type)
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return fmt.Errorf("%w: it is empty or cut short", ErrMalformed)
	}
	if strings.Contains(err.Error(), "unknown field") {
		return fmt.Errorf("%w: %s", ErrMalformed, strings.TrimPrefix(err.Error(), "json: "))
	}
	return fmt.Errorf("%w: %s", ErrMalformed, err.Error())
}

func isJSON(contentType string) bool {
	mt, _, _ := strings.Cut(contentType, ";")
	mt = strings.TrimSpace(strings.ToLower(mt))
	return mt == "application/json" || strings.HasSuffix(mt, "+json")
}

// JSON writes one JSON document with a status. It is the only place a success
// body is encoded, so the header and the body cannot disagree about what is
// being sent.
func JSON(w http.ResponseWriter, status int, body any) {
	b, err := json.Marshal(body)
	if err != nil {
		// Encoding our own response failed, which is a bug rather than a
		// request problem. There is nothing useful to say to the client.
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"code":"server_error","message":"Something went wrong on the server."}}`))
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

// APIError writes the v1 error envelope for one code and message, with the
// status the contract pairs with that code. message is shown to the driver as
// written, so it must be a complete sentence with no jargon.
func APIError(w http.ResponseWriter, code wire.Code, message string) {
	status := code.HTTPStatus()
	if status == 0 {
		status = http.StatusInternalServerError
	}
	JSON(w, status, wire.NewError(code, message))
}

// Problem answers a request that cannot be served. A caller that wants JSON
// gets the v1 error envelope; a browser gets one line of plain text, because
// the alternative is an error page that says less.
func Problem(w http.ResponseWriter, r *http.Request, status int, message string) {
	if wantsJSON(r) {
		JSON(w, status, wire.NewError(codeFor(status), message))
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, message+"\n")
}

func wantsJSON(r *http.Request) bool {
	if strings.HasPrefix(r.URL.Path, "/api/") || r.URL.Path == "/.well-known/sim-telemetry.json" {
		return true
	}
	accept := strings.ToLower(r.Header.Get("Accept"))
	return strings.Contains(accept, "application/json")
}

func codeFor(status int) wire.Code {
	for _, c := range wire.Codes() {
		if c.HTTPStatus() == status {
			return c
		}
	}
	return wire.CodeServerError
}
