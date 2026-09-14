package web_test

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pacenote-sim/server/internal/web"
)

func TestBytes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   int64
		want string
	}{
		{"nothing", 0, "0 B"},
		{"a few bytes", 512, "512 B"},
		{"a kibibyte", 1024, "1.0 KiB"},
		{"a few megabytes", 5 * 1024 * 1024, "5.0 MiB"},
		{"a gibibyte and a half", 1536 * 1024 * 1024, "1.5 GiB"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := require.New(t)
			r.Equal(tc.want, web.Bytes(tc.in))
		})
	}
}

func TestCount(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   int64
		want string
	}{
		{"nothing", 0, "0"},
		{"under a thousand", 942, "942"},
		{"a thousand", 1000, "1 000"},
		{"a million and change", 1234567, "1 234 567"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := require.New(t)
			r.Equal(tc.want, web.Count(tc.in))
		})
	}
}

func TestSince(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   time.Duration
		want string
	}{
		{"just started", 3 * time.Second, "3 seconds"},
		{"minutes", 7 * time.Minute, "7 minutes"},
		{"hours and minutes", 2*time.Hour + 5*time.Minute, "2 hours 5 minutes"},
		{"days and hours", 50 * time.Hour, "2 days 2 hours"},
		{"a negative clock is not a negative uptime", -time.Second, "0 seconds"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := require.New(t)
			r.Equal(tc.want, web.Since(tc.in))
		})
	}
}

func TestDateTimeSaysNeverForTheZeroTime(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	r.Equal("never", web.DateTime(time.Time{}))
	r.NotEqual("never", web.DateTime(time.Now()))
}

func TestAssetsServeTheStylesheet(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	rec := httptest.NewRecorder()
	web.Assets().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, web.AssetPrefix+"app.css", http.NoBody))
	r.Equal(http.StatusOK, rec.Code)
	r.Contains(rec.Header().Get("Content-Type"), "text/css")
	r.Contains(rec.Body.String(), "--bg:")
	r.NotEmpty(rec.Header().Get("Cache-Control"))
}

func TestLapTime(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		ms   int
		want string
	}{
		{"no lap is a dash rather than a time of nothing", 0, "—"},
		{"a negative time is not a time either", -1, "—"},
		{"under a minute keeps the seconds unpadded", 45_123, "45.123s"},
		{"a lap time reads the way a dash shows it", 95_400, "1:35.400"},
		{"seconds are padded so the column lines up", 61_050, "1:01.050"},
		{"a long lap at the Nordschleife", 487_654, "8:07.654"},
		{"exactly two minutes", 120_000, "2:00.000"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := require.New(t)
			r.Equal(tc.want, web.LapTime(tc.ms))
		})
	}
}
