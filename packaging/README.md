# Pacenote server — community edition

Pacenote records what your drivers do in the sim: every stint, every lap, and the full trace behind
each one, uploaded from their machines to a server you run. This is that server — one binary, one
PostgreSQL database, set up in a browser.

It is self-hosted and it serves one organisation. Everything it stores lives in your database, on
your machine.

## What you need before you start

- **PostgreSQL 15 or newer** — the server creates its own tables on first start, so give it an
  empty database and a user allowed to create tables in it.
- **A host name that points at this machine**, not an IP address. An address cannot get a
  certificate, and it pins every client you hand out to one machine.
- **A port drivers can reach**, 8080 by default.
- Nothing else. No runtime, no package manager, no container unless you want one.

If you do not have PostgreSQL and would rather not install it, use the compose file below — it
brings up a database alongside the server.

## Starting it

There are two ways, and they run the same program.

### The binary

Unzip the package, then run it:

```
./pacenote-server
```

That is the whole command. On Windows it is `pacenote-server.exe`, and on macOS the first run may be
blocked until you allow it in System Settings, because the binary is not notarised.

It binds `:8080` and prints where to go. If port 8080 is taken, or you are behind a proxy that
expects something else:

```
PACENOTE_LISTEN=:9000 ./pacenote-server
```

### Docker compose

The compose file in this package brings up the server and a PostgreSQL 17 database together. Set a
database password first, because the file will not start without one:

```
echo "POSTGRES_PASSWORD=$(openssl rand -base64 24)" > .env
docker compose up
```

It serves on port 8080. If something else on the machine already has that port, set `PACENOTE_PORT`
in the same `.env`.

The server waits for the database to report healthy before it starts, so the first `up` takes a few
seconds longer than the second. Leave it attached for the first run — the setup token is printed to
the log, and `docker compose logs server` will show it again if you lose the window.

Pin the image before you rely on it. The file defaults to `:latest`, which is convenient for a first
look and wrong for a server you care about:

```
echo "PACENOTE_IMAGE=ghcr.io/pacenote/pacenote-server:v0.1.0" >> .env
```

## What the first run looks like

The server starts in setup mode. It serves the wizard and nothing else, and it prints three lines:

```
Pacenote v0.1.0 — first run
Open  http://your-server:8080/setup
Token 7QK4-M2XF-8DNA-0123-4567-89AB-CDEF-GHJK        (this terminal only, once)
```

Open that address and type the token. Then the wizard asks six things:

1. **Your database** — a connection string. It is tested before you move on, and if it fails you are
   told which thing failed: unreachable, wrong credentials, no such database, or a server too old.
   Under compose this is already filled in.
2. **Your organisation name** — what drivers see when their client connects.
3. **An administrator account** — an email address and a password. This is the only password on the
   server; drivers never have one, because their machines are paired instead.
4. **Your public address** — the host name drivers' clients will reach.
5. **How it is reached** — behind your own proxy, or with a certificate this server gets for itself.
6. **An Anthropic key**, optional — it switches on the coaching prose, billed to your own account.
   Leave it empty and those features are simply absent.

Then it creates the schema, writes its configuration and starts serving. Nothing needs restarting.

### Why there is a token

You are probably running this on a machine strangers can reach, and until an administrator account
exists, whoever loads the page first would become one. The token closes that window. It is printed
to the terminal and never written to a log, it works once, and a new one is minted every time the
server starts — so a token sitting in an old scrollback is already dead.

Under compose the token reaches the container log rather than a private terminal, because a
container's stdout is its log. If other people can read your Docker logs, finish setup before they
do, or run the binary directly for the first start.

### Setup runs once, and the database is what remembers

Deleting the data directory does not bring the wizard back. A single row in your database records
that setup finished, written in the same transaction that created your administrator account — so
either both exist or neither does, and a server that can reach a database holding that row will not
offer to be set up again.

That matters most when the data directory is not durable. If the configuration file is gone but the
database is intact, give the server its connection string and it starts normally:

```
PACENOTE_DATABASE_URL=postgres://pacenote:...@db:5432/pacenote ./pacenote-server
```

If neither the file nor that variable is there, the server stops and says what it needs rather than
guessing.

## Where your data is

Almost all of it is in PostgreSQL — drivers, devices, stints, laps, traces, settings and the audit
log. The rest is a small directory beside the binary:

```
./pacenote-server                 the binary
./pacenote-data/                  created when setup finishes, mode 0700
    config.json                  the connection string and the listen addresses, mode 0600
    autocert/                    the certificate cache, if this server holds its own
```

Under compose that directory is the `pacenote-data` volume and the database is the `pacenote-db`
volume.

`config.json` also holds the key that encrypts your Anthropic key before it is stored, which is why
a database dump contains no usable credential — and why losing the data directory means retyping
that one API key. The server tells you when that has happened rather than failing one call at a
time.

## Backing it up

Two things, and one of them is small.

**The database**, which is everything that matters:

```
pg_dump --format=custom --file=pacenote-$(date +%F).dump "$PACENOTE_DATABASE_URL"
```

Under compose, run it inside the database container:

```
docker compose exec -T db pg_dump --format=custom -U pacenote pacenote > pacenote-$(date +%F).dump
```

Restore into an empty database with `pg_restore`. The server will find the completed setup row and
start in normal mode.

**The data directory**, which is a few kilobytes and holds your encryption key:

```
tar czf pacenote-data-$(date +%F).tar.gz pacenote-data/
```

Keep it somewhere other than the server, and keep it away from the database dump — together they
open your stored API key, and apart neither does. Traces are the bulk of the database and they
compress well, so test a restore before you need one.

## Upgrading

The server applies its own schema migrations at startup, so an upgrade is replacing the binary.

1. Back up the database. This is the step people skip.
2. Stop the server.
3. Replace `pacenote-server` with the new one from the new package. Leave `pacenote-data/` alone.
4. Start it. It migrates the schema on the way up and logs each migration it applies.

Under compose, change the tag in `.env` and run `docker compose pull && docker compose up -d`.

Migrations run forward only. If an upgrade goes wrong, restoring the database dump and putting the
old binary back is the way out — which is why step one is step one. Read the changelog before you
move between minor versions, because that is where anything you have to do by hand is written down.

## Getting the Windows client to your drivers

Drivers install a Windows client that captures from iRacing and uploads to your server.

The panel builds it for you. **Clients** writes your server's address into a copy of the client, so
a driver installs one file and types nothing. Hand them the link; the build history records every
one you made.

Unless you sign it, they will see a SmartScreen warning the first time: "Windows protected your PC",
then **More info** and **Run anyway**. Tell them to expect it, because a warning nobody warned them
about is the one they refuse.

To remove it, upload your own code-signing certificate — a `.pfx` or `.p12` — under **Clients**.
Every client built afterwards is signed with it, and the panel records which certificate signed
each file. The certificate and its password are sealed with your data key, so a database dump
carries neither.

## Ports, environment and logs

The public port serves drivers and the admin panel. A second port serves Prometheus metrics and
pprof, and it stays on loopback — the server refuses to start if you point it anywhere else,
because that port publishes its internals.

Every value can come from the environment, for anyone automating a deployment:

| Variable | What it is |
|---|---|
| `PACENOTE_DATA_DIR` | where `config.json` lives — default: `pacenote-data` beside the binary |
| `PACENOTE_DATABASE_URL` | the PostgreSQL connection string |
| `PACENOTE_LISTEN` | the public address — default `:8080` |
| `PACENOTE_METRICS_LISTEN` | the private address — default `127.0.0.1:9090`, and it must stay on loopback |
| `PACENOTE_SECRET_KEY` | the data key that opens stored API keys |
| `PACENOTE_LOG_LEVEL` | `debug`, `info`, `warn` or `error` |

Flags: `-data` for the data directory, `-log-level`, and `-version`.

Logs are structured JSON on stdout, one line per event, with a redaction layer in front of them. A
token, an `Authorization` header, a connection string's password or an API key cannot reach a log
line, even by mistake.

## Verifying what you downloaded

Every release ships a `checksums.txt` covering every archive. Check yours before you unzip it:

```
sha256sum --check --ignore-missing checksums.txt
```

On macOS that is `shasum -a 256 -c --ignore-missing checksums.txt`.

## Licence and support

Read `LICENSE`. This is free software under the GNU General Public License, version 3 or later: run
it for anything, including commercially, change it, and run your changed copy. If you hand a changed
copy to somebody else, hand over your changes on the same terms.

There is no warranty and no support contract behind this package. `CHANGELOG.md` records what
changed in each release.
