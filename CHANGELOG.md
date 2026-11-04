# Changelog

## [0.3.1] - 2026-11-04

### Changed
- **`doctor` explains what it checks.** It prints the config file it read
  (or the paths it searched), the `LB_*` variables that replaced values, and
  where the MQTT host came from. It warns when `mqtt.host` is still the
  installer's example `192.168.1.50`, and notes when the systemd service
  reads a different file.
- **`doctor` runs every check.** A failed check no longer stops the rest;
  the exit code is 1 if any check failed.
- **Hints on network errors.** Unknown host, refused connection and
  timeout each get a next step, including Docker service names (`mqtt`)
  used from the host and `localhost` used inside a container.

## [0.3.0] - 2026-10-28

The bridge no longer talks to CloudKit directly. It sends events to the
Lumen Bridge Relay, which writes them to CloudKit for the Lumen apps.

### Changed
- **Relay instead of CloudKit Web Services.** `lumen-bridge auth` (Apple ID
  web sign-in) is removed. Pair once with `lumen-bridge pair --code <6 digits>`
  using the code shown in the Lumen app (Settings → Paired Bridges). The bridge
  stores a revocable device token (file mode 0600).
- `pair` now writes the token to `relay.device_token_path` (from `--config`,
  `--token-path` or `LB_RELAY_DEVICE_TOKEN_PATH`), so systemd and Docker
  installs keep it in their state directory. Accepts `https://` relay URLs.
- The Docker image stores the token in `/data/device-token.json`.
- `~/` in `device_token_path` is now expanded.
- `doctor` fails when the relay `/health` does not answer 200.

### Added
- Snapshot upload on `new`: Frigate's per-event snapshot (`frigate.base_url`)
  or a live MQTT snapshot received since the event started.
- MP4 clip upload on `end` (needs `frigate.base_url`), attached to the same
  event record.
- Work queue: relay calls no longer run on the MQTT receive thread; clip
  uploads run on their own queue so a large MP4 never delays the next alert.
- Retries with exponential backoff for network errors, HTTP 429 and 5xx.
  4xx (revoked token, bad payload) are not retried.
- `/metrics` reports `dropped` (queue full) events.
- `SHA256SUMS` published with the release archives; `install.sh` verifies it.

### Fixed
- Frigate 0.17 MQTT decoding: zones are now `entered_zones` ∪ `current_zones`
  (de-duplicated). Before, a zone that only appeared in `current_zones` was
  lost whenever `entered_zones` was non-empty.
- `sub_label` is accepted as a string or as `[name, score]`; other shapes no
  longer risk dropping the event.
- Malformed payloads (invalid ids/names, score outside 0–1) are dropped.
- Event start/end times keep millisecond precision.
- A retained snapshot from an older detection is never attached to a new event.
- A disabled Frigate client or dry-run relay could be seen as configured.
- CI and the Docker build used Go 1.22/1.23 while `go.mod` requires 1.25;
  they now read the version from `go.mod`.

### Removed
- Prebuilt binary that was committed under `bin/`.

## [0.2.0] - 2026-04-27

CloudKit Web Services prototype (direct write with an Apple ID web session).
Superseded by 0.3.0.

## [0.1.0]

Initial skeleton: config, MQTT subscription, event decoding.
