// Copyright 2026 LiveKit, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package synchronizer

import (
	"testing"
	"time"

	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
	"github.com/stretchr/testify/require"

	"github.com/livekit/media-sdk/jitter"
)

// --- Test helpers ---

type mockTrackRemote struct {
	id    string
	codec webrtc.RTPCodecParameters
	kind  webrtc.RTPCodecType
	ssrc  webrtc.SSRC
}

func (m *mockTrackRemote) ID() string                       { return m.id }
func (m *mockTrackRemote) Codec() webrtc.RTPCodecParameters { return m.codec }
func (m *mockTrackRemote) Kind() webrtc.RTPCodecType        { return m.kind }
func (m *mockTrackRemote) SSRC() webrtc.SSRC                { return m.ssrc }

func newMockAudioTrack(id string, ssrc uint32) *mockTrackRemote {
	return &mockTrackRemote{
		id:    id,
		codec: webrtc.RTPCodecParameters{RTPCodecCapability: webrtc.RTPCodecCapability{ClockRate: 48000}},
		kind:  webrtc.RTPCodecTypeAudio,
		ssrc:  webrtc.SSRC(ssrc),
	}
}

func newMockVideoTrack(id string, ssrc uint32) *mockTrackRemote {
	return &mockTrackRemote{
		id:    id,
		codec: webrtc.RTPCodecParameters{RTPCodecCapability: webrtc.RTPCodecCapability{ClockRate: 90000}},
		kind:  webrtc.RTPCodecTypeVideo,
		ssrc:  webrtc.SSRC(ssrc),
	}
}

func makeExtPacket(ts uint32, sn uint16, receivedAt time.Time) jitter.ExtPacket {
	return jitter.ExtPacket{
		ReceivedAt: receivedAt,
		Packet:     &rtp.Packet{Header: rtp.Header{Timestamp: ts, SequenceNumber: sn}},
	}
}

// --- Tests ---

func TestSyncEngine_ImplementsSyncInterface(t *testing.T) {
	// Compile-time check that SyncEngine implements Sync and that
	// syncEngineTrack implements TrackSync. Without the second assertion,
	// dropping or breaking a TrackSync method would only surface at the
	// AddTrack return-type assignment sites — which would be reported as a
	// confusing assignment error rather than a missing-method error.
	var _ Sync = (*SyncEngine)(nil)
	var _ TrackSync = (*syncEngineTrack)(nil)
}

func TestSyncEngine_FallbackToWallClockBeforeSRs(t *testing.T) {
	engine := NewSyncEngine()

	track := newMockAudioTrack("audio-1", 1000)
	ts := engine.AddTrack(track, "alice")

	now := time.Now()

	// Prime the track with the first packet.
	pkt0 := makeExtPacket(0, 0, now)
	_, _, done := ts.PrimeForStart(pkt0)
	require.True(t, done, "without start gate, track should be ready immediately")

	// Get PTS for first packet (same as prime packet).
	pts0, err := ts.GetPTS(pkt0)
	require.NoError(t, err)
	require.GreaterOrEqual(t, int64(pts0), int64(0), "first PTS should be >= 0")

	// Second packet 100ms later.
	pkt1 := makeExtPacket(4800, 1, now.Add(100*time.Millisecond))
	pts1, err := ts.GetPTS(pkt1)
	require.NoError(t, err)
	require.Greater(t, int64(pts1), int64(0), "second packet PTS should be > 0")
	require.Greater(t, int64(pts1), int64(pts0), "PTS should advance")
}

func TestSyncEngine_TransitionsToNTPAfterSRs(t *testing.T) {
	engine := NewSyncEngine()

	track := newMockAudioTrack("audio-1", 1000)
	ts := engine.AddTrack(track, "alice")

	now := time.Now()

	// Prime and get initial wall-clock PTS.
	pkt0 := makeExtPacket(0, 0, now)
	ts.PrimeForStart(pkt0)
	pts0, err := ts.GetPTS(pkt0)
	require.NoError(t, err)

	// Get a wall-clock PTS at 500ms.
	pkt1 := makeExtPacket(24000, 1, now.Add(500*time.Millisecond))
	pts1, err := ts.GetPTS(pkt1)
	require.NoError(t, err)
	require.Greater(t, int64(pts1), int64(pts0))

	// Feed 5 sender reports to make NTP estimator ready (minSamplesReady=4,
	// so 3 was not enough — the original test was passing only because it
	// never actually exercised the NTP path).
	for i := 0; i < 5; i++ {
		srTime := now.Add(time.Duration(i) * time.Second)
		rtpTS := uint32(i) * 48000
		ntpTime := ntpToUint64(srTime)
		sr := makeSenderReport(1000, ntpTime, rtpTS)
		engine.OnRTCP(sr)
	}

	// Get PTS after NTP transition - should still be valid and advancing.
	pkt2 := makeExtPacket(48000, 2, now.Add(time.Second))
	pts2, err := ts.GetPTS(pkt2)
	require.NoError(t, err)
	require.Greater(t, int64(pts2), int64(pts1), "PTS should continue to advance after NTP transition")
}

func TestSyncEngine_MonotonicPTS(t *testing.T) {
	engine := NewSyncEngine()

	track := newMockAudioTrack("audio-1", 1000)
	ts := engine.AddTrack(track, "alice")

	now := time.Now()

	// Prime with first packet.
	pkt0 := makeExtPacket(0, 0, now)
	ts.PrimeForStart(pkt0)

	var lastPTS time.Duration
	for i := 0; i < 100; i++ {
		recvAt := now.Add(time.Duration(i) * 20 * time.Millisecond)
		rtpTS := uint32(i) * 960 // 20ms at 48kHz
		pkt := makeExtPacket(rtpTS, uint16(i), recvAt)
		pts, err := ts.GetPTS(pkt)
		require.NoError(t, err)
		require.GreaterOrEqual(t, int64(pts), int64(lastPTS),
			"PTS must be monotonically non-decreasing: packet %d got %v, last was %v", i, pts, lastPTS)
		lastPTS = pts
	}
}

func TestSyncEngine_EndDrain(t *testing.T) {
	engine := NewSyncEngine()

	track := newMockAudioTrack("audio-1", 1000)
	ts := engine.AddTrack(track, "alice")

	now := time.Now()

	// Prime and push some packets.
	pkt0 := makeExtPacket(0, 0, now)
	ts.PrimeForStart(pkt0)
	ts.GetPTS(pkt0)

	for i := 1; i <= 10; i++ {
		recvAt := now.Add(time.Duration(i) * 20 * time.Millisecond)
		rtpTS := uint32(i) * 960
		pkt := makeExtPacket(rtpTS, uint16(i), recvAt)
		ts.GetPTS(pkt)
	}

	require.Equal(t, int64(0), engine.GetEndedAt(), "endedAt should be 0 before End()")

	engine.End()

	require.Greater(t, engine.GetEndedAt(), int64(0), "endedAt should be > 0 after End()")
	require.Greater(t, engine.GetStartedAt(), int64(0), "startedAt should be > 0")
}

// makeSenderReport creates an rtcp.SenderReport with the given fields.
func makeSenderReport(ssrc uint32, ntpTime uint64, rtpTime uint32) *rtcp.SenderReport {
	return &rtcp.SenderReport{
		SSRC:    ssrc,
		NTPTime: ntpTime,
		RTPTime: rtpTime,
	}
}

func TestSyncEngineTrack_wallClockPTSForRTP_NotInitialized(t *testing.T) {
	engine := NewSyncEngine()
	track := newMockAudioTrack("audio-1", 1000)
	st := engine.AddTrack(track, "alice").(*syncEngineTrack)

	_, ok := st.wallClockPTSForRTP(0, time.Now())
	require.False(t, ok, "should report not-ok before initialization")
}

func TestSyncEngineTrack_wallClockPTSForRTP_UsesRTPDelta(t *testing.T) {
	engine := NewSyncEngine()
	track := newMockAudioTrack("audio-1", 1000)
	st := engine.AddTrack(track, "alice").(*syncEngineTrack)

	startTime := time.Now()
	st.mu.Lock()
	st.startTime = startTime
	st.lastTS = 48000 // 1s at 48kHz
	st.lastWallPTSSlewed = time.Second
	st.lastWallPTSUnslewed = time.Second
	st.initialized = true
	st.mu.Unlock()

	// SR RTPTime is 1s ahead of lastTS → sensor returns unslewed baseline + rtpDelta = 2s.
	wallPTS, ok := st.wallClockPTSForRTP(96000, startTime.Add(2*time.Second))
	require.True(t, ok)
	require.Equal(t, 2*time.Second, wallPTS)
}

func TestSyncEngineTrack_wallClockPTSForRTP_BackwardRTPReportsNotOk(t *testing.T) {
	// Backward RTP would wrap uint32 into a huge unslewed delta — sensor must skip
	engine := NewSyncEngine()
	track := newMockAudioTrack("audio-1", 1000)
	st := engine.AddTrack(track, "alice").(*syncEngineTrack)

	startTime := time.Now()
	st.mu.Lock()
	st.startTime = startTime
	st.lastTS = 48000 * 100 // 100s of media already
	st.lastWallPTSSlewed = 100 * time.Second
	st.lastWallPTSUnslewed = 100 * time.Second
	st.initialized = true
	st.mu.Unlock()

	_, ok := st.wallClockPTSForRTP(48000, startTime.Add(time.Second))
	require.False(t, ok, "backward RTP (reorder or uint32 wrap) must not produce a sensor value")
}

func TestSyncEngineTrack_wallClockPTSForRTP_HugeForwardJumpReportsNotOk(t *testing.T) {
	// 30s+ forward rtpDelta = stream restart before GetPTS re-anchored; sensor must skip
	engine := NewSyncEngine()
	track := newMockAudioTrack("audio-1", 1000)
	st := engine.AddTrack(track, "alice").(*syncEngineTrack)

	startTime := time.Now()
	st.mu.Lock()
	st.startTime = startTime
	st.lastTS = 48000
	st.lastWallPTSSlewed = time.Second
	st.lastWallPTSUnslewed = time.Second
	st.initialized = true
	st.mu.Unlock()

	// 31s of forward RTP (48000Hz × 31s).
	_, ok := st.wallClockPTSForRTP(48000+48000*31, startTime.Add(31*time.Second))
	require.False(t, ok, "30s+ forward RTP jump must not produce a sensor value")
}

// driftScenario runs N packets and SRs through an engine and returns the
// sequence of drift values reported via OnSenderReport.
func driftScenario(t *testing.T, audioCompensated bool) ([]time.Duration, time.Duration, *SyncEngine) {
	t.Helper()
	opts := []SyncEngineOption{}
	if audioCompensated {
		opts = append(opts, WithSyncEngineAudioDriftCompensated())
	}
	engine := NewSyncEngine(opts...)
	track := newMockAudioTrack("audio-1", 1000)
	ts := engine.AddTrack(track, "alice")

	var drifts []time.Duration
	ts.OnSenderReport(func(d time.Duration) { drifts = append(drifts, d) })

	now := time.Now()
	pkt0 := makeExtPacket(0, 0, now)
	ts.PrimeForStart(pkt0)
	ts.GetPTS(pkt0)
	// 50 packets at 48kHz / 20ms each → lastTS=48000, lastPTS≈1s.
	for i := 1; i <= 50; i++ {
		pkt := makeExtPacket(uint32(i)*960, uint16(i), now.Add(time.Duration(i)*20*time.Millisecond))
		ts.GetPTS(pkt)
	}

	// 5 SRs to bring NTP regression online. RTP advances at nominal 48kHz, so
	// for any RTP timestamp X, sessionPTS ≈ X / 48000.
	for i := 1; i <= 5; i++ {
		srWall := now.Add(time.Duration(i) * 200 * time.Millisecond)
		rtpTS := uint32(i) * 9600 // 200ms * 48kHz
		engine.OnRTCP(makeSenderReport(1000, ntpToUint64(srWall), rtpTS))
	}

	lastRTP := uint32(5) * 9600
	sessionPTS, err := engine.timeline.GetSessionPTS("alice", "audio-1", lastRTP)
	require.NoError(t, err)
	return drifts, sessionPTS, engine
}

func TestSyncEngine_OnRTCP_DriftSign_AudioDriftCompensated(t *testing.T) {
	drifts, sessionPTS, _ := driftScenario(t, true)
	require.NotEmpty(t, drifts, "expected at least one drift callback")

	// For audio-compensated mode: drift = wallPTS - sessionPTS.
	// After 50 packets at nominal rate, wallPTS for the SR's RTP timestamp is
	// derived from lastPTS (≈1s) + RTP delta. lastRTP=48000 == lastTS, so
	// rtpDerived = lastPTS ≈ 1s. drift ≈ 1s - sessionPTS.
	last := drifts[len(drifts)-1]
	require.InDelta(t, float64(time.Second-sessionPTS), float64(last), float64(20*time.Millisecond),
		"drift should equal wallPTS (~1s) minus sessionPTS; got %v, sessionPTS=%v", last, sessionPTS)
}

func TestSyncEngine_OnRTCP_DriftSign_NonCompensated(t *testing.T) {
	drifts, sessionPTS, _ := driftScenario(t, false)
	require.NotEmpty(t, drifts, "expected at least one drift callback")

	// Non-compensated: drift = sessionPTS - expectedElapsed. expectedElapsed is
	// now() - sessionStart at SR receive time; in this fast-running test it's
	// roughly the test execution duration (tiny). So drift ≈ sessionPTS.
	last := drifts[len(drifts)-1]
	require.InDelta(t, float64(sessionPTS), float64(last), float64(50*time.Millisecond),
		"drift should ≈ sessionPTS minus near-zero expectedElapsed; got %v, sessionPTS=%v", last, sessionPTS)
}

func TestSyncEngine_OnRTCP_DriftCallback_NotCalledBeforeTrackInitialized(t *testing.T) {
	// SR arrives before any packets — engine has no startedAt yet, so the
	// callback must not fire (regardless of audioDriftCompensated).
	engine := NewSyncEngine(WithSyncEngineAudioDriftCompensated())
	track := newMockAudioTrack("audio-1", 1000)
	ts := engine.AddTrack(track, "alice")

	fired := false
	ts.OnSenderReport(func(d time.Duration) { fired = true })

	// Feed enough SRs to make the timeline ready but no packets first.
	now := time.Now()
	for i := 1; i <= 6; i++ {
		srWall := now.Add(time.Duration(i) * 200 * time.Millisecond)
		engine.OnRTCP(makeSenderReport(1000, ntpToUint64(srWall), uint32(i)*9600))
	}

	require.False(t, fired, "onSR must not fire before any packets have initialized the track")
}

// primeNTPReady drives enough SRs and forward packets through the track to
// leave it in a steady NTP-ready state with non-zero lastNtpPTS. The SRs are
// fed directly to the timeline (rather than via engine.OnRTCP) so that
// receivedAt can be controlled — using engine.OnRTCP would stamp receivedAt
// with time.Now(), producing a large negative OWD when senderNtp is in the
// test's synthetic future. Returns the time of the last forward packet.
func primeNTPReady(t *testing.T, engine *SyncEngine, tr *syncEngineTrack, participantID, trackID string, now time.Time) time.Time {
	t.Helper()
	const clockRate = uint32(48000)
	tr.PrimeForStart(makeExtPacket(0, 0, now))
	tr.GetPTS(makeExtPacket(0, 0, now))
	// Feed enough SRs to make the NTP regression ready. receivedAt == srTime
	// so the OWD estimator settles at ~0 and sessionPTS values stay positive.
	for i := 1; i <= 5; i++ {
		srTime := now.Add(time.Duration(i) * time.Second)
		engine.timeline.OnSenderReport(participantID, trackID, clockRate, ntpToUint64(srTime), uint32(i)*clockRate, srTime)
	}
	// Process forward packets so lastNtpPTS / transitionSlew get populated via
	// the real flow rather than direct field assignment. RTP timestamps match
	// the SR range to keep regression extrapolation accurate.
	lastRecv := now
	for i := 1; i <= 5; i++ {
		lastRecv = now.Add(time.Duration(i) * time.Second)
		tr.GetPTS(makeExtPacket(uint32(i)*clockRate, uint16(i), lastRecv))
	}
	tr.mu.Lock()
	require.True(t, tr.hasLastNtpPTS, "test setup: NTP regression should be ready after 5 SRs + 5 packets")
	require.True(t, tr.ntpTransitioned, "test setup: ntpTransitioned should be set after first NTP-ready packet")
	tr.mu.Unlock()
	return lastRecv
}

func TestSyncEngine_BackwardRTPDoesNotResetNTPState(t *testing.T) {
	// A single backward (reordered) packet must NOT trigger the discontinuity
	// reset that wipes lastNtpPTS / ntpCorrection / transitionSlew / etc.
	// Without the signed-delta guard, an unsigned uint32 subtraction would
	// wrap a one-frame backward delta into ~24h and trip the 30s threshold.
	engine := NewSyncEngine()
	track := newMockAudioTrack("audio-1", 1000)
	tr := engine.AddTrack(track, "alice").(*syncEngineTrack)

	now := time.Now()
	lastRecv := primeNTPReady(t, engine, tr, "alice", "audio-1", now)

	// Snapshot state populated by the real flow.
	tr.mu.Lock()
	initialLastNtpPTS := tr.lastNtpPTS
	initialTransitionSlew := tr.transitionSlew
	initialLastTS := tr.lastTS
	initialLastWallPTSSlewed := tr.lastWallPTSSlewed
	initialLastWallPTSUnslewed := tr.lastWallPTSUnslewed
	initialLastSlewPTS := tr.lastSlewPTS
	tr.mu.Unlock()
	require.Greater(t, int64(initialLastNtpPTS), int64(0), "lastNtpPTS should be populated by the prime sequence")

	// Feed a backward packet (one frame behind lastTS). NTP regression remains
	// ready, so this should NOT trigger any clearing — neither the
	// discontinuity reset nor the NTP-unavailable cleanup.
	backPkt := makeExtPacket(uint32(4)*48000, 100, lastRecv.Add(20*time.Millisecond))
	_, _ = tr.GetPTS(backPkt)

	tr.mu.Lock()
	defer tr.mu.Unlock()
	require.Equal(t, initialLastNtpPTS, tr.lastNtpPTS, "lastNtpPTS must survive backward reorder")
	require.True(t, tr.ntpTransitioned, "ntpTransitioned must survive backward reorder")
	// transitionSlew may decay slightly via the normal slew loop on the
	// backward packet, but must not be wiped to zero.
	require.InDelta(t, float64(initialTransitionSlew), float64(tr.transitionSlew), float64(10*time.Millisecond),
		"transitionSlew must survive backward reorder (initial %v, got %v)", initialTransitionSlew, tr.transitionSlew)
	// RTP/wall/slew baselines must stay anchored to the last forward packet so
	// the next forward packet's expected-extrapolation baselines line up.
	require.Equal(t, initialLastTS, tr.lastTS, "lastTS must not shift backward on reorder")
	require.Equal(t, initialLastWallPTSSlewed, tr.lastWallPTSSlewed, "lastWallPTSSlewed must not shift backward on reorder")
	require.Equal(t, initialLastWallPTSUnslewed, tr.lastWallPTSUnslewed, "lastWallPTSUnslewed must not shift backward on reorder")
	require.Equal(t, initialLastSlewPTS, tr.lastSlewPTS, "lastSlewPTS must not shift backward on reorder")
}

func TestSyncEngine_ForwardDiscontinuityStillResetsNTPState(t *testing.T) {
	// Sanity check: a genuine large forward RTP jump (≥30s of media) MUST still
	// trigger the discontinuity reset. The signed-delta fix must not disable
	// this path.
	engine := NewSyncEngine()
	track := newMockAudioTrack("audio-1", 1000)
	tr := engine.AddTrack(track, "alice").(*syncEngineTrack)

	now := time.Now()
	lastRecv := primeNTPReady(t, engine, tr, "alice", "audio-1", now)

	tr.mu.Lock()
	require.Greater(t, int64(tr.lastNtpPTS), int64(0), "lastNtpPTS should be populated before the jump")
	lastTS := tr.lastTS
	tr.mu.Unlock()

	// Jump 60s of RTP forward — well past the 30s discontinuity threshold.
	bigJumpTS := lastTS + uint32(48000*60)
	tr.GetPTS(makeExtPacket(bigJumpTS, 200, lastRecv.Add(60*time.Second)))

	tr.mu.Lock()
	defer tr.mu.Unlock()
	require.False(t, tr.hasLastNtpPTS, "hasLastNtpPTS must be reset on forward discontinuity")
	require.Equal(t, time.Duration(0), tr.lastNtpPTS, "lastNtpPTS must be reset on forward discontinuity")
	require.Equal(t, time.Duration(0), tr.transitionSlew, "transitionSlew must be reset on forward discontinuity")
	require.False(t, tr.ntpTransitioned, "ntpTransitioned must be reset on forward discontinuity")
}

func TestSyncEngine_LatePacketDoesNotPerturbDriftSensor(t *testing.T) {
	// Late packet must bump slewed (cliff protection) but not unslewed (sensor)
	engine := NewSyncEngine(WithSyncEngineAudioDriftCompensated())
	track := newMockAudioTrack("audio-1", 1000)
	tr := engine.AddTrack(track, "alice").(*syncEngineTrack)

	now := time.Now()
	tr.PrimeForStart(makeExtPacket(0, 0, now))
	tr.GetPTS(makeExtPacket(0, 0, now))
	for i := 1; i <= 10; i++ {
		tr.GetPTS(makeExtPacket(uint32(i)*960, uint16(i), now.Add(time.Duration(i)*20*time.Millisecond)))
	}
	tr.mu.Lock()
	baseSlewed := tr.lastWallPTSSlewed
	baseUnslewed := tr.lastWallPTSUnslewed
	tr.mu.Unlock()

	// Packet 11: nominal ts, receivedAt +200ms late → slew absorbs 200ms·α = 2ms.
	tr.GetPTS(makeExtPacket(11*960, 11, now.Add(11*20*time.Millisecond+200*time.Millisecond)))

	tr.mu.Lock()
	slewedGain := tr.lastWallPTSSlewed - baseSlewed
	unslewedGain := tr.lastWallPTSUnslewed - baseUnslewed
	tr.mu.Unlock()

	require.InDelta(t, float64(20*time.Millisecond), float64(unslewedGain), float64(200*time.Microsecond),
		"unslewed baseline must not absorb receivedAt jitter; got gain %v", unslewedGain)
	require.InDelta(t, float64(22*time.Millisecond), float64(slewedGain), float64(500*time.Microsecond),
		"slewed baseline should gain rtpDelta + slew absorption (~22ms); got %v", slewedGain)
	require.Greater(t, slewedGain-unslewedGain, time.Millisecond,
		"slewed must move measurably above unslewed on late-packet injection; slewed=%v unslewed=%v",
		slewedGain, unslewedGain)
}

func TestSyncEngine_PublisherSkewVisibleToDriftSensor(t *testing.T) {
	// Slew leaking into the sensor would blind tempo to publisher skew in steady state
	engine := NewSyncEngine(WithSyncEngineAudioDriftCompensated())
	track := newMockAudioTrack("audio-1", 1000)
	tr := engine.AddTrack(track, "alice").(*syncEngineTrack)

	now := time.Now()
	tr.PrimeForStart(makeExtPacket(0, 0, now))
	tr.GetPTS(makeExtPacket(0, 0, now))

	const packets = 500
	const walltickMs = 20
	const rtpSamplesPerPacket = 970 // 970/48000 ≈ 20.208ms → ~1% publisher-fast
	var lastReceivedAt time.Time
	var lastRTP uint32
	for i := 1; i <= packets; i++ {
		lastReceivedAt = now.Add(time.Duration(i*walltickMs) * time.Millisecond)
		lastRTP = uint32(i) * rtpSamplesPerPacket
		tr.GetPTS(makeExtPacket(lastRTP, uint16(i), lastReceivedAt))
	}

	tr.mu.Lock()
	slewedOverWall := tr.lastWallPTSSlewed - time.Duration(packets*walltickMs)*time.Millisecond
	unslewedOverWall := tr.lastWallPTSUnslewed - time.Duration(packets*walltickMs)*time.Millisecond
	tr.mu.Unlock()

	// Slewed steady state ≈ R·δ·(1-α)/α — bounded, not linear.
	require.Less(t, slewedOverWall, 100*time.Millisecond,
		"slewed baseline must stay bounded near wallElapsed; got %v", slewedOverWall)
	// Unslewed grows linearly: 500 × ~0.208ms ≈ 104ms.
	require.Greater(t, unslewedOverWall, 90*time.Millisecond,
		"unslewed baseline must track publisher-skew accumulation; got %v", unslewedOverWall)

	sensor, ok := tr.wallClockPTSForRTP(lastRTP, lastReceivedAt)
	require.True(t, ok)
	tr.mu.Lock()
	require.Equal(t, tr.lastWallPTSUnslewed, sensor,
		"sensor value must equal lastWallPTSUnslewed when SR RTPTime matches lastTS")
	tr.mu.Unlock()
}

func TestSyncEngine_DiscontinuityReAnchorsUnslewedBaseline(t *testing.T) {
	// 30s+ jump with wallclock unchanged must snap both baselines; otherwise unslewed carries phantom drift
	engine := NewSyncEngine()
	track := newMockAudioTrack("audio-1", 1000)
	tr := engine.AddTrack(track, "alice").(*syncEngineTrack)

	now := time.Now()
	lastRecv := primeNTPReady(t, engine, tr, "alice", "audio-1", now)

	tr.mu.Lock()
	lastTS := tr.lastTS
	tr.mu.Unlock()

	bigJumpTS := lastTS + uint32(48000*60)
	tr.GetPTS(makeExtPacket(bigJumpTS, 200, lastRecv.Add(20*time.Millisecond)))

	tr.mu.Lock()
	defer tr.mu.Unlock()
	require.Equal(t, tr.lastWallPTSSlewed, tr.lastWallPTSUnslewed,
		"post-discontinuity, both wall baselines must be re-anchored to the same wallElapsed")
	require.Less(t, tr.lastWallPTSUnslewed, 30*time.Second,
		"unslewed baseline must NOT carry the 60s rtpDelta jump forward; got %v", tr.lastWallPTSUnslewed)
}

func TestSyncEngine_OnSR_NotCalledAfterRemoveTrack(t *testing.T) {
	// After RemoveTrack closes the track, a subsequent SR for the same SSRC
	// (if it slipped in via an in-flight RTCP packet) must not invoke onSR.
	// In practice, RemoveTrack also removes the track from the SSRC map, so
	// OnRTCP would early-return — but Close() nulls onSR as defense in depth
	// for the snapshot-and-drop-lock race in OnRTCP.
	engine := NewSyncEngine()
	track := newMockAudioTrack("audio-1", 1000)
	tr := engine.AddTrack(track, "alice").(*syncEngineTrack)

	fired := false
	tr.OnSenderReport(func(d time.Duration) { fired = true })

	now := time.Now()
	tr.PrimeForStart(makeExtPacket(0, 0, now))
	tr.GetPTS(makeExtPacket(0, 0, now))

	// Close the track directly to simulate the post-snapshot race.
	tr.Close()

	tr.mu.Lock()
	require.Nil(t, tr.onSR, "Close() must drop the SR callback")
	require.True(t, tr.closed, "Close() must set closed=true")
	tr.mu.Unlock()

	require.False(t, fired, "no callback should have fired during Close")
}
