package clientbuild

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"strings"
	"unicode/utf8"
)

// The shape of the region. Every offset here is also written down in the
// client that reads it (github.com/pacenote-sim/telemetry/internal/stamp); the two halves are
// separate programs in separate repositories, so this is a contract and not an
// implementation detail. Changing a number here means changing it there and
// giving the format a new version.
const (
	// RegionSize is the whole reserved region, markers included.
	RegionSize = 1024
	// MarkerSize is the length of each marker.
	MarkerSize = 16
	// HeaderSize is what sits between the head marker and the payload: the
	// version, one reserved byte, the length and the checksum.
	HeaderSize = 8
	// PayloadMax is how many bytes of payload the region has room for.
	PayloadMax = RegionSize - 2*MarkerSize - HeaderSize
)

// The offsets of the header fields, from the start of the region.
const (
	offVersion  = MarkerSize
	offReserved = offVersion + 1
	offLength   = offReserved + 1
	offChecksum = offLength + 2
	offPayload  = MarkerSize + HeaderSize
	offTail     = RegionSize - MarkerSize
)

// The format versions.
const (
	// VersionBlank is the region of a prebuilt client: room reserved, nothing
	// written into it.
	VersionBlank = 0
	// Version1 is the key=value payload this build writes and the shipped
	// client reads.
	Version1 = 1
)

// The payload keys of [Version1].
const (
	// KeyAddress is the server address the client talks to.
	KeyAddress = "url"
	// KeyBuild identifies the build that wrote the stamp, so a driver's
	// support question can be traced back to a row in the build history.
	KeyBuild = "build"
)

// The markers, assembled at run time from halves.
//
// A whole marker written as one constant would be one constant in this
// server's own binary too. That does not matter here — this server scans a
// client and never itself — but the client reads its own region, and keeping
// the two halves apart on both sides means the rule is the same rule in both
// repositories rather than a subtlety somebody has to rediscover.
// The halves spell HD and TL rather than HEAD and TAIL because a marker is
// exactly [MarkerSize] bytes: "PACENOTE" is one byte longer than the name this
// format was written under, and the suffix gives that byte up rather than the
// region changing shape and the format needing a new version.
var (
	markerPrefix = "PACENOTE"
	headMarker   = []byte(markerPrefix + "STAMP_HD")
	tailMarker   = []byte(markerPrefix + "STAMP_TL")
)

// The errors a caller acts on.
var (
	// ErrNoRegion reports that a file carries no reserved region. It is what a
	// client built before stamping existed looks like, and the panel says so
	// rather than writing into a file it does not recognise.
	ErrNoRegion = errors.New("clientbuild: this binary has no reserved region")
	// ErrManyRegions reports that a file carries more than one. Nothing should
	// produce such a file, and the safe thing to do with one is refuse it.
	ErrManyRegions = errors.New("clientbuild: this binary has more than one reserved region")
	// ErrBlank reports that a region has never been stamped.
	ErrBlank = errors.New("clientbuild: this binary carries no stamp")
	// ErrCorrupt reports a region that does not check out.
	ErrCorrupt = errors.New("clientbuild: the stamp in this binary is not readable")
	// ErrTooLong reports a payload with no room in the region.
	ErrTooLong = errors.New("clientbuild: the stamp does not fit in the reserved region")
)

// Stamp is what is written into the region, and what reads back out of it.
type Stamp struct {
	// Address is the server the built client talks to: a scheme and a host,
	// with no trailing slash.
	Address string
	// Build identifies the build this copy came from, or is empty.
	Build string
}

// Payload renders the stamp as the bytes the region carries.
func (s Stamp) Payload() []byte {
	var b strings.Builder
	b.WriteString(KeyAddress + "=" + s.Address)
	if s.Build != "" {
		b.WriteString("\n" + KeyBuild + "=" + s.Build)
	}
	return []byte(b.String())
}

// Find returns the offset of the reserved region in a binary.
//
// A candidate is a head marker with a tail marker exactly [RegionSize]-16
// bytes after it. Anything else that happens to hold the head marker — a
// string in the client's own code, a byte sequence inside a compressed asset —
// is not a region and is passed over.
func Find(binaryFile []byte) (int, error) {
	found := -1
	for at := 0; ; {
		i := bytes.Index(binaryFile[at:], headMarker)
		if i < 0 {
			break
		}
		start := at + i
		at = start + 1
		if start+RegionSize > len(binaryFile) {
			continue
		}
		if !bytes.Equal(binaryFile[start+offTail:start+RegionSize], tailMarker) {
			continue
		}
		if found >= 0 {
			return 0, ErrManyRegions
		}
		found = start
	}
	if found < 0 {
		return 0, ErrNoRegion
	}
	return found, nil
}

// Write stamps a binary in place.
//
// It writes [RegionSize] bytes and nothing else: the file that comes back is
// the same length, the same layout and byte-identical everywhere outside the
// region. The caller owns the slice, which is a copy of the prebuilt client
// and never the prebuilt client itself.
func Write(binaryFile []byte, s Stamp) error {
	if s.Address == "" {
		return errors.New("clientbuild: a stamp with no server address is not a stamp")
	}
	payload := s.Payload()
	if len(payload) > PayloadMax {
		return fmt.Errorf("%w: it is %d bytes and there is room for %d",
			ErrTooLong, len(payload), PayloadMax)
	}
	at, err := Find(binaryFile)
	if err != nil {
		return err
	}

	region := binaryFile[at : at+RegionSize]
	// The payload area is cleared before it is filled, so a shorter stamp
	// cannot leave the tail of a longer one behind it. The length and the
	// checksum would refuse those bytes anyway; not carrying somebody's old
	// address around in a file drivers download is the better reason.
	clear(region[offPayload:offTail])
	region[offVersion] = Version1
	region[offReserved] = 0
	//nolint:gosec // G115: len(payload) is bounded by PayloadMax above, well inside a uint16.
	binary.BigEndian.PutUint16(region[offLength:offChecksum], uint16(len(payload)))
	binary.BigEndian.PutUint32(region[offChecksum:offPayload], crc32.ChecksumIEEE(payload))
	copy(region[offPayload:offTail], payload)
	return nil
}

// Read reads the stamp out of a binary: the same check the client itself makes
// at startup, so the panel can show an operator what a file they are about to
// hand out actually says.
func Read(binaryFile []byte) (Stamp, error) {
	at, err := Find(binaryFile)
	if err != nil {
		return Stamp{}, err
	}
	return ReadRegion(binaryFile[at : at+RegionSize])
}

// ReadRegion parses one region.
func ReadRegion(region []byte) (Stamp, error) {
	if len(region) != RegionSize {
		return Stamp{}, fmt.Errorf("%w: it is %d bytes rather than %d",
			ErrCorrupt, len(region), RegionSize)
	}
	if !bytes.Equal(region[:MarkerSize], headMarker) || !bytes.Equal(region[offTail:], tailMarker) {
		return Stamp{}, fmt.Errorf("%w: the markers are not where they should be", ErrCorrupt)
	}

	switch version := region[offVersion]; version {
	case VersionBlank:
		return Stamp{}, ErrBlank
	case Version1:
	default:
		return Stamp{}, fmt.Errorf("%w: it is version %d, and this build writes version %d",
			ErrCorrupt, version, Version1)
	}
	if region[offReserved] != 0 {
		return Stamp{}, fmt.Errorf("%w: the reserved byte is not zero", ErrCorrupt)
	}

	n := int(binary.BigEndian.Uint16(region[offLength:offChecksum]))
	if n > PayloadMax {
		return Stamp{}, fmt.Errorf("%w: it claims %d bytes of payload and there is room for %d",
			ErrCorrupt, n, PayloadMax)
	}
	payload := region[offPayload : offPayload+n]
	if want := binary.BigEndian.Uint32(region[offChecksum:offPayload]); crc32.ChecksumIEEE(payload) != want {
		return Stamp{}, fmt.Errorf("%w: the checksum does not match the payload", ErrCorrupt)
	}
	return fields(payload)
}

// fields reads the key=value lines of a version 1 payload. An unknown key is
// skipped, so a later server can add one without breaking a client that is
// already installed on a driver's machine.
func fields(payload []byte) (Stamp, error) {
	if !utf8.Valid(payload) {
		return Stamp{}, fmt.Errorf("%w: the payload is not text", ErrCorrupt)
	}
	var out Stamp
	for line := range strings.SplitSeq(string(payload), "\n") {
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		switch key {
		case KeyAddress:
			out.Address = value
		case KeyBuild:
			out.Build = value
		}
	}
	if out.Address == "" {
		return Stamp{}, fmt.Errorf("%w: it carries no server address", ErrCorrupt)
	}
	return out, nil
}
