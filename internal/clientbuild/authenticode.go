package clientbuild

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash"
	"sort"
)

// The Windows executable, as the signature has to see it.
//
// Authenticode does not hash the file. It hashes the file with three regions
// left out — the header checksum, the directory entry that points at the
// signature, and the signature itself — because all three change when the file
// is signed, and a hash that covered them could never match afterwards. Every
// byte outside those three is covered, which is what makes the signature a
// statement about the whole program.
//
// The layout below is read out of the file rather than assumed. A PE32 client
// and a PE32+ client put their data directories in different places, and a
// client built for arm64 is a different file again; guessing an offset here
// would produce a signature that Windows rejects on exactly the machines the
// operator did not test on.

// The certificate table is the fifth entry in the optional header's data
// directory, and it is the only one this package touches.
const certificateDirIndex = 4

// The WIN_CERTIFICATE header that wraps the signature at the end of the file:
// a length, a revision, a type, and then the PKCS#7.
const (
	// winCertHeaderSize is the four fields before the signature itself.
	winCertHeaderSize = 8
	// winCertRevision2 is WIN_CERT_REVISION_2_0, the only revision Windows
	// accepts for an Authenticode signature.
	winCertRevision2 uint16 = 0x0200
	// winCertTypePKCS is WIN_CERT_TYPE_PKCS_SIGNED_DATA.
	winCertTypePKCS uint16 = 0x0002
	// winCertAlign is the boundary the certificate table sits on. Windows
	// walks the table entry by entry using each one's length, and a length
	// that is not rounded up to this leaves the walk misaligned.
	winCertAlign = 8
)

// The two optional header shapes, and where each keeps the thing this package
// needs. The numbers are offsets into the optional header itself.
const (
	pe32Magic     = 0x10b
	pe32PlusMagic = 0x20b
	// offCheckSum is the header checksum, which changes when the file does.
	offCheckSum = 64
	// offSizeOfHeaders is where the headers stop and the sections start.
	offSizeOfHeaders = 60
	// offNumberOfRvaAndSizes32 and offNumberOfRvaAndSizes64 are where the
	// count of data directories sits in each shape. The directories follow it.
	offNumberOfRvaAndSizes32 = 92
	offNumberOfRvaAndSizes64 = 108
)

// sectionHeaderSize is one entry in the section table.
const sectionHeaderSize = 40

// ErrNotPE reports a file this package cannot read as a Windows executable. It
// is separate from the PE parsing [Source] does because signing needs offsets
// that debug/pe does not expose.
var ErrNotPE = errors.New("clientbuild: that is not a Windows executable this server can sign")

// peLayout is where everything the signature cares about lives in one file.
type peLayout struct {
	// checkSumAt is the four bytes of header checksum, left out of the hash.
	checkSumAt int
	// certDirAt is the eight bytes of directory entry pointing at the
	// signature, left out of the hash.
	certDirAt int
	// headersEnd is SizeOfHeaders: the hash covers the headers up to here and
	// then jumps to the sections.
	headersEnd int
	// sections is every section's bytes on disk, in the order they appear in
	// the file rather than in the section table. Authenticode hashes them in
	// file order, and a linker is free to write the table in another.
	sections []peSection
	// certAt and certBytes are the signature already on the file, if there is
	// one. A stamped copy has none — stamping breaks a signature, so anything
	// here was inherited from the prebuilt client and is about to be replaced.
	certAt, certBytes int
}

// peSection is one section's bytes on disk.
type peSection struct{ at, size int }

// readPE finds everything [peDigest] and [attach] need, and refuses a file
// whose headers do not hang together.
//
// Every bound is checked against the file's real length rather than trusted.
// The prebuilt client is a file an operator copied onto the server, so it is
// input: a header claiming a section runs past the end of the file must be an
// error here and not a panic three lines later.
func readPE(body []byte) (peLayout, error) {
	var l peLayout
	if len(body) < 0x40 || body[0] != 'M' || body[1] != 'Z' {
		return l, fmt.Errorf("%w: it does not start with a DOS header", ErrNotPE)
	}
	peAt := int(binary.LittleEndian.Uint32(body[0x3c:]))
	if peAt < 0 || peAt+24 > len(body) {
		return l, fmt.Errorf("%w: its DOS header points outside the file", ErrNotPE)
	}
	if string(body[peAt:peAt+4]) != "PE\x00\x00" {
		return l, fmt.Errorf("%w: there is no PE header where the DOS header points", ErrNotPE)
	}

	coff := body[peAt+4:]
	sections := int(binary.LittleEndian.Uint16(coff[2:]))
	optionalSize := int(binary.LittleEndian.Uint16(coff[16:]))
	optAt := peAt + 24
	if optionalSize < offNumberOfRvaAndSizes32+4 || optAt+optionalSize > len(body) {
		return l, fmt.Errorf("%w: its optional header runs past the end of the file", ErrNotPE)
	}

	var dirCountAt int
	switch magic := binary.LittleEndian.Uint16(body[optAt:]); magic {
	case pe32Magic:
		dirCountAt = offNumberOfRvaAndSizes32
	case pe32PlusMagic:
		dirCountAt = offNumberOfRvaAndSizes64
	default:
		return l, fmt.Errorf("%w: its optional header is marked 0x%x, which is neither PE32 nor PE32+", ErrNotPE, magic)
	}
	if dirCountAt+4 > optionalSize {
		return l, fmt.Errorf("%w: its optional header stops before the data directories", ErrNotPE)
	}

	directories := int(binary.LittleEndian.Uint32(body[optAt+dirCountAt:]))
	if directories <= certificateDirIndex {
		return l, fmt.Errorf("%w: it has %d data directories, so there is nowhere to record a signature",
			ErrNotPE, directories)
	}
	l.certDirAt = optAt + dirCountAt + 4 + certificateDirIndex*8
	if l.certDirAt+8 > optAt+optionalSize {
		return l, fmt.Errorf("%w: its certificate directory entry falls outside its optional header", ErrNotPE)
	}
	l.checkSumAt = optAt + offCheckSum

	l.headersEnd = int(binary.LittleEndian.Uint32(body[optAt+offSizeOfHeaders:]))
	if l.headersEnd < l.certDirAt+8 || l.headersEnd > len(body) {
		return l, fmt.Errorf("%w: it says its headers are %d bytes, which cannot be right", ErrNotPE, l.headersEnd)
	}

	tableAt := optAt + optionalSize
	for i := range sections {
		at := tableAt + i*sectionHeaderSize
		if at+sectionHeaderSize > len(body) {
			return l, fmt.Errorf("%w: its section table runs past the end of the file", ErrNotPE)
		}
		size := int(binary.LittleEndian.Uint32(body[at+16:]))
		start := int(binary.LittleEndian.Uint32(body[at+20:]))
		if size == 0 {
			// A section with no bytes on disk — .bss and its like. It is not
			// hashed because there is nothing of it in the file.
			continue
		}
		if start < 0 || size < 0 || start+size > len(body) {
			return l, fmt.Errorf("%w: section %d claims bytes past the end of the file", ErrNotPE, i+1)
		}
		l.sections = append(l.sections, peSection{at: start, size: size})
	}
	sort.Slice(l.sections, func(i, j int) bool { return l.sections[i].at < l.sections[j].at })

	at := int(binary.LittleEndian.Uint32(body[l.certDirAt:]))
	size := int(binary.LittleEndian.Uint32(body[l.certDirAt+4:]))
	if size > 0 {
		if at < l.headersEnd || at+size > len(body) {
			return l, fmt.Errorf("%w: it says it is signed, and the signature is not where it says", ErrNotPE)
		}
		l.certAt, l.certBytes = at, size
	}
	return l, nil
}

// signedEnd is where the program stops and the signature begins: the whole file
// when there is no signature on it, and everything before the certificate table
// when there is.
func (l peLayout) signedEnd(body []byte) int {
	if l.certBytes == 0 {
		return len(body)
	}
	return l.certAt
}

// peDigest writes the Authenticode hash of body into h.
//
// The order is the format's own and not the file's: the headers up to the
// checksum, the headers between the checksum and the certificate directory
// entry, the rest of the headers, then every section's bytes in file order,
// then whatever the linker left after the last section. Anything else produces
// a hash that no Windows machine will agree with.
func peDigest(body []byte, l peLayout, h hash.Hash) {
	end := l.signedEnd(body)
	h.Write(body[:l.checkSumAt])
	h.Write(body[l.checkSumAt+4 : l.certDirAt])
	h.Write(body[l.certDirAt+8 : l.headersEnd])

	hashed := l.headersEnd
	for _, s := range l.sections {
		h.Write(body[s.at : s.at+s.size])
		hashed += s.size
	}
	// Bytes after the last section and before the signature. Debug data and
	// alignment padding live here, and leaving them out would let somebody
	// append to a signed file without breaking the signature.
	if end > hashed {
		h.Write(body[hashed:end])
	}
}

// prepare is the file as it will be hashed and as it will be shipped, minus
// the signature: any signature it already carried cut off, and its length
// rounded up to the boundary the certificate table sits on.
//
// The rounding has to happen here rather than while attaching, and that is the
// one detail in this file that is easy to get wrong and impossible to notice.
// Windows finds the end of the hashed region by reading where the certificate
// table starts, so any padding written before the table is inside the region
// Windows hashes. Padding a file after hashing it produces a signature that
// verifies nowhere.
func prepare(body []byte, l peLayout) []byte {
	program := body[:l.signedEnd(body)]
	aligned := (len(program) + winCertAlign - 1) / winCertAlign * winCertAlign
	out := make([]byte, aligned)
	copy(out, program)
	// The certificate directory is cleared before the hash is taken, because a
	// file that arrived signed has an entry in it and the hash must be the one
	// an unsigned file would give.
	clear(out[l.certDirAt : l.certDirAt+8])
	return out
}

// attach puts a PKCS#7 signature on the end of a prepared executable and points
// the file's certificate directory at it.
//
// It returns a new slice. The input is left alone so that a failure anywhere
// after this leaves the caller holding the unsigned copy it started with,
// rather than something half-signed it cannot tell apart from a good one.
func attach(prepared []byte, l peLayout, pkcs7 []byte) ([]byte, error) {
	switch {
	case len(pkcs7) == 0:
		return nil, errors.New("clientbuild: there is no signature to attach")
	case len(prepared)%winCertAlign != 0:
		return nil, errors.New("clientbuild: the copy being signed was not prepared for a signature")
	}
	// dwLength counts the header and the signature and not the padding after
	// it, which is how every other tool writes it and how Windows walks the
	// table from one entry to the next.
	entry := winCertHeaderSize + len(pkcs7)
	padded := (entry + winCertAlign - 1) / winCertAlign * winCertAlign
	start := len(prepared)

	out := make([]byte, start+padded)
	copy(out, prepared)
	binary.LittleEndian.PutUint32(out[start:], uint32(entry)) //nolint:gosec // G115: a PKCS#7 signature is kilobytes.
	binary.LittleEndian.PutUint16(out[start+4:], winCertRevision2)
	binary.LittleEndian.PutUint16(out[start+6:], winCertTypePKCS)
	copy(out[start+winCertHeaderSize:], pkcs7)

	binary.LittleEndian.PutUint32(out[l.certDirAt:], uint32(start))    //nolint:gosec // G115: bounded by the file's length.
	binary.LittleEndian.PutUint32(out[l.certDirAt+4:], uint32(padded)) //nolint:gosec // G115: as above.
	binary.LittleEndian.PutUint32(out[l.checkSumAt:], checksum(out, l.checkSumAt))
	return out, nil
}

// checksum is the PE header checksum: a sixteen-bit ones-complement sum of the
// whole file with the checksum field itself read as zero, plus the file's
// length.
//
// Windows does not check it for a program a person runs, and it would be fair
// to leave the zero the Go linker writes. It is computed because every other
// tool that signs a PE computes it, and a file that differs from what signtool
// would have produced is a file whose differences somebody has to explain.
func checksum(body []byte, checkSumAt int) uint32 {
	// The four bytes of the field itself count as zero, which is what they
	// were when the linker computed the value now being replaced. They are
	// zeroed a byte at a time rather than a word at a time so that a header at
	// an odd offset does not quietly take a fifth byte with it.
	at := func(i int) uint32 {
		if i >= checkSumAt && i < checkSumAt+4 {
			return 0
		}
		return uint32(body[i])
	}
	var sum uint32
	for i := 0; i+1 < len(body); i += 2 {
		sum += at(i) | at(i+1)<<8
		sum = (sum & 0xffff) + (sum >> 16)
	}
	if len(body)%2 == 1 {
		sum += at(len(body) - 1)
		sum = (sum & 0xffff) + (sum >> 16)
	}
	sum = (sum & 0xffff) + (sum >> 16)
	return sum + uint32(len(body)) //nolint:gosec // G115: a client is megabytes.
}
