# Changelog

What changed in each release, newest first. Versions follow [semantic versioning](https://semver.org):
the major changes when an upgrade needs you to do something, the minor when there is something new,
the patch when something was wrong.

## [Unreleased]

Nothing yet.

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
