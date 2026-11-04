# Lumen Bridge for Linux

A small headless daemon that sends [Frigate NVR](https://github.com/blakeblackshear/frigate)
detections to the **Lumen for Frigate** apps (iPhone, iPad, Mac, Apple Watch,
Vision Pro). It is the Linux/Docker companion of
[Lumen Bridge for macOS](https://github.com/lorislabapp/lumen-bridge): run it
next to Frigate on your NAS, Pi, Unraid or Proxmox box instead of keeping a
Mac on.

> **Status: v0.3.2.** MQTT decoding, relay forwarding, snapshot and clip
> upload are implemented and unit-tested against the relay's API contract.
> Not yet validated end-to-end on a real Frigate + iPhone setup by the
> maintainers — please report problems in
> [Issues](https://github.com/lorislabapp/lumen-bridge-linux/issues).

## How it works

```
Frigate ──MQTT (LAN)──▶ lumen-bridge ──HTTPS──▶ Lumen Bridge Relay ──▶ CloudKit ──▶ Lumen apps
   ▲                        │                    (Cloudflare Worker)
   └── HTTP :5000 (LAN) ────┘  snapshot / clip
```

1. The bridge subscribes to `frigate/events` on your MQTT broker.
2. On a `new` detection it uploads the event snapshot and sends the event to
   the relay. The relay writes a `FrigateEvent` record to CloudKit; the Lumen
   apps receive it through their CloudKit subscription.
3. On `end` it uploads the recorded MP4 clip and attaches it to the same record.
4. `update` messages are ignored (the record already exists).

The bridge holds no Apple credentials. It authenticates to the relay with a
device token you create by pairing it with the Lumen app. You can revoke it
any time from the app.

## Requirements

- Frigate 0.13+ with MQTT enabled (decoding tested against Frigate 0.17 payloads).
- Network access from the bridge to your MQTT broker, and (recommended) to
  Frigate's internal API on port 5000.
- Outbound HTTPS to `relay.lorislab.fr` and Apple's CloudKit asset CDN.
- The Lumen for Frigate app with push delivery set to **Apple Bridge**.

## Install — Docker

```bash
mkdir -p lumen-bridge
curl -fsSL -o lumen-bridge/config.yaml \
  https://raw.githubusercontent.com/lorislabapp/lumen-bridge-linux/main/config.example.yaml
$EDITOR lumen-bridge/config.yaml            # mqtt.host, credentials, frigate.base_url
sudo chown -R 65532:65532 lumen-bridge      # image runs as distroless "nonroot"

# 1. Check the config
docker run --rm -v "$PWD/lumen-bridge:/data" ghcr.io/lorislabapp/lumen-bridge-linux:0.3.2 \
  doctor --config /data/config.yaml

# 2. Pair (code from the Lumen app, see "Pairing" below)
docker run --rm -it -v "$PWD/lumen-bridge:/data" ghcr.io/lorislabapp/lumen-bridge-linux:0.3.2 \
  pair --config /data/config.yaml --code 123456

# 3. Run
docker run -d --name lumen-bridge --restart unless-stopped \
  -v "$PWD/lumen-bridge:/data" ghcr.io/lorislabapp/lumen-bridge-linux:0.3.2
```

The image is multi-arch (amd64, arm64). A compose snippet is in
[`docker-compose.example.yml`](docker-compose.example.yml).

## Install — systemd (Debian / Ubuntu / Raspberry Pi OS)

```bash
curl -fsSL https://raw.githubusercontent.com/lorislabapp/lumen-bridge-linux/main/scripts/install.sh \
  | sudo LB_VERSION=v0.3.2 bash
```

The script creates a `lumen-bridge` system user, installs the binary to
`/usr/local/bin`, verifies it against the release `SHA256SUMS`, writes
`/etc/lumen-bridge/config.yaml` (first install only) and a hardened systemd
unit. Then:

```bash
sudo $EDITOR /etc/lumen-bridge/config.yaml
sudo -u lumen-bridge lumen-bridge doctor --config /etc/lumen-bridge/config.yaml
sudo -u lumen-bridge lumen-bridge pair --config /etc/lumen-bridge/config.yaml --code 123456
sudo systemctl enable --now lumen-bridge
journalctl -fu lumen-bridge
```

Manual install: download `lumen-bridge-linux-<arch>.tar.gz` and `SHA256SUMS`
from [Releases](https://github.com/lorislabapp/lumen-bridge-linux/releases),
run `sha256sum -c SHA256SUMS --ignore-missing`, and use
[`systemd/lumen-bridge.service`](systemd/lumen-bridge.service).

## Pairing with the Lumen app

1. In Lumen: **Settings → Paired Bridges** (shown when push delivery is
   *Apple Bridge*) **→ Pair a new Bridge**. The app shows a 6-digit code,
   valid 5 minutes.
2. On the bridge host run `lumen-bridge pair --code <code>` (with `--config`
   as above). It waits for the app.
3. In the app, name the bridge and confirm. The token is sent encrypted
   with a key derived from the code (HKDF-SHA256 + AES-GCM) and saved to
   `relay.device_token_path` with mode 0600.
4. Start or restart the daemon. To unpair, revoke the bridge in
   **Paired Bridges**.

## Configuration

`./config.yaml` or `/etc/lumen-bridge/config.yaml` (or `--config PATH`).
Every field can be set by environment variable; env wins over the file.
See [`config.example.yaml`](config.example.yaml).

| Key | Env | Default |
|---|---|---|
| `mqtt.host` (required) | `LB_MQTT_HOST` | — |
| `mqtt.port` | `LB_MQTT_PORT` | `1883` |
| `mqtt.tls` | `LB_MQTT_TLS` | `false` |
| `mqtt.username` / `mqtt.password` | `LB_MQTT_USERNAME` / `LB_MQTT_PASSWORD` | empty |
| `mqtt.topic_prefix` | `LB_MQTT_TOPIC_PREFIX` | `frigate` |
| `mqtt.client_id` | `LB_MQTT_CLIENT_ID` | `lumen-bridge-linux` |
| `frigate.base_url` (recommended) | `LB_FRIGATE_BASE_URL` | empty |
| `relay.url` | `LB_RELAY_URL` | `https://relay.lorislab.fr` |
| `relay.device_token_path` | `LB_RELAY_DEVICE_TOKEN_PATH` | `~/.config/lumen-bridge/device-token.json` |
| — | `LB_RELAY_DEVICE_TOKEN` | token value instead of a file |
| — | `LB_HEALTH_ADDR` | `127.0.0.1:9090` |

`frigate.base_url` should be Frigate's internal API, e.g.
`http://192.168.1.50:5000`. With Frigate authentication enabled, port 8971
needs a login; port 5000 does not — keep it on your LAN. Without
`base_url`, the bridge uses live MQTT snapshots only and uploads no clips.

## Operations

- `lumen-bridge run [--dry-run] [--debug]` — `--dry-run` decodes and logs
  events without sending anything.
- `lumen-bridge doctor` — checks config, broker reachability, device token,
  relay and Frigate. It prints which config file it read and which `LB_*`
  variables replaced values, runs every check even after a failure, and
  exits 1 if any check failed.
  - Doctor shows an address you no longer use? It is reading another file.
    Without `--config` it uses `./config.yaml`, then
    `/etc/lumen-bridge/config.yaml`. Pass `--config` with the file the
    bridge really uses.
  - Broker set by Docker service name (e.g. `mqtt`)? That name only resolves
    inside the Docker network. Run doctor in the bridge container:
    `docker compose run --rm lumen-bridge doctor --config /data/config.yaml`.
    A bridge on the host needs the broker's IP, or `localhost` if port 1883
    is published.
- Health server (default `127.0.0.1:9090`): `/healthz`, `/readyz` (503 while
  MQTT is disconnected), `/metrics` (JSON counters: received, forwarded,
  skipped, dropped, errors, snapshot_uploads, clip_uploads).
- Relay errors: network, 429 and 5xx are retried with backoff; 4xx are
  logged and skipped (401 = token revoked → pair again).

## Privacy — what leaves your network

Sent to the Lumen Bridge Relay (a Cloudflare Worker run by LorisLabs), which
stores it in the LorisLabs CloudKit container `iCloud.com.lorislabapp.lumenbridge`:

- per event: Frigate event id, camera name, label (e.g. `person`), zone names,
  top score, start time;
- the event snapshot JPEG and, when available, the MP4 clip;
- the device token, which identifies your pairing. Records are tagged with
  your pairing's user reference.

Never sent: MQTT credentials, broker or Frigate addresses, `sub_label`
(recognised face or plate names), Frigate config, live streams or any other
MQTT topic. The bridge opens no inbound port.

## Not in this release

- Zone-specific alerts (e.g. "mailbox"), HomeKit and the other options of
  the macOS Bridge 1.1.
- Alerts based on Frigate review items (`frigate/reviews`).

## Build from source

```bash
go test -race ./...
CGO_ENABLED=0 go build -trimpath -ldflags '-s -w' -o lumen-bridge ./cmd/lumen-bridge
```

Requires the Go version in `go.mod`. History: [CHANGELOG.md](CHANGELOG.md).

## License

MIT — see [LICENSE](LICENSE).
