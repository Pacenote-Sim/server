// Package web holds what the server-rendered pages share: the one stylesheet,
// and the template helpers that format a number for an operator.
//
// There is no JavaScript framework here and no content delivery network. The
// admin panel is HTML and one stylesheet, both inside the binary, which is what
// lets the content security policy be "default-src 'none'" and what keeps the
// panel working on a machine with no route to the internet.
package web

import (
	"embed"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"strings"
	"time"
)

//go:embed assets
var assetsFS embed.FS

// AssetPrefix is where the stylesheet is mounted. It is a constant because the
// content security policy, the templates and the route all have to agree.
const AssetPrefix = "/assets/"

// Assets serves the embedded stylesheet. Files are immutable for the life of a
// build, so they are cached for a day; a new build serves a new file at the
// same path, which is fine for one stylesheet on an admin page.
func Assets() http.Handler {
	sub, err := fs.Sub(assetsFS, "assets")
	if err != nil {
		// The embed directive above guarantees the directory exists. If that
		// ever stops being true it is a build fault, not a runtime one.
		panic("web: embedded assets are missing: " + err.Error())
	}
	files := http.FileServerFS(sub)
	return http.StripPrefix(AssetPrefix, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=86400")
		files.ServeHTTP(w, r)
	}))
}

// FuncMap is the template helper set both the wizard and the admin panel use.
func FuncMap() template.FuncMap {
	return template.FuncMap{
		"bytes":    Bytes,
		"since":    Since,
		"count":    Count,
		"datetime": DateTime,
		"laptime":  LapTime,
	}
}

// LapTime renders a lap in the spelling a driver reads on their own dash:
// minutes, seconds to the millisecond. Zero is a dash rather than "0:00.000",
// because zero here means "no clean lap in this stint" and a time of nothing is
// not a time.
func LapTime(ms int) string {
	if ms <= 0 {
		return "—"
	}
	m := ms / 60000
	s := float64(ms%60000) / 1000
	if m == 0 {
		return fmt.Sprintf("%.3fs", s)
	}
	return fmt.Sprintf("%d:%06.3f", m, s)
}

// Bytes renders a size the way an operator reads one. Powers of 1024, because
// that is what PostgreSQL reports and disagreeing with the database about how
// big the database is would be its own support ticket.
func Bytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit && exp < 4; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTP"[exp])
}

// Count renders a row count with thin separators, so six digits can be read at
// a glance.
func Count(n int64) string {
	s := fmt.Sprintf("%d", n)
	if n < 0 {
		return s
	}
	var b strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(' ')
		}
		b.WriteRune(c)
	}
	return b.String()
}

// Since renders a duration as an operator would say it: the largest two units
// that matter and no more.
func Since(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%d seconds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%d minutes", int(d.Minutes()))
	case d < 24*time.Hour:
		h := int(d.Hours())
		m := int(d.Minutes()) - h*60
		return fmt.Sprintf("%d hours %d minutes", h, m)
	default:
		days := int(d.Hours()) / 24
		h := int(d.Hours()) - days*24
		return fmt.Sprintf("%d days %d hours", days, h)
	}
}

// DateTime renders an instant in the server's own time zone, seconds included,
// because a diagnostics page is read next to a log file.
func DateTime(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	return t.Local().Format("2006-01-02 15:04:05 MST")
}
