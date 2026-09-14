package clientbuild

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// The permissions of the built clients and the directory holding them. They
// are the data directory's own: a built client is not a secret, but it is this
// server's file and nothing on the machine needs to read it except this
// server, which serves it to a signed-in operator.
const (
	artifactMode fs.FileMode = 0o600
	artifactDir  fs.FileMode = 0o700
)

// ReferenceBytes is how much randomness a build's reference carries. It names
// the file on disk and travels in the download address, so it is wide enough
// that one cannot be guessed from another.
const ReferenceBytes = 8

// ErrNotReady reports that there is nothing to build from: no prebuilt client
// where this server was told to look, or one it cannot stamp.
var ErrNotReady = errors.New("clientbuild: there is no prebuilt client to build from")

// Request is one press of the build button.
type Request struct {
	// Address is the server the built client will talk to.
	Address string
	// Signing is the route the operator chose.
	Signing Signing
	// Program is what Windows calls this client in the dialog it shows a
	// driver who runs it. It is the operator's own organisation, so that a
	// driver in two teams can tell whose client they are about to install.
	// Empty leaves the field out, which Windows renders as "unknown".
	Program string
}

// Artifact is a built client: the file, and everything the page says about it.
type Artifact struct {
	// Reference identifies this build. It is the file's name on disk and the
	// last part of its download address.
	Reference string
	// Name is what the file is called when a driver downloads it.
	Name string
	// Path is where it is on this server.
	Path string
	// Size is the built file's size. Stamping does not change it; signing
	// does, by the length of the signature put on the end.
	Size int64
	// SHA256 is the hex digest of the built file, which is what an operator
	// tells their drivers to check.
	SHA256 string
	// Address is read back out of the file after it was written, so the page
	// shows what the client will actually do rather than what was asked for.
	Address string
	// Signing is the route it was built down.
	Signing Signing
	// SignedBy is the certificate that signed it, for the page to show and the
	// history to record. Empty for an unsigned build.
	SignedBy string
	// Client is the prebuilt client it was stamped from.
	Client Client
	// At is when it was built.
	At time.Time
}

// Builder makes clients out of the prebuilt one.
type Builder struct {
	// Source is the prebuilt client.
	Source Source
	// Dir is where built clients are written. Empty means this server cannot
	// keep one, which [Builder.Build] refuses rather than building into
	// nowhere.
	Dir string
	// Now is the clock, injectable for tests.
	Now func() time.Time
	// Certificate is the operator's own code-signing certificate, or nil when
	// they have not given this server one.
	//
	// It is asked for at the moment a client is built rather than held, for
	// two reasons. An operator uploads and removes a certificate while the
	// server runs, so a captured value would go stale; and the private key is
	// sealed in the database, so holding it would mean holding an opened
	// private key in memory for the life of the process to serve a button
	// somebody presses once a month.
	Certificate func(context.Context) (*Certificate, error)
}

// ErrNoCertificate reports that a signed client was asked for on a server with
// no certificate to sign it with.
var ErrNoCertificate = errors.New("clientbuild: this server has no certificate to sign with")

// Ready reports whether pressing build would work.
func (b Builder) Ready() bool { return b.Dir != "" && b.Source.Inspect().Ready() }

// Build copies the prebuilt client, writes the address into the copy, and
// checks the result before it is offered to anyone.
//
// The check is the point of doing it this way round. The copy is verified to
// be the same size as the original, byte-identical outside the region, still a
// PE executable, and to read back the address that was asked for. A file that
// fails any of those is not written, because the next thing that happens to it
// is that a team's drivers install it.
func (b Builder) Build(ctx context.Context, req Request) (Artifact, error) {
	now := time.Now
	if b.Now != nil {
		now = b.Now
	}
	if b.Dir == "" {
		return Artifact{}, fmt.Errorf("%w: this server has nowhere to keep one", ErrNotReady)
	}
	if req.Address == "" {
		return Artifact{}, errors.New("clientbuild: a client needs a server address to be built for")
	}
	// The certificate is fetched before anything is built, so that a server
	// that cannot sign says so instead of producing a file and failing at the
	// last step with a stamped copy already on the disk.
	certificate, err := b.certificate(ctx, req.Signing)
	if err != nil {
		return Artifact{}, err
	}

	client, original := b.Source.load()
	if !client.Ready() {
		if client.Problem != "" {
			return Artifact{}, fmt.Errorf("%w: %s", ErrNotReady, client.Problem)
		}
		return Artifact{}, ErrNotReady
	}

	reference, err := newReference()
	if err != nil {
		return Artifact{}, err
	}

	stamped := bytes.Clone(original)
	if err := Write(stamped, Stamp{Address: req.Address, Build: reference}); err != nil {
		return Artifact{}, err
	}
	if err := verify(original, stamped, req.Address); err != nil {
		return Artifact{}, err
	}

	// Signing comes last and changes the file, which is the whole reason the
	// checks above run against the stamped copy rather than this one: every
	// edit to a signed Windows executable breaks its signature, so there is
	// nothing this server may do to the bytes after this line.
	finished := stamped
	name := filepath.Base(b.Source.Path)
	art := Artifact{
		Reference: reference,
		Name:      name,
		Path:      filepath.Join(b.Dir, reference+filepath.Ext(name)),
		Address:   req.Address,
		Signing:   req.Signing,
		Client:    client,
		At:        now(),
	}
	if certificate != nil {
		if finished, err = Sign(stamped, *certificate, Opus{Program: req.Program, URL: req.Address}); err != nil {
			return Artifact{}, err
		}
		art.SignedBy = certificate.Describe().Subject
	}
	art.Size = int64(len(finished))
	sum := sha256.Sum256(finished)
	art.SHA256 = hex.EncodeToString(sum[:])

	if err := b.write(art.Path, finished); err != nil {
		return Artifact{}, err
	}
	return art, nil
}

// certificate is the one to sign this build with, or nil for a build that is
// not signed. A route that needs a certificate on a server that has none is
// refused here rather than further down.
func (b Builder) certificate(ctx context.Context, route Signing) (*Certificate, error) {
	if route != SigningOwnCertificate {
		if !route.Valid() {
			return nil, errors.New("clientbuild: " + route.Unavailable(false))
		}
		return nil, nil
	}
	if b.Certificate == nil {
		return nil, ErrNoCertificate
	}
	cert, err := b.Certificate(ctx)
	if err != nil {
		return nil, err
	}
	if cert == nil {
		return nil, ErrNoCertificate
	}
	return cert, nil
}

// verify is the check that stands between a stamped copy and a driver.
func verify(original, stamped []byte, address string) error {
	if len(original) != len(stamped) {
		return fmt.Errorf("clientbuild: stamping changed the file's size, from %d to %d bytes",
			len(original), len(stamped))
	}
	at, err := Find(stamped)
	if err != nil {
		return err
	}
	if !bytes.Equal(original[:at], stamped[:at]) ||
		!bytes.Equal(original[at+RegionSize:], stamped[at+RegionSize:]) {
		return errors.New("clientbuild: stamping changed a byte outside the reserved region")
	}
	if _, machineErr := peMachine(stamped); machineErr != nil {
		return fmt.Errorf("clientbuild: the stamped copy is not a valid Windows executable: %w", machineErr)
	}
	read, err := Read(stamped)
	if err != nil {
		return fmt.Errorf("clientbuild: the stamped copy does not read back: %w", err)
	}
	if read.Address != address {
		return fmt.Errorf("clientbuild: the stamped copy points at %q rather than %q", read.Address, address)
	}
	return nil
}

// write puts the built client on the disk, through a temporary file and a
// rename so that a download can never catch a half-written one.
func (b Builder) write(path string, body []byte) error {
	if err := os.MkdirAll(b.Dir, artifactDir); err != nil {
		return fmt.Errorf("clientbuild: %s could not be created: %w", b.Dir, err)
	}
	tmp, err := os.CreateTemp(b.Dir, "build.*.tmp")
	if err != nil {
		return fmt.Errorf("clientbuild: a temporary file could not be created in %s: %w", b.Dir, err)
	}
	name := tmp.Name()
	// A no-op once the rename has succeeded, and the cleanup if it has not.
	defer func() { _ = os.Remove(name) }()

	err = errors.Join(tmp.Chmod(artifactMode), writeAll(tmp, body), tmp.Sync(), tmp.Close())
	if err != nil {
		return fmt.Errorf("clientbuild: %s could not be written: %w", name, err)
	}
	if err := os.Rename(name, path); err != nil {
		return fmt.Errorf("clientbuild: %s could not be moved into place: %w", name, err)
	}
	return nil
}

func writeAll(f *os.File, body []byte) error {
	_, err := f.Write(body)
	return err
}

// Open returns the built client with this reference, for the download route. A
// reference that is not one this package minted is refused before it reaches
// the file system, so the address bar cannot be used to read another file.
func (b Builder) Open(reference string) (*os.File, error) {
	path, err := b.PathFor(reference)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(path) //nolint:gosec // G304: PathFor accepts only a hex reference this package minted, joined onto the builds directory.
	if err != nil {
		return nil, fmt.Errorf("clientbuild: that build could not be opened: %w", err)
	}
	return f, nil
}

// Exists reports whether the file for this build is still on the server. The
// history outlives the files — an operator can clear the directory, and a
// build made on a machine that has since been rebuilt is a row with nothing
// behind it — so the page asks rather than offering a link into nothing.
func (b Builder) Exists(reference string) bool {
	path, err := b.PathFor(reference)
	if err != nil {
		return false
	}
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

// PathFor is where the build with this reference is kept.
func (b Builder) PathFor(reference string) (string, error) {
	if b.Dir == "" {
		return "", ErrNotReady
	}
	if !ValidReference(reference) {
		return "", fmt.Errorf("clientbuild: %q is not a build reference", reference)
	}
	return filepath.Join(b.Dir, reference+filepath.Ext(filepath.Base(b.Source.Path))), nil
}

// ValidReference reports whether a string is shaped like a reference this
// package mints: hexadecimal, and exactly as long as one.
func ValidReference(s string) bool {
	if len(s) != 2*ReferenceBytes {
		return false
	}
	for _, c := range s {
		if !strings.ContainsRune("0123456789abcdef", c) {
			return false
		}
	}
	return true
}

func newReference() (string, error) {
	b := make([]byte, ReferenceBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("clientbuild: this machine would not give a random name to the build: %w", err)
	}
	return hex.EncodeToString(b), nil
}
