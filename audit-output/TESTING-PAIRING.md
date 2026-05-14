# Testing Bridge Pairing — End-to-End Guide

**Date:** 2026-05-14  
**Status:** Ready for testing  
**Components:** Relay + Bridge CLI + App

---

## Prerequisites

1. **Relay deployed** to Cloudflare Workers
2. **Bridge built** from ~/GitHub/lumen-bridge-linux
3. **App** with new pairing views (Xcode)
4. **CloudKit token** extracted manually (for MVP testing)

---

## Step 1: Deploy Relay to Cloudflare

```bash
cd ~/GitHub/lumen-bridge-relay

# Install dependencies
npm install

# Login to Cloudflare (one-time)
npx wrangler login

# Deploy to dev environment first
npx wrangler deploy --env dev

# Expected output:
# ✨ Deployed to relay-dev.lorislab.fr
```

**Test relay health:**
```bash
curl https://relay-dev.lorislab.fr/health
# Expected: {"status":"ok"}
```

---

## Step 2: Build Bridge with pair command

```bash
cd ~/GitHub/lumen-bridge-linux

# Build
go build -o lumen-bridge ./cmd/lumen-bridge

# Test help
./lumen-bridge pair --help

# Expected output:
# Usage of pair:
#   -code string
#     	6-digit pairing code from app (required)
#   -relay string
#     	Relay server URL (default "wss://relay.lorislab.fr")
```

---

## Step 3: Add pairing views to app (Xcode)

1. Open `Lumen for Frigate.xcodeproj`
2. Add files to project:
   - `Lumen for Frigate/Services/BridgePairingService.swift`
   - `Lumen for Frigate/Views/Settings/BridgePairingView.swift`
   - `Lumen for Frigate/Views/Settings/BridgePairingViewModel.swift`
3. Add navigation link in SettingsView:

```swift
NavigationLink("Pair Bridge") {
    BridgePairingView()
}
```

4. Build and run on iOS Simulator or device

---

## Step 4: End-to-End Test (Manual Token)

### 4.1: In the app (iOS/macOS)

1. Go to **Settings → Pair Bridge**
2. Tap **"Start Pairing"**
3. App displays **6-digit code** (e.g., `837249`)
4. **Leave app open** on this screen

### 4.2: On the Bridge server (LXC/Linux)

```bash
# Use the code from the app
./lumen-bridge pair --code 837249 --relay wss://relay-dev.lorislab.fr

# Expected output:
# 🔗 Connecting to relay...
# ⏳ Waiting for app to confirm...
# (waits here for token)
```

### 4.3: Extract CloudKit token manually (MVP workaround)

Since we don't have automatic token extraction yet, get it manually:

**Option A: From existing token.json (if you have one)**
```bash
cat ~/.config/lumen-bridge/token.json
# Copy the ckSession value
```

**Option B: From Safari DevTools (iCloud.com)**
1. Open Safari → https://www.icloud.com
2. Open DevTools (Develop → Show Web Inspector)
3. Go to **Network** tab
4. Refresh the page
5. Find a request to `database/1/com.apple.cloudkit`
6. Look at **Request Headers** → Cookie
7. Find `X-Apple-CloudKit-Session=xxx...`
8. Copy the long token value after `=`

### 4.4: Submit token in app

1. In the app, scroll down to **"Or paste your CloudKit token manually"**
2. Paste the ckSession token
3. Tap **"Submit Token"**

### 4.5: Bridge receives token

```bash
# Bridge output:
# 📦 Received encrypted token from app
# ✅ Token received and saved to ~/.config/lumen-bridge/token.json
# ✅ Bridge is ready
#
# Next steps:
#   1. Restart the bridge: systemctl restart lumen-bridge
#   2. Check logs: journalctl -u lumen-bridge -f
```

### 4.6: Verify token was saved

```bash
cat ~/.config/lumen-bridge/token.json

# Expected:
# {
#   "ckSession": "eyJfYXV0..."
# }
```

### 4.7: Restart Bridge and test CloudKit

```bash
# Restart Bridge
systemctl restart lumen-bridge

# Check logs (should NOT see HTTP 401 anymore)
journalctl -u lumen-bridge -f

# Expected (no auth errors):
# INFO: connected; subscribing
# INFO: snapshot cache subscribed
```

---

## Step 5: Trigger a Frigate Event (Full E2E)

1. **Trigger detection** in Frigate (walk in front of camera)
2. **Check Bridge logs:**
   ```bash
   journalctl -u lumen-bridge -f
   
   # Expected:
   # INFO: received event: new (person, front_door)
   # INFO: uploading snapshot
   # INFO: forwarded event to CloudKit
   ```
3. **Check iOS/macOS notification** arrives within 5-10 seconds

---

## Troubleshooting

### Bridge: "failed to connect to relay"

**Cause:** Relay not deployed or wrong URL  
**Fix:**
```bash
# Test relay manually
curl https://relay-dev.lorislab.fr/health

# If 404, deploy relay:
cd ~/GitHub/lumen-bridge-relay
npx wrangler deploy --env dev
```

### Bridge: "connection closed"

**Cause:** Pairing code expired (5 min limit) or wrong code  
**Fix:** Start fresh in the app, get new code

### App: "Server error (410)"

**Cause:** Session expired  
**Fix:** Tap "Cancel" and start new pairing

### Bridge: "decryption failed"

**Cause:** Code mismatch between app and CLI  
**Fix:** Make sure you typed the exact 6-digit code from the app

### Bridge still gets HTTP 401 after pairing

**Cause:** Token saved but Bridge config points to wrong file  
**Fix:**
```bash
# Check Bridge config
cat /etc/lumen-bridge/config.yaml

# Make sure it reads token from correct path:
# (or set environment variable LB_CLOUDKIT_TOKEN_FILE)

# Restart Bridge
systemctl restart lumen-bridge
```

---

## Known Limitations (MVP)

1. **Manual token input required** — Automatic CloudKit token extraction not yet implemented
2. **No WebSocket feedback to app** — App doesn't know when Bridge connects (timer-only)
3. **Code-derived encryption** — Not full ECDH (Phase 2 improvement)
4. **Dev relay only** — Production relay not yet deployed

---

## Next Steps (Post-MVP)

1. **Automatic token extraction** — Extract ckSession from app's CKContainer
2. **WebSocket feedback** — App listens for Bridge connection confirmation
3. **Full ECDH** — E2E encryption with ephemeral keypairs
4. **Production relay** — Deploy to relay.lorislab.fr
5. **Integration with SettingsView** — Add prominent "Setup Bridge" card

---

## Success Criteria

✅ User can pair Bridge without DevTools  
✅ Token is encrypted end-to-end  
✅ Bridge receives valid CloudKit token  
✅ Notifications start working immediately after pairing  
✅ UX is "Apple-like" (simple, guided, visual feedback)

---

**Test Date:** ___________  
**Tested By:** ___________  
**Result:** ☐ Pass  ☐ Fail  
**Notes:**
