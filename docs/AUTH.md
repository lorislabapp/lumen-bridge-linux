# Authentication — Lumen Bridge for Linux

Since v0.3.0 the bridge holds no Apple or CloudKit credentials.

- The **Lumen Bridge Relay** (Cloudflare Worker, `relay.lorislab.fr`) holds the
  only CloudKit Server-to-Server key and writes records on the bridge's behalf.
- The bridge authenticates with a **device token** (`Authorization: Bearer …`)
  scoped to `bridge`, minted by the relay when you pair from the Lumen app.

## Pairing flow

1. The Lumen app calls `POST /pair/create`; the relay returns a 6-digit code
   (valid 5 minutes).
2. `lumen-bridge pair --code <code>` opens `wss://relay.lorislab.fr/pair/ws/<code>`.
3. The app calls `POST /pair/mint-bridge-token`. The relay mints the token and
   sends it through the WebSocket encrypted with
   HKDF-SHA256(code, salt `lumen-bridge-v1-mvp`) → AES-256-GCM.
4. The bridge decrypts it and stores `{token, id, user_ref, scope}` at
   `relay.device_token_path` (mode 0600).

Revoke a bridge from the app (**Paired Bridges**); the relay then answers 401
and the bridge logs it. Pair again to get a new token.

## Endpoints used by the daemon

| Endpoint | Purpose |
|---|---|
| `POST /events/ingest` | create the `FrigateEvent` record (optional snapshot receipt) |
| `POST /assets/request-upload` | presigned CloudKit upload URL for `snapshot` or `clip` |
| `POST /events/update-asset` | attach an uploaded clip to the record |
| `GET /health` | used by `lumen-bridge doctor` |

`lumen-bridge auth` (CloudKit Web Services sign-in, v0.2.0) was removed.
