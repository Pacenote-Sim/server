// Package packaging describes what a release package contains.
//
// It exists so that the answer lives in one place instead of three. The same
// list has to be true of .goreleaser.yml, of the zip an operator downloads and
// of the README that tells them what is in it, and the tests beside this file
// check all three against what is written here. Nothing in the server imports
// it: the binary does not read its own packaging.
//
// The names are the contract an operator sees, so changing one is a change to
// the product, not a refactor. Someone has a script that fetches
// pacenote-ce_1.2.0_linux_amd64.zip.
package packaging

import (
	"fmt"
	"path"
	"slices"
	"strings"
)

// Project is the name every archive starts with. It is not the binary's name —
// "pacenote-ce" says which edition the download is, and pacenote-server says
// which program is inside it.
const Project = "pacenote-ce"

// Binary is the program inside the archive, without the .exe that Windows
// wants. See [BinaryName].
const Binary = "pacenote-server"

// Checksums is the single file covering every archive in a release.
const Checksums = "checksums.txt"

// Platform is one build target.
type Platform struct {
	// OS is a GOOS: linux, darwin or windows.
	OS string
	// Arch is a GOARCH: amd64 or arm64.
	Arch string
}

// String is "linux/amd64", the spelling the Makefile and the docs use.
func (p Platform) String() string { return p.OS + "/" + p.Arch }

// Platforms is every target a release ships, and therefore every target that
// is supported. Adding one here is a promise to keep it building.
//
// windows/arm64 is absent on purpose: nobody runs a team server on it, and a
// binary nobody runs is a binary nobody notices breaking.
func Platforms() []Platform {
	return []Platform{
		{OS: "linux", Arch: "amd64"},
		{OS: "linux", Arch: "arm64"},
		{OS: "darwin", Arch: "amd64"},
		{OS: "darwin", Arch: "arm64"},
		{OS: "windows", Arch: "amd64"},
	}
}

// Docs is every file an operator reads, in the order they need them: what this
// is and how to run it, then the terms, then what changed. They sit beside the
// binary rather than in a docs directory, because a directory is one more thing
// to open.
//
// The value is the path in this repository the file is taken from; the key is
// where it lands in the archive.
func Docs() map[string]string {
	return map[string]string{
		"README.md":          "packaging/README.md",
		"docker-compose.yml": "packaging/docker-compose.yml",
		"LICENSE":            "LICENSE",
		"CHANGELOG.md":       "CHANGELOG.md",
	}
}

// BinaryName is the binary's file name on one platform.
func BinaryName(goos string) string {
	if goos == "windows" {
		return Binary + ".exe"
	}
	return Binary
}

// ArchiveName is the file an operator downloads:
//
//	pacenote-ce_0.1.0_linux_amd64.zip
//
// The version carries no leading v, because that is what GoReleaser's .Version
// interpolates and a name that disagrees with the tool that writes it is a name
// that will drift.
func ArchiveName(version string, p Platform) string {
	return fmt.Sprintf("%s_%s_%s_%s.zip", Project, strings.TrimPrefix(version, "v"), p.OS, p.Arch)
}

// Contents is every entry inside one platform's archive, sorted, which is what
// a test compares a real zip against.
func Contents(p Platform) []string {
	names := make([]string, 0, len(Docs())+1)
	for name := range Docs() {
		names = append(names, name)
	}
	names = append(names, BinaryName(p.OS))
	slices.Sort(names)
	return names
}

// Sources is every path in this repository a release archive draws from, so a
// test can check that each one still exists before a tag discovers it does not.
func Sources() []string {
	srcs := make([]string, 0, len(Docs()))
	for _, src := range Docs() {
		srcs = append(srcs, path.Clean(src))
	}
	slices.Sort(srcs)
	return srcs
}
