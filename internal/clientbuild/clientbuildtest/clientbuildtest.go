// Package clientbuildtest builds the fixtures the client-building tests need:
// a file shaped like a Windows executable, with a reserved region inside it.
//
// It is a fixture and not a real client on purpose. A real one is eleven
// megabytes of another repository's build output, which no test in this one
// can produce; this is a few kilobytes, it is deterministic, a test can put
// the region anywhere inside it, and debug/pe parses it. The real article is
// exercised end to end against a client built by wails3.
//
// The layout below is spelled out from the format in the package comment of
// clientbuild rather than imported from it. A fixture that reused the code
// under test could only ever prove that code agrees with itself, and the
// client at the other end of this format has its own, separate implementation
// of exactly these offsets.
package clientbuildtest

import (
	"bytes"
	"debug/pe"
	"encoding/binary"
	"hash/crc32"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// The region, as the format describes it.
const (
	// RegionSize is the whole reserved region.
	RegionSize = 1024
	// MarkerSize is the length of each marker.
	MarkerSize = 16
	// OffVersion, OffReserved, OffLength, OffChecksum and OffPayload are the
	// header fields between the head marker and the payload.
	OffVersion  = 16
	OffReserved = 17
	OffLength   = 18
	OffChecksum = 20
	OffPayload  = 24
	// OffTail is where the tail marker starts.
	OffTail = RegionSize - MarkerSize
	// Version1 is the format version a stamped region carries.
	Version1 = 1
)

// Head is the marker that opens the region.
func Head() []byte { return []byte("PACENOTESTAMP_HD") }

// Tail is the marker that closes it.
func Tail() []byte { return []byte("PACENOTESTAMP_TL") }

// BlankRegion is the region as a prebuilt client carries it: the two markers,
// and room between them.
func BlankRegion() []byte {
	b := make([]byte, RegionSize)
	copy(b, Head())
	copy(b[OffTail:], Tail())
	return b
}

// StampedRegion is a region with a payload written into it by hand, which is
// how the tests build the cases that are wrong in one specific way: take this,
// and break one field of it.
func StampedRegion(payload string) []byte {
	b := BlankRegion()
	b[OffVersion] = Version1
	//nolint:gosec // G115: every payload a test writes is far inside a uint16.
	binary.BigEndian.PutUint16(b[OffLength:], uint16(len(payload)))
	binary.BigEndian.PutUint32(b[OffChecksum:], crc32.ChecksumIEEE([]byte(payload)))
	copy(b[OffPayload:], payload)
	return b
}

// The optional header fields this fixture fills in, as offsets into it. They
// are listed because two of them are one field apart from each other and the
// difference does not show: a fixture that put FileAlignment where
// SectionAlignment goes still parsed, still stamped, and was rejected by every
// real signature verifier, because a section whose size on disk is not a
// multiple of the alignment it declares is not a PE anybody will load.
const (
	offEntryPoint          = 16
	offSectionAlignment    = 32
	offFileAlignment       = 36
	offSizeOfImage         = 56
	offSizeOfHeaders       = 60
	offNumberOfRvaAndSizes = 108
)

// FileAlignment is what a section's bytes on disk are rounded up to. It is the
// smallest value a PE may declare, which keeps the fixture small.
const FileAlignment = 0x200

// PE wraps bytes in the smallest thing that is a valid Windows executable: a
// DOS header, a PE header, one section, and the bytes as that section's data,
// padded out to the alignment the header declares.
func PE(body []byte) []byte {
	const (
		lfanew     = 0x80
		sectionsAt = lfanew + 4 + 20 + 240
		rawAt      = 0x400
	)
	// A section's bytes on disk run to a multiple of the file alignment. A
	// file that says otherwise is one a verifier reads past the end of.
	raw := (len(body) + FileAlignment - 1) / FileAlignment * FileAlignment
	out := make([]byte, rawAt+raw)
	copy(out, "MZ")
	binary.LittleEndian.PutUint32(out[0x3c:], lfanew)
	copy(out[lfanew:], "PE\x00\x00")

	fh := out[lfanew+4:]
	binary.LittleEndian.PutUint16(fh[0:], pe.IMAGE_FILE_MACHINE_AMD64)
	binary.LittleEndian.PutUint16(fh[2:], 1)    // one section
	binary.LittleEndian.PutUint16(fh[16:], 240) // the PE32+ optional header
	binary.LittleEndian.PutUint16(fh[18:], 0x0022)

	oh := out[lfanew+4+20:]
	binary.LittleEndian.PutUint16(oh[0:], 0x20b) // PE32+
	binary.LittleEndian.PutUint32(oh[offEntryPoint:], 0x1000)
	binary.LittleEndian.PutUint32(oh[offSectionAlignment:], 0x1000)
	binary.LittleEndian.PutUint32(oh[offFileAlignment:], FileAlignment)
	//nolint:gosec // G115: a fixture is kilobytes; nothing here approaches a uint32.
	binary.LittleEndian.PutUint32(oh[offSizeOfImage:], uint32(len(out)+0x1000))
	binary.LittleEndian.PutUint32(oh[offSizeOfHeaders:], rawAt)
	binary.LittleEndian.PutUint32(oh[offNumberOfRvaAndSizes:], 16)

	sh := out[sectionsAt:]
	copy(sh[0:], ".data\x00\x00\x00")
	//nolint:gosec // G115: as above.
	binary.LittleEndian.PutUint32(sh[8:], uint32(len(body)))
	binary.LittleEndian.PutUint32(sh[12:], 0x1000)
	//nolint:gosec // G115: as above.
	binary.LittleEndian.PutUint32(sh[16:], uint32(raw))
	binary.LittleEndian.PutUint32(sh[20:], rawAt)
	binary.LittleEndian.PutUint32(sh[36:], 0xC0000040)

	copy(out[rawAt:], body)
	return out
}

// Client is a fixture client: filler, a region, more filler, all inside a PE.
// The filler is there so that a test asserting "nothing outside the region
// changed" has something outside the region to assert about.
func Client(region []byte) []byte {
	var body bytes.Buffer
	body.Write(bytes.Repeat([]byte{0xAB}, 3000))
	body.Write(region)
	body.Write(bytes.Repeat([]byte{0xCD}, 1500))
	return PE(body.Bytes())
}

// WriteAt puts a fixture on the disk and returns its path.
func WriteAt(tb testing.TB, path string, body []byte) string {
	tb.Helper()
	require.NoError(tb, os.MkdirAll(filepath.Dir(path), 0o700))
	require.NoError(tb, os.WriteFile(path, body, 0o600))
	return path
}

// Install writes a stampable client into a directory of its own and returns
// its path, which is what a test wanting "a server that has a client" needs.
func Install(tb testing.TB) string {
	tb.Helper()
	return WriteAt(tb, filepath.Join(tb.TempDir(), "pacenote-telemetry.exe"), Client(BlankRegion()))
}

// MultiSectionPE is a fixture shaped more like a real client than [PE] is:
// several sections, and debug data after the last of them that belongs to no
// section at all.
//
// Both of those are places the Authenticode hash can go wrong in a way one
// section cannot show: the sections have to be hashed one after another with
// their alignment padding, and the bytes after the last of them have to be
// hashed too — leaving them out would let somebody append to a signed file
// without breaking the signature.
//
// The section table is written in file order, which is what every linker does.
// [ShuffleSectionTable] makes one that is not, for the test that the hash
// follows the file rather than the table.
func MultiSectionPE(region []byte) []byte {
	const (
		lfanew     = 0x80
		sectionsAt = lfanew + 4 + 20 + 240
		rawAt      = 0x400
		sections   = 3
	)
	bodies := [sections][]byte{
		bytes.Repeat([]byte{0xAB}, 2*FileAlignment),
		region,
		bytes.Repeat([]byte{0xCD}, FileAlignment),
	}
	// The bytes after the last section: a linker's debug directory lands here
	// and nothing owns it.
	trailer := bytes.Repeat([]byte{0xEE}, 300)

	total := rawAt
	starts := [sections]int{}
	for i, b := range bodies {
		starts[i] = total
		total += (len(b) + FileAlignment - 1) / FileAlignment * FileAlignment
	}
	out := make([]byte, total+len(trailer))
	copy(out[total:], trailer)

	copy(out, "MZ")
	binary.LittleEndian.PutUint32(out[0x3c:], lfanew)
	copy(out[lfanew:], "PE\x00\x00")

	fh := out[lfanew+4:]
	binary.LittleEndian.PutUint16(fh[0:], pe.IMAGE_FILE_MACHINE_AMD64)
	binary.LittleEndian.PutUint16(fh[2:], sections)
	binary.LittleEndian.PutUint16(fh[16:], 240)
	binary.LittleEndian.PutUint16(fh[18:], 0x0022)

	oh := out[lfanew+4+20:]
	binary.LittleEndian.PutUint16(oh[0:], 0x20b)
	binary.LittleEndian.PutUint32(oh[offEntryPoint:], 0x1000)
	binary.LittleEndian.PutUint32(oh[offSectionAlignment:], 0x1000)
	binary.LittleEndian.PutUint32(oh[offFileAlignment:], FileAlignment)
	//nolint:gosec // G115: a fixture is kilobytes; nothing here approaches a uint32.
	binary.LittleEndian.PutUint32(oh[offSizeOfImage:], uint32(len(out)+0x1000))
	binary.LittleEndian.PutUint32(oh[offSizeOfHeaders:], rawAt)
	binary.LittleEndian.PutUint32(oh[offNumberOfRvaAndSizes:], 16)

	names := [sections]string{".text\x00\x00\x00", ".rdata\x00\x00", ".data\x00\x00\x00"}
	//nolint:gosec // G115: a fixture is kilobytes; no size or address here approaches a uint32.
	for row := range sections {
		i := row
		sh := out[sectionsAt+row*40:]
		copy(sh[0:], names[row])
		binary.LittleEndian.PutUint32(sh[8:], uint32(len(bodies[i])))
		binary.LittleEndian.PutUint32(sh[12:], uint32(0x1000*(i+1)))
		binary.LittleEndian.PutUint32(sh[16:], uint32((len(bodies[i])+FileAlignment-1)/FileAlignment*FileAlignment))
		binary.LittleEndian.PutUint32(sh[20:], uint32(starts[i]))
		binary.LittleEndian.PutUint32(sh[36:], 0xC0000040)
		copy(out[starts[i]:], bodies[i])
	}
	return out
}

// ShuffleSectionTable reverses the order of the rows in a PE's section table,
// leaving every other byte of the file alone.
//
// The file it returns is the same program: the sections are where they were and
// hold what they held, and only the order they are listed in has changed. The
// Authenticode hash is defined over the sections in the order they sit in the
// file, so a signature over this file and a signature over the original must be
// the same signature. A hash that read the table in order instead would produce
// a different one, and that is what the test using this catches.
func ShuffleSectionTable(body []byte) []byte {
	out := bytes.Clone(body)
	peAt := int(binary.LittleEndian.Uint32(out[0x3c:]))
	count := int(binary.LittleEndian.Uint16(out[peAt+4+2:]))
	tableAt := peAt + 24 + int(binary.LittleEndian.Uint16(out[peAt+4+16:]))

	rows := make([][]byte, count)
	for i := range rows {
		rows[i] = bytes.Clone(out[tableAt+i*40 : tableAt+(i+1)*40])
	}
	for i, row := range rows {
		copy(out[tableAt+(count-1-i)*40:], row)
	}
	return out
}
