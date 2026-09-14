package setup

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pacenote-sim/server/internal/config"
	"github.com/pacenote-sim/server/internal/db"
)

// The wizard's own small parts: where each step lives, what a failure reads
// like, and how a session ages out.
//
// The steps are a table rather than seven tests because the thing worth
// checking is that the three lists agree — a step with a path and no template
// is a page that 500s, and a step with a title and no path is a link nobody can
// follow. Getting that wrong is exactly the kind of mistake that only shows up
// on the one step somebody skipped while testing by hand.

func TestEveryStepHasAPathATemplateAndATitle(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	steps := []struct {
		step  step
		path  string
		page  string
		title string
	}{
		{stepToken, "/setup", "token", "Setup"},
		{stepDatabase, "/setup/database", "database", "Database"},
		{stepOrganisation, "/setup/organisation", "organisation", "Organisation"},
		{stepAccount, "/setup/account", "account", "Administrator"},
		{stepAddress, "/setup/address", "address", "Public address"},
		{stepFinish, "/setup/finish", "finish", "Finish"},
		{stepDone, "/setup/done", "done", "Done"},
	}
	paths := map[string]bool{}
	for _, s := range steps {
		r.Equal(s.path, s.step.path())
		r.Equal(s.page, s.step.page())
		r.Equal(s.title, s.step.title())
		r.False(paths[s.path], "%q is served by two steps", s.path)
		paths[s.path] = true
	}

	// A step that is not one lands on the first page rather than on nothing.
	// It cannot happen today; it is what stops a step added later from being
	// served as a blank screen.
	unknown := step(99)
	r.Equal("/setup", unknown.path())
	r.Equal("token", unknown.page())
	r.Equal("Setup", unknown.title())
}

// The two steps with no form behind them. Asking for their form is not a
// mistake — the renderer asks every step — and the answer is nothing rather
// than a zero value some template would try to range over.
func TestTheStepsWithNothingToFillIn(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	w := &Wizard{deps: Deps{DataDir: "/srv/pacenote-data"}}
	s := &wizardSession{}
	r.Nil(w.formFor(s, stepToken))
	r.Nil(w.formFor(s, stepDone))
	r.Nil(w.formFor(s, step(99)))
}

// What a failure from a leaf package reads like by the time an operator sees
// it: the package prefix off the front, a capital letter, a full stop.
func TestErrorsAreTurnedIntoSentences(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	r.Equal("That host is not a host.", sentence(errors.New("config: that host is not a host"), "config: "))
	//nolint:revive // error-strings: a capitalised, punctuated error is the input under test.
	r.Equal("Already A Sentence.", sentence(errors.New("Already A Sentence."), "config: "))
	r.Equal("A prefix that is not there.", sentence(errors.New("a prefix that is not there"), "db: "))
	r.Empty(sentence(errors.New("config: "), "config: "), "a message that is only a prefix")
}

// How the confirmation page describes what is about to happen, in each of the
// two ways a server can be reached and with a connection string it cannot read.
func TestTheConfirmationPageDescribesTheServer(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	r.Contains(tlsDescription(config.TLSAuto), "gets its own certificate")
	r.Contains(tlsDescription(config.TLSProxy), "your own proxy in front")

	r.Equal("not set", summariseDatabase(""))
	r.Equal("the database you entered", summariseDatabase("this is not a connection string"))

	// A password is never shown: the page exists so an operator can check the
	// database, on a screen somebody might be looking at over their shoulder.
	summary := summariseDatabase("postgres://ana:hunter2@db.example.com:5432/pacenote")
	r.NotContains(summary, "hunter2")
	r.Contains(summary, "pacenote")
}

// A session that has been sitting open too long is gone. The wizard holds an
// administrator's password in memory between steps, so a browser tab left open
// overnight has to stop being a way back in.
func TestASessionAgesOut(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	now := time.Now()
	w := &Wizard{
		deps:     Deps{Now: func() time.Time { return now }},
		sessions: map[string]*wizardSession{},
	}
	s := &wizardSession{id: "a-session", created: now}
	w.sessions[s.id] = s

	withCookie := func(value string) *http.Request {
		req := httptest.NewRequest(http.MethodGet, "/setup/database", http.NoBody)
		if value != "" {
			req.AddCookie(&http.Cookie{Name: SessionCookie, Value: value})
		}
		return req
	}

	r.Same(s, w.session(withCookie(s.id)))
	r.Nil(w.session(withCookie("")), "a request with no cookie")
	r.Nil(w.session(withCookie("no such session")))

	now = now.Add(SessionTTL + time.Second)
	r.Nil(w.session(withCookie(s.id)), "a session past its life was handed back")
	r.NotContains(w.sessions, s.id, "and was left in memory")

	// And the sweep clears what nobody came back for at all.
	w.sessions["stale"] = &wizardSession{id: "stale", created: now.Add(-2 * SessionTTL)}
	w.sessions["fresh"] = &wizardSession{id: "fresh", created: now}
	w.sweepSessions()
	r.NotContains(w.sessions, "stale")
	r.Contains(w.sessions, "fresh")
}

// What a failure reads like by the time it reaches the operator.
//
// The wizard is the one place where every error is somebody's first impression
// of this software, and the thing they are holding is a connection string or an
// email address rather than a stack trace. Each of these turns a failure into
// the next thing to do.
func TestWhatAFailureReadsLike(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	// A connection this server could not make carries its own sentence, which
	// names the host and the reason rather than the driver's wording.
	connErr := &db.ConnectError{Message: "That host does not answer on port 5432."}
	r.Equal("That host does not answer on port 5432.", describeError(connErr))
	r.Equal("That host does not answer on port 5432.",
		describeError(fmt.Errorf("setup: %w", connErr)))

	// The two that mean somebody got here first.
	r.Contains(describeError(db.ErrSetupAlreadyComplete), "Sign in to the admin panel instead")
	r.Contains(describeError(db.ErrAdminEmailTaken), "already has an account on this server")

	// Anything else keeps its own words, with a short package prefix taken off
	// the front and a full stop put on the end.
	r.Equal("Setup could not finish: the disk is full.", describeError(errors.New("db: the disk is full")))
	// A colon far enough in is part of the sentence rather than a package
	// prefix, so it is left alone. The rule is a length because a package name
	// is short and a sentence is not.
	r.Equal("Setup could not finish: the connection was refused, and then: refused again.",
		describeError(errors.New("the connection was refused, and then: refused again")))
}

// A wizard built with nothing filled in still works. It is how the server
// builds one on a first run, where there is no log to write to yet and nothing
// counting attempts.
func TestAWizardBuiltWithTheLeastPossible(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	w, err := New(Deps{Token: "7QK4-M2XF-8DNA-0123-4567-89AB-CDEF-GHJK", DataDir: t.TempDir()})
	r.NoError(err)
	r.NotNil(w)
	r.False(w.Completed(), "a wizard that has not run reports itself finished")
	r.NotNil(w.Handler())
}
