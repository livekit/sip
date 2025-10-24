# Local Testing Guide for Session Timers

Complete guide to test RFC 4028 Session Timers locally on your Mac without needing external SIP providers.

## Quick Setup (5 Minutes)

### 1. Install Dependencies

```bash
# Install Homebrew if you don't have it
/bin/bash -c "$(curl -fsSL https://raw.githubusercontent.com/Homebrew/install/HEAD/install.sh)"

# Install SIPp (SIP testing tool)
brew install sipp

# Install tcpdump (for packet capture) - usually pre-installed on Mac
which tcpdump || echo "tcpdump not found"
```

### 2. Build LiveKit SIP

```bash
cd /Users/andrewbull/workplace/livekit_sip/sip

# Build the binary
go build -o livekit-sip ./cmd/livekit-sip
```

### 3. Create Test Configuration

Create `config-local-test.yaml`:

```yaml
# LiveKit connection (use your actual LiveKit server or local instance)
api_key: "your-api-key"
api_secret: "your-api-secret"
ws_url: "ws://localhost:7880"  # or your LiveKit server

# Redis (required)
redis:
  address: "localhost:6379"

# SIP settings
sip_port: 5060
rtp_port:
  start: 10000
  end: 20000

# Enable session timers with SHORT intervals for testing
session_timer:
  enabled: true
  default_expires: 120  # 2 minutes (for quick testing)
  min_se: 30            # 30 seconds minimum
  prefer_refresher: "uac"
  use_update: false

# Debug logging
logging:
  level: debug
```

---

## Option 1: Test with SIPp (Recommended)

SIPp is a SIP protocol test tool that can simulate real SIP endpoints.

### Create SIPp Test Scenario

Create a file `test_session_timer.xml`:

```xml
<?xml version="1.0" encoding="ISO-8859-1" ?>
<!DOCTYPE scenario SYSTEM "sipp.dtd">

<scenario name="Session Timer Test">
  <!-- Send INVITE with Session-Expires -->
  <send retrans="500">
    <![CDATA[
      INVITE sip:test@127.0.0.1:5060 SIP/2.0
      Via: SIP/2.0/UDP 127.0.0.1:5070;branch=[branch]
      From: sipp <sip:sipp@127.0.0.1:5070>;tag=[call_number]
      To: test <sip:test@127.0.0.1:5060>
      Call-ID: [call_id]
      CSeq: 1 INVITE
      Contact: sip:sipp@127.0.0.1:5070
      Max-Forwards: 70
      Content-Type: application/sdp
      Content-Length: [len]
      Session-Expires: 120;refresher=uac
      Min-SE: 30
      Supported: timer

      v=0
      o=user1 53655765 2353687637 IN IP4 127.0.0.1
      s=-
      c=IN IP4 127.0.0.1
      t=0 0
      m=audio 6000 RTP/AVP 0 8
      a=rtpmap:0 PCMU/8000
      a=rtpmap:8 PCMA/8000
    ]]>
  </send>

  <!-- Wait for 100/180/183 -->
  <recv response="100" optional="true" />
  <recv response="180" optional="true" />
  <recv response="183" optional="true" />

  <!-- Wait for 200 OK -->
  <recv response="200" rtd="true">
    <action>
      <ereg regexp="Session-Expires: ([0-9]+)" search_in="hdr" header="Session-Expires:" assign_to="session_expires" />
      <ereg regexp="refresher=([a-z]+)" search_in="hdr" header="Session-Expires:" assign_to="refresher" />
      <log message="✅ Got 200 OK with Session-Expires: [$session_expires], Refresher: [$refresher]"/>
    </action>
  </recv>

  <!-- Send ACK -->
  <send>
    <![CDATA[
      ACK sip:test@127.0.0.1:5060 SIP/2.0
      Via: SIP/2.0/UDP 127.0.0.1:5070;branch=[branch]
      From: sipp <sip:sipp@127.0.0.1:5070>;tag=[call_number]
      To: test <sip:test@127.0.0.1:5060>[peer_tag_param]
      Call-ID: [call_id]
      CSeq: 1 ACK
      Contact: sip:sipp@127.0.0.1:5070
      Max-Forwards: 70
      Content-Length: 0
    ]]>
  </send>

  <!-- Wait for session refresh from LiveKit (should come at 60 seconds) -->
  <recv request="INVITE" timeout="70000">
    <action>
      <ereg regexp="CSeq: ([0-9]+)" search_in="hdr" header="CSeq:" assign_to="cseq" />
      <log message="✅ Received session refresh re-INVITE with CSeq: [$cseq]"/>
    </action>
  </recv>

  <!-- Send 200 OK to refresh -->
  <send retrans="500">
    <![CDATA[
      SIP/2.0 200 OK
      [last_Via:]
      [last_From:]
      [last_To:]
      [last_Call-ID:]
      [last_CSeq:]
      Contact: <sip:127.0.0.1:5070>
      Content-Type: application/sdp
      Session-Expires: 120;refresher=uac
      Content-Length: [len]

      v=0
      o=user1 53655765 2353687638 IN IP4 127.0.0.1
      s=-
      c=IN IP4 127.0.0.1
      t=0 0
      m=audio 6000 RTP/AVP 0 8
      a=rtpmap:0 PCMU/8000
      a=rtpmap:8 PCMA/8000
    ]]>
  </send>

  <!-- Wait for ACK -->
  <recv request="ACK">
    <action>
      <log message="✅ Session refresh complete!"/>
    </action>
  </recv>

  <!-- Hold call for a bit -->
  <pause milliseconds="5000"/>

  <!-- Send BYE to end call -->
  <send retrans="500">
    <![CDATA[
      BYE sip:test@127.0.0.1:5060 SIP/2.0
      Via: SIP/2.0/UDP 127.0.0.1:5070;branch=[branch]
      From: sipp <sip:sipp@127.0.0.1:5070>;tag=[call_number]
      To: test <sip:test@127.0.0.1:5060>[peer_tag_param]
      Call-ID: [call_id]
      CSeq: 2 BYE
      Contact: sip:sipp@127.0.0.1:5070
      Max-Forwards: 70
      Content-Length: 0
    ]]>
  </send>

  <!-- Wait for 200 OK to BYE -->
  <recv response="200" />

  <log message="✅ Test complete!"/>
</scenario>
```

### Run the Test

**Terminal 1 - Start LiveKit SIP:**
```bash
cd /Users/andrewbull/workplace/livekit_sip/sip

# Start with test config
./livekit-sip --config config-local-test.yaml
```

**Terminal 2 - Start packet capture (optional but recommended):**
```bash
sudo tcpdump -i lo0 -w /tmp/session_timer_test.pcap 'port 5060'
```

**Terminal 3 - Run SIPp test:**
```bash
cd /Users/andrewbull/workplace/livekit_sip

sipp -sf test_session_timer.xml \
     -m 1 \
     -l 1 \
     -p 5070 \
     -trace_screen \
     127.0.0.1:5060
```

### What You Should See

**In LiveKit SIP logs (Terminal 1):**
```
DEBUG processing invite fromIP=127.0.0.1
DEBUG Negotiated session timer from INVITE sessionExpires=120 minSE=30 refresher=uac
INFO  Started session timer sessionExpires=120 expiryWarning=88s weAreRefresher=true
INFO  Accepting the call

[... 60 seconds later ...]

INFO  Sending session refresh
DEBUG Created re-INVITE with CSeq: 2
INFO  Session refresh successful
```

**In SIPp output (Terminal 3):**
```
✅ Got 200 OK with Session-Expires: 120, Refresher: uac
✅ Received session refresh re-INVITE with CSeq: 2
✅ Session refresh complete!
✅ Test complete!

------------------------------ Test Terminated --------------------------------

----------------------------- Statistics Screen -------
  Successful call(s)        : 1
  Failed call(s)            : 0
```

---

## Option 2: Test with Linphone (GUI Application)

Linphone is a free SIP softphone with a GUI that you can use for manual testing.

### Install Linphone

```bash
brew install --cask linphone
```

Or download from: https://www.linphone.org/

### Configure Linphone

1. Open Linphone
2. Go to Preferences → Network → Ports
   - Set SIP port to 5070 (to avoid conflict with LiveKit)
3. Go to Preferences → Advanced
   - Enable "Session timers"
   - Set session timer interval to 120 seconds

### Manual Test

1. **Start LiveKit SIP** in Terminal 1
2. **In Linphone**, make a call to: `test@127.0.0.1:5060`
3. **Monitor LiveKit logs** - you should see session timer negotiation
4. **Wait 60 seconds** - LiveKit should send a refresh re-INVITE
5. **Keep call active for 2+ minutes** to see multiple refreshes

---

## Option 3: Test with Command-Line Tools

### Using PJSUA (PJSIP User Agent)

Install PJSIP:
```bash
brew install pjproject
```

Create a config file `pjsua.cfg`:
```
--id=sip:test@127.0.0.1
--registrar=sip:127.0.0.1:5060
--realm=*
--username=test
--password=test
```

Run:
```bash
pjsua --config-file=pjsua.cfg
```

Make a call:
```
>>> call sip:test@127.0.0.1:5060
```

---

## Automated Test Script

Create `run_local_test.sh`:

```bash
#!/bin/bash

echo "🧪 LiveKit SIP Session Timer Local Test"
echo "========================================"
echo ""

# Colors
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
RED='\033[0;31m'
NC='\033[0m' # No Color

# Check if SIPp is installed
if ! command -v sipp &> /dev/null; then
    echo -e "${RED}❌ SIPp not found. Install with: brew install sipp${NC}"
    exit 1
fi

# Check if LiveKit SIP is built
if [ ! -f "./sip/livekit-sip" ]; then
    echo -e "${YELLOW}⚠️  Building LiveKit SIP...${NC}"
    cd sip && go build -o livekit-sip ./cmd/livekit-sip
    if [ $? -ne 0 ]; then
        echo -e "${RED}❌ Build failed${NC}"
        exit 1
    fi
    cd ..
fi

# Create test config if it doesn't exist
if [ ! -f "config-local-test.yaml" ]; then
    echo -e "${YELLOW}⚠️  Creating test configuration...${NC}"
    cat > config-local-test.yaml << 'EOF'
api_key: "test-key"
api_secret: "test-secret"
ws_url: "ws://localhost:7880"

redis:
  address: "localhost:6379"

sip_port: 5060
rtp_port:
  start: 10000
  end: 20000

session_timer:
  enabled: true
  default_expires: 120
  min_se: 30
  prefer_refresher: "uac"
  use_update: false

logging:
  level: debug
EOF
fi

# Start LiveKit SIP in background
echo -e "${YELLOW}▶️  Starting LiveKit SIP...${NC}"
./sip/livekit-sip --config config-local-test.yaml > /tmp/livekit-sip-test.log 2>&1 &
LIVEKIT_PID=$!

# Wait for it to start
sleep 2

# Check if it's running
if ! ps -p $LIVEKIT_PID > /dev/null; then
    echo -e "${RED}❌ LiveKit SIP failed to start${NC}"
    cat /tmp/livekit-sip-test.log
    exit 1
fi

echo -e "${GREEN}✅ LiveKit SIP running (PID: $LIVEKIT_PID)${NC}"

# Start packet capture in background
echo -e "${YELLOW}📦 Starting packet capture...${NC}"
sudo tcpdump -i lo0 -w /tmp/session_timer_test.pcap 'port 5060' > /dev/null 2>&1 &
TCPDUMP_PID=$!
sleep 1

# Monitor logs in background
tail -f /tmp/livekit-sip-test.log | while read line; do
    if echo "$line" | grep -q "Negotiated session timer"; then
        echo -e "${GREEN}✅ Session timer negotiated${NC}"
    fi
    if echo "$line" | grep -q "Started session timer"; then
        echo -e "${GREEN}✅ Timer started${NC}"
    fi
    if echo "$line" | grep -q "Sending session refresh"; then
        echo -e "${GREEN}✅ Refresh sent (at 60 seconds)${NC}"
    fi
    if echo "$line" | grep -q "Session refresh successful"; then
        echo -e "${GREEN}✅ Refresh successful${NC}"
    fi
done &
TAIL_PID=$!

# Run SIPp test
echo -e "${YELLOW}📞 Running SIPp test...${NC}"
echo ""
sipp -sf test_session_timer.xml \
     -m 1 \
     -l 1 \
     -p 5070 \
     127.0.0.1:5060

SIPP_RESULT=$?

# Cleanup
echo ""
echo -e "${YELLOW}🧹 Cleaning up...${NC}"
kill $LIVEKIT_PID 2>/dev/null
kill $TCPDUMP_PID 2>/dev/null
kill $TAIL_PID 2>/dev/null

# Results
echo ""
echo "========================================"
if [ $SIPP_RESULT -eq 0 ]; then
    echo -e "${GREEN}✅ TEST PASSED${NC}"
    echo ""
    echo "📊 Results:"
    echo "  - Logs: /tmp/livekit-sip-test.log"
    echo "  - Packet capture: /tmp/session_timer_test.pcap"
    echo ""
    echo "View packet capture:"
    echo "  wireshark /tmp/session_timer_test.pcap"
else
    echo -e "${RED}❌ TEST FAILED${NC}"
    echo ""
    echo "Check logs:"
    echo "  tail -100 /tmp/livekit-sip-test.log"
fi
echo "========================================"
```

Make it executable and run:
```bash
chmod +x run_local_test.sh
./run_local_test.sh
```

---

## Verify Results

### Check Packet Capture

```bash
# Install Wireshark if you don't have it
brew install --cask wireshark

# Open the capture
wireshark /tmp/session_timer_test.pcap
```

In Wireshark:
1. Filter: `sip`
2. Look for the sequence:
   - Initial INVITE with `Session-Expires: 120`
   - 200 OK with `Session-Expires: 120`
   - Re-INVITE with `CSeq: 2` (about 60 seconds later)
   - 200 OK to re-INVITE

### Analyze Logs

```bash
# See the full conversation
cat /tmp/livekit-sip-test.log | grep -i session

# Check timing
cat /tmp/livekit-sip-test.log | grep "session refresh"
```

---

## Quick Verification Checklist

Run this after your test:

```bash
#!/bin/bash

echo "Session Timer Test Verification"
echo "================================"

LOG="/tmp/livekit-sip-test.log"

# Check negotiation
if grep -q "Negotiated session timer" $LOG; then
    echo "✅ Session timer negotiated"
else
    echo "❌ No negotiation found"
fi

# Check timer started
if grep -q "Started session timer" $LOG; then
    echo "✅ Timer started"
else
    echo "❌ Timer not started"
fi

# Check refresh sent
if grep -q "Sending session refresh" $LOG; then
    echo "✅ Refresh sent"
else
    echo "⚠️  No refresh sent (call may have been too short)"
fi

# Check refresh successful
if grep -q "Session refresh successful" $LOG; then
    echo "✅ Refresh successful"
else
    echo "⚠️  No successful refresh recorded"
fi

# Check for errors
if grep -q "ERROR" $LOG; then
    echo "⚠️  Errors found in logs"
    grep "ERROR" $LOG
fi
```

---

## Troubleshooting Local Tests

### SIPp fails to connect

**Error:** `Cannot bind to local port`

**Fix:**
```bash
# Check what's using port 5070
lsof -i :5070

# Kill if needed or use different port
sipp -sf test_session_timer.xml -p 5071 127.0.0.1:5060
```

### LiveKit SIP won't start

**Error:** `address already in use`

**Fix:**
```bash
# Check what's using port 5060
lsof -i :5060

# Kill the process or use different port in config
```

### Redis not running

**Error:** `cannot connect to redis`

**Fix:**
```bash
# Start Redis
brew services start redis

# Or use Docker
docker run -d -p 6379:6379 redis:alpine
```

### No refresh happening

**Cause:** Call duration too short

**Fix:** Update the SIPp scenario to wait longer:
```xml
<!-- Wait for refresh (120s session, refresh at 60s) -->
<pause milliseconds="65000"/>  <!-- Increase this -->
```

---

## Next Steps

Once local testing works:

1. ✅ Unit tests pass
2. ✅ SIPp test passes
3. ✅ Session timer negotiates
4. ✅ Refresh happens at correct time
5. → Test with real SIP provider (see `TESTING_WITH_PROVIDERS.md`)
6. → Load testing with multiple concurrent calls
7. → Integration with your LiveKit rooms

---

## Additional Test Scenarios

### Test Expiry (No Refresh)

Modify the SIPp scenario to NOT respond to re-INVITE:

```xml
<!-- Don't respond to re-INVITE -->
<recv request="INVITE" optional="true" timeout="70000" />
<!-- No 200 OK sent - let it expire -->

<!-- Wait for BYE from LiveKit due to expiry -->
<recv request="BYE" timeout="70000">
    <action>
        <log message="✅ Received BYE due to session expiry"/>
    </action>
</recv>
```

### Test 422 Response (Interval Too Small)

Create scenario that requests very short interval:

```xml
Session-Expires: 20;refresher=uac
Min-SE: 20
```

LiveKit should reject with 422 and suggest Min-SE: 30

---

## Quick Debug Commands

```bash
# Watch live logs with color highlighting
tail -f /tmp/livekit-sip-test.log | grep --color=always -E "session|refresh|timer|INVITE"

# Count successful refreshes
grep -c "Session refresh successful" /tmp/livekit-sip-test.log

# See timing between refresh and response
grep "Session refresh" /tmp/livekit-sip-test.log | cat -n

# Extract all session timer logs
grep -i "session" /tmp/livekit-sip-test.log > session_timer_debug.txt
```

That's it! You now have everything you need to test session timers locally. Start with the automated script (`run_local_test.sh`) for the quickest results.
