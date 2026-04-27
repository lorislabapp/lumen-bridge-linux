# Authentication — Lumen Bridge for Linux

The bridge writes to **your** iCloud private database. Two credentials are required:

1. **Container API token** — identifies the *container* `iCloud.com.lorislabapp.lumenbridge`. Generated once by LorisLabs in the [CloudKit Dashboard](https://icloud.developer.apple.com/dashboard/) and bundled with the binary. Not user-secret.

2. **User token** — identifies *which user's iCloud private database* to write to. Each user obtains theirs via Apple's iCloud sign-in web flow.

## v0.0.1 — manual env-var mode (current)

The interactive sign-in flow ships in **v0.2.0**. Until then, both tokens are passed via env vars so the rest of the daemon can be exercised end-to-end.

### Step 1 — get the container API token

A bundled token will ship with the v0.1.0 binary. For source builds today, generate one yourself in the CloudKit Dashboard:

1. Sign in to [CloudKit Dashboard](https://icloud.developer.apple.com/dashboard/) with the LorisLabs Apple Developer account.
2. Select container `iCloud.com.lorislabapp.lumenbridge`.
3. Sidebar → **API Tokens** → **+** → label "Linux Bridge", environment `production`, role `Read/Write` on `Private Database`.
4. Copy the token string.

```bash
export LB_CK_API_TOKEN=<token-from-dashboard>
```

### Step 2 — get a user token

Apple doesn't expose a CLI for this. Until v0.2.0 lands the web flow, the simplest way is to use [CloudKit JS](https://developer.apple.com/documentation/cloudkitjs/cloudkit/authenticated_user_record) in a browser tab:

1. Open [`https://icloud.developer.apple.com/dashboard/...`](https://icloud.developer.apple.com/dashboard/) and sign in.
2. Open DevTools → Network tab → filter on `cloudkit`.
3. Trigger any read against your private database (e.g. browse a record type).
4. Copy the value of the `ckSession` query param from one of the requests.

```bash
export LB_CK_USER_TOKEN=<ckSession-from-network-tab>
```

This is fragile and per-user. The v0.2.0 web flow will replace it with a one-line `lumen-bridge auth` command.

## v0.2.0 — interactive web flow (planned)

```
$ lumen-bridge auth
Open this URL in a browser to sign in to iCloud:
    https://api.apple-cloudkit.com/auth/sign-in?ckSession=...

Waiting for sign-in (will time out in 10 min)...
✓ Authenticated as kevin@kevinn.ie
✓ Token saved to /home/kevin/.config/lumen-bridge/token.json
$ lumen-bridge run
```

The daemon will spin up an ephemeral HTTP listener on `localhost:0`, embed that as the `redirectUri`, block on the redirect, persist the returned `ckSession` token to `~/.config/lumen-bridge/token.json` (mode 0600), and exit. Subsequent `run` invocations read the cached token automatically. Tokens are refreshed transparently when CloudKit returns 401.

## Security model

- The **API token** authenticates the *app*. If leaked, an attacker can write to *their own* private database (via their own user token), not yours. Bundling it with the binary is fine.
- The **user token** authenticates *you*. If leaked, an attacker can read AND write your iCloud private database (limited to the bridge's container). Treat it like a password: file mode 0600, never in version control, rotate via Apple's iCloud account settings if compromised.
- The bridge writes **only** to your CloudKit private database, scoped to the `iCloud.com.lorislabapp.lumenbridge` container. It cannot read or write any other Apple data (Photos, Mail, etc.).
- All traffic to Apple is HTTPS over CloudKit Web Services. The bridge uses no third-party servers, no analytics, no telemetry.
