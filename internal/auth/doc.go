// Package auth holds the server's credentials: how a password is hashed, how a
// random token is minted, and how any of it is compared.
//
// It is a leaf package on purpose. Every comparison in here is constant time,
// every random value comes from crypto/rand, and no secret is ever stored in
// the form it was presented in — a password becomes an argon2id digest, a token
// becomes its SHA-256. A database dump therefore contains nothing that can be
// replayed, which is the property the whole design is for.
//
// Two kinds of credential live here and they are not interchangeable:
//
//   - An administrator signs in to the admin panel with an email and a
//     password, and gets a session cookie.
//   - A driver's machine never has a password. It is paired once and holds a
//     256-bit device token.
package auth
