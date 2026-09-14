// Package clientbuild turns one prebuilt Windows client into a client that
// knows this team's address.
//
// It does not compile anything. The community edition ships a generic
// telemetry.exe, and building a client means copying that file and writing the
// operator's server address into a fixed-size region reserved inside it. That
// is milliseconds of work, it needs no Go toolchain, and it runs on the
// cheapest host an operator can rent — which is the whole reason the admin
// panel can offer a build button at all.
//
// # The region
//
// [RegionSize] bytes, sitting in the client's data section:
//
//	[0:16]      head marker
//	[16]        format version — 0 in a prebuilt binary, 1 once stamped
//	[17]        reserved, zero
//	[18:20]     payload length, big-endian uint16
//	[20:24]     CRC-32 (IEEE) of the payload, big-endian
//	[24:1008]   the payload, [PayloadMax] bytes of room
//	[1008:1024] tail marker
//
// Two markers rather than one, and a scan rather than an offset. A Go binary's
// layout moves with every rebuild, so an offset recorded today is the wrong
// offset tomorrow; and a single marker could appear anywhere — inside a
// compressed asset, inside a string — so the head marker is only a region if
// the tail marker is exactly [RegionSize]-16 bytes after it. [Find] requires
// exactly one such pair and refuses a file with two, because a file with two
// is a file this code does not understand well enough to modify.
//
// The payload is versioned, length-prefixed and checksummed so that the client
// reading it can tell a stamp from the zeroes it was born with, and a good
// stamp from a half-written one. Its own format is key=value lines, which a
// client that has never heard of a key can read past.
//
// # What stamping does not change
//
// Not the file's size, not its layout, not one byte outside the region. A
// stamped copy is the prebuilt binary with 1024 bytes rewritten in place, so
// it is still the same valid PE executable — [Inspect] and [Artifact.Verify]
// check that rather than assume it.
//
// What it does invalidate is a signature. Any edit to a signed Windows
// executable breaks its Authenticode signature, which is why signing happens
// last: [Sign] is given the stamped copy, and nothing touches the bytes after
// it returns.
//
// # Signing
//
// An operator who gives this server a certificate gets every client it builds
// signed with it, in the same press of the button. The certificate is a PKCS#12
// file they upload, held sealed in the database; the signature is Authenticode,
// built here rather than shelled out to a tool that does not exist on the hosts
// these servers run on.
//
// A signature says who built the file and that nobody has changed it since.
// Whether it stops Windows warning the driver is a separate question and
// belongs to the certificate rather than to this code: one from a public
// authority stops it once the file has a download reputation, and one the
// operator issued themselves stops it only on machines that trust them.
//
// Signatures made here are not timestamped. A timestamp is what keeps a
// signature valid after the certificate behind it expires, and getting one
// means asking a timestamping authority over the network — which this server
// otherwise never does. Until that is a thing an operator opts into, a client
// stops verifying on the day the certificate expires, and the build page says
// so.
package clientbuild
