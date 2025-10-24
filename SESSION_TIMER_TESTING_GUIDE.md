# Session Timer Testing Guide

This guide provides comprehensive instructions for testing the RFC 4028 Session Timer implementation in LiveKit SIP.

## Table of Contents
1. [Unit Tests](#1-unit-tests)
2. [Manual Testing with SIPp](#2-manual-testing-with-sipp)
3. [Integration Testing](#3-integration-testing-with-real-providers)
4. [Testing with Docker](#4-testing-with-docker)
5. [Debugging and Troubleshooting](#5-debugging-and-troubleshooting)

---

## 1. Unit Tests

### Running the Tests

```bash
cd /Users/andrewbull/workplace/livekit_sip/sip

# Run all session timer tests
go test -v ./pkg/sip -run TestSessionTimer

# Run with coverage
go test -v -cover ./pkg/sip -run TestSessionTimer

# Generate coverage report
go test -coverprofile=coverage.out ./pkg/sip -run TestSessionTimer
go tool cover -html=coverage.out -o coverage.html
```

### Expected Output

```
=== RUN   TestSessionTimerNegotiateInvite
=== RUN   TestSessionTimerNegotiateInvite/valid_session_timer_with_refresher=uac
=== RUN   TestSessionTimerNegotiateInvite/valid_session_timer_with_refresher=uas
=== RUN   TestSessionTimerNegotiateInvite/session_interval_too_small
=== RUN   TestSessionTimerNegotiateInvite/no_session_expires_header
--- PASS: TestSessionTimerNegotiateInvite (0.00s)
=== RUN   TestSessionTimerNegotiateResponse
--- PASS: TestSessionTimerNegotiateResponse (0.00s)
=== RUN   TestSessionTimerAddHeadersToRequest
--- PASS: TestSessionTimerAddHeadersToRequest (0.00s)
=== RUN   TestSessionTimerAddHeadersToResponse
--- PASS: TestSessionTimerAddHeadersToResponse (0.00s)
=== RUN   TestSessionTimerRefreshCallback
--- PASS: TestSessionTimerRefreshCallback (0.75s)
=== RUN   TestSessionTimerExpiryCallback
--- PASS: TestSessionTimerExpiryCallback (2.50s)
=== RUN   TestSessionTimerOnRefreshReceived
--- PASS: TestSessionTimerOnRefreshReceived (2.50s)
=== RUN   TestSessionTimerStop
--- PASS: TestSessionTimerStop (1.50s)
PASS
ok      github.com/livekit/sip/pkg/sip  7.250s
```

---

## 2. Manual Testing with SIPp

SIPp is a powerful SIP testing tool that can simulate SIP endpoints with session timer support.

### Install SIPp

```bash
# macOS
brew install sipp

# Ubuntu/Debian
sudo apt-get install sipp

# Build from source
git clone https://github.com/SIPp/sipp.git
cd sipp
./build.sh
```

### SIPp Scenario: UAC with Session Timer

Create a file `uac_with_timer.xml`:

```xml
<?xml version="1.0" encoding="ISO-8859-1" ?>
<!DOCTYPE scenario SYSTEM "sipp.dtd">

<scenario name="UAC with Session Timer">
  <!-- Send INVITE with Session-Expires -->
  <send retrans="500">
    <![CDATA[
      INVITE sip:[service]@[remote_ip]:[remote_port] SIP/2.0
      Via: SIP/2.0/[transport] [local_ip]:[local_port];branch=[branch]
      From: sipp <sip:sipp@[local_ip]:[local_port]>;tag=[pid]SIPpTag00[call_number]
      To: sut <sip:[service]@[remote_ip]:[remote_port]>
      Call-ID: [call_id]
      CSeq: 1 INVITE
      Contact: sip:sipp@[local_ip]:[local_port]
      Max-Forwards: 70
      Subject: Performance Test
      Content-Type: application/sdp
      Content-Length: [len]
      Session-Expires: 1800;refresher=uac
      Min-SE: 90
      Supported: timer

      v=0
      o=user1 53655765 2353687637 IN IP[local_ip_type] [local_ip]
      s=-
      c=IN IP[local_ip_type] [local_ip]
      t=0 0
      m=audio [auto_media_port] RTP/AVP 0 8
      a=rtpmap:0 PCMU/8000
      a=rtpmap:8 PCMA/8000
    ]]>
  </send>

  <!-- Wait for 100 Trying or 180 Ringing -->
  <recv response="100" optional="true" />
  <recv response="180" optional="true" />

  <!-- Wait for 200 OK -->
  <recv response="200" rtd="true">
    <action>
      <ereg regexp="Session-Expires: ([0-9]+)" search_in="hdr" header="Session-Expires:" assign_to="1,session_expires" />
      <ereg regexp="refresher=([a-z]+)" search_in="hdr" header="Session-Expires:" assign_to="1,refresher" />
      <log message="Negotiated Session-Expires: [$session_expires], Refresher: [$refresher]"/>
    </action>
  </recv>

  <!-- Send ACK -->
  <send>
    <![CDATA[
      ACK sip:[service]@[remote_ip]:[remote_port] SIP/2.0
      Via: SIP/2.0/[transport] [local_ip]:[local_port];branch=[branch]
      From: sipp <sip:sipp@[local_ip]:[local_port]>;tag=[pid]SIPpTag00[call_number]
      To: sut <sip:[service]@[remote_ip]:[remote_port]>[peer_tag_param]
      Call-ID: [call_id]
      CSeq: 1 ACK
      Contact: sip:sipp@[local_ip]:[local_port]
      Max-Forwards: 70
      Subject: Performance Test
      Content-Length: 0
    ]]>
  </send>

  <!-- Pause for half the session interval (900 seconds for 1800) -->
  <!-- For testing, use a shorter duration -->
  <pause milliseconds="5000"/>

  <!-- Expect re-INVITE from LiveKit (if LiveKit is refresher) -->
  <recv request="INVITE" optional="true">
    <action>
      <log message="Received session refresh re-INVITE from LiveKit"/>
    </action>
  </recv>

  <!-- Send 200 OK to re-INVITE -->
  <send retrans="500" optional="true">
    <![CDATA[
      SIP/2.0 200 OK
      [last_Via:]
      [last_From:]
      [last_To:]
      [last_Call-ID:]
      [last_CSeq:]
      Contact: <sip:[local_ip]:[local_port];transport=[transport]>
      Content-Type: application/sdp
      Session-Expires: 1800;refresher=uac
      Content-Length: [len]

      v=0
      o=user1 53655765 2353687638 IN IP[local_ip_type] [local_ip]
      s=-
      c=IN IP[local_ip_type] [local_ip]
      t=0 0
      m=audio [auto_media_port] RTP/AVP 0 8
      a=rtpmap:0 PCMU/8000
      a=rtpmap:8 PCMA/8000
    ]]>
  </send>

  <!-- Wait for ACK to re-INVITE -->
  <recv request="ACK" optional="true" />

  <!-- Hold call for a bit -->
  <pause milliseconds="5000"/>

  <!-- Send BYE to end call -->
  <send retrans="500">
    <![CDATA[
      BYE sip:[service]@[remote_ip]:[remote_port] SIP/2.0
      Via: SIP/2.0/[transport] [local_ip]:[local_port];branch=[branch]
      From: sipp <sip:sipp@[local_ip]:[local_port]>;tag=[pid]SIPpTag00[call_number]
      To: sut <sip:[service]@[remote_ip]:[remote_port]>[peer_tag_param]
      Call-ID: [call_id]
      CSeq: 2 BYE
      Contact: sip:sipp@[local_ip]:[local_port]
      Max-Forwards: 70
      Subject: Performance Test
      Content-Length: 0
    ]]>
  </send>

  <!-- Wait for 200 OK to BYE -->
  <recv response="200" />
</scenario>
```

### Run SIPp Test

```bash
# Start LiveKit SIP server first with session timers enabled
# Then run SIPp:

sipp -sf uac_with_timer.xml \
     -s test \
     -m 1 \
     -l 1 \
     -trace_msg \
     -trace_err \
     [livekit-sip-ip]:[livekit-sip-port]
```

### SIPp Scenario: UAS with Session Timer

Create `uas_with_timer.xml`:

```xml
<?xml version="1.0" encoding="ISO-8859-1" ?>
<!DOCTYPE scenario SYSTEM "sipp.dtd">

<scenario name="UAS with Session Timer">
  <!-- Wait for INVITE -->
  <recv request="INVITE">
    <action>
      <ereg regexp="Session-Expires: ([0-9]+)" search_in="hdr" header="Session-Expires:" assign_to="1,session_expires" />
      <log message="Received INVITE with Session-Expires: [$session_expires]"/>
    </action>
  </recv>

  <!-- Send 180 Ringing -->
  <send>
    <![CDATA[
      SIP/2.0 180 Ringing
      [last_Via:]
      [last_From:]
      [last_To:];tag=[pid]SIPpTag01[call_number]
      [last_Call-ID:]
      [last_CSeq:]
      Contact: <sip:[local_ip]:[local_port];transport=[transport]>
      Content-Length: 0
    ]]>
  </send>

  <!-- Send 200 OK with Session-Expires -->
  <send retrans="500">
    <![CDATA[
      SIP/2.0 200 OK
      [last_Via:]
      [last_From:]
      [last_To:];tag=[pid]SIPpTag01[call_number]
      [last_Call-ID:]
      [last_CSeq:]
      Contact: <sip:[local_ip]:[local_port];transport=[transport]>
      Content-Type: application/sdp
      Session-Expires: 1800;refresher=uac
      Require: timer
      Content-Length: [len]

      v=0
      o=user1 53655765 2353687637 IN IP[local_ip_type] [local_ip]
      s=-
      c=IN IP[local_ip_type] [local_ip]
      t=0 0
      m=audio [auto_media_port] RTP/AVP 0 8
      a=rtpmap:0 PCMU/8000
      a=rtpmap:8 PCMA/8000
    ]]>
  </send>

  <!-- Wait for ACK -->
  <recv request="ACK" />

  <!-- Wait for session refresh from LiveKit -->
  <recv request="INVITE">
    <action>
      <log message="Received session refresh re-INVITE"/>
    </action>
  </recv>

  <!-- Respond to refresh -->
  <send>
    <![CDATA[
      SIP/2.0 200 OK
      [last_Via:]
      [last_From:]
      [last_To:]
      [last_Call-ID:]
      [last_CSeq:]
      Contact: <sip:[local_ip]:[local_port];transport=[transport]>
      Content-Type: application/sdp
      Session-Expires: 1800;refresher=uac
      Content-Length: [len]

      v=0
      o=user1 53655765 2353687638 IN IP[local_ip_type] [local_ip]
      s=-
      c=IN IP[local_ip_type] [local_ip]
      t=0 0
      m=audio [auto_media_port] RTP/AVP 0 8
      a=rtpmap:0 PCMU/8000
      a=rtpmap:8 PCMA/8000
    ]]>
  </send>

  <!-- Wait for ACK -->
  <recv request="ACK" />

  <!-- Wait for BYE or send BYE -->
  <recv request="BYE" />

  <send>
    <![CDATA[
      SIP/2.0 200 OK
      [last_Via:]
      [last_From:]
      [last_To:]
      [last_Call-ID:]
      [last_CSeq:]
      Content-Length: 0
    ]]>
  </send>
</scenario>
```

Run as UAS:

```bash
sipp -sf uas_with_timer.xml \
     -p 5060 \
     -trace_msg \
     -trace_screen
```

---

## 3. Integration Testing with Real Providers

### Test with Twilio

1. **Configure LiveKit SIP with session timers enabled:**

```yaml
# config.yaml
session_timer:
  enabled: true
  default_expires: 1800
  min_se: 90
  prefer_refresher: "uac"
  use_update: false
```

2. **Create a SIP Trunk in Twilio** pointing to your LiveKit SIP server

3. **Make a test call** to the trunk and monitor logs:

```bash
tail -f /var/log/livekit-sip.log | grep -i "session"
```

Expected log output:
```
INFO  Negotiated session timer from INVITE  sessionExpires=1800 minSE=90 refresher=uac
INFO  Started session timer  sessionExpires=1800 expiryWarning=1733 weAreRefresher=true
INFO  Sending session refresh
INFO  Session refresh successful
```

### Test with Other Providers

**Telnyx:**
```yaml
# Telnyx typically uses 1800 seconds
session_timer:
  enabled: true
  default_expires: 1800
```

**Vonage/Nexmo:**
```yaml
# May require shorter intervals
session_timer:
  enabled: true
  default_expires: 900
  min_se: 90
```

**Generic SIP Provider:**
Test compatibility by checking their documentation for session timer support.

---

## 4. Testing with Docker

### Create a Test Environment

Create `docker-compose-test.yml`:

```yaml
version: '3.8'

services:
  livekit-sip:
    image: livekit/sip:latest
    ports:
      - "5060:5060/udp"
      - "5060:5060/tcp"
      - "10000-20000:10000-20000/udp"
    volumes:
      - ./config.yaml:/config.yaml
    environment:
      - LIVEKIT_API_KEY=${LIVEKIT_API_KEY}
      - LIVEKIT_API_SECRET=${LIVEKIT_API_SECRET}
      - LIVEKIT_WS_URL=${LIVEKIT_WS_URL}
    command: ["--config", "/config.yaml"]

  sipp-uac:
    image: ctaloi/sipp
    depends_on:
      - livekit-sip
    volumes:
      - ./sipp_scenarios:/scenarios
    command: >
      sipp -sf /scenarios/uac_with_timer.xml
      -s test -m 5 -l 1
      livekit-sip:5060

  sipp-uas:
    image: ctaloi/sipp
    ports:
      - "5070:5060/udp"
    volumes:
      - ./sipp_scenarios:/scenarios
    command: >
      sipp -sf /scenarios/uas_with_timer.xml
      -p 5060
```

Run tests:
```bash
docker-compose -f docker-compose-test.yml up
```

---

## 5. Debugging and Troubleshooting

### Enable Debug Logging

Update `config.yaml`:

```yaml
logging:
  level: debug

session_timer:
  enabled: true
  default_expires: 120  # Short interval for testing
  min_se: 90
  prefer_refresher: "uac"
```

### Monitor SIP Traffic

**Using tcpdump:**
```bash
sudo tcpdump -i any -n port 5060 -A -s 0
```

**Using ngrep:**
```bash
sudo ngrep -d any -W byline port 5060
```

**Using Wireshark:**
1. Start capture on port 5060
2. Filter: `sip`
3. Look for Session-Expires headers in INVITE and 200 OK
4. Verify re-INVITE messages are sent at half the interval

### Key Things to Verify

#### 1. Header Negotiation
Look for these headers in SIP messages:

**In INVITE:**
```
Session-Expires: 1800;refresher=uac
Min-SE: 90
Supported: timer
```

**In 200 OK:**
```
Session-Expires: 1800;refresher=uac
Require: timer
```

#### 2. Refresh Timing
- If session expires = 1800 seconds
- Refresh should occur at ~900 seconds (half interval)
- Check logs for "Sending session refresh" at expected time

#### 3. Refresh re-INVITE
Verify the re-INVITE has:
- Same Call-ID
- Incremented CSeq
- Session-Expires header
- Same SDP (no media change)

**Example re-INVITE:**
```
INVITE sip:user@example.com SIP/2.0
Call-ID: abc123@livekit
CSeq: 2 INVITE
Session-Expires: 1800;refresher=uac
Content-Type: application/sdp
...
```

#### 4. Expiry Handling
Test session expiry by:
1. Starting a call with short interval (120 seconds)
2. Not sending refresh
3. Verify BYE is sent at ~88 seconds (120 - min(32, 120/3))

### Common Issues and Solutions

**Issue: Timer doesn't start**
- Check `enabled: true` in config
- Verify remote endpoint supports `Supported: timer`
- Check logs for negotiation errors

**Solution:**
```bash
grep -i "session timer" /var/log/livekit-sip.log
```

**Issue: Refresh not sent**
- Verify refresher role is correct
- Check timer is started (look for "Started session timer" log)
- Ensure call is still active

**Solution:**
```bash
# Check if LiveKit is the refresher
grep "weAreRefresher=true" /var/log/livekit-sip.log
```

**Issue: Call terminates prematurely**
- Check if refresh was successful
- Verify remote endpoint responds to re-INVITE
- Check for network issues

**Solution:**
Enable packet capture and verify re-INVITE is sent and 200 OK received.

---

## 6. Performance Testing

### Load Test with SIPp

Test multiple concurrent calls with session timers:

```bash
sipp -sf uac_with_timer.xml \
     -s test \
     -r 10 \
     -l 100 \
     -m 1000 \
     -trace_stat \
     [livekit-sip-ip]:5060
```

Parameters:
- `-r 10`: 10 calls per second
- `-l 100`: 100 concurrent calls
- `-m 1000`: 1000 total calls

### Monitor Performance

```bash
# Watch CPU and memory
top -p $(pidof livekit-sip)

# Count active timers (approximate)
lsof -p $(pidof livekit-sip) | grep -c timer

# Monitor goroutines (if metrics exposed)
curl http://localhost:6060/debug/pprof/goroutine?debug=1
```

---

## 7. Quick Test Checklist

- [ ] Unit tests pass
- [ ] Session timer negotiation works (inbound)
- [ ] Session timer negotiation works (outbound)
- [ ] Headers present in INVITE
- [ ] Headers present in 200 OK
- [ ] Timer starts after call establishment
- [ ] Refresh sent at half interval
- [ ] re-INVITE has correct headers
- [ ] re-INVITE maintains dialog state
- [ ] 200 OK to refresh is handled
- [ ] ACK to refresh is sent
- [ ] Timer resets after refresh
- [ ] Session expires without refresh (BYE sent)
- [ ] Timer stops on call termination
- [ ] Works with disabled config
- [ ] Works with various SIP providers

---

## Example Test Session

Here's a complete test session:

```bash
# 1. Start LiveKit SIP with debug logging
cd /Users/andrewbull/workplace/livekit_sip/sip
go run ./cmd/livekit-sip --config config-test.yaml

# 2. In another terminal, run unit tests
go test -v ./pkg/sip -run TestSessionTimer

# 3. Start SIPp UAS
sipp -sf uas_with_timer.xml -p 5070 -trace_msg &

# 4. Make test call from LiveKit to SIPp
# (Trigger via API or test script)

# 5. Monitor logs
tail -f /tmp/livekit-sip.log | grep -E "(session|refresh|timer)"

# 6. Capture packets
sudo tcpdump -i lo0 -w session_timer_test.pcap port 5060 &

# 7. Wait for refresh (or use short interval)
# Watch for re-INVITE in logs and packet capture

# 8. Verify in Wireshark
wireshark session_timer_test.pcap
# Filter: sip.CSeq.method == "INVITE"
# Verify CSeq increments and Session-Expires present
```

---

## Additional Resources

- **RFC 4028**: https://datatracker.ietf.org/doc/html/rfc4028
- **SIPp Documentation**: http://sipp.sourceforge.net/doc/reference.html
- **LiveKit Docs**: https://docs.livekit.io/
- **Wireshark SIP Analysis**: https://wiki.wireshark.org/SIP

---

**Need Help?**
- Check the implementation summary: `RFC_4028_IMPLEMENTATION_SUMMARY.md`
- Review logs with debug level enabled
- Use packet captures to verify SIP message flow
- Test with SIPp before testing with real providers
