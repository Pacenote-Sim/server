package app

import (
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pacenote-sim/server/internal/auth"
	"github.com/pacenote-sim/server/internal/config"
	"github.com/pacenote-sim/server/internal/logging"
)

// The pieces of startup that have no way in from outside: replacing the data
// key in the configuration file, reading one that is not readable, and the
// mode that is not one.

// Regenerating the data key writes it where the next startup will look, keeps
// everything else in the file, and leaves the old file in place if it cannot.
//
// The panel does this while the server is running, which is the whole reason it
// goes through a file rather than a variable: an operator who replaces the key
// and restarts must find the new one, or every credential their plugins hold
// becomes unreadable.
func TestTheDataKeyIsWrittenIntoTheConfigurationFile(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	ctx := context.Background()

	dir := filepath.Join(t.TempDir(), "pacenote-data")
	original := config.Config{
		DatabaseURL:   "postgres://localhost/pacenote",
		Listen:        "127.0.0.1:8080",
		MetricsListen: "127.0.0.1:9090",
		ClientBinary:  "/srv/pacenote-telemetry.exe",
	}
	r.NoError(original.Save(dir))

	w := &dataKeyFile{cfg: original, dir: dir}
	key, err := auth.NewSecretKey()
	r.NoError(err)
	r.NoError(w.save(ctx, key))

	// The key is on disk and everything beside it survived.
	back, err := config.Load(dir)
	r.NoError(err)
	r.Equal(key.String(), back.SecretKey)
	r.Equal(original.DatabaseURL, back.DatabaseURL)
	r.Equal(original.ClientBinary, back.ClientBinary)
	r.Equal(original.Listen, back.Listen)

	// And a second key replaces the first rather than being added beside it.
	second, err := auth.NewSecretKey()
	r.NoError(err)
	r.NoError(w.save(ctx, second))
	back, err = config.Load(dir)
	r.NoError(err)
	r.Equal(second.String(), back.SecretKey)
	r.NotEqual(key.String(), back.SecretKey)

	// A file that cannot be written is an error rather than a key the process
	// believes it has stored. The panel turns that into a sentence saying
	// nothing has changed, which is true only because this said so.
	blocked := filepath.Join(t.TempDir(), "not-a-directory")
	r.NoError(os.WriteFile(blocked, []byte("this is a file"), 0o600))
	r.Error((&dataKeyFile{cfg: original, dir: filepath.Join(blocked, "data")}).save(ctx, key))
}

// A data key the configuration file cannot offer.
//
// None of these stops the server. A server with no readable key is a server
// where a plugin's stored credential cannot be opened, and the plugin's own
// page says so — which is a great deal better than refusing to start and
// leaving an operator with no page to read it on.
func TestAnUnreadableDataKeyDoesNotStopTheServer(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	r.Nil(secretKey(config.Config{}, logging.Discard()), "a server with no key")
	r.Nil(secretKey(config.Config{SecretKey: "not base64 at all!!"}, logging.Discard()),
		"a key that is not readable")
	r.Nil(secretKey(config.Config{SecretKey: "c2hvcnQ"}, logging.Discard()),
		"a key of the wrong length")

	key, err := auth.NewSecretKey()
	r.NoError(err)
	r.Equal([]byte(key), []byte(secretKey(config.Config{SecretKey: key.String()}, logging.Discard())))
}

// A mode this server does not have. Nothing produces one today; the branch is
// what stops a mode added later from starting a server that serves nothing.
func TestAModeThatIsNotOne(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	p := &phase{mode: Mode("halfway")}
	_, err := p.buildPublic(context.Background())
	r.Error(err)
	r.Contains(err.Error(), `"halfway" is not a mode`)
}

// The address printed on the terminal for an operator to paste.
//
// A server bound to every interface knows its port and not its host, and
// "[::]:8080" is not something anybody can type. The host name is a guess, and
// a guess they can correct is better than a string they cannot use.
func TestTheAddressPrintedForAnOperator(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	r.Equal("http://127.0.0.1:8080/setup", setupURL("127.0.0.1:8080"))
	r.Equal("http://pacenote.example.com:443/setup", setupURL("pacenote.example.com:443"))

	// Every interface, in each of the three ways it is written.
	name := hostName()
	r.NotEmpty(name)
	for _, addr := range []string{":8080", "0.0.0.0:8080", "[::]:8080"} {
		r.Equal("http://"+net.JoinHostPort(name, "8080")+"/setup", setupURL(addr),
			"%q printed an address nobody can type", addr)
	}

	// Something that is not an address at all still prints something, because
	// the alternative is a first run with no instruction on the screen.
	r.Equal("http://not-an-address/setup", setupURL("not-an-address"))
}

// A server that has not been told whose it is still says something on the
// terminal. It happens between the wizard finishing and the settings being
// read, and "Pacenote —" with nothing after it reads as a broken start.
func TestTheTerminalAlwaysNamesSomething(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	var out strings.Builder
	p := &phase{
		mode:       ModeNormal,
		opts:       Options{Terminal: &out, Version: "v0.0.0-test", Log: logging.Discard()},
		settings:   config.Settings{},
		listenAddr: "127.0.0.1:8080",
	}
	p.announce(context.Background(), "127.0.0.1:9090")
	r.Contains(out.String(), "Pacenote v0.0.0-test — Pacenote")
	r.Contains(out.String(), "Bound 127.0.0.1:8080")
}

// The automatic-certificate mode, wired end to end on ports a test may bind.
//
// The ports are the only thing standing in for the real ones. Everything else
// is what a server on the open internet runs: the certificate cache in the data
// directory, the host policy naming exactly one host, and the challenge
// listener beside the one serving drivers.
func TestTheAutomaticCertificateModeWiresBothPorts(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	dir := t.TempDir()
	p := &phase{opts: Options{DataDir: dir, Log: logging.Discard()}}
	p.tlsPorts.https, p.tlsPorts.challenge = "127.0.0.1:0", "127.0.0.1:0"

	settings := config.DefaultSettings("Iberian GT Championship", "pacenote.example.com", config.TLSAuto)
	servers, err := p.autocertServers(settings, http.NotFoundHandler())
	r.NoError(err)
	t.Cleanup(func() {
		for _, s := range servers {
			_ = s.ln.Close()
		}
	})

	r.Len(servers, 2, "an automatic certificate needs a port to serve on and a port to be checked on")
	r.Equal("https", servers[0].name)
	r.Equal("certificate check", servers[1].name)
	r.NotEmpty(p.listenAddr)

	// The cache is in the data directory, because losing it means asking a
	// certificate authority for another certificate and those are rationed.
	r.DirExists(filepath.Join(dir, "autocert"))

	// The port that could not be opened is named, so an operator reading it
	// knows which one to go and free.
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	r.NoError(err)
	defer func() { _ = taken.Close() }()

	blocked := &phase{opts: Options{DataDir: t.TempDir(), Log: logging.Discard()}}
	blocked.tlsPorts.https, blocked.tlsPorts.challenge = taken.Addr().String(), "127.0.0.1:0"
	_, err = blocked.autocertServers(settings, http.NotFoundHandler())
	r.ErrorContains(err, "port 443 could not be opened")

	blocked.tlsPorts.https, blocked.tlsPorts.challenge = "127.0.0.1:0", taken.Addr().String()
	_, err = blocked.autocertServers(settings, http.NotFoundHandler())
	r.ErrorContains(err, "port 80 could not be opened")
}

// A certificate cache this server cannot create. It is a hard stop rather than
// a server that starts and cannot renew: a certificate it cannot keep is a
// certificate that expires without anybody noticing.
func TestTheAutomaticCertificateModeNeedsSomewhereToKeepOne(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	blocked := filepath.Join(t.TempDir(), "not-a-directory")
	r.NoError(os.WriteFile(blocked, []byte("this is a file"), 0o600))

	p := &phase{opts: Options{DataDir: blocked, Log: logging.Discard()}}
	_, err := p.autocertServers(
		config.DefaultSettings("Iberian GT Championship", "pacenote.example.com", config.TLSAuto),
		http.NotFoundHandler())
	r.Error(err)
}
