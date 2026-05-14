# Lumen Bridge Linux — Quick Fix Guide

**Date:** 2026-05-14  
**For:** Kevin  
**Goal:** Get CloudKit authentication working in 5 minutes

---

## TL;DR — The Problem

The bridge has the **API token** but is missing the **user token**. CloudKit requires BOTH. The user token is obtained via a one-time web sign-in flow.

---

## Step-by-Step Fix (5 minutes)

### Step 1: SSH into the LXC

```bash
ssh root@10.9.8.88
lxc-attach -n 168
```

### Step 2: Set the API token env var

The auth command needs this env var to work:

```bash
export LB_CK_API_TOKEN="954be7451333604dcf1fd26a52365e25fa18f9db3834d150ae0f8656d8b4d0fc"
```

To make it persist across reboots:

```bash
cat >> /etc/profile.d/lumen-bridge.sh <<'EOF'
export LB_CK_API_TOKEN="954be7451333604dcf1fd26a52365e25fa18f9db3834d150ae0f8656d8b4d0fc"
EOF
source /etc/profile.d/lumen-bridge.sh
```

### Step 3: Run the auth command

```bash
lumen-bridge auth
```

You'll see output like:

```
✦ Lumen Bridge — sign-in helper

  1. Open this URL in any browser to walk through sign-in:
       http://127.0.0.1:34567/

  2. The form will direct you to Apple's sign-in page; on success
     paste the resulting ckSession token back into the form.

  Token will be saved to: /root/.config/lumen-bridge/token.json
  Listening for the form submission… (timeout: 10 min)
```

### Step 4: Open the localhost URL in your browser

**Option A: If you're on the Proxmox host itself (10.9.8.88)**

Open a browser on the Proxmox machine and navigate to the printed localhost URL.

**Option B: If you're SSH'd from your Mac**

Use SSH port forwarding to access the localhost URL from your Mac:

```bash
# In a NEW terminal on your Mac:
ssh -L 34567:localhost:34567 root@10.9.8.88 "lxc-attach -n 168 -- sleep infinity"

# Then open in your Mac's browser:
open http://localhost:34567/
```

*(Replace 34567 with whatever port the daemon printed)*

### Step 5: Complete the web flow

The localhost page will:
1. Show you a "Sign in with Apple" button
2. Click it → redirected to Apple's iCloud sign-in
3. Sign in with your Apple ID (the one that owns the Lumen for Frigate app)
4. After sign-in, the page will display a long `ckSession` token
5. Copy that token
6. Paste it into the form on the localhost page
7. Submit

The daemon will print:

```
✓ token saved to /root/.config/lumen-bridge/token.json
  issued at: 2026-05-14T07:30:00Z

  Next:
    lumen-bridge run
```

### Step 6: Restart the daemon

```bash
systemctl restart lumen-bridge
```

### Step 7: Verify it's working

```bash
journalctl -u lumen-bridge -f
```

You should see:
- ✅ `"connected; subscribing"` to MQTT
- ✅ CloudKit writes succeeding (no more 401 errors)
- ✅ `"snapshot upload success"` messages

If you still see 401 errors, check:

```bash
cat /root/.config/lumen-bridge/token.json
```

Should contain both `api_token` and `user_token` fields.

---

## Troubleshooting

### "LB_CK_API_TOKEN env var is required"

→ You forgot Step 2. Run:

```bash
export LB_CK_API_TOKEN="954be7451333604dcf1fd26a52365e25fa18f9db3834d150ae0f8656d8b4d0fc"
```

### "Authentication Error - This action could not be completed"

This was the OLD error when the helper page was broken. It's now fixed (deployed 2026-05-14).

If you still see it:
1. Check that https://lorislab.fr/lumen-bridge/auth returns 200 (not 403)
2. Try opening the page directly: https://lorislab.fr/lumen-bridge/auth.html?apiToken=954be7451333604dcf1fd26a52365e25fa18f9db3834d150ae0f8656d8b4d0fc
3. Complete the sign-in there, copy the token, then paste it into the localhost form manually

### "token.json file missing api_token"

The saved token file is corrupt. Delete it and re-run auth:

```bash
rm /root/.config/lumen-bridge/token.json
lumen-bridge auth
```

### CloudKit still returns 401 after auth

Verify the container exists in CloudKit Dashboard:
1. Go to https://icloud.developer.apple.com/dashboard/
2. Select container `iCloud.com.lorislabapp.lumenbridge`
3. Check that Web Services are enabled
4. Check that Private Database has the `FrigateEvent` record type

If the container doesn't exist or isn't configured, that's a separate issue (see main investigation doc).

---

## Next Steps After Success

Once CloudKit writes are working:

1. **Test from iOS app:** Open Lumen for Frigate on your iPhone/iPad. Trigger a detection in Frigate. You should get a push notification.

2. **Verify records in CloudKit:** Go to CloudKit Dashboard → `iCloud.com.lorislabapp.lumenbridge` → Database: Private → `FrigateEvent`. You should see records appearing.

3. **Monitor performance:** The daemon exposes a health endpoint at `http://127.0.0.1:9090/healthz` (accessible only from within the LXC). You can curl it to see stats.

4. **Enable clip uploads (optional):** Edit `/etc/lumen-bridge/config.yaml` and set:
   ```yaml
   frigate:
     base_url: http://192.168.3.160:5000  # your Frigate URL
   ```
   Then restart. The bridge will now attach MP4 clips to events (not just snapshots).

---

## Why This Wasn't Working Before

See `audit-output/cloudkit-auth-investigation.md` for the full technical deep-dive, but in summary:

- ❌ User token file didn't exist (`/root/.config/lumen-bridge/token.json`)
- ❌ Auth command couldn't run because `LB_CK_API_TOKEN` env var wasn't set
- ❌ Helper page was unreachable (403) — now fixed
- ✅ API token was always valid
- ✅ MQTT connection was always working
- ✅ Code is correct

The bridge was doing everything right except authentication. This fix is purely operational, not a code bug.

---

## Time Estimate

- Step 1-2: 30 seconds
- Step 3-5: 2-3 minutes (depends on Apple sign-in speed)
- Step 6-7: 30 seconds
- **Total: ~4 minutes**

Good luck! 🚀
