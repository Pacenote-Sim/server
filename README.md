# Pacenote server — community edition

One binary. Point it at a PostgreSQL database, run it, and set it up in a browser.

Drivers' machines upload stints, laps and traces to it; an operator runs it for their team. It is
self-hosted, it serves one organisation, and everything it stores lives in your database.

## What you need

- PostgreSQL 15 or newer, and an empty database on it. The user it connects as needs permission to
  create tables.
- A host name pointing at the machine — not an IP address. An address cannot get a certificate, and
  it pins every client you build to one machine.
- Nothing else. No runtime, no package manager, no container unless you want one.

## Running it

```
./pacenote-server
```

On the first run it prints a token and waits:

```
Pacenote v1.0.0 — first run
Open  http://your-server:8080/setup
Token 7QK4-M2XF-8DNA-0123-4567-89AB-CDEF-GHJK        (this terminal only, once)
```

Open that address, type the token, and the wizard asks four things:

1. **Your database** — a connection string, tested before you move on. A failure says which one it
   was: unreachable, wrong credentials, no such database, or a server too old.
2. **Your organisation name** — what drivers see when their client connects.
3. **An administrator account** — an email address and a password. Drivers do not get one; their
   machines are paired instead.
4. **Your public address** — the host name clients reach, and whether this server sits behind your
   own proxy or gets a certificate for itself.

Then it builds the schema, writes its configuration and starts serving. Nothing needs restarting.

The token exists because until there is an administrator account, whoever loads the page first would
become one. It is printed to the terminal, never logged, good once, and minted afresh on every
start.

## Setup runs once

Deleting the data directory does not bring the wizard back. A row in your database records that
setup finished, written in the same transaction that created your administrator — so either both
exist or neither does.

That matters when the data directory is not durable. In a container on an ephemeral volume, give it
the connection string in the environment:

```
PACENOTE_DATABASE_URL=postgres://pacenote:...@db:5432/pacenote ./pacenote-server
```

With no configuration file and no such variable, the server stops and says so rather than guessing.
It will not run the wizard, because the database may already hold an administrator.

## What sits beside the binary

```
./pacenote-server                the binary
./pacenote-data/                 created when setup finishes, mode 0700
    config.json                  connection string, listen addresses, data key — mode 0600
    autocert/                    the certificate cache, if this server holds its own
    plugins/                     one directory per plugin
    client/                      the prebuilt Windows client, if you build clients here
```

Everything else is in PostgreSQL. Backup and restore are `pg_dump` and `pg_restore`.

`config.json` holds the data key that seals every credential in the database — a plugin's API key,
the signing certificate. That is why a database dump carries nothing usable, and why losing the data
directory means entering those again. The panel says so rather than letting a feature fail one call
at a time.

## Plugins

Anything that calls a third party is a plugin: coaching, a voice, results export. The server names
no vendor and holds no credential for one.

Drop a directory holding a manifest and a binary into `<data>/plugins/` and the server finds it. A
plugin is a separate process — one that crashes does not take the server with it. It can be told
when a lap or stint finishes, be asked for something with a deadline, keep tables of its own in your
database, and serve pages at `/plugin/<its name>/`.

The panel lists what each one declared, renders the settings it asked for, and caps what it may
spend per day.

The panel can also install from the [marketplace](https://www.pacenote.tech/plugins/), the plugins
Pacenote has reviewed and approved. It is off until you turn it on: this server never goes online
otherwise. On, it reads a signed index from pacenote.tech once an hour, verifies it against a key
built into the binary, and installs a plugin with one button, checking the package against the
checksum in the index before anything is unpacked.

## Configuration from the environment

Every value in the file can be overridden:

| Variable | What it is |
|---|---|
| `PACENOTE_DATA_DIR` | where `config.json` lives — default: `pacenote-data` beside the binary |
| `PACENOTE_DATABASE_URL` | the PostgreSQL connection string |
| `PACENOTE_LISTEN` | the public address — default `:8080` |
| `PACENOTE_METRICS_LISTEN` | the private address — default `127.0.0.1:9090`, and it must stay on loopback |
| `PACENOTE_SECRET_KEY` | the data key that opens stored credentials |
| `PACENOTE_CLIENT_BINARY` | the prebuilt Windows client, if it is not in `<data>/client/` |
| `PACENOTE_LOG_LEVEL` | `debug`, `info`, `warn` or `error` |

Flags: `-data`, `-log-level`, `-version`.

## The two ports

The public one serves drivers, plugins and the admin panel. The private one serves Prometheus
metrics and pprof, and binds to loopback — the server refuses to start if you point it elsewhere,
because that port publishes its internals.

## Logs

Structured JSON on stdout, one line per event, with a redaction layer in front. A token, an
`Authorization` header, a connection string's password or an API key cannot reach a log line: the
handler removes them rather than trusting every call site to remember.

## Building it

Go 1.26 or newer. No code generation in a normal build — the schema, the queries and the pages are
committed.

```
make build          # this machine
make build-cross    # linux/amd64, linux/arm64, darwin/arm64, darwin/amd64, windows/amd64
make check          # what continuous integration runs
```

Static, `CGO_ENABLED=0`, `-trimpath`, one file per platform.

### Tests

`make test` needs nothing but Go. The tests that need a real PostgreSQL are behind a build tag and
an environment variable:

```
make test-postgres PACENOTE_TEST_DATABASE_URL=postgres://you@localhost:5432/postgres
make cover          # the same, with the coverage floors
```

Each of those tests creates its own empty database and drops it afterwards.

### Changing the schema or a query

Migrations are goose files in `internal/db/migrations`, embedded and applied at startup. Queries are
plain SQL in `internal/db/queries`, turned into typed Go by sqlc:

```
make sqlc
```

No ORM, no query builder. Every query is reviewable as SQL.

## Releasing

Push a tag:

```
git tag vX.Y.Z && git push origin vX.Y.Z
```

`.github/workflows/release.yml` takes the notes from that version's `CHANGELOG.md` section — and
fails before publishing if there is none — then cross-builds every platform, packages the zips,
writes `checksums.txt`, creates the GitHub release and pushes a multi-architecture image.

The same artefacts locally, without a tag:

```
make dist          # the zips and checksums.txt, in dist/
make test-dist     # unzip one, run it, check it reaches the wizard
make release-dry   # the whole rehearsal, including the release notes
make image         # the container image
```

`make dist` needs [GoReleaser](https://goreleaser.com). What goes in each zip lives in
`internal/packaging`, and the tests there check `.goreleaser.yml`, the compose file and the
Dockerfile against it, so the package cannot drift from what the operator README describes.

### The workspace in continuous integration

This module requires `github.com/pacenote-sim/protocol` and `github.com/pacenote-sim/plugin` by
version, like any module. Every workflow still checks this repository out into `server/`, the other
two beside it, and writes a `go.work` over all three — `.github/actions/go-workspace` does that once
for all of them — so that a change in one repository is tested with the others *before* it is
tagged, and so the container image can be built from the three sources together. A checkout of this
repository on its own builds too, against the tagged versions in `go.mod`.

| Name | What it is |
|---|---|
| `PROTOCOL_REPOSITORY`, `PLUGIN_REPOSITORY` (variables) | `owner/name` — default: that name under the same owner |
| `PROTOCOL_REF`, `PLUGIN_REF` (variables) | the ref to build against — default: `main` |
| `PROTOCOL_TOKEN` (secret) | a token that can read them, needed only while they are private |

## Licence

**GNU General Public License, version 3 or later.** Full text in `LICENSE`, and it ships in every
package.

Run it for anything, including commercially. Change it, and run your changed copy. If you hand a
changed copy to somebody else, hand over your changes on the same terms.

Plugins are separate. A plugin is its own program in its own process, so it is not linked into the
server and is not a derivative work of it. The interface it is written against is its own module
under Apache-2.0, and a plugin may carry whatever licence its author chooses.
