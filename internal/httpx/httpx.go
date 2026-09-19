// Package httpx is the server's HTTP plumbing: the hardened [http.Server], the
// middleware every route wears, and the decoders that read a request body.
//
// It exists because the settings in [Harden] are the ones that get forgotten.
// A timeout that is not set is a slowloris; a body that is not capped is a
// memory limit; a JSON decoder that ignores unknown fields turns a client's
// typo into a silent default. This package is every one of those decided once
// and applied centrally, so that no route can be created without them.
//
// Routing is net/http.ServeMux and nothing else. Go 1.22's method and wildcard
// patterns cover the whole API, and a framework on the request path would be
// one more dependency between a driver's lap and the database.
package httpx

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"runtime/debug"
	"strings"
	"time"
)

// The D-3 hardening table, as constants. Changing one of these is a decision,
// so it is made here rather than at a call site.
const (
	// ReadHeaderTimeout is the slowloris guard: a client that opens a
	// connection and dribbles a header has five seconds to finish.
	ReadHeaderTimeout = 5 * time.Second
	// ReadTimeout and WriteTimeout bound a whole request and a whole response.
	ReadTimeout  = 30 * time.Second
	WriteTimeout = 30 * time.Second
	// IdleTimeout is how long a kept-alive connection may sit unused.
	IdleTimeout = 120 * time.Second
	// MaxHeaderBytes caps the header block.
	MaxHeaderBytes = 16 << 10
	// ShutdownTimeout is how long a graceful shutdown waits for requests in
	// flight before the process stops anyway.
	ShutdownTimeout = 20 * time.Second
)

// Harden applies the D-3 table to a server. Every [http.Server] this binary
// creates goes through it.
func Harden(s *http.Server) *http.Server {
	s.ReadHeaderTimeout = ReadHeaderTimeout
	s.ReadTimeout = ReadTimeout
	s.WriteTimeout = WriteTimeout
	s.IdleTimeout = IdleTimeout
	s.MaxHeaderBytes = MaxHeaderBytes
	return s
}

// NewServer builds a hardened server for one address and handler.
//
//nolint:gosec // G112: ReadHeaderTimeout is set by Harden, which the analyser cannot see through.
func NewServer(addr string, h http.Handler) *http.Server {
	return Harden(&http.Server{Addr: addr, Handler: h})
}

// Serve runs srv on ln until ctx is cancelled, then shuts it down gracefully.
// It returns nil for an ordinary shutdown, so a caller can treat any non-nil
// result as a fault.
func Serve(ctx context.Context, srv *http.Server, ln net.Listener) error {
	errc := make(chan error, 1)
	go func() {
		err := srv.Serve(ln)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		errc <- err
	}()

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		// A fresh context: the one that was cancelled cannot be the one that
		// bounds the wait for requests still in flight.
		sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), ShutdownTimeout)
		defer cancel()
		if err := srv.Shutdown(sctx); err != nil {
			return fmt.Errorf("httpx: shutdown did not finish: %w", err)
		}
		<-errc
		return nil
	}
}

// Middleware is the ordinary shape. Nothing in this server needs another.
type Middleware func(http.Handler) http.Handler

// Chain wraps h in every middleware, outermost first, so Chain(h, a, b) reads
// in the order the request travels: a, then b, then h.
func Chain(h http.Handler, ms ...Middleware) http.Handler {
	for i := len(ms) - 1; i >= 0; i-- {
		h = ms[i](h)
	}
	return h
}

// Recover turns a panic in a handler into a 500 and a log line, rather than
// into a dead process. A panic mid-session would otherwise lose every driver's
// laps that were in flight, which makes this correctness rather than polish.
func Recover(log *slog.Logger) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				rec := recover()
				if rec == nil {
					return
				}
				// ErrAbortHandler is net/http's own signal that a handler
				// gave up on purpose. It must travel: the server logs it and
				// closes the connection, and swallowing it here would turn a
				// deliberate abort into a 500.
				if err, ok := rec.(error); ok && errors.Is(err, http.ErrAbortHandler) {
					panic(rec)
				}
				log.LogAttrs(r.Context(), slog.LevelError, "handler panicked",
					slog.String("method", r.Method),
					slog.String("path", r.URL.Path),
					slog.Any("panic", rec),
					slog.String("stack", string(debug.Stack())),
				)
				Problem(w, r, http.StatusInternalServerError, "Something went wrong on the server. Try again.")
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// LogRequests writes one line per request. The path is logged and the query
// string is not, because a query string is where a token ends up when someone
// takes a shortcut.
//
// One route is quieter than the rest. A running client posts where it is on
// the circuit every second, and at one line a second per driver that route
// alone fills the log, so a successful one is written at debug. A failed one
// is written like everything else: a heartbeat is worth reading when it stops
// working, not while it does.
func LogRequests(log *slog.Logger) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(rec, r)
			level := slog.LevelInfo
			if heartbeat(r) && rec.status < http.StatusBadRequest {
				level = slog.LevelDebug
			}
			log.LogAttrs(r.Context(), level, "request",
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.Int("status", rec.status),
				slog.Int64("bytes", rec.written),
				slog.String("took", time.Since(start).Round(time.Millisecond).String()),
				slog.String("remote", clientHost(r)),
			)
		})
	}
}

// heartbeat reports the live telemetry ping: the one route a client calls on
// a timer rather than when something happened.
func heartbeat(r *http.Request) bool {
	return r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/live")
}

// Measure reports how one route finished, for the metrics port.
//
// The route is given rather than taken from the request, because it is the
// ServeMux pattern and not the path: a label per driver would be a metric per
// driver, and a time series that grows with the roster is one nobody can query.
// It follows that Measure is applied where a route is registered, which is the
// only place the pattern is known.
//
// A nil report is no middleware at all, so a server with no metrics port pays
// nothing for the ones it is not collecting.
func Measure(route string, report func(route string, status int, took time.Duration)) Middleware {
	if report == nil {
		return func(next http.Handler) http.Handler { return next }
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(rec, r)
			report(route, rec.status, time.Since(start))
		})
	}
}

// SecureHeaders sets the response headers that cost nothing and close off a
// whole class of problem. The content security policy is strict because the
// admin panel is server-rendered with no framework and no content delivery
// network: there is no inline script to allow and nothing to fetch from
// elsewhere.
func SecureHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		h.Set("Content-Security-Policy",
			"default-src 'none'; img-src 'self' data:; style-src 'self'; "+
				"form-action 'self'; frame-ancestors 'none'; base-uri 'none'")
		if r.TLS != nil {
			h.Set("Strict-Transport-Security", "max-age=31536000")
		}
		next.ServeHTTP(w, r)
	})
}

// NoStore stops a browser and any proxy from keeping a page. Every page the
// admin panel serves is either a form with a token in it or a view of private
// data, so none of them may be cached.
func NoStore(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

// clientHost is the remote address without its port. The port is noise in a log
// line and the address alone is what a rate limiter and an operator both want.
func clientHost(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// ClientHost is [clientHost] for the packages that rate-limit by address.
//
// It deliberately does not read X-Forwarded-For. Behind the operator's own
// proxy that header is trustworthy and in front of one it is whatever the
// caller typed, and this package cannot tell which it is in. Rate limiting the
// proxy's own address is wrong but safe; trusting a forged header is neither.
func ClientHost(r *http.Request) string { return clientHost(r) }

type statusRecorder struct {
	http.ResponseWriter
	status  int
	written int64
	wrote   bool
}

func (s *statusRecorder) WriteHeader(code int) {
	if s.wrote {
		return
	}
	s.wrote = true
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if !s.wrote {
		s.wrote = true
	}
	n, err := s.ResponseWriter.Write(b)
	s.written += int64(n)
	if err != nil {
		return n, fmt.Errorf("httpx: cannot write the response: %w", err)
	}
	return n, nil
}

// Unwrap lets http.ResponseController reach the real writer, so a handler that
// needs to flush still can.
func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }
