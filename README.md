# Lumen Bridge for Linux

**A headless daemon that bridges [Frigate NVR](https://github.com/blakeblackshear/frigate) detection events to Apple's push infrastructure via the user's own iCloud — without any third-party server in between.**

The Linux companion to [Lumen Bridge for macOS](https://github.com/lorislabapp/lumen-bridge). Same role, same architecture, different host: where the macOS Bridge runs on a Mac in your menu bar, the Linux Bridge runs as a Docker container or systemd service alongside Frigate itself, on the same NAS / Pi / Synology / Unraid box.

> **Status: early skeleton (2026-04-27).** Compiles, loads config, connects to Frigate's MQTT broker, decodes events. CloudKit write path is stubbed; the next milestone is end-to-end with Apple's CloudKit Web Services REST API.

## Why Linux Bridge exists

Most Frigate users run Frigate in Docker, on Linux. Asking them to also run a Mac 24/7 just to forward events to iCloud is friction. Linux Bridge removes that constraint:

```
Frigate (Docker, on your NAS)
    │
    │  MQTT (frigate/events, LAN only)
    ▼
Lumen Bridge for Linux  (this daemon, same NAS)
    │
    │  CloudKit Web Services REST   →   user's iCloud private database
    ▼
Apple CloudKit + APNs
    │
    │  silent push via CKQuerySubscription
    ▼
iPhone + iPad + Mac + Apple Watch (cellular) + Vision Pro
```

No Cloudflare. No Docker push relay. No third-party cloud. Detection events stay between your local network, Apple's CloudKit (private database scoped to *your* Apple ID), and your Apple devices.

## How it works

The Linux Bridge writes records of type `FrigateEvent` to the CloudKit container `iCloud.com.lorislabapp.lumenbridge` — the same container the macOS Bridge writes to and the same one the iOS / iPadOS / watchOS / visionOS Lumen for Frigate clients subscribe to via `CKQuerySubscription`. Result: a single `lumen-bridge-linux` daemon plus any Lumen client gives you camera notifications across every device under your Apple ID, with zero infrastructure in the middle.

Record schema (mirrors the macOS Bridge):

| Field         | Type     | Source                              |
|---------------|----------|-------------------------------------|
| `recordName`  | string   | Frigate event ID                    |
| `camera`      | String   | Frigate camera name                 |
| `label`       | String   | Detection label (`person`, `car`)   |
| `zones`       | [String] | Frigate zones triggered             |
| `topScore`    | Double   | Highest confidence score            |
| `detectedAt`  | Date     | Frigate `start_time`                |
| `snapshot`    | CKAsset  | JPEG snapshot (optional)            |
| `clip`        | CKAsset  | Finalised MP4 clip (optional)       |

## Authentication model

Linux Bridge uses **CloudKit Web Services** (Apple's REST API), not the native `CloudKit.framework`. Two credentials are needed:

1. **Container API token** — identifies the *container* (`iCloud.com.lorislabapp.lumenbridge`). One token per container, generated in [CloudKit Dashboard](https://icloud.developer.apple.com/dashboard/) by the LorisLabs developer account. Distributed with the binary; not user-secret.

2. **User token** — identifies *which user's iCloud private database* to write to. Each user gets their own at first-run via Apple's iCloud sign-in web flow:

```
$ lumen-bridge auth
Open this URL in a browser to sign in to iCloud:
    https://api.apple-cloudkit.com/auth/sign-in?ckSession=...
Waiting for sign-in… (token saved to ~/.config/lumen-bridge/token.json)
✓ Authenticated as <apple-id>
```

Tokens persist locally (`~/.config/lumen-bridge/token.json`, file mode 0600) and refresh automatically.

## Quick start (Docker)

```bash
mkdir -p ~/lumen-bridge && cd ~/lumen-bridge
cat > config.yaml <<'EOF'
mqtt:
  host: 192.168.3.160
  port: 1883
  username: frigate
  password: <YOUR_EMQX_PASSWORD>
cloudkit:
  container: iCloud.com.lorislabapp.lumenbridge
  environment: production
EOF

# First-time auth (interactive; opens a sign-in URL you copy to a browser)
docker run --rm -it -v $PWD:/data ghcr.io/lorislabapp/lumen-bridge-linux auth

# Run the daemon
docker run -d \
  --name lumen-bridge \
  --restart unless-stopped \
  -v $PWD:/data \
  ghcr.io/lorislabapp/lumen-bridge-linux run
```

## Quick start (systemd, native binary)

```bash
sudo install -m 0755 lumen-bridge /usr/local/bin/
sudo install -m 0644 systemd/lumen-bridge.service /etc/systemd/system/
sudo mkdir -p /etc/lumen-bridge && sudo install -m 0600 config.yaml /etc/lumen-bridge/config.yaml
sudo -u lumen-bridge lumen-bridge auth   # one-time
sudo systemctl daemon-reload
sudo systemctl enable --now lumen-bridge.service
```

## Configuration reference

`config.yaml` (also overridable via env vars `LB_*`):

```yaml
mqtt:
  host: <broker-host>             # required (LB_MQTT_HOST)
  port: 1883                      # default 1883 (LB_MQTT_PORT)
  username: <broker-username>     # optional (LB_MQTT_USERNAME)
  password: <broker-password>     # optional (LB_MQTT_PASSWORD)
  topic_prefix: frigate           # default "frigate" (LB_MQTT_TOPIC_PREFIX)
  client_id: lumen-bridge-linux   # default (LB_MQTT_CLIENT_ID)

cloudkit:
  container: iCloud.com.lorislabapp.lumenbridge
  environment: production         # production | development
  api_token_path: /etc/lumen-bridge/api.token   # optional, defaults bundled
  user_token_path: ~/.config/lumen-bridge/token.json
```

## Build from source

```bash
git clone https://github.com/lorislabapp/lumen-bridge-linux
cd lumen-bridge-linux
go build -o lumen-bridge ./cmd/lumen-bridge
./lumen-bridge --help
```

## Roadmap

| Milestone | Status | Notes |
|---|---|---|
| **0.0.1** Project skeleton, MQTT decode, config loader   | ✅ shipped | this commit |
| **0.1.0** CloudKit Web Services write path, end-to-end   | ⏳ next     | sign requests with ECDSA, write `FrigateEvent` |
| **0.2.0** Apple ID web auth flow + token persistence     | ⏳          | replaces env-var token mode |
| **0.3.0** Snapshot CKAsset upload                        | ⏳          | reuse macOS Bridge's snapshot caching logic |
| **0.4.0** Clip MP4 backfill on `end` events              | ⏳          | mirrors `attachClip` in macOS Bridge |
| **0.5.0** Multi-hub presence (announce ourselves to other Bridge instances) | ⏳ | matches the v0.2 macOS feature |
| **1.0.0** Stable: Docker Hub release, Helm chart, signed binaries for arm64 + amd64 | ⏳ | distribution polish |

## Companion projects

- **[Lumen Bridge for macOS](https://github.com/lorislabapp/lumen-bridge)** — same daemon, native Swift, runs in the menu bar.
- **[Lumen for Frigate (iOS / iPadOS / watchOS / visionOS)](https://apps.apple.com/app/id6760238729)** — the user-facing app that subscribes to the bridge's CloudKit records and renders the notifications. Required.
- **[Frigate](https://github.com/blakeblackshear/frigate)** — the upstream NVR project that publishes the MQTT events.

## License

MIT — see [LICENSE](LICENSE). Open source from day one. Contributions welcome.
