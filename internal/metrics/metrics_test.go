package metrics_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"

	"github.com/pacenote-sim/server/internal/metrics"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

func TestHandlerServesMetricsAndPprof(t *testing.T) {
	t.Parallel()
	h := metrics.New().Handler()

	cases := []struct {
		name string
		path string
		want string
	}{
		{"the metrics themselves", "/metrics", "go_goroutines"},
		{"the pprof index", "/debug/pprof/", "goroutine"},
		{"a note at the root", "/", "private port"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := require.New(t)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tc.path, http.NoBody))
			r.Equal(http.StatusOK, rec.Code)
			r.Contains(rec.Body.String(), tc.want)
		})
	}
}

func TestObserveIsLabelledByRouteNotPath(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	m := metrics.New()
	m.Observe("GET /admin/drivers/{id}", http.StatusOK, 3*time.Millisecond)
	m.Observe("GET /admin/drivers/{id}", http.StatusNotFound, time.Millisecond)

	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", http.NoBody))
	body := rec.Body.String()
	r.Contains(body, `route="GET /admin/drivers/{id}"`)
	r.Contains(body, `status="2xx"`)
	r.Contains(body, `status="4xx"`)
}

func TestSetupAttemptsAreCounted(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	m := metrics.New()
	m.SetupAttempts.WithLabelValues("rejected").Inc()
	m.AdminLogins.WithLabelValues("accepted").Inc()
	m.SamplePool(1, 2, 3)

	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", http.NoBody))
	body := rec.Body.String()
	r.Contains(body, `pacenote_setup_attempts_total{outcome="rejected"} 1`)
	r.Contains(body, `pacenote_admin_logins_total{outcome="accepted"} 1`)
	r.Contains(body, `pacenote_database_pool_connections{state="total"} 3`)
}

func TestSamplePoolEveryStopsWithItsContext(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	m := metrics.New()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		m.SamplePoolEvery(ctx, time.Millisecond, func() (int32, int32, int32) { return 1, 2, 3 })
	}()
	time.Sleep(10 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		r.Fail("the sampler outlived its context")
	}
}
