# Changelog

What changed in each release, newest first. Versions follow [semantic versioning](https://semver.org):
the major changes when an upgrade needs you to do something, the minor when there is something new,
the patch when something was wrong.

## [Unreleased]

## [0.2.0] — 2026-09-16

### Added

- **Plugins ask each other, through the server.** A plugin that declares `asks: ["drivers"]` in its
  manifest is handed the server when it starts and can put a question — `drivers.lookup`, with a
  payload of its own — to the plugin whose name the kind carries. The server stamps who asked,
  checks both manifests, charges the answering plugin's daily cap, applies the deadline, stops a
  question that would pass through more than three plugins, and never reads the payload. The panel
  shows *Asks drivers for information.* on the plugin's page. Plugin interface version 3.
- **A client can reach a plugin's driver routes with its device token.** The token a client uploads
  with now identifies it on `/plugin/<name>/…` the way it does on the API, so a plugin is told which
  driver is calling whether they came from a browser or from the telemetry client. The token itself
  is kept from the plugin: an `Authorization` header carrying one of this server's device tokens is
  not forwarded, whether or not it was still valid.
- **A plugin's pages are linked from its page in the panel,** under *Its pages*.
- **`scripts/demo.sql`** writes demo drivers, stints and laps, for a server nobody has driven on yet.
  Not in the release package.

### Changed

- **A plugin is handed the corner analysis and the car setup as the documents the client sent,**
  not copied into fields the server names. A client that measures something new reaches every
  plugin without a change here. Plugin interface version 2.
- **The server no longer works out the fuel per lap or the spread across the tyres for a plugin.**
  A plugin that wants them has the four temperatures and the fuel used.
- **`GET /me` lists the plugins a client can talk to** — each running plugin, its version, and the
  routes a client could reach under `/plugin/<name>/`. A client, or a plugin of the client's own,
  decides what it knows how to use; the server no longer guesses on its behalf.
- Tables that list rows have room between their columns.

### Fixed

- **A plugin that needs a credential could not be used.** A required key was checked against the
  plain settings, where a credential never is, so it was refused for not having one while holding it.
- **A plugin could not serve its pages until it was configured,** including the page you configure it
  on. It is now told what is set and decides for itself.

### Removed

- **The server asking plugins for anything.** The four coaching jobs and the voice job were kinds the
  server enumerated and never asked for; a request kind is now whatever the answering plugin
  declares, spelled `<its name>.<what>`, and the only thing that asks is another plugin.
- **The `coach` and `setups` features.** They were advertised whenever a plugin's manifest *declared*
  it answered a coaching request — engineer declares them and nothing asks — so a client was told it
  had a coach with no endpoint behind it. A plugin is not a feature; see `GET /me`.

- **`POST /tts`.** The server no longer relays speech. A voice plugin serves its own route under
  `/plugin/<name>/` and a client calls it there; this server never held a voice, and now it does not
  hold the endpoint either. Protocol 0.2.0 drops `TTSRequest` and the `tts` feature with it. No
  published client calls the old route.

## [0.1.0] — 2026-09-14

The first release. A telemetry server a team runs themselves: drivers pair a client, their laps
arrive, and plugins do the rest.

### Added

- **One static binary** for linux/amd64, linux/arm64, darwin/arm64, darwin/amd64 and windows/amd64,
  with no runtime to install. PostgreSQL 15 or later is the only thing it needs.
- **A first-run setup wizard** behind a single-use token printed to the terminal: database,
  organisation, administrator account, public address and TLS mode. Schema migrations live in the
  binary and are applied at startup.
- **Pairing.** A driver starts it in their client and reads out a six-character code; you approve it
  in the panel. That machine is then paired to that driver.
- **The telemetry API:** stints, laps with their traces, session summaries and reference laps. Laps
  compress to roughly a fifteenth of their size on the way in.
- **Per-corner analysis on every lap:** apex speed, the reference lap's speed at the same point, the
  speed lost between them, brake still applied at the apex, distance to the throttle pickup, and a
  named pattern — an early apex, a late brake, a slow exit — where there is one.
- **Car setup on every stint:** each wheel's cold and hot pressure, its tread temperatures and
  depths inner to outer, the wing, and the rest of the sheet as named values with their units.
- **Reference laps per simulator,** so a comparison is only ever within the simulator the lap was
  driven in.
- **The admin panel:** drivers, devices, pairing approval, what is stored and for how long, and the
  organisation's own settings.
- **Build a Windows client from the panel.** The server writes its own address into a copy of the
  client, so a driver installs one file and has nothing to type.
- **Sign the clients you build.** Upload a `.pfx` or `.p12` and every client built afterwards is
  signed with it. The certificate and its password are sealed with your data key, and the build
  history records which one signed each file.
- **Plugins.** Separate programs the server starts, speaks to over a local channel, restarts when
  they crash and gives up on when they keep crashing. A crashing plugin does not take the server
  with it.
- **A page for each plugin,** with the configuration fields it declared. Credentials are sealed with
  your data key and shown only as their last four characters.
- **A database per plugin.** One that asks for tables gets a PostgreSQL role and schema of its own,
  and joins them to your drivers, stints and laps through read-only views in a single query.
- **A daily token cap per plugin,** beside what that plugin has spent today and this month. The
  server enforces it: a plugin past its cap is not called at all.
- **A plugin can serve pages of its own,** at `/plugin/<its name>/`. There is no second port to
  open: the server forwards the request and writes back what the plugin answers. A plugin declares
  who may reach it — anyone, a signed-in driver, an operator, or nobody the server checks — and it
  never sees the server's own cookies or another plugin's.
- **Drivers can sign in,** through a plugin that authenticates them. The plugin decides who somebody
  is, by whatever means you chose; the server holds the session. You can see every browser a driver
  is signed in to on their page, and sign one out or all of them — separately from revoking their
  machines, because a machine uploads laps and a browser reads them.
- **Spoken cues,** relayed to whichever plugin answers. The server names no voice service and holds
  no credential for one.
- **Prometheus metrics and pprof** on a private port held to loopback.
- **A container image** on the GitHub registry, distroless and non-root, with a compose file that
  brings it up alongside PostgreSQL.

### Upgrade notes

None. This is the first release.

[Unreleased]: https://github.com/pacenote-sim/server/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/pacenote-sim/server/releases/tag/v0.1.0
