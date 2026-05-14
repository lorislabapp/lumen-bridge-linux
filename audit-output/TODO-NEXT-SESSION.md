# TODO — Next Session (Post 2026-05-14)

**Status:** Ready for E2E testing  
**Priority:** HIGH — Bridge pairing is product-critical

---

## Immediate Next Steps (Order Matters)

### 1. Deploy Relay to Cloudflare Workers ⏱️ 15 min

```bash
cd ~/GitHub/lumen-bridge-relay

# Install dependencies
npm install

# Login to Cloudflare (one-time, opens browser)
npx wrangler login

# Deploy to dev environment
npx wrangler deploy --env dev

# Expected output:
# ✨ Deployed to relay-dev.lorislab.fr

# Test health endpoint
curl https://relay-dev.lorislab.fr/health
# Should return: {"status":"ok"}
```

**Potential issues:**
- Need to create Cloudflare account if not exists
- Need to enable Durable Objects (free tier OK for testing)
- DNS: relay-dev.lorislab.fr must point to Cloudflare Worker

---

### 2. Add Pairing Views to Lumen App (Xcode) ⏱️ 10 min

1. Open `~/GitHub/Lumen for Frigate/Lumen for Frigate.xcodeproj`
2. Verify files are in project:
   - `Services/BridgePairingService.swift`
   - `Views/Settings/BridgePairingView.swift`
   - `Views/Settings/BridgePairingViewModel.swift`
3. Add navigation link in `SettingsView.swift`:

```swift
// In SettingsView.swift, add after other navigation links:
NavigationLink {
    BridgePairingView()
} label: {
    Label("Pair Bridge", systemImage: "server.rack")
}
```

4. Build and run (⌘R)
5. Navigate to Settings → Pair Bridge
6. Verify UI shows correctly

**Potential issues:**
- Files not added to target → Right-click → Target Membership
- Import errors → Clean build folder (⇧⌘K)

---

### 3. E2E Test on Your Homelab ⏱️ 20 min

Follow `TESTING-PAIRING.md` step-by-step.

**Quick version:**

#### On iOS/macOS app:
1. Settings → Pair Bridge → Start Pairing
2. Note the 6-digit code (e.g., `837249`)
3. Leave app open on this screen

#### On Bridge server (LXC 168):
```bash
# Build Bridge if needed
cd ~/GitHub/lumen-bridge-linux
go build -o lumen-bridge ./cmd/lumen-bridge

# Run pair command with code from app
./lumen-bridge pair --code 837249 --relay wss://relay-dev.lorislab.fr
```

**Expected output:**
```
🔗 Connecting to relay...
⏳ Waiting for app to confirm...
```

#### Get CloudKit token (manual for MVP):

**Option A:** If you have existing token:
```bash
cat ~/.config/lumen-bridge/token.json
# Copy the ckSession value
```

**Option B:** Extract from Safari (one-time):
1. Safari → https://www.icloud.com → DevTools
2. Network tab → Reload
3. Find request to `database/1/com.apple.cloudkit`
4. Copy `X-Apple-CloudKit-Session` cookie value

#### In app:
1. Scroll down to "Or paste your CloudKit token manually"
2. Paste the `ckSession` token
3. Tap "Submit Token"

#### Bridge receives token:
```
📦 Received encrypted token from app
✅ Token received and saved to ~/.config/lumen-bridge/token.json
✅ Bridge is ready

Next steps:
  1. Restart the bridge: systemctl restart lumen-bridge
  2. Check logs: journalctl -u lumen-bridge -f
```

#### Verify:
```bash
# Restart Bridge
systemctl restart lumen-bridge

# Check logs (should NOT see HTTP 401 anymore)
journalctl -u lumen-bridge -f

# Should see:
# INFO: connected; subscribing
# INFO: snapshot cache subscribed
```

#### Trigger Frigate event:
1. Walk in front of a camera
2. Wait 5-10 seconds
3. Check notification on iOS/macOS

**Success = notification arrives!** 🎉

---

## If E2E Test Succeeds ✅

### Next Priority: Auto Token Extraction

**Why:** Manual paste is OK for MVP but not shippable to users.

**Approach:**
1. Research how to extract `ckSession` from CKContainer
2. Options:
   - Intercept URLSession requests (swizzle)
   - Read from keychain (CloudKit stores tokens there)
   - Use CKContainer internal APIs (if accessible)
3. Implement in `BridgePairingService.extractCloudKitToken()`
4. Remove manual text field from UI

**Timeline:** 1-2 days research + implementation

---

## If E2E Test Fails ❌

### Debugging Checklist

#### Relay issues:
```bash
# Test relay manually
curl -X POST https://relay-dev.lorislab.fr/pair/create \
  -H "Content-Type: application/json" \
  -d '{"session_id":"test-123"}'

# Should return:
# {"code":"123456","relay_url":"wss://...","expires_at":"..."}

# If 404: Relay not deployed
# If 500: Durable Objects not enabled
# If timeout: DNS issue
```

#### Bridge issues:
```bash
# Test Bridge can reach relay
nc -zv relay-dev.lorislab.fr 443

# Test WebSocket manually (use wscat)
npm install -g wscat
wscat -c "wss://relay-dev.lorislab.fr/pair/ws/123456"
```

#### App issues:
```bash
# Check app logs in Xcode console
# Look for network errors, JSON decode errors
```

---

## After Successful E2E Test

### Phase 2 Enhancements (Not MVP)

1. **WebSocket feedback** — App listens for Bridge connection
2. **Full ECDH encryption** — Ephemeral keypairs (not code-derived)
3. **QR code option** — Scan instead of typing code
4. **Production relay** — Deploy to relay.lorislab.fr
5. **Documentation** — User-facing guide on lorislab.fr

### Integration with Settings

Add prominent card in SettingsView:

```swift
Section {
    VStack(alignment: .leading, spacing: 8) {
        Label("Bridge Setup", systemImage: "server.rack")
            .font(.headline)
        
        Text("Connect your self-hosted Bridge to iCloud")
            .font(.caption)
            .foregroundColor(.secondary)
        
        NavigationLink("Pair Bridge") {
            BridgePairingView()
        }
        .buttonStyle(.borderedProminent)
        .controlSize(.small)
    }
    .padding(.vertical, 8)
} header: {
    Text("Self-Hosted Bridge")
}
```

---

## Repo Status

### lumen-bridge-relay
- ✅ Code written
- ✅ Committed (2d08c55)
- ⏳ Not deployed yet
- ❌ No remote configured

**Action:** Add GitHub repo + push:
```bash
cd ~/GitHub/lumen-bridge-relay
gh repo create lorislabapp/lumen-bridge-relay --private
git remote add origin git@github.com:lorislabapp/lumen-bridge-relay.git
git push -u origin main
```

### lumen-bridge-linux
- ✅ Code written
- ✅ Committed (dfba762)
- ✅ Pushed to main

### Lumen for Frigate
- ✅ Code written
- ✅ Committed (cdf80ee)
- ✅ Pushed to main
- ⏳ Files need to be added to Xcode project

---

## Success Metrics

After E2E test succeeds, track:
- [ ] Bridge receives token without errors
- [ ] token.json contains valid ckSession
- [ ] Bridge connects to CloudKit (no HTTP 401)
- [ ] Frigate event → notification arrives within 10 seconds
- [ ] UX feels "Apple-like" (simple, guided)
- [ ] Error messages are clear if something fails

---

## If You Need Help

**Read first:**
1. `bridge-token-provisioning-design.md` — Full architecture
2. `TESTING-PAIRING.md` — Step-by-step test guide
3. Session summary at `/tmp/session-summary-2026-05-14.md`

**Common issues documented in TESTING-PAIRING.md:**
- Relay connection failures
- Token decryption errors
- Session expiry
- Bridge still gets HTTP 401 after pairing

**If stuck:** Open issue in lumen-bridge-linux repo with:
- Exact command run
- Full error output
- Bridge logs (`journalctl -u lumen-bridge`)
- Relay logs (if accessible)

---

## Timeline Estimate

If everything works first try:
- Deploy relay: 15 min
- Add to Xcode: 10 min
- E2E test: 20 min
- **Total: 45 min** ⏱️

With debugging:
- +30 min for relay issues
- +30 min for app issues
- +30 min for Bridge issues
- **Total: 2-3 hours** 🐛

---

**Last Updated:** 2026-05-14 14:45 CEST  
**Priority:** HIGH — Test before moving to other features  
**Blocked By:** None — all code is ready

---

Good luck! 🚀

Remember: This is MVP with manual token input. Auto-extraction comes after successful E2E test proves the architecture works.
