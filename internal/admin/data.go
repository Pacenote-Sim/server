package admin

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pacenote-sim/server/internal/auth"
	"github.com/pacenote-sim/server/internal/config"
	"github.com/pacenote-sim/server/internal/db"
)

// DataPath is where the data page is mounted.
const DataPath = "/admin/data"

// PruneConfirmation is the word an operator types to run a prune. It is the
// name of the thing about to happen rather than "yes", so that a phrase copied
// out of a support message cannot be pasted into the wrong box.
const PruneConfirmation = "prune"

// PruneTimeout bounds one whole prune. A job that has been going for six hours
// is a job that is not going to finish, and one that stops on its own leaves
// the page able to start another rather than showing "running" for ever.
const PruneTimeout = 6 * time.Hour

// pruneRefreshSeconds is how often the data page reloads itself while a prune
// is running. Often enough that the bar moves, rarely enough that a page doing
// eight aggregates is not being rebuilt continuously.
const pruneRefreshSeconds = 3

// storageListLength is how many drivers the storage list names. It is the
// answer to "who is using the disk", and past the first screenful nobody is.
const storageListLength = 25

// PruneProgress is what a retention prune is doing, or what the last one did.
//
// It is exported because it is the one piece of this panel that outlives a
// request: a caller shutting the server down, or a test that must not close the
// database underneath a job, needs to be able to ask.
type PruneProgress struct {
	// Running is whether a prune is going on right now.
	Running bool
	// StartedAt and FinishedAt bound the run. FinishedAt is zero while it is
	// still going.
	StartedAt, FinishedAt time.Time
	// Cutoff is the instant traces had to be older than.
	Cutoff time.Time
	// Actor is the administrator who started it.
	Actor string
	// Total is how many laps were going to lose their trace when it began, and
	// Done how many have. Total can be short of Done's eventual value if laps
	// aged past the cutoff while the job ran, which is why the page reads
	// "of about".
	Total, Done int64
	// Freed is how many bytes the preview said would be freed.
	Freed int64
	// Err is why it stopped early, or empty.
	Err string
}

// Started reports whether a prune has ever been run in this process.
func (p PruneProgress) Started() bool { return !p.StartedAt.IsZero() }

// Percent is how far through the job is, between 0 and 100.
func (p PruneProgress) Percent() int {
	switch {
	case p.Total <= 0:
		return 100
	case p.Done >= p.Total:
		return 100
	default:
		return int(100 * p.Done / p.Total)
	}
}

// pruner owns the one background job this panel runs.
//
// One at a time, deliberately: two prunes with different cutoffs would report
// each other's progress, and there is no reason to run two when the second has
// nothing left to do.
type pruner struct {
	mu    sync.Mutex
	state PruneProgress
}

// begin claims the job. It reports false when one is already running, which the
// page says rather than starting a second.
func (p *pruner) begin(actor string, cutoff, now time.Time, total, freed int64) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.state.Running {
		return false
	}
	p.state = PruneProgress{
		Running:   true,
		StartedAt: now,
		Cutoff:    cutoff,
		Actor:     actor,
		Total:     total,
		Freed:     freed,
	}
	return true
}

func (p *pruner) advance(n int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.state.Done += n
}

func (p *pruner) finish(now time.Time, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.state.Running = false
	p.state.FinishedAt = now
	if err != nil {
		p.state.Err = err.Error()
	}
}

func (p *pruner) snapshot() PruneProgress {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.state
}

// PruneProgress reports what a retention prune is doing right now, or what the
// last one in this process did.
func (p *Panel) PruneProgress() PruneProgress { return p.prunes.snapshot() }

// storageRow is one driver's share of the disk.
type storageRow struct {
	Name       string
	Href       string
	Laps       int64
	TraceBytes int64
	// Share is this driver's traces as a percentage of all of them, so that
	// "one driver is most of it" is readable without arithmetic.
	Share string
}

// dataForm is the data page.
type dataForm struct {
	Stats db.DataStats
	// TraceShare is the traces as a percentage of the whole database, which is
	// the number that makes the point of this page.
	TraceShare string
	// PrunedLaps is how many laps are held with no trace — laps a prune has
	// already been over, or that arrived without one.
	PrunedLaps      int64
	OldestStint     moment
	Storage         []storageRow
	StorageMore     int64
	RetentionMonths int
	// RetentionLabel is the same number as a phrase, so the page says "1
	// month" rather than "1 months".
	RetentionLabel string
	KeepsForever   bool
	// Preview is what the stored setting would clear today. It is read even
	// when nothing is about to be pressed, because the point of the page is
	// that an operator sees the consequence before they choose to cause it.
	Preview        db.TraceSet
	PreviewCutoff  time.Time
	HasPreview     bool
	PreviewOldest  moment
	PreviewNewest  moment
	Prune          PruneProgress
	PruneRunning   bool
	PruneFinished  bool
	MaxMonths      int
	ConfirmationIs string
}

// getData renders what is stored and what it costs.
func (p *Panel) getData(w http.ResponseWriter, r *http.Request, sess db.AdminSession) {
	p.renderData(w, r, sess, "", "", http.StatusOK)
}

func (p *Panel) renderData(w http.ResponseWriter, r *http.Request, sess db.AdminSession, notice, problem string, status int) {
	ctx := r.Context()
	csrf := p.sessions.EnsureCSRF(w, r)
	now := p.deps.Now()

	form := dataForm{
		MaxMonths:      config.MaxRetentionMonths,
		ConfirmationIs: PruneConfirmation,
	}

	settings, err := p.deps.Settings(ctx)
	if err != nil {
		p.deps.Log.LogAttrs(ctx, slog.LevelError, "settings unreadable", slog.Any("error", err))
		if problem == "" {
			problem = "The retention setting could not be read — the database did not answer."
		}
	}
	form.RetentionMonths = settings.Retention.TraceMonths
	form.RetentionLabel = plural(int64(settings.Retention.TraceMonths), "month")
	form.KeepsForever = !settings.Retention.Keeps()

	if form.Stats, err = p.deps.Store.DataTotals(ctx); err != nil {
		p.deps.Log.LogAttrs(ctx, slog.LevelError, "the stored data could not be measured", slog.Any("error", err))
		if problem == "" {
			problem = "What is stored could not be read — the database did not answer."
		}
	}
	form.TraceShare = shareOf(form.Stats.TraceBytes, form.Stats.DatabaseBytes)
	form.PrunedLaps = form.Stats.Laps - form.Stats.Traces
	form.OldestStint = momentAt(now, form.Stats.OldestStintAt)

	storage, err := p.deps.Store.StorageByDriver(ctx, storageListLength)
	if err != nil {
		p.deps.Log.LogAttrs(ctx, slog.LevelError, "the storage per driver could not be read", slog.Any("error", err))
	}
	for _, s := range storage {
		if s.Laps == 0 && s.TraceBytes == 0 {
			continue
		}
		form.Storage = append(form.Storage, storageRow{
			Name:       s.Name,
			Href:       driverHref(s.ID),
			Laps:       s.Laps,
			TraceBytes: s.TraceBytes,
			Share:      shareOf(s.TraceBytes, form.Stats.TraceBytes),
		})
	}
	if more := form.Stats.Drivers - int64(len(form.Storage)); more > 0 {
		form.StorageMore = more
	}

	if cutoff, ok := settings.Retention.Cutoff(now); ok {
		form.HasPreview = true
		form.PreviewCutoff = cutoff
		if form.Preview, err = p.deps.Store.TracesOlderThan(ctx, cutoff); err != nil {
			p.deps.Log.LogAttrs(ctx, slog.LevelError, "the retention preview could not be read", slog.Any("error", err))
			form.HasPreview = false
		}
		form.PreviewOldest = momentAt(now, form.Preview.OldestAt)
		form.PreviewNewest = momentAt(now, form.Preview.NewestAt)
	}

	form.Prune = p.prunes.snapshot()
	form.PruneRunning = form.Prune.Running
	form.PruneFinished = form.Prune.Started() && !form.Prune.Running

	data := pageData{
		Title:    "Data",
		SignedIn: true,
		Email:    sess.Email,
		CSRF:     csrf,
		Nav:      navFor("Data"),
		Notice:   notice,
		Error:    problem,
		Status:   status,
		Form:     form,
	}
	if form.PruneRunning {
		data.Refresh = pruneRefreshSeconds
	}
	p.render(w, r, "data", data)
}

// postRetention saves how long traces are kept.
//
// Saving deletes nothing. It sets the horizon the preview and the prune both
// read, and the page says so beside the button — a setting that started
// deleting the moment it was saved would be a destructive action disguised as a
// form field.
func (p *Panel) postRetention(w http.ResponseWriter, r *http.Request, sess db.AdminSession) {
	if !p.beginForm(w, r) {
		return
	}
	ctx := r.Context()

	months, err := strconv.Atoi(strings.TrimSpace(r.PostFormValue("trace_months")))
	if err != nil {
		p.renderData(w, r, sess, "",
			"Retention is a number of months, and 0 means keeping every trace for ever.",
			http.StatusUnprocessableEntity)
		return
	}
	next := config.Retention{TraceMonths: months}
	if bad := config.ValidateRetention(next); bad != nil {
		p.renderData(w, r, sess, "", operatorMessage(bad), http.StatusUnprocessableEntity)
		return
	}

	settings, err := p.deps.Settings(ctx)
	if err != nil {
		p.deps.Log.LogAttrs(ctx, slog.LevelError, "settings unreadable", slog.Any("error", err))
		p.renderData(w, r, sess, "",
			"The settings could not be read, so nothing was changed — the database did not answer.",
			http.StatusInternalServerError)
		return
	}
	if settings.Retention == next {
		p.renderData(w, r, sess, "Nothing to save — that is already what is stored.", "", http.StatusOK)
		return
	}

	updated := settings
	updated.Retention = next
	if err := p.deps.Store.SaveSettings(ctx, updated); err != nil {
		p.deps.Log.LogAttrs(ctx, slog.LevelError, "the retention could not be saved", slog.Any("error", err))
		p.renderData(w, r, sess, "",
			"That change was not saved — the database did not answer.", http.StatusInternalServerError)
		return
	}
	p.settingsChanged(ctx, sess, db.ActionRetentionChanged, "retention", []string{"trace_months"})

	notice := "Saved — nothing has been deleted. The figure below is what a prune would clear with this setting, and you run the prune yourself."
	if !next.Keeps() {
		notice = "Saved — traces are now kept for ever, and there is nothing for a prune to clear."
	}
	p.renderData(w, r, sess, notice, "", http.StatusOK)
}

// postPrune starts a retention prune.
//
// It answers immediately and does the work behind: a prune of a season's traces
// is minutes of work, and a request that held the connection open for it would
// be a timeout in a proxy rather than a page. What the operator gets back is the
// page with a bar on it, refreshing itself until the job is done.
func (p *Panel) postPrune(w http.ResponseWriter, r *http.Request, sess db.AdminSession) {
	if !p.beginForm(w, r) {
		return
	}
	ctx := r.Context()
	now := p.deps.Now()

	if !auth.EqualString(PruneConfirmation, strings.TrimSpace(r.PostFormValue("confirm"))) {
		p.renderData(w, r, sess, "",
			"Type "+PruneConfirmation+" to confirm — every trace is still here until you do.",
			http.StatusUnprocessableEntity)
		return
	}

	settings, err := p.deps.Settings(ctx)
	if err != nil {
		p.deps.Log.LogAttrs(ctx, slog.LevelError, "settings unreadable", slog.Any("error", err))
		p.renderData(w, r, sess, "",
			"The retention setting could not be read, so nothing was deleted — the database did not answer.",
			http.StatusInternalServerError)
		return
	}
	cutoff, ok := settings.Retention.Cutoff(now)
	if !ok {
		p.renderData(w, r, sess, "",
			"Retention is set to keep every trace, so there is nothing to prune. Set a number of months first.",
			http.StatusConflict)
		return
	}

	// The same query the page previewed with, run again now: the operator is
	// told what it found, and the job clears exactly that set.
	set, err := p.deps.Store.TracesOlderThan(ctx, cutoff)
	if err != nil {
		p.deps.Log.LogAttrs(ctx, slog.LevelError, "the retention preview could not be read", slog.Any("error", err))
		p.renderData(w, r, sess, "",
			"What would be deleted could not be counted, so nothing was — the database did not answer.",
			http.StatusInternalServerError)
		return
	}
	if set.Empty() {
		p.renderData(w, r, sess,
			"Nothing to prune — no trace on this server is older than "+
				plural(int64(settings.Retention.TraceMonths), "month")+".",
			"", http.StatusOK)
		return
	}

	if !p.prunes.begin(sess.Email, cutoff, now, set.Laps, set.TraceBytes) {
		p.renderData(w, r, sess, "",
			"A prune is already running. Wait for it to finish — it shows its progress below.",
			http.StatusConflict)
		return
	}

	if err := p.deps.Store.WriteAudit(ctx, sess.Email, db.ActionTracesPruned,
		plural(set.Laps, "lap trace"), []string{"trace"}); err != nil {
		p.deps.Log.LogAttrs(ctx, slog.LevelError, "the prune could not be recorded", slog.Any("error", err))
	}
	p.deps.Log.LogAttrs(ctx, slog.LevelWarn, "retention prune started",
		slog.Int64("laps", set.Laps), slog.Int64("bytes", set.TraceBytes),
		slog.String("cutoff", cutoff.Format(time.RFC3339)))

	// The job outlives this request on purpose, so the context it runs on must
	// not be the request's. It keeps the request's values and loses its
	// cancellation, and carries a deadline of its own so that a job that
	// cannot finish stops rather than holding the panel's one slot for ever.
	go p.runPrune(context.WithoutCancel(ctx), cutoff)

	p.renderData(w, r, sess, "Pruning "+plural(set.Laps, "lap trace")+" — it runs in the background and this page follows it.",
		"", http.StatusOK)
}

// runPrune clears the traces one batch at a time until there are none left.
func (p *Panel) runPrune(ctx context.Context, cutoff time.Time) {
	ctx, cancel := context.WithTimeout(ctx, PruneTimeout)
	defer cancel()

	var cleared int64
	for {
		n, err := p.deps.Store.PruneTracesOlderThan(ctx, cutoff, db.PruneBatch)
		if err != nil {
			p.prunes.finish(p.deps.Now(), err)
			p.deps.Log.LogAttrs(ctx, slog.LevelError, "the retention prune stopped early",
				slog.Int64("cleared", cleared), slog.Any("error", err))
			return
		}
		if n == 0 {
			p.prunes.finish(p.deps.Now(), nil)
			p.deps.Log.LogAttrs(ctx, slog.LevelInfo, "retention prune finished",
				slog.Int64("cleared", cleared))
			return
		}
		cleared += n
		p.prunes.advance(n)
	}
}

// shareOf renders part as a percentage of whole, or an empty string when there
// is no share to speak of.
func shareOf(part, whole int64) string {
	if whole <= 0 || part <= 0 {
		return ""
	}
	pct := 100 * float64(part) / float64(whole)
	if pct < 1 {
		return "under 1%"
	}
	return strconv.FormatFloat(pct, 'f', 0, 64) + "%"
}
