package httpx_test

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pacenote-sim/protocol/wire"
	"github.com/pacenote-sim/server/internal/httpx"
	"github.com/pacenote-sim/server/internal/logging"
)

func TestHardenAppliesTheTable(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	// The defaults every route is built on. They are asserted rather than
	// merely set, because the whole point of the table is that they are the
	// settings that get forgotten.
	srv := httpx.NewServer(":0", http.NotFoundHandler())
	r.Equal(5*time.Second, srv.ReadHeaderTimeout)
	r.Equal(30*time.Second, srv.ReadTimeout)
	r.Equal(30*time.Second, srv.WriteTimeout)
	r.Equal(120*time.Second, srv.IdleTimeout)
	r.Equal(16*1024, srv.MaxHeaderBytes)
}

type payload struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

func TestDecodeJSON(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		body        string
		contentType string
		limit       int64
		wantErr     error
		want        payload
	}{
		{
			name: "an ordinary document", body: `{"name":"ana","count":3}`,
			contentType: "application/json", limit: 1024,
			want: payload{Name: "ana", Count: 3},
		},
		{
			name: "no content type is accepted", body: `{"name":"ana"}`,
			limit: 1024, want: payload{Name: "ana"},
		},
		{
			name: "an unknown field is a mistake, not a default",
			body: `{"name":"ana","speed":3}`, contentType: "application/json",
			limit: 1024, wantErr: httpx.ErrMalformed,
		},
		{
			name: "a second document is refused",
			body: `{"name":"ana"}{"name":"bea"}`, contentType: "application/json",
			limit: 1024, wantErr: httpx.ErrMalformed,
		},
		{
			name: "a body over the cap is refused",
			body: `{"name":"` + strings.Repeat("a", 200) + `"}`, contentType: "application/json",
			limit: 64, wantErr: httpx.ErrTooLarge,
		},
		{
			name: "an empty body is refused", body: ``,
			contentType: "application/json", limit: 1024, wantErr: httpx.ErrMalformed,
		},
		{
			name: "broken JSON is refused", body: `{"name":`,
			contentType: "application/json", limit: 1024, wantErr: httpx.ErrMalformed,
		},
		{
			name: "the wrong type in a field is refused", body: `{"count":"three"}`,
			contentType: "application/json", limit: 1024, wantErr: httpx.ErrMalformed,
		},
		{
			name: "a body that is not JSON at all is refused", body: `name=ana`,
			contentType: "application/x-www-form-urlencoded", limit: 1024, wantErr: httpx.ErrMalformed,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := require.New(t)
			req := httptest.NewRequest(http.MethodPost, "/api/v1/thing", strings.NewReader(tc.body))
			if tc.contentType != "" {
				req.Header.Set("Content-Type", tc.contentType)
			}
			rec := httptest.NewRecorder()

			var got payload
			err := httpx.DecodeJSON(rec, req, &got, tc.limit)
			if tc.wantErr != nil {
				r.ErrorIs(err, tc.wantErr)
				return
			}
			r.NoError(err)
			r.Equal(tc.want, got)
		})
	}
}

func TestParseFormCaps(t *testing.T) {
	t.Parallel()

	t.Run("an ordinary form is read", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		body := url.Values{"token": {"7QK4"}}.Encode()
		req := httptest.NewRequest(http.MethodPost, "/setup", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.NoError(httpx.ParseForm(httptest.NewRecorder(), req, httpx.FormLimit))
		r.Equal("7QK4", req.PostFormValue("token"))
	})

	t.Run("a form over the cap is refused", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		body := url.Values{"token": {strings.Repeat("a", 4096)}}.Encode()
		req := httptest.NewRequest(http.MethodPost, "/setup", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		err := httpx.ParseForm(httptest.NewRecorder(), req, 128)
		r.ErrorIs(err, httpx.ErrTooLarge)
	})
}

func TestProblemChoosesItsShape(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		path     string
		accept   string
		wantJSON bool
		wantCode wire.Code
	}{
		{"a browser asking for a page", "/admin/login", "text/html", false, ""},
		{"an api path", "/api/v1/me", "", true, wire.CodeNotFound},
		{"discovery", "/.well-known/sim-telemetry.json", "", true, wire.CodeNotFound},
		{"a client asking for json", "/admin/login", "application/json", true, wire.CodeNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := require.New(t)
			req := httptest.NewRequest(http.MethodGet, tc.path, http.NoBody)
			if tc.accept != "" {
				req.Header.Set("Accept", tc.accept)
			}
			rec := httptest.NewRecorder()
			httpx.Problem(rec, req, http.StatusNotFound, "There is nothing at that address on this server.")

			r.Equal(http.StatusNotFound, rec.Code)
			if !tc.wantJSON {
				r.Contains(rec.Header().Get("Content-Type"), "text/plain")
				return
			}
			r.Contains(rec.Header().Get("Content-Type"), "application/json")
			var env wire.ErrorEnvelope
			r.NoError(json.Unmarshal(rec.Body.Bytes(), &env))
			r.NotNil(env.Error)
			r.Equal(tc.wantCode, env.Error.Code)
			r.Equal("There is nothing at that address on this server.", env.Error.Message)
		})
	}
}

func TestAPIErrorUsesTheContractStatus(t *testing.T) {
	t.Parallel()
	for _, code := range wire.Codes() {
		t.Run(string(code), func(t *testing.T) {
			t.Parallel()
			r := require.New(t)
			rec := httptest.NewRecorder()
			httpx.APIError(rec, code, "A complete sentence for the driver.")
			r.Equal(code.HTTPStatus(), rec.Code)
			var env wire.ErrorEnvelope
			r.NoError(json.Unmarshal(rec.Body.Bytes(), &env))
			r.Equal(code, env.Error.Code)
		})
	}
}

func TestSecureHeaders(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	h := httpx.SecureHeaders(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin", http.NoBody))

	r.Equal("nosniff", rec.Header().Get("X-Content-Type-Options"))
	r.Equal("DENY", rec.Header().Get("X-Frame-Options"))
	r.Contains(rec.Header().Get("Content-Security-Policy"), "default-src 'none'")
	r.Contains(rec.Header().Get("Content-Security-Policy"), "frame-ancestors 'none'")
	r.Empty(rec.Header().Get("Strict-Transport-Security"), "no HSTS on a plain connection")
}

func TestRecoverTurnsAPanicIntoAnAnswer(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	h := httpx.Recover(logging.Discard())(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("a handler bug")
	}))
	rec := httptest.NewRecorder()
	r.NotPanics(func() {
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin", http.NoBody))
	})
	r.Equal(http.StatusInternalServerError, rec.Code)
	r.NotContains(rec.Body.String(), "a handler bug", "a panic message is for the log, not the caller")
}

func TestChainRunsOutermostFirst(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	var order []string
	mw := func(name string) httpx.Middleware {
		return func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				order = append(order, name)
				next.ServeHTTP(w, req)
			})
		}
	}
	h := httpx.Chain(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		order = append(order, "handler")
	}), mw("a"), mw("b"))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", http.NoBody))
	r.Equal([]string{"a", "b", "handler"}, order)
}

func TestClientHost(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		remote string
		want   string
	}{
		{"an address with a port", "203.0.113.4:51234", "203.0.113.4"},
		{"an IPv6 address", "[2001:db8::1]:51234", "2001:db8::1"},
		{"no port at all", "203.0.113.4", "203.0.113.4"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := require.New(t)
			req := httptest.NewRequest(http.MethodGet, "/", http.NoBody)
			req.RemoteAddr = tc.remote
			req.Header.Set("X-Forwarded-For", "198.51.100.9")
			r.Equal(tc.want, httpx.ClientHost(req),
				"a forwarded header is whatever the caller typed unless a proxy is known to be in front")
		})
	}
}

func TestServeShutsDownGracefully(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	r.NoError(err)

	started := make(chan struct{})
	release := make(chan struct{})
	srv := httpx.NewServer(ln.Addr().String(), http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(started)
		<-release
		w.WriteHeader(http.StatusNoContent)
	}))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- httpx.Serve(ctx, srv, ln) }()

	resp := make(chan int, 1)
	go func() {
		req, err := http.NewRequestWithContext(context.Background(),
			http.MethodGet, "http://"+ln.Addr().String()+"/", http.NoBody)
		if err != nil {
			return
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			return
		}
		defer func() { _ = res.Body.Close() }()
		resp <- res.StatusCode
	}()

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		cancel()
		r.Fail("the handler never ran")
	}

	cancel() // shutdown begins while a request is in flight
	time.Sleep(20 * time.Millisecond)
	close(release) // the handler finishes

	select {
	case status := <-resp:
		r.Equal(http.StatusNoContent, status, "a request in flight is finished, not cut off")
	case <-time.After(5 * time.Second):
		r.Fail("the in-flight request never completed")
	}

	select {
	case err := <-done:
		r.NoError(err, "an ordinary shutdown is not a failure")
	case <-time.After(5 * time.Second):
		r.Fail("Serve did not return")
	}
}
