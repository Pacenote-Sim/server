package plugins

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pacenote-sim/plugin"
	"github.com/pacenote-sim/server/internal/auth"
	"github.com/pacenote-sim/server/internal/db"
)

// What an operator has configured, resolved.
//
// These test configure directly rather than through a running plugin, for two
// reasons. The bug they cover was in this function and nowhere else — a
// subprocess proves it only incidentally. And a test that needs a live plugin
// to declare a required credential needs the test fixture to be able to declare
// one, which lives in another repository: a server test that only passes once
// somebody else's change is published is a test that fails on a clean checkout.

// settingsStore answers the one question configure asks of a store. The
// embedded interface supplies the rest of the method set and is nil, so a call
// to anything else is a panic naming the method — which is what it should be,
// since configure has no business calling one.
type settingsStore struct {
	Store
	rows []db.PluginSettingRow
}

func (s settingsStore) PluginSettings(context.Context, string) ([]db.PluginSettingRow, error) {
	return s.rows, nil
}

// configuring builds an instance holding declared settings and a store, which
// is all configure touches.
func configuring(t *testing.T, declared []plugin.Setting, rows []db.PluginSettingRow) *instance {
	t.Helper()
	key, err := auth.NewSecretKey()
	require.NoError(t, err)

	return &instance{
		host: &Host{opts: Options{
			Store:   settingsStore{rows: rows},
			Keyring: auth.NewKeyring(key),
		}},
		scrub:      &scrubber{},
		declared:   declared,
		valueRules: valueRulesFor(declared),
		manifest:   plugin.Manifest{Name: "coach"},
	}
}

// sealed is a credential stored the way the panel stores one.
func sealed(t *testing.T, i *instance, name, value string) db.PluginSettingRow {
	t.Helper()
	raw, err := i.host.opts.Keyring.Key().Seal(value)
	require.NoError(t, err)
	return db.PluginSettingRow{Name: name, Sealed: raw}
}

// A required credential is satisfied by the sealed value, because that is where
// a credential is.
//
// Settings arrive in two maps: plain values in one, sealed credentials in the
// other. The required check ran against the plain map, which by construction
// never holds a credential — so a plugin that asked for an API key was refused
// for not having one while the key sat in the database, sealed and readable.
// Every plugin declaring a required credential was unusable.
func TestARequiredCredentialIsSatisfiedByTheSealedOne(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	declared := []plugin.Setting{{
		Name: "api_key", Label: "Anthropic key", Kind: plugin.KindSecret, Required: true,
	}}

	// Nothing entered: refused, which is the half that always worked.
	empty := configuring(t, declared, nil)
	_, _, err := empty.configure(context.Background())
	r.ErrorIs(err, plugin.ErrNotConfigured)

	// Entered, sealed, and readable: accepted, and handed over.
	i := configuring(t, declared, nil)
	i.host.opts.Store = settingsStore{rows: []db.PluginSettingRow{
		sealed(t, i, "api_key", "sk-the-operators-own-key"),
	}}

	_, secrets, err := i.configure(context.Background())
	r.NoError(err, "a credential was entered, stored and sealed, and the plugin was told to fill it in")
	got, ok := secrets.Get("api_key")
	r.True(ok, "the credential was accepted and not handed over")
	r.Equal("sk-the-operators-own-key", got.Value())
}

// A plugin that is not finished being set up still gets what is set.
//
// Serving a page is a weaker promise than answering a request: there is no key
// to write a cue with, but a plugin's pages are often how it gets configured,
// so the caller that serves one needs the settings that are present. The error
// travels with them, and the callers that must refuse still do.
func TestAnUnfinishedSetupStillYieldsWhatIsSet(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	i := configuring(t,
		[]plugin.Setting{
			{Name: "api_key", Label: "Anthropic key", Kind: plugin.KindSecret, Required: true},
			{Name: "model", Label: "Model", Kind: plugin.KindText},
		},
		[]db.PluginSettingRow{{Name: "model", Value: "claude-sonnet-5"}},
	)

	values, _, err := i.configure(context.Background())
	r.ErrorIs(err, plugin.ErrNotConfigured, "an unfinished setup was reported as finished")
	r.Equal("claude-sonnet-5", values.String("model"),
		"the settings that are present were thrown away with the one that is not")
}

// A setting that is required and not a credential is still required of the
// plain map, which is where it is.
func TestARequiredPlainSettingIsStillRequired(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	declared := []plugin.Setting{{Name: "host", Label: "Host", Kind: plugin.KindText, Required: true}}

	_, _, err := configuring(t, declared, nil).configure(context.Background())
	r.ErrorIs(err, plugin.ErrNotConfigured)

	_, _, err = configuring(t, declared,
		[]db.PluginSettingRow{{Name: "host", Value: "example.com"}}).configure(context.Background())
	r.NoError(err)
}
