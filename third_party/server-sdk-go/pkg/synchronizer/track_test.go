// Copyright 2025 LiveKit, Inc.
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
	"github.com/livekit/mediatransportutil"
	"github.com/livekit/mediatransportutil/pkg/latency"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/utils/mono"
	"github.com/livekit/protocol/utils/rtputil"
)

// ---- test fakes & helpers ----

type fakeTrack struct {
	id   string
	rate uint32
	kind webrtc.RTPCodecType
}

func (f fakeTrack) ID() string { return f.id }
func (f fakeTrack) Codec() webrtc.RTPCodecParameters {
	return webrtc.RTPCodecParameters{RTPCodecCapability: webrtc.RTPCodecCapability{ClockRate: f.rate}}
}
func (f fakeTrack) Kind() webrtc.RTPCodecType { return f.kind }
func (f fakeTrack) SSRC() webrtc.SSRC         { return 1234 }

func newTSForTests(tc *testing.T, clockRate uint32, kind webrtc.RTPCodecType) *TrackSynchronizer {
	t := &TrackSynchronizer{
		sync:                 nil, // construct directly to avoid depending on Synchronizer
		track:                fakeTrack{id: "t", rate: clockRate, kind: kind},
		logger:               logger.NewTestLogger(tc),
		RTPConverter:         rtputil.NewRTPConverter(int64(clockRate)),
		maxTsDiff:            200 * time.Millisecond,
		maxDriftAdjustment:   5 * time.Millisecond,
		senderReportSyncMode: SenderReportSyncModeWithoutRebase,
		propagationDelayEstimator: latency.NewOWDEstimator(
			latency.OWDEstimatorParamsDefault,
		),
	}
	// set a stable startTime well in the past to make time.Since(startTime) > 0
	t.startTime = time.Now().Add(-150 * time.Millisecond)
	// pick an arbitrary RTP base
	t.startRTP = 1000
	return t
}

func newTSPipelineForTests(t *testing.T, running, delay time.Duration) *TrackSynchronizer {
	s := NewSynchronizerWithOptions(
		WithMediaRunningTime(func() (time.Duration, bool) { return running, true }, delay),
	)
	ts := newTrackSynchronizer(s, fakeTrack{id: "pipeline", rate: 90000, kind: webrtc.RTPCodecTypeAudio})
	ts.logger = logger.NewTestLogger(t)
	ts.startTime = time.Now().Add(-running)
	return ts
}

// ---- tests ----

func TestApplyQuantizedStartTimeAdvance_ExactQuanta(t *testing.T) {
	ts := newTSForTests(t, 48000, webrtc.RTPCodecTypeAudio)
	base := ts.startTime

	// 25ms delta with 5ms step -> apply 25ms, residual 0
	applied := ts.applyQuantizedStartTimeAdvance(25 * time.Millisecond)
	require.Equal(t, 25*time.Millisecond, applied)
	require.Equal(t, 25*time.Millisecond, base.Sub(ts.startTime))
	require.Equal(t, time.Duration(0), ts.startTimeAdjustResidual)
	require.Equal(t, 25*time.Millisecond, ts.totalStartTimeAdjustment)
}

func TestApplyQuantizedStartTimeAdvance_ResidualCarryAcrossCalls(t *testing.T) {
	ts := newTSForTests(t, 48000, webrtc.RTPCodecTypeAudio)
	base := ts.startTime

	// First call: 3ms (<5ms step) -> apply 0, residual=3ms
	applied1 := ts.applyQuantizedStartTimeAdvance(3 * time.Millisecond)
	require.Equal(t, time.Duration(0), applied1)
	require.Equal(t, 3*time.Millisecond, ts.startTimeAdjustResidual)
	require.Equal(t, ts.startTime, base)
	require.Equal(t, 3*time.Millisecond, ts.startTimeAdjustResidual)
}

func TestApplyQuantizedStartTimeAdvance_NoOpForZero(t *testing.T) {
	ts := newTSForTests(t, 48000, webrtc.RTPCodecTypeAudio)
	base := ts.startTime

	applied := ts.applyQuantizedStartTimeAdvance(0)
	require.Equal(t, time.Duration(0), applied)
	require.Equal(t, ts.startTime, base)
	require.Equal(t, time.Duration(0), ts.startTimeAdjustResidual)
}

func TestGetPTSWithoutRebase_Increasing(t *testing.T) {
	clock := uint32(48000)
	ts := newTSForTests(t, clock, webrtc.RTPCodecTypeAudio)

	// Simulate accepting two frames in order: 20ms and then 20ms later
	// Convert 20ms -> RTP ticks
	rtp20ms := ts.ToRTP(20 * time.Millisecond)

	now := time.Now()
	// First packet initializes lastTS path
	ts.lastTS = 0
	ts.lastPTS = 0

	adj1, err := ts.getPTSWithoutRebase(jitter.ExtPacket{
		Packet:     &rtp.Packet{Header: rtp.Header{Timestamp: ts.startRTP + rtp20ms}},
		ReceivedAt: now,
	})
	require.NoError(t, err)

	adj2, err := ts.getPTSWithoutRebase(jitter.ExtPacket{
		Packet:     &rtp.Packet{Header: rtp.Header{Timestamp: ts.startRTP + 2*rtp20ms}},
		ReceivedAt: now,
	})
	require.NoError(t, err)

	require.Greater(t, adj2, adj1)
}

func TestGetPTSWithoutRebase_NegativeAdjustedPTS(t *testing.T) {
	sync := NewSynchronizerWithOptions()
	base := time.Now()
	futureStart := base.Add(500 * time.Millisecond)
	sync.getOrSetStartedAt(futureStart.UnixNano())

	track := fakeTrack{id: "neg", rate: 90000, kind: webrtc.RTPCodecTypeVideo}
	ts := newTrackSynchronizer(sync, track)
	ts.logger = logger.NewTestLogger(t)

	firstReceivedAt := base
	firstPacket := jitter.ExtPacket{
		Packet: &rtp.Packet{
			Header:  rtp.Header{Timestamp: 90000, SequenceNumber: 1},
			Payload: []byte{0x01},
		},
		ReceivedAt: firstReceivedAt,
	}

	ts.initialize(firstPacket)
	require.Less(t, ts.currentPTSOffset, time.Duration(0), "expected negative PTS offset when synchronizer start is later")

	stepTS := ts.ToRTP(10 * time.Millisecond)
	secondPacket := jitter.ExtPacket{
		Packet: &rtp.Packet{
			Header:  rtp.Header{Timestamp: firstPacket.Timestamp + stepTS, SequenceNumber: 2},
			Payload: []byte{0x02},
		},
		ReceivedAt: firstReceivedAt.Add(10 * time.Millisecond),
	}

	adjusted, err := ts.GetPTS(secondPacket)
	require.NoError(t, err)
	require.Less(t, adjusted, time.Duration(0), "expected negative adjusted PTS")
}

func TestInitializeUsesBufferedPacketTiming(t *testing.T) {
	sync := NewSynchronizerWithOptions()
	syncStart := mono.Now().Add(-250 * time.Millisecond)
	sync.getOrSetStartedAt(syncStart.UnixNano())

	track := fakeTrack{id: "init", rate: 90000, kind: webrtc.RTPCodecTypeVideo}
	ts := newTrackSynchronizer(sync, track)
	ts.logger = logger.NewTestLogger(t)

	receivedAt := time.Now().Add(-50 * time.Millisecond)
	packet := &rtp.Packet{Header: rtp.Header{Timestamp: 42}}
	extPkt := jitter.ExtPacket{Packet: packet, ReceivedAt: receivedAt}

	ts.initialize(extPkt)

	require.True(t, ts.initialized, "track should initialize")
	require.Equal(t, receivedAt, ts.startTime, "startTime should match buffered arrival time")
	require.Equal(t, packet.Timestamp, ts.startRTP, "startRTP should come from packet timestamp")
	require.InDelta(t, receivedAt.Sub(syncStart), ts.currentPTSOffset, float64(5*time.Millisecond))
}

func TestGetPTSWithRebase_PropelsForward(t *testing.T) {
	clock := uint32(48000)
	ts := newTSForTests(t, clock, webrtc.RTPCodecTypeAudio)
	ts.senderReportSyncMode = SenderReportSyncModeRebase

	ts.maxTsDiff = 30 * time.Millisecond

	// 1) Seed: make adjusted ~500ms on the first packet.
	ts.startTime = mono.Now().Add(-500 * time.Millisecond)
	ts.currentPTSOffset = 0
	ts.lastPTS = 0
	ts.startRTP = 100000
	ts.lastTS = ts.startRTP

	rtp500ms := ts.ToRTP(500 * time.Millisecond)
	rtp10ms := ts.ToRTP(10 * time.Millisecond)

	// First packet (~500ms)
	ts1 := ts.startRTP + rtp500ms
	adj1, err := ts.getPTSWithRebase(jitter.ExtPacket{
		Packet:     &rtp.Packet{Header: rtp.Header{Timestamp: ts1}},
		ReceivedAt: mono.Now(),
	})
	require.NoError(t, err)
	require.InDelta(t, 500*time.Millisecond, adj1, float64(20*time.Millisecond))

	// 2) Simulate startTime shift LATER (closer to now) so next estimatedPTS is tiny (~5–10ms)
	ts.startTime = mono.Now().Add(-5 * time.Millisecond)

	// Second packet: +10ms RTP so ts != lastTS. After correction, adjusted will be tiny and < lastPTSAdjusted.
	ts2 := ts1 + rtp10ms
	prev := ts.lastPTSAdjusted      // ~500ms from first call
	want := prev + time.Millisecond // propel to ~501ms

	adj2, err := ts.getPTSWithRebase(jitter.ExtPacket{
		Packet:     &rtp.Packet{Header: rtp.Header{Timestamp: ts2}},
		ReceivedAt: mono.Now(),
	})
	require.NoError(t, err)
	require.Equal(t, want, adj2)
}

func TestExpectedAdjustedPTSAtSenderReport_UsesBaseOffset(t *testing.T) {
	ts := newTSForTests(t, 48000, webrtc.RTPCodecTypeAudio)
	ts.startTime = time.Unix(0, 1_000_000_000)
	ts.basePTSOffset = 100 * time.Millisecond

	got := ts.expectedAdjustedPTSAtSenderReport(ts.startTime.Add(600 * time.Millisecond).UnixNano())
	require.Equal(t, 700*time.Millisecond, got)
}

func TestApplyOneShotDriftCorrection_SkipsWithoutDeadline(t *testing.T) {
	ts := newTSForTests(t, 48000, webrtc.RTPCodecTypeAudio)
	ts.currentPTSOffset = 25 * time.Millisecond
	ts.desiredPTSOffset = 25 * time.Millisecond

	ts.applyOneShotDriftCorrection(
		600*time.Millisecond,
		50*time.Millisecond,
		650*time.Millisecond,
		0,
		false,
		&augmentedSenderReport{},
	)

	require.Equal(t, 25*time.Millisecond, ts.currentPTSOffset)
	require.Equal(t, 25*time.Millisecond, ts.desiredPTSOffset)
	require.EqualValues(t, 0, ts.stats.numOneShotCorrections)
}

func TestApplyOneShotDriftCorrection_AcceptsCandidateSlightlyAboveOneDelay(t *testing.T) {
	ts := newTSForTests(t, 48000, webrtc.RTPCodecTypeAudio)
	ts.currentPTSOffset = 0
	ts.desiredPTSOffset = 0
	ts.maxMediaRunningTimeDelay = 100 * time.Millisecond

	deadline := 500 * time.Millisecond
	// candidateAdjustedPTS = ptsSR + (currentPTSOffset + drift) = 600 + (0 + 50) = 650ms
	// Above deadline+1*delay (600ms) but within deadline+2*delay (700ms) — should be accepted.
	ptsSR := 600 * time.Millisecond
	drift := 50 * time.Millisecond

	ts.applyOneShotDriftCorrection(
		ptsSR,
		drift,
		ptsSR+ts.currentPTSOffset,
		deadline,
		true,
		&augmentedSenderReport{},
	)

	require.Equal(t, drift, ts.currentPTSOffset, "correction should be applied")
	require.EqualValues(t, 1, ts.stats.numOneShotCorrections)
}

func TestApplyOneShotDriftCorrection_RejectsAboveTwoDelays(t *testing.T) {
	ts := newTSForTests(t, 48000, webrtc.RTPCodecTypeAudio)
	ts.currentPTSOffset = 0
	ts.desiredPTSOffset = 0
	ts.maxMediaRunningTimeDelay = 100 * time.Millisecond

	deadline := 500 * time.Millisecond
	// candidateAdjustedPTS = 600 + (0 + 150) = 750ms
	// Above deadline+2*delay (700ms) — should be rejected.
	ptsSR := 600 * time.Millisecond
	drift := 150 * time.Millisecond

	ts.applyOneShotDriftCorrection(
		ptsSR,
		drift,
		ptsSR+ts.currentPTSOffset,
		deadline,
		true,
		&augmentedSenderReport{},
	)

	require.Equal(t, time.Duration(0), ts.currentPTSOffset, "correction should be rejected")
	require.EqualValues(t, 0, ts.stats.numOneShotCorrections)
}

func TestShouldAdjustPTS_Deadband_Suppresses(t *testing.T) {
	clock := uint32(48000)
	ts := newTSForTests(t, clock, webrtc.RTPCodecTypeVideo) // video avoids audio gating path
	ts.maxDriftAdjustment = 5 * time.Millisecond
	ts.currentPTSOffset = 100 * time.Millisecond

	// within dead-band: +4ms
	ts.desiredPTSOffset = 104 * time.Millisecond

	// ensure throttle window has elapsed
	ts.nextPTSAdjustmentAt = mono.Now().Add(-time.Second)

	require.False(t, ts.shouldAdjustPTS(80*time.Millisecond), "delta < step should suppress adjustment")
}

func TestShouldAdjustPTS_Deadband_BoundaryAdjusts(t *testing.T) {
	clock := uint32(48000)
	ts := newTSForTests(t, clock, webrtc.RTPCodecTypeVideo)
	ts.maxDriftAdjustment = 5 * time.Millisecond
	ts.currentPTSOffset = 100 * time.Millisecond

	// exactly at boundary: +5ms
	ts.desiredPTSOffset = 105 * time.Millisecond
	ts.nextPTSAdjustmentAt = mono.Now().Add(-time.Second)

	require.True(t, ts.shouldAdjustPTS(80*time.Millisecond), "delta == step should allow adjustment")
}

func TestShouldAdjustPTS_Deadband_AboveAdjusts(t *testing.T) {
	clock := uint32(48000)
	ts := newTSForTests(t, clock, webrtc.RTPCodecTypeVideo)
	ts.maxDriftAdjustment = 5 * time.Millisecond
	ts.currentPTSOffset = 100 * time.Millisecond

	// above dead-band: +12ms
	ts.desiredPTSOffset = 112 * time.Millisecond
	ts.nextPTSAdjustmentAt = mono.Now().Add(-time.Second)

	require.True(t, ts.shouldAdjustPTS(80*time.Millisecond), "delta > step should allow adjustment")
}

func TestPrimeForStartWithStartGate(t *testing.T) {
	clock := uint32(90000)
	ts := newTSForTests(t, clock, webrtc.RTPCodecTypeVideo)
	ts.startGate = newStartGate(clock, webrtc.RTPCodecTypeVideo, ts.logger)
	ts.sync = NewSynchronizerWithOptions()

	stepDur := 20 * time.Millisecond
	step := ts.ToRTP(stepDur)
	baseTS := ts.startRTP
	base := time.Now()

	for i := 0; i < 5; i++ {
		timestamp := baseTS + uint32(i+1)*step
		pkt := jitter.ExtPacket{
			Packet:     &rtp.Packet{Header: rtp.Header{Timestamp: timestamp}, Payload: []byte{0x01}},
			ReceivedAt: base.Add(time.Duration(i+1) * stepDur),
		}
		ready, dropped, done := ts.PrimeForStart(pkt)
		require.False(t, done)
		require.Zero(t, dropped)
		require.Nil(t, ready)
	}

	finalPkt := jitter.ExtPacket{
		Packet:     &rtp.Packet{Header: rtp.Header{Timestamp: baseTS + uint32(6*step)}, Payload: []byte{0x02}},
		ReceivedAt: base.Add(6 * stepDur),
	}
	ready, dropped, done := ts.PrimeForStart(finalPkt)
	require.True(t, done, "gate should be done after final packet")
	require.Zero(t, dropped, "no packets should be dropped")
	require.NotEmpty(t, ready, "ready should have at least the final packet")
	require.True(t, ts.initialized, "track should be initialized")
}

func TestPrimeForStartWithoutStartGate(t *testing.T) {
	clock := uint32(48000)
	ts := newTSForTests(t, clock, webrtc.RTPCodecTypeAudio)
	ts.startGate = nil
	ts.sync = NewSynchronizerWithOptions()

	pkt := jitter.ExtPacket{
		Packet:     &rtp.Packet{Header: rtp.Header{Timestamp: ts.startRTP + 1234}, Payload: []byte{0x01}},
		ReceivedAt: time.Now(),
	}

	ready, dropped, done := ts.PrimeForStart(pkt)
	require.True(t, done, "gate should be done after packet")
	require.Zero(t, dropped, "no packets should be dropped")
	require.Len(t, ready, 1, "ready should have the packet")
	require.True(t, ts.initialized, "track should be initialized")
}

func TestShouldAdjustPTS_Deadband_NegativeDelta_Suppresses(t *testing.T) {
	clock := uint32(48000)
	ts := newTSForTests(t, clock, webrtc.RTPCodecTypeVideo)
	ts.maxDriftAdjustment = 5 * time.Millisecond
	ts.currentPTSOffset = 100 * time.Millisecond

	// within dead-band on negative side: -4ms
	ts.desiredPTSOffset = 96 * time.Millisecond
	ts.nextPTSAdjustmentAt = mono.Now().Add(-time.Second)

	require.False(t, ts.shouldAdjustPTS(80*time.Millisecond), "negative delta with |delta| < step should suppress adjustment")
}

func TestShouldAdjustPTS_Deadband_NegativeDelta_AboveAdjusts(t *testing.T) {
	clock := uint32(48000)
	ts := newTSForTests(t, clock, webrtc.RTPCodecTypeVideo)
	ts.maxDriftAdjustment = 5 * time.Millisecond
	ts.currentPTSOffset = 100 * time.Millisecond

	// beyond dead-band on negative side: -6ms
	ts.desiredPTSOffset = 94 * time.Millisecond
	ts.nextPTSAdjustmentAt = mono.Now().Add(-time.Second)

	require.True(t, ts.shouldAdjustPTS(80*time.Millisecond), "negative delta with |delta| > step should allow adjustment")
}

func TestShouldAdjustPTS_AudioGating_DisabledBlocks(t *testing.T) {
	clock := uint32(48000)
	ts := newTSForTests(t, clock, webrtc.RTPCodecTypeAudio)

	// Force the path where audio gating can block adjustments:
	ts.senderReportSyncMode = SenderReportSyncModeWithoutRebase
	ts.audioPTSAdjustmentsDisabled = true

	ts.maxDriftAdjustment = 5 * time.Millisecond
	ts.currentPTSOffset = 100 * time.Millisecond
	ts.desiredPTSOffset = 140 * time.Millisecond // large delta
	ts.nextPTSAdjustmentAt = mono.Now().Add(-time.Second)

	require.False(t, ts.shouldAdjustPTS(80*time.Millisecond), "audio gating should block adjustment when disabled")
}

func TestShouldAdjustPTS_NoPTSRegression(t *testing.T) {
	clock := uint32(48000)
	ts := newTSForTests(t, clock, webrtc.RTPCodecTypeVideo)
	ts.maxDriftAdjustment = 5 * time.Millisecond
	ts.currentPTSOffset = 100 * time.Millisecond

	ts.lastPTS = 200 * time.Millisecond
	currentPTS := ts.lastPTS + ts.maxDriftAdjustment - time.Millisecond

	ts.desiredPTSOffset = 80 * time.Millisecond
	ts.nextPTSAdjustmentAt = mono.Now().Add(-time.Second)

	require.False(t, ts.shouldAdjustPTS(currentPTS), "should not adjust PTS when it would regress")
}

func TestNormalizePTSToMediaPipelineTimeline_NoPipeline(t *testing.T) {
	ts := newTSForTests(t, 90000, webrtc.RTPCodecTypeAudio)
	ptsIn := 5 * time.Second
	adjusted, ptsOut := ts.normalizePTSToMediaPipelineTimeline(ptsIn, 0, mono.Now(), 0, false)
	require.Equal(t, ptsIn, ptsOut)
	require.Equal(t, ptsIn+ts.currentPTSOffset, adjusted)
}

func TestNormalizePTSToMediaPipelineTimeline_FreshBehindDoesNotCorrect(t *testing.T) {
	running := 10 * time.Second
	delay := 50 * time.Millisecond
	ts := newTSPipelineForTests(t, running, delay)
	ptsIn := running - delay - time.Second
	ts.lastPTS = ptsIn
	ts.lastPTSAdjusted = ptsIn
	sampleTS := ts.ToRTP(ptsIn)
	initialStartRTP := ts.startRTP
	initialTimely := mono.Now()
	ts.lastTimelyPacket = initialTimely

	deadline, hasDeadline := ts.sync.getExternalMediaDeadline()
	adjusted, ptsOut := ts.normalizePTSToMediaPipelineTimeline(ptsIn, sampleTS, mono.Now(), deadline, hasDeadline)
	require.Equal(t, ptsIn, ptsOut)
	require.Equal(t, ptsIn+ts.currentPTSOffset, adjusted)
	require.Equal(t, initialStartRTP, ts.startRTP, "fresh lag must not rebase immediately")
}

func TestNormalizePTSToMediaPipelineTimeline_CorrectsAfterLongLag(t *testing.T) {
	running := 20 * time.Second
	delay := 100 * time.Millisecond
	ts := newTSPipelineForTests(t, running, delay)
	ptsIn := running - delay - 5*time.Second
	ts.lastPTS = ptsIn
	ts.lastPTSAdjusted = ptsIn
	sampleTS := ts.ToRTP(ptsIn)
	ts.lastTimelyPacket = mono.Now().Add(-cMaxTimelyPacketAge - time.Second)

	deadline, hasDeadline := ts.sync.getExternalMediaDeadline()
	require.True(t, hasDeadline)

	adjusted, ptsOut := ts.normalizePTSToMediaPipelineTimeline(ptsIn, sampleTS, mono.Now(), deadline, hasDeadline)
	expectedPTS := deadline + ts.maxMediaRunningTimeDelay - ts.currentPTSOffset
	require.InDelta(t, float64(expectedPTS), float64(ptsOut), float64(3*time.Millisecond))
	require.InDelta(t, float64(expectedPTS+ts.currentPTSOffset), float64(adjusted), float64(3*time.Millisecond))
}

func TestAcceptableSRDrift_UsesOldPacketThreshold(t *testing.T) {
	ts := newTSForTests(t, 48000, webrtc.RTPCodecTypeAudio)
	ts.oldPacketThreshold = 400 * time.Millisecond
	ts.maxMediaRunningTimeDelay = 0

	require.True(t, ts.acceptableSRDrift(399*time.Millisecond))
	require.True(t, ts.acceptableSRDrift(-399*time.Millisecond))
	require.False(t, ts.acceptableSRDrift(400*time.Millisecond))
}

func TestAcceptableSRDrift_PrefersMediaRunningTimeDelay(t *testing.T) {
	ts := newTSForTests(t, 48000, webrtc.RTPCodecTypeAudio)
	ts.oldPacketThreshold = time.Second
	ts.maxMediaRunningTimeDelay = 150 * time.Millisecond

	require.True(t, ts.acceptableSRDrift(149*time.Millisecond))
	require.True(t, ts.acceptableSRDrift(-149*time.Millisecond))
	require.False(t, ts.acceptableSRDrift(151*time.Millisecond))
}

func TestAcceptableSRDrift_FallsBackToDefault(t *testing.T) {
	ts := newTSForTests(t, 48000, webrtc.RTPCodecTypeAudio)
	ts.oldPacketThreshold = 0
	ts.maxMediaRunningTimeDelay = 0

	require.True(t, ts.acceptableSRDrift(time.Second))
	require.False(t, ts.acceptableSRDrift(3*time.Second))
}

func TestOnSenderReportWithRebase_OneShotSkipsStartTimeAdjustmentScenario(t *testing.T) {
	newConfiguredTS := func(mode SenderReportSyncMode) *TrackSynchronizer {
		ts := newTSForTests(t, 48000, webrtc.RTPCodecTypeAudio)
		ts.senderReportSyncMode = mode
		ts.oneShotDriftCorrectionThreshold = time.Second
		// Make RTP progression (~120ms) run far ahead of wall time since start (~20ms), which makes rebase mode pull startTime earlier.
		ts.startTime = time.Now().Add(-20 * time.Millisecond)
		ts.basePTSOffset = 0
		ts.currentPTSOffset = 0
		ts.desiredPTSOffset = 0
		ts.startRTP = 1000
		ts.lastPTS = 120 * time.Millisecond
		ts.lastTS = ts.startRTP + ts.ToRTP(ts.lastPTS)
		return ts
	}

	rebase := newConfiguredTS(SenderReportSyncModeRebase)
	rebaseBase := rebase.startTime
	rebase.onSenderReportWithRebase(&rtcp.SenderReport{
		SSRC:    uint32(rebase.track.SSRC()),
		NTPTime: uint64(mediatransportutil.ToNtpTime(time.Now())),
		RTPTime: rebase.lastTS,
	})
	require.True(t, rebase.startTime.Before(rebaseBase), "expected rebase mode to advance startTime earlier in this scenario")

	oneShot := newConfiguredTS(SenderReportSyncModeOneShot)
	oneShotBase := oneShot.startTime
	oneShot.onSenderReportWithRebase(&rtcp.SenderReport{
		SSRC:    uint32(oneShot.track.SSRC()),
		NTPTime: uint64(mediatransportutil.ToNtpTime(time.Now())),
		RTPTime: oneShot.lastTS,
	})
	require.Equal(t, oneShotBase, oneShot.startTime)
}

func TestNewSynchronizerWithOptions_NormalizesLegacySenderReportModes(t *testing.T) {
	syncLegacy := NewSynchronizerWithOptions(WithRTCPSenderReportRebaseEnabled())
	tsLegacy := newTrackSynchronizer(syncLegacy, fakeTrack{id: "legacy", rate: 48000, kind: webrtc.RTPCodecTypeAudio})
	require.Equal(t, SenderReportSyncModeRebase, tsLegacy.senderReportSyncMode)

	syncDefault := NewSynchronizerWithOptions()
	tsDefault := newTrackSynchronizer(syncDefault, fakeTrack{id: "default", rate: 48000, kind: webrtc.RTPCodecTypeAudio})
	require.Equal(t, SenderReportSyncModeWithoutRebase, tsDefault.senderReportSyncMode)
}

func TestNewSynchronizerWithOptions_ExplicitOneShotModeUsesThreshold(t *testing.T) {
	sync := NewSynchronizerWithOptions(
		WithSenderReportSyncMode(SenderReportSyncModeOneShot),
		WithOneShotDriftCorrectionThreshold(100*time.Millisecond),
	)
	ts := newTrackSynchronizer(sync, fakeTrack{id: "explicit-one-shot", rate: 48000, kind: webrtc.RTPCodecTypeAudio})
	require.Equal(t, SenderReportSyncModeOneShot, ts.senderReportSyncMode)
	require.True(t, ts.usesRebasedSenderReports())
	require.True(t, ts.usesOneShotSenderReportSync())
	require.Equal(t, 100*time.Millisecond, ts.oneShotDriftCorrectionThreshold)
}
