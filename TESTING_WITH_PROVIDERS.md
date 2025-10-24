# Testing Session Timers with Real SIP Providers

This guide shows you how to test the RFC 4028 Session Timer implementation with actual SIP providers.

## Quick Start

### 1. Enable Session Timers in Config

Edit your `config.yaml`:

```yaml
session_timer:
  enabled: true
  default_expires: 1800    # 30 minutes
  min_se: 90              # RFC minimum
  prefer_refresher: "uac" # LiveKit as refresher
  use_update: false

# Enable debug logging to see what's happening
logging:
  level: debug
```

### 2. Restart LiveKit SIP

```bash
# If running as a service
sudo systemctl restart livekit-sip

# If running directly
./livekit-sip --config config.yaml
```

### 3. Make a Test Call

The easiest way is through LiveKit's dashboard or API.

---

## Testing with Twilio

Twilio supports RFC 4028 session timers by default.

### Setup

1. **Create a SIP Trunk** in Twilio Console:
   - Go to: https://console.twilio.com/us1/develop/phone-numbers/sip-trunking
   - Create a new trunk
   - Set Origination URI to: `sip:your-livekit-server.com:5060`

2. **Configure LiveKit SIP** to accept calls from Twilio:

```yaml
# In your SIP trunk configuration
trunks:
  - name: "twilio"
    numbers: ["+15551234567"]
    auth_username: "twilio_user"
    auth_password: "your_secure_password"

session_timer:
  enabled: true
  default_expires: 1800
  min_se: 90
  prefer_refresher: "uac"
```

3. **Make a test call**:
   - Call the Twilio number from your phone
   - Or use Twilio's API to make an outbound call

### What to Look For

**In LiveKit logs:**
```bash
tail -f /var/log/livekit-sip.log | grep -i session
```

You should see:
```
INFO  processing invite  fromIP=54.172.60.0  # Twilio IP
INFO  Negotiated session timer from INVITE  sessionExpires=1800 minSE=90 refresher=uac
INFO  Started session timer  sessionExpires=1800 expiryWarning=1733s weAreRefresher=true
```

After ~15 minutes (900 seconds):
```
INFO  Sending session refresh
INFO  Session refresh successful
```

**Verify with tcpdump:**
```bash
sudo tcpdump -i any -n 'host 54.172.60.0 and port 5060' -A
```

Look for the re-INVITE with `Session-Expires` header.

---

## Testing with Telnyx

Telnyx also supports session timers.

### Setup

1. **Create a SIP Connection** in Telnyx Portal:
   - Go to: https://portal.telnyx.com/#/app/connections
   - Create a new "IP" connection
   - Add your LiveKit server IP to ACL
   - Enable authentication (optional)

2. **Configure LiveKit:**

```yaml
trunks:
  - name: "telnyx"
    outbound_address: "sip.telnyx.com"
    numbers: ["+15559876543"]
    auth_username: "your_telnyx_username"
    auth_password: "your_telnyx_password"

session_timer:
  enabled: true
  default_expires: 1800
  min_se: 90
  prefer_refresher: "uac"
```

3. **Test Outbound Call:**

```bash
# Using LiveKit CLI (if you have it)
lk sip create-dispatch-rule \
  --url YOUR_LIVEKIT_URL \
  --api-key YOUR_API_KEY \
  --api-secret YOUR_API_SECRET \
  --trunk-id telnyx \
  --to "+15551234567"
```

Or via API:

```python
import requests

response = requests.post(
    "https://your-livekit-server.com/twirp/livekit.SIPService/CreateSIPParticipant",
    headers={
        "Authorization": "Bearer YOUR_TOKEN",
        "Content-Type": "application/json"
    },
    json={
        "sip_trunk_id": "telnyx",
        "sip_call_to": "+15551234567",
        "room_name": "test-room",
        "participant_identity": "sip-user"
    }
)
print(response.json())
```

### Monitor the Call

```bash
# Watch logs
docker logs -f livekit-sip | grep -E "(session|refresh|timer)"
```

Expected output:
```
INFO  Adding session timer headers to INVITE
INFO  Negotiated session timer from response  sessionExpires=1800 refresher=uac
INFO  Started session timer after call is established
INFO  [15 min later] Sending session refresh
INFO  Session refresh successful
```

---

## Testing with Vonage (Nexmo)

### Setup

1. **Create SIP Endpoint** in Vonage Dashboard:
   - Go to: https://dashboard.nexmo.com/
   - Create a SIP endpoint

2. **Configure LiveKit:**

```yaml
trunks:
  - name: "vonage"
    outbound_address: "sip.nexmo.com"
    numbers: ["+15554445555"]
    auth_username: "your_vonage_username"
    auth_password: "your_vonage_password"

session_timer:
  enabled: true
  default_expires: 900  # Vonage may prefer shorter intervals
  min_se: 90
  prefer_refresher: "uac"
```

---

## Real-World Testing Scenarios

### Scenario 1: Long-Duration Call (Test Refresh)

**Goal:** Verify session refresh happens automatically

```yaml
session_timer:
  enabled: true
  default_expires: 300  # 5 minutes (short for testing)
  min_se: 90
```

**Steps:**
1. Make a call through your provider
2. Let it run for 6+ minutes
3. Check logs for refresh at 2.5 minutes
4. Verify call doesn't drop

**Expected behavior:**
- At 2.5 min: "Sending session refresh"
- At 2.5 min: "Session refresh successful"
- At 5.0 min: Second refresh
- Call continues normally

### Scenario 2: Session Expiry (Test Timeout)

**Goal:** Verify call terminates if refresh fails

**Steps:**
1. Make a call with session timer enabled
2. Block re-INVITE responses with firewall:
   ```bash
   # Temporarily block SIP responses (CAREFUL!)
   sudo iptables -A INPUT -p udp --sport 5060 -m string --string "CSeq: 2 INVITE" --algo bm -j DROP
   ```
3. Wait for expiry time
4. Verify BYE is sent

**Expected behavior:**
- At ~4.5 min: "Session timer expired, terminating call"
- Call ends gracefully with BYE

**Cleanup:**
```bash
sudo iptables -D INPUT -p udp --sport 5060 -m string --string "CSeq: 2 INVITE" --algo bm -j DROP
```

### Scenario 3: Provider Without Session Timer Support

**Goal:** Verify graceful fallback

**Steps:**
1. Configure session timers enabled
2. Call a provider that doesn't support session timers
3. Verify call works normally without timers

**Expected behavior:**
```
INFO  No Session-Expires header in response
INFO  Session timer not negotiated (remote doesn't support)
```

Call proceeds normally without session timer.

---

## Debugging Real Provider Issues

### Issue: Provider rejects calls with 501 Not Implemented

**Symptom:** Calls fail when session timer is enabled

**Diagnosis:**
```bash
tcpdump -i any -n port 5060 -A | grep -A 5 "501"
```

**Solution:** The provider doesn't support session timers. Disable them:
```yaml
session_timer:
  enabled: false
```

### Issue: Provider uses different interval

**Symptom:** Provider responds with different `Session-Expires` value

**Diagnosis:**
```bash
grep "Negotiated session timer" /var/log/livekit-sip.log
```

Output might show:
```
INFO  Negotiated session timer from response  sessionExpires=600 refresher=uac
```

**Solution:** Provider accepted but modified the interval. This is normal - use the negotiated value.

### Issue: Provider sets refresher=uas (they want to refresh)

**Symptom:** No refresh from LiveKit, but call stays alive

**Diagnosis:**
```bash
grep "weAreRefresher=false" /var/log/livekit-sip.log
```

**Solution:** This is correct! The provider is refreshing. You should see:
```
INFO  Started session timer  weAreRefresher=false
```

LiveKit will monitor for incoming refreshes and terminate if none arrive.

---

## Packet Capture Analysis

### Capture During Call

```bash
# Start capture before making call
sudo tcpdump -i any -w session_timer_capture.pcap 'port 5060'

# Make your test call
# ... wait for refresh ...
# End call

# Stop capture (Ctrl+C)
```

### Analyze with Wireshark

1. Open `session_timer_capture.pcap` in Wireshark
2. Apply filter: `sip`
3. Look for message sequence:

```
1. INVITE (with Session-Expires: 1800;refresher=uac) →
2. ← 100 Trying
3. ← 180 Ringing
4. ← 200 OK (with Session-Expires: 1800;refresher=uac)
5. ACK →
   ... call active ...
6. INVITE (with CSeq: 2, Session-Expires: 1800) →  [This is the refresh!]
7. ← 200 OK
8. ACK →
   ... call continues ...
9. BYE →
10. ← 200 OK
```

### Key Things to Verify

✅ **Initial INVITE has:**
- `Session-Expires: 1800;refresher=uac`
- `Min-SE: 90`
- `Supported: timer`

✅ **200 OK response has:**
- `Session-Expires: 1800;refresher=uac` (or negotiated value)
- `Require: timer`

✅ **Refresh re-INVITE has:**
- Same Call-ID as original
- Incremented CSeq (e.g., CSeq: 2 INVITE)
- `Session-Expires` header
- Same SDP body (no media change)

---

## Provider-Specific Notes

### Twilio
- ✅ Full RFC 4028 support
- Default interval: 1800 seconds
- Always responds with `Session-Expires` in 200 OK
- Prefers UAC as refresher

### Telnyx
- ✅ Full RFC 4028 support
- Default interval: 1800 seconds
- Flexible refresher role
- Good for testing

### Vonage/Nexmo
- ✅ Supports session timers
- May prefer shorter intervals (900-1200 seconds)
- Test with: `default_expires: 900`

### Bandwidth
- ✅ Supports session timers
- Standard implementation
- Works well with default settings

### Twilio Elastic SIP Trunking
- ✅ Full support
- Handles refresh automatically on their side if they're refresher
- Good for production use

---

## Production Deployment Checklist

Before deploying to production:

- [ ] Test with your specific SIP provider
- [ ] Verify session timers negotiate correctly
- [ ] Test at least one full refresh cycle (15-30 min call)
- [ ] Monitor for any 501/405 errors
- [ ] Check provider documentation for session timer support
- [ ] Test both inbound and outbound calls
- [ ] Verify call quality is not affected
- [ ] Monitor CPU/memory impact with timers enabled
- [ ] Test failover scenarios
- [ ] Document your provider's session timer behavior

---

## Quick Provider Test

Here's a simple test script you can run:

```bash
#!/bin/bash

echo "Testing Session Timers with SIP Provider"
echo "========================================="
echo ""

# 1. Check config
echo "1. Checking configuration..."
if grep -q "enabled: true" config.yaml; then
    echo "✅ Session timers enabled"
else
    echo "❌ Session timers not enabled in config.yaml"
    exit 1
fi

# 2. Start log monitoring
echo ""
echo "2. Starting log monitor..."
echo "   (Open another terminal to make a test call)"
echo ""
tail -f /var/log/livekit-sip.log | while read line; do
    if echo "$line" | grep -q "Negotiated session timer"; then
        echo "✅ Session timer negotiated"
        echo "   $line"
    fi

    if echo "$line" | grep -q "Started session timer"; then
        echo "✅ Timer started"
        echo "   $line"
    fi

    if echo "$line" | grep -q "Sending session refresh"; then
        echo "✅ Refresh sent (at half-interval)"
        echo "   $line"
    fi

    if echo "$line" | grep -q "Session refresh successful"; then
        echo "✅ Refresh successful"
        echo "   $line"
    fi

    if echo "$line" | grep -q "Session timer expired"; then
        echo "⚠️  Session expired (no refresh received)"
        echo "   $line"
    fi
done
```

Save as `test_session_timer.sh`, make executable:
```bash
chmod +x test_session_timer.sh
./test_session_timer.sh
```

Then make a test call and watch the output!

---

## Getting Help

If session timers aren't working with your provider:

1. **Check logs** for negotiation failures
2. **Capture packets** to see actual SIP messages
3. **Contact provider support** - ask about RFC 4028 support
4. **Try different settings**:
   ```yaml
   session_timer:
     enabled: true
     default_expires: 1800  # Try: 900, 1200, 1800
     prefer_refresher: "uas" # Try switching roles
   ```

5. **Disable if not needed**:
   ```yaml
   session_timer:
     enabled: false
   ```

Most modern SIP providers support session timers, but some legacy systems may not. The implementation gracefully falls back when not supported.
