package admin

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/pacenote-sim/server/internal/clientbuild"
	"github.com/pacenote-sim/server/internal/config"
	"github.com/pacenote-sim/server/internal/db"
	"github.com/pacenote-sim/server/internal/httpx"
)

// ClientsPath is where the build client page is mounted.
const ClientsPath = "/admin/clients"

// ClientDownloadPath is the prefix a built client is downloaded from. The last
// part of the address is the build's reference, which is also the name of the
// file on the disk and the identifier stamped inside the binary.
const ClientDownloadPath = ClientsPath + "/download/"

// buildFormAddress is the name of the address field, used by the page and by
// the tests that post to it.
const buildFormAddress = "address"

// buildFormSigning is the name of the signing field.
const buildFormSigning = "signing"

// signingChoice is one of the two ways a client can be signed, as the page
// shows it.
type signingChoice struct {
	// Value is what the radio button posts.
	Value string
	// Label is its name.
	Label string
	// Chosen is whether it is the one selected.
	Chosen bool
	// Available is whether this server can actually do it. An option that is
	// not available is shown, described and disabled — the choice is real and
	// an operator meeting it for the first time should see both.
	Available bool
	// Description is what it means for the operator and for their drivers.
	Description string
	// Why is the reason it cannot be chosen here, for the options that cannot.
	Why string
}

// builtRow is one client that was built, as the page lists it.
type builtRow struct {
	At      moment
	Actor   string
	Address string
	Signing string
	Version string
	Name    string
	Bytes   int64
	SHA256  string
	Href    string
	Gone    bool
	// SignedBy is the certificate that signed it, empty for an unsigned build.
	// It is shown rather than derived from the route, because the certificate
	// an operator signs with today is not the one they signed with last year.
	SignedBy string
}

// clientsForm is the build client page.
type clientsForm struct {
	// Client is the prebuilt client this server stamps copies of, including
	// the case where there is not one yet.
	Client clientbuild.Client
	// Modified and Built are the prebuilt client's file date and the date the
	// toolchain recorded inside it.
	Modified moment
	Built    moment
	// CanBuild is whether pressing the button would produce a file.
	CanBuild bool
	// Address is what the address field holds: what the operator typed if they
	// have typed something, and this server's own address otherwise.
	Address string
	// Signing is the routes, and Certificate is what this server holds to sign
	// with. The two are one decision: the second route exists only while there
	// is a certificate, and the card that uploads one sits beside it.
	Signing     []signingChoice
	Certificate certificateForm
	// Latest is the newest build, or nil when nothing has been built here.
	Latest *builtRow
	// Fresh is whether Latest is the build that was just made, which is what
	// turns the card into an answer rather than a record.
	Fresh bool
	// Rows is the build history, and Links the page under it.
	Rows  []builtRow
	Links pageLinks
	// Total is how many clients have been built here, which is more than one
	// page's worth on a server that has been running a while.
	Total int64
	// ConfigVariable is the environment variable that points this server at a
	// client somewhere else, named in the empty state so that an operator who
	// keeps the file elsewhere knows how to say so.
	ConfigVariable string
	// ClientFile is what the prebuilt client is called, for the same reason.
	ClientFile string
	// CertificatePath and CertificateRemovePath are where the certificate form
	// posts. They are on the form rather than written into the template so
	// that the route and the page cannot drift apart.
	CertificatePath       string
	CertificateRemovePath string
}

// getClients renders the build page.
func (p *Panel) getClients(w http.ResponseWriter, r *http.Request, sess db.AdminSession) {
	p.renderClients(w, r, sess, clientsState{}, "", "", http.StatusOK)
}

// clientsState is what a failed or successful post carries back into the page:
// the values the operator chose, so a rejected form comes back as they left it
// rather than as it started.
type clientsState struct {
	address string
	signing clientbuild.Signing
	fresh   bool
}

func (p *Panel) renderClients(w http.ResponseWriter, r *http.Request, sess db.AdminSession,
	state clientsState, notice, problem string, status int,
) {
	ctx := r.Context()
	csrf := p.sessions.EnsureCSRF(w, r)
	now := p.deps.Now()
	q := r.URL.Query()

	form := clientsForm{
		Client:         p.deps.Clients.Source.Inspect(),
		ConfigVariable: config.EnvClientBinary,
		ClientFile:     config.ClientFileName,

		CertificatePath:       CertificatePath,
		CertificateRemovePath: CertificateRemovePath,
	}
	form.Modified = momentAt(now, form.Client.ModifiedAt)
	form.Built = momentAt(now, form.Client.BuiltAt)
	form.CanBuild = p.deps.Clients.Dir != "" && form.Client.Ready()

	form.Address = state.address
	if form.Address == "" {
		form.Address = p.ownAddress(ctx)
	}
	form.Certificate = p.certificateOf(ctx, now)
	chosen := state.signing
	if chosen == "" {
		chosen = clientbuild.SigningNone
	}
	form.Signing = signingChoices(chosen, form.Certificate.CanSign())

	cursor := clientBuildCursorOf(q)
	rows, err := p.deps.Store.ClientBuilds(ctx, db.ClientBuildQuery{After: cursor, Limit: db.PageSize + 1})
	if err != nil {
		p.deps.Log.LogAttrs(ctx, slog.LevelError, "the build history could not be read", slog.Any("error", err))
		if problem == "" {
			problem = "The build history could not be read — the database did not answer."
		}
	}

	pg := pager[db.ClientBuild]{base: ClientsPath}
	kept, links := pg.page(rows, db.PageSize, cursor == nil, func(b db.ClientBuild) url.Values {
		c := b.Cursor()
		return url.Values{paramAfter: {itoa(c.ID)}, paramFrom: {stamp(c.At)}}
	})
	form.Links = links
	for i := range kept {
		form.Rows = append(form.Rows, p.builtRowOf(now, &kept[i]))
	}
	if cursor == nil && len(form.Rows) > 0 {
		latest := form.Rows[0]
		form.Latest = &latest
		form.Fresh = state.fresh
	}
	if form.Total, err = p.deps.Store.CountClientBuilds(ctx); err != nil {
		p.deps.Log.LogAttrs(ctx, slog.LevelError, "the builds could not be counted", slog.Any("error", err))
	}

	p.render(w, r, "clients", pageData{
		Title:    "Build client",
		SignedIn: true,
		Email:    sess.Email,
		CSRF:     csrf,
		Nav:      navFor("Build client"),
		Notice:   notice,
		Error:    problem,
		Status:   status,
		Form:     form,
	})
}

// builtRowOf turns a history row into what the page shows, which includes
// whether the file is still on this server.
func (p *Panel) builtRowOf(now time.Time, b *db.ClientBuild) builtRow {
	row := builtRow{
		At:       momentAt(now, b.At),
		Actor:    b.Actor,
		Address:  b.Address,
		Signing:  clientbuild.Signing(b.Signing).Label(),
		Version:  b.ClientVersion,
		Name:     b.FileName,
		Bytes:    b.FileBytes,
		SHA256:   b.SHA256,
		SignedBy: b.SignedBy,
	}
	if p.deps.Clients.Exists(b.Reference) {
		row.Href = ClientDownloadPath + b.Reference
	} else {
		row.Gone = true
	}
	return row
}

// ownAddress is this server's own address, which is what the field holds
// before an operator types anything: the client they are about to build talks
// back to the server they are building it on, and that address is one this
// server already knows.
func (p *Panel) ownAddress(ctx context.Context) string {
	settings, err := p.deps.Settings(ctx)
	if err != nil {
		p.deps.Log.LogAttrs(ctx, slog.LevelError, "settings unreadable", slog.Any("error", err))
		return ""
	}
	return settings.BaseURL()
}

// postBuildClient builds one client.
func (p *Panel) postBuildClient(w http.ResponseWriter, r *http.Request, sess db.AdminSession) {
	if !p.beginForm(w, r) {
		return
	}
	ctx := r.Context()

	signing := clientbuild.Signing(strings.TrimSpace(r.PostFormValue(buildFormSigning)))
	if signing == "" {
		signing = clientbuild.SigningNone
	}
	typed := strings.TrimSpace(r.PostFormValue(buildFormAddress))
	state := clientsState{address: typed, signing: signing}

	canSign := p.certificateOf(ctx, p.deps.Now()).CanSign()
	if !signing.Valid() || !signing.Available(canSign) {
		p.renderClients(w, r, sess, state, "", signing.Unavailable(canSign), http.StatusUnprocessableEntity)
		return
	}

	address, err := ClientAddress(typed)
	if err != nil {
		p.renderClients(w, r, sess, state, "", operatorMessage(err), http.StatusUnprocessableEntity)
		return
	}
	state.address = address

	// There is no separate "is there a client" check before this: the build
	// reads the file it is about to copy, and asking first would read eleven
	// megabytes twice to answer a question the build answers anyway.
	art, err := p.deps.Clients.Build(ctx, clientbuild.Request{
		Address: address, Signing: signing, Program: p.publisher(ctx),
	})
	switch {
	case errors.Is(err, clientbuild.ErrNotReady):
		p.deps.Log.LogAttrs(ctx, slog.LevelWarn, "a client was asked for and there is none to build from",
			slog.Any("error", err))
		p.renderClients(w, r, sess, state, "",
			"There is no client on this server to build from, so nothing was built. The card above says what to put where.",
			http.StatusConflict)
		return
	case err != nil:
		p.deps.Log.LogAttrs(ctx, slog.LevelError, "the client could not be built", slog.Any("error", err))
		p.renderClients(w, r, sess, state, "",
			"That client was not built — "+strings.TrimPrefix(err.Error(), "clientbuild: ")+".",
			http.StatusInternalServerError)
		return
	}

	if _, err := p.deps.Store.RecordClientBuild(ctx, db.NewClientBuild{
		Actor:         sess.Email,
		Address:       art.Address,
		Signing:       string(art.Signing),
		Reference:     art.Reference,
		FileName:      art.Name,
		FileBytes:     art.Size,
		SHA256:        art.SHA256,
		ClientVersion: clientVersionOf(art.Client),
		SignedBy:      art.SignedBy,
	}); err != nil {
		p.deps.Log.LogAttrs(ctx, slog.LevelError, "the build could not be recorded", slog.Any("error", err))
		p.renderClients(w, r, sess, state, "",
			"The client was built, but this server could not write it into the build history — the database did not answer. The file is on the server and the history is missing a row.",
			http.StatusInternalServerError)
		return
	}

	if err := p.deps.Store.WriteAudit(ctx, sess.Email, db.ActionClientBuilt, art.Address,
		[]string{"address", "signing"}); err != nil {
		p.deps.Log.LogAttrs(ctx, slog.LevelError, "the build could not be recorded in the audit trail", slog.Any("error", err))
	}
	p.deps.Log.LogAttrs(ctx, slog.LevelInfo, "client built",
		slog.String("address", art.Address),
		slog.String("signing", string(art.Signing)),
		slog.String("reference", art.Reference))

	p.renderClients(w, r, sess, clientsState{address: address, signing: signing, fresh: true},
		"Built — the file below is a client that talks to "+art.Address+". Check the address before you hand it out.",
		"", http.StatusOK)
}

// getClientDownload serves one built client.
func (p *Panel) getClientDownload(w http.ResponseWriter, r *http.Request, _ db.AdminSession) {
	ctx := r.Context()
	reference := r.PathValue("reference")

	build, err := p.deps.Store.ClientBuildByReference(ctx, reference)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			httpx.Problem(w, r, http.StatusNotFound, "Nothing was built under that reference on this server.")
			return
		}
		p.deps.Log.LogAttrs(ctx, slog.LevelError, "that build could not be read", slog.Any("error", err))
		httpx.Problem(w, r, http.StatusInternalServerError, "Something went wrong on the server. Try again.")
		return
	}

	f, err := p.deps.Clients.Open(build.Reference)
	if err != nil {
		p.deps.Log.LogAttrs(ctx, slog.LevelWarn, "a built client is no longer on this server",
			slog.String("reference", build.Reference), slog.Any("error", err))
		httpx.Problem(w, r, http.StatusNotFound,
			"That client is no longer on this server. Build it again — the address it carried is in the history.")
		return
	}
	defer func() { _ = f.Close() }()

	info, err := f.Stat()
	if err != nil {
		httpx.Problem(w, r, http.StatusInternalServerError, "Something went wrong on the server. Try again.")
		return
	}

	w.Header().Set("Content-Type", "application/vnd.microsoft.portable-executable")
	w.Header().Set("Content-Disposition", `attachment; filename="`+sanitiseFileName(build.FileName)+`"`)
	http.ServeContent(w, r, build.FileName, info.ModTime(), f)
}

// ClientAddress is the address an operator may stamp into a client.
//
// It is the client's own rule, applied here so that the mistake is caught on
// this page rather than by a driver whose app will not start: a scheme and a
// host, nothing else, and plain HTTP only for a server on the driver's own
// machine. A missing scheme is filled in rather than refused, because typing a
// host name is what an operator does.
func ClientAddress(typed string) (string, error) {
	address := strings.TrimSpace(typed)
	if address == "" {
		return "", errors.New("a client needs a server address — the one your drivers' clients will talk to")
	}
	if !strings.Contains(address, "://") {
		address = defaultScheme(address) + "://" + address
	}
	u, err := url.Parse(address)
	if err != nil {
		return "", errors.New("that is not an address this server can read — it should look like https://pacenote.example.com")
	}
	switch {
	case u.Scheme != "http" && u.Scheme != "https":
		return "", errors.New("a client talks HTTP or HTTPS, so " + u.Scheme + " is not an address it can reach")
	case u.Host == "":
		return "", errors.New("that address names no host — it should look like https://pacenote.example.com")
	case u.User != nil:
		return "", errors.New("a server address carries no user name or password")
	case u.Path != "" && u.Path != "/":
		return "", errors.New("a server address is a host, not a path — drop everything after the host name")
	case u.RawQuery != "" || u.Fragment != "":
		return "", errors.New("a server address carries no query or fragment")
	case u.Scheme == "http" && !isLoopback(u.Hostname()):
		return "", errors.New("a client only talks plain HTTP to a server on the driver's own machine — use https:// for anything else, or the client will refuse to start")
	}
	return u.Scheme + "://" + u.Host, nil
}

// defaultScheme is HTTPS for everything except this machine, which is the same
// rule the client applies to what a driver types.
func defaultScheme(hostport string) string {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	if isLoopback(host) {
		return "http"
	}
	return "https"
}

// isLoopback reports whether a host name is the machine the client runs on. It
// is the client's own rule: plain HTTP is allowed to a server there and
// nowhere else.
func isLoopback(host string) bool {
	host = strings.Trim(host, "[]")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// clientVersionOf is what the history records as the version a copy was
// stamped from: the module version when the toolchain recorded one, and the
// commit when it did not.
func clientVersionOf(c clientbuild.Client) string {
	switch {
	case c.Version != "" && c.Version != "(devel)":
		return c.Version
	case c.Revision != "":
		return c.ShortRevision()
	default:
		return "unknown"
	}
}

// publisher is what Windows calls the client in the dialog it shows a driver:
// the operator's own organisation, so that a driver in two teams can tell
// whose client they are installing.
func (p *Panel) publisher(ctx context.Context) string {
	settings, err := p.deps.Settings(ctx)
	if err != nil {
		return ""
	}
	return settings.Organisation
}

// signingChoices is the two routes, with the one the operator chose marked.
func signingChoices(chosen clientbuild.Signing, certificate bool) []signingChoice {
	descriptions := map[clientbuild.Signing]string{
		clientbuild.SigningNone:           "Free and immediate — Windows will warn your drivers the app is untrusted, and they choose to run it anyway. It is one dialog on first run: More info, then Run anyway. Tell your drivers to expect it and most of them get past it.",
		clientbuild.SigningOwnCertificate: "This server signs every client it builds with a certificate you give it, so a driver can see who the file came from and that nobody changed it on the way. Whether Windows stops warning them depends on the certificate: one from a public authority stops the warning once it has built a download reputation, and one you made yourself or issued from your own authority stops it only on machines that trust your authority.",
	}
	out := make([]signingChoice, 0, len(clientbuild.SigningRoutes()))
	for _, s := range clientbuild.SigningRoutes() {
		out = append(out, signingChoice{
			Value:       string(s),
			Label:       s.Label(),
			Chosen:      s == chosen && s.Available(certificate),
			Available:   s.Available(certificate),
			Description: descriptions[s],
			Why:         s.Unavailable(certificate),
		})
	}
	return out
}

// clientBuildCursorOf reads the history cursor out of a request's parameters,
// or reports that this is the first page.
func clientBuildCursorOf(q url.Values) *db.ClientBuildCursor {
	id, err := strconv.ParseInt(q.Get(paramAfter), 10, 64)
	if err != nil || id <= 0 {
		return nil
	}
	at, err := time.Parse(time.RFC3339Nano, q.Get(paramFrom))
	if err != nil {
		return nil
	}
	return &db.ClientBuildCursor{At: at, ID: id}
}

// sanitiseFileName keeps the download's file name to what it should be: the
// base name, with nothing in it that could break out of the header it is
// written into.
func sanitiseFileName(name string) string {
	name = filepath.Base(name)
	name = strings.Map(func(r rune) rune {
		if r < 0x20 || r == '"' || r == '\\' || r == '/' {
			return -1
		}
		return r
	}, name)
	if name == "" || name == "." {
		return "client.exe"
	}
	return name
}
