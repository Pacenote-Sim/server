package clientbuild

import (
	"bytes"
	"crypto/sha256"
	"debug/buildinfo"
	"debug/pe"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"time"
)

// MaxClientBytes is the largest prebuilt client this server will read into
// memory. The real one is a little over ten megabytes; the limit is here so
// that a path pointing at something enormous by mistake is an error message
// rather than a server that stops answering.
const MaxClientBytes = 256 << 20

// Client is everything the build page says about the prebuilt client: where it
// came from, which build it is, and whether it can be stamped.
//
// It is a value that always renders. A server with no client binary yet is the
// normal state of a fresh installation, and the page has to be able to say so
// plainly — so "not there" is a field and not an error.
type Client struct {
	// Path is where this server looked.
	Path string
	// Present is whether there was a file there.
	Present bool
	// Size and ModifiedAt are the file's own facts.
	Size       int64
	ModifiedAt time.Time
	// SHA256 is the hex digest of the prebuilt file, so an operator can check
	// that what they copied onto the server is what they downloaded.
	SHA256 string
	// Module, Version, Revision and BuiltAt come from the build information
	// the Go toolchain records inside every binary it links. They are how the
	// page answers "which version is this" without anybody maintaining a
	// version file beside the executable.
	Module   string
	Version  string
	Revision string
	BuiltAt  time.Time
	// GOOS and GOARCH are what the client was built for, read from the same
	// place. A macOS build sitting in the Windows client's path is a mistake
	// worth naming before an operator hands it to a driver.
	GOOS, GOARCH string
	// Machine is the processor the PE header names, in words.
	Machine string
	// Stampable is whether this server can build clients from this file.
	Stampable bool
	// StampedWith is the address the file already carries, which a prebuilt
	// client normally does not. It is shown rather than hidden: a generic
	// client that already points somewhere is worth knowing about.
	StampedWith string
	// Problem is why it cannot be used, in the words the operator needs.
	// Empty when there is nothing wrong.
	Problem string
}

// Source is the prebuilt Windows client this server stamps copies of.
type Source struct {
	// Path is the file. Empty means this server has not been told where the
	// client is, which the page says rather than guessing.
	Path string
}

// Inspect reads the prebuilt client and reports what it is.
//
// It never returns an error. Every way this can go wrong is something the
// operator has to fix in the file system — a missing file, the wrong file, a
// file this server may not read — and each of those is a sentence on the page
// rather than a stack trace in a log.
//
// It reads the whole file and digests it, which is tens of milliseconds on the
// eleven megabytes the real client weighs. That is a page an operator opens
// rarely, and the digest is the reason they opened it: it is how they check
// that what they copied onto the server is what they downloaded.
func (s Source) Inspect() Client {
	c, _ := s.load()
	return c
}

// load is [Source.Inspect] with the bytes it read, for the caller that is
// about to stamp a copy of them and should not read eleven megabytes twice to
// do it.
func (s Source) load() (Client, []byte) {
	c := Client{Path: s.Path}
	if s.Path == "" {
		c.Problem = "This server has not been told where the prebuilt client is."
		return c, nil
	}

	info, err := os.Stat(s.Path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return c, nil
	case err != nil:
		c.Problem = "That file could not be read — " + err.Error() + "."
		return c, nil
	case info.IsDir():
		c.Present = true
		c.Problem = "That path is a directory, not the client executable."
		return c, nil
	case info.Size() > MaxClientBytes:
		c.Present = true
		c.Size = info.Size()
		c.ModifiedAt = info.ModTime()
		c.Problem = fmt.Sprintf("That file is %d bytes, which is far larger than a client — check the path.", info.Size())
		return c, nil
	}

	c.Present = true
	c.Size = info.Size()
	c.ModifiedAt = info.ModTime()

	// The path is the operator's own configuration, and reading the file there
	// is this function's whole job.
	body, err := os.ReadFile(s.Path)
	if err != nil {
		c.Problem = "That file could not be read — " + err.Error() + "."
		return c, nil
	}
	c.describe(body)
	return c, body
}

// describe fills in everything that is read out of the bytes themselves.
func (c *Client) describe(body []byte) {
	sum := sha256.Sum256(body)
	c.SHA256 = hex.EncodeToString(sum[:])

	if info, err := buildinfo.Read(bytes.NewReader(body)); err == nil {
		c.Module = info.Main.Path
		c.Version = info.Main.Version
		for _, setting := range info.Settings {
			switch setting.Key {
			case "GOOS":
				c.GOOS = setting.Value
			case "GOARCH":
				c.GOARCH = setting.Value
			case "vcs.revision":
				c.Revision = setting.Value
			case "vcs.time":
				if at, err := time.Parse(time.RFC3339, setting.Value); err == nil {
					c.BuiltAt = at
				}
			}
		}
	}

	machine, err := peMachine(body)
	if err != nil {
		c.Problem = "That file is not a Windows executable — " + err.Error() + "."
		return
	}
	c.Machine = machine

	switch _, err := Find(body); {
	case errors.Is(err, ErrNoRegion):
		c.Problem = "That client has no reserved region, so there is nowhere to write an address — it was built before this server could stamp one."
		return
	case errors.Is(err, ErrManyRegions):
		c.Problem = "That client has more than one reserved region, which this server will not guess between."
		return
	case err != nil:
		c.Problem = "That client could not be read — " + err.Error() + "."
		return
	}

	if stamped, err := Read(body); err == nil {
		c.StampedWith = stamped.Address
	}
	c.Stampable = true
}

// Ready reports whether a client can be built from this file right now.
func (c Client) Ready() bool { return c.Present && c.Stampable }

// Describes reports whether the Go toolchain's build information was readable,
// which is what the version on the page comes from.
func (c Client) Describes() bool { return c.Version != "" || c.Revision != "" }

// ShortRevision is the first twelve characters of the commit, which is what a
// person compares.
func (c Client) ShortRevision() string {
	if len(c.Revision) > 12 {
		return c.Revision[:12]
	}
	return c.Revision
}

// ShortSum is the head of the digest, for the places where the whole of it
// would push everything else off the line.
func (c Client) ShortSum() string {
	if len(c.SHA256) > 16 {
		return c.SHA256[:16]
	}
	return c.SHA256
}

// peMachine reports the processor a PE file was built for, and refuses a file
// that is not one. It is how "still a valid Windows executable" is checked
// rather than assumed, both of the prebuilt client and of every copy stamped
// from it.
func peMachine(body []byte) (string, error) {
	f, err := pe.NewFile(bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()

	switch f.Machine {
	case pe.IMAGE_FILE_MACHINE_AMD64:
		return "x86-64", nil
	case pe.IMAGE_FILE_MACHINE_ARM64:
		return "arm64", nil
	case pe.IMAGE_FILE_MACHINE_I386:
		return "x86", nil
	default:
		return fmt.Sprintf("machine 0x%x", f.Machine), nil
	}
}
