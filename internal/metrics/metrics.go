// Package metrics is the private port: Prometheus counters and pprof, bound to
// loopback and nothing else.
//
// It is a separate listener rather than a route on the public server because
// the two have different audiences. The public port is the internet; this one
// is the operator's own machine, and putting a profiler behind the same
// timeouts and the same address as a driver's upload would be a mistake in both
// directions.
//
// There is no vendor here and no agent to install. An operator who wants
// dashboards points their own Prometheus at this port; one who does not, does
// not notice it.
package metrics

import (
	"context"
	"net/http"
	"net/http/pprof"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Registry is this server's metrics. It is its own registry rather than the
// default one, so that a dependency registering a collector cannot silently
// change what this server publishes.
type Registry struct {
	reg *prometheus.Registry

	// Requests counts finished HTTP requests by route and status.
	Requests *prometheus.CounterVec
	// RequestDuration is how long they took.
	RequestDuration *prometheus.HistogramVec
	// SetupAttempts counts setup token attempts, by outcome. It is the signal
	// that someone is guessing at a server that has not been set up yet.
	SetupAttempts *prometheus.CounterVec
	// AdminLogins counts admin sign-in attempts, by outcome.
	AdminLogins *prometheus.CounterVec
	// DatabasePoolConns reports the pool's state, sampled when scraped.
	DatabasePoolConns *prometheus.GaugeVec
}

// New builds the registry with the process and Go collectors already in it.
func New() *Registry {
	reg := prometheus.NewRegistry()
	reg.MustRegister(
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		collectors.NewGoCollector(),
	)
	m := &Registry{
		reg: reg,
		Requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "pacenote_http_requests_total",
			Help: "Finished HTTP requests, by route pattern and status.",
		}, []string{"route", "status"}),
		RequestDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "pacenote_http_request_seconds",
			Help:    "How long a request took, by route pattern.",
			Buckets: prometheus.DefBuckets,
		}, []string{"route"}),
		SetupAttempts: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "pacenote_setup_attempts_total",
			Help: "Setup token attempts, by outcome.",
		}, []string{"outcome"}),
		AdminLogins: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "pacenote_admin_logins_total",
			Help: "Admin sign-in attempts, by outcome.",
		}, []string{"outcome"}),
		DatabasePoolConns: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "pacenote_database_pool_connections",
			Help: "Connections in the database pool, by state.",
		}, []string{"state"}),
	}
	reg.MustRegister(m.Requests, m.RequestDuration, m.SetupAttempts, m.AdminLogins, m.DatabasePoolConns)
	return m
}

// Handler serves /metrics and the pprof endpoints. It is only ever mounted on
// the loopback listener; internal/config refuses a metrics address that is not
// a loopback one, so this cannot be exposed by a configuration mistake.
func (m *Registry) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", promhttp.HandlerFor(m.reg, promhttp.HandlerOpts{
		Registry:          m.reg,
		EnableOpenMetrics: true,
	}))
	mux.HandleFunc("GET /debug/pprof/", pprof.Index)
	mux.HandleFunc("GET /debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("GET /debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("GET /debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("GET /debug/pprof/trace", pprof.Trace)
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("pacenote private port — /metrics and /debug/pprof/ are here\n"))
	})
	return mux
}

// Observe records one finished request. route is the ServeMux pattern rather
// than the path, so a page per driver does not become a metric per driver.
func (m *Registry) Observe(route string, status int, took time.Duration) {
	m.Requests.WithLabelValues(route, statusClass(status)).Inc()
	m.RequestDuration.WithLabelValues(route).Observe(took.Seconds())
}

// SamplePool records the database pool's state.
func (m *Registry) SamplePool(acquired, idle, total int32) {
	m.DatabasePoolConns.WithLabelValues("acquired").Set(float64(acquired))
	m.DatabasePoolConns.WithLabelValues("idle").Set(float64(idle))
	m.DatabasePoolConns.WithLabelValues("total").Set(float64(total))
}

// SamplePoolEvery keeps the pool gauges fresh until ctx is cancelled. It is a
// goroutine the caller owns, and it returns when the context does.
func (m *Registry) SamplePoolEvery(ctx context.Context, every time.Duration, sample func() (acquired, idle, total int32)) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.SamplePool(sample())
		}
	}
}

func statusClass(status int) string {
	switch {
	case status < 200:
		return "1xx"
	case status < 300:
		return "2xx"
	case status < 400:
		return "3xx"
	case status < 500:
		return "4xx"
	default:
		return "5xx"
	}
}
