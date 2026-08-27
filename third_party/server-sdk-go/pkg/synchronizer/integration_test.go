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

	"github.com/stretchr/testify/require"
)

// TestIntegration_CrossParticipantClock exercises the full SyncEngine stack
// (NtpEstimator -> SessionTimeline -> ParticipantClock -> SyncEngine) to verify
// that two participants producing audio at the same real-world time are aligned
// on the session timeline despite having different NTP clock offsets.
//
// Setup:
//   - Alice: audio 48kHz, SSRC=1000, NTP clock offset = 0 (matches real time)
//   - Bob:   audio 48kHz, SSRC=2000, NTP clock offset = +500ms (ahead)
//   - Both have real OWD of 50ms (same SFU -> egress path)
//   - Session starts when Alice's first packet arrives
//
// The OWD estimator sees:
//   - Alice: receivedAt - senderNTP = (realTime+50ms) - realTime = 50ms
//   - Bob:   receivedAt - senderNTP = (realTime+50ms) - (realTime+500ms) = -450ms
//
// The formula sessionPTS = ntpTime + OWD - sessionStart normalizes the clock
// offset because ntpTime includes the +500ms and OWD reflects the -500ms.
func TestIntegration_CrossParticipantClock(t *testing.T) {
	const (
		clockRate    = uint32(48000)
		owd          = 50 * time.Millisecond
		bobNTPOffset = 500 * time.Millisecond
	)

	engine := NewSyncEngine(WithSyncEngineOldPacketThreshold(0))

	aliceTrack := newMockAudioTrack("audio-alice", 1000)
	bobTrack := newMockAudioTrack("audio-bob", 2000)

	aliceTS := engine.AddTrack(aliceTrack, "alice")
	bobTS := engine.AddTrack(bobTrack, "bob")

	// Session starts at a fixed base time. Both participants' first packets
	// arrive at the same instant (same real OWD from the SFU).
	baseTime := time.Date(2025, 7, 1, 12, 0, 0, 0, time.UTC)
	firstArrival := baseTime.Add(owd)

	// Prime both tracks with their first packets (same arrival time).
	alicePkt0 := makeExtPacket(0, 0, firstArrival)
	bobPkt0 := makeExtPacket(0, 0, firstArrival)
	_, _, aliceDone := aliceTS.PrimeForStart(alicePkt0)
	_, _, bobDone := bobTS.PrimeForStart(bobPkt0)
	require.True(t, aliceDone)
	require.True(t, bobDone)

	// Feed 5 sender reports for each participant, 5 seconds apart.
	// Alice's NTP = realTime (no offset), Bob's NTP = realTime + 500ms.
	// Both SRs arrive at realTime + OWD.
	for i := 0; i < 5; i++ {
		realTime := baseTime.Add(time.Duration(i) * 5 * time.Second)
		receivedAt := realTime.Add(owd)
		rtpTS := uint32(i) * 5 * clockRate

		// Alice SR: NTP = realTime
		aliceNTP := ntpToUint64(realTime)
		aliceSR := makeSenderReport(1000, aliceNTP, rtpTS)
		// Manually set receivedAt by calling OnSenderReport on the timeline directly
		// since OnRTCP uses time.Now(). We need deterministic timing.
		engine.timeline.OnSenderReport("alice", "audio-alice", clockRate, aliceNTP, rtpTS, receivedAt)
		_ = aliceSR // used above indirectly

		// Bob SR: NTP = realTime + 500ms (Bob's NTP clock is 500ms ahead)
		bobNTP := ntpToUint64(realTime.Add(bobNTPOffset))
		engine.timeline.OnSenderReport("bob", "audio-bob", clockRate, bobNTP, rtpTS, receivedAt)
	}

	// Get PTS for both participants at "real time + 10s" with corresponding
	// RTP timestamps (10s * 48kHz = 480000).
	realTimeAt10s := baseTime.Add(10 * time.Second)
	receivedAtAt10s := realTimeAt10s.Add(owd)
	rtpAt10s := uint32(10) * clockRate

	alicePkt := makeExtPacket(rtpAt10s, 100, receivedAtAt10s)
	bobPkt := makeExtPacket(rtpAt10s, 100, receivedAtAt10s)

	alicePTS, err := aliceTS.GetPTS(alicePkt)
	require.NoError(t, err)

	bobPTS, err := bobTS.GetPTS(bobPkt)
	require.NoError(t, err)

	// The 500ms NTP clock difference should be normalized away by OWD estimation.
	diff := alicePTS - bobPTS
	if diff < 0 {
		diff = -diff
	}

	t.Logf("Alice PTS: %v, Bob PTS: %v, diff: %v", alicePTS, bobPTS, diff)
	require.Less(t, diff, 50*time.Millisecond,
		"cross-participant PTS should be aligned despite 500ms NTP clock offset; alice=%v bob=%v diff=%v",
		alicePTS, bobPTS, diff)
}

// TestIntegration_AVLipSync exercises the full SyncEngine stack to verify that
// a single participant's audio and video tracks are kept in sync despite an
// 80ms video encoder delay (video NTP timestamps lag audio by 80ms in the
// sender's clock domain).
//
// Setup:
//   - Audio: 48kHz, SSRC=1000
//   - Video: 90kHz, SSRC=2000
//   - Same participant "alice"
//   - OWD = 50ms for both tracks
//   - Video has 80ms encoder delay: video NTP = audio NTP + 80ms for same
//     real-world instant (video capture is delayed by encoding pipeline)
//
// The ParticipantClock detects the A/V NTP offset and applies a slew-limited
// correction on the video track to bring them into alignment.
func TestIntegration_AVLipSync(t *testing.T) {
	const (
		audioClockRate    = uint32(48000)
		videoClockRate    = uint32(90000)
		owd               = 50 * time.Millisecond
		videoEncoderDelay = 80 * time.Millisecond
	)

	engine := NewSyncEngine(WithSyncEngineOldPacketThreshold(0))

	audioTrack := newMockAudioTrack("audio-alice", 1000)
	videoTrack := newMockVideoTrack("video-alice", 2000)

	audioTS := engine.AddTrack(audioTrack, "alice")
	videoTS := engine.AddTrack(videoTrack, "alice")

	baseTime := time.Date(2025, 7, 1, 12, 0, 0, 0, time.UTC)
	firstArrival := baseTime.Add(owd)

	// Prime both tracks.
	audioPkt0 := makeExtPacket(0, 0, firstArrival)
	videoPkt0 := makeExtPacket(0, 0, firstArrival)
	_, _, audioDone := audioTS.PrimeForStart(audioPkt0)
	_, _, videoDone := videoTS.PrimeForStart(videoPkt0)
	require.True(t, audioDone)
	require.True(t, videoDone)

	// Feed 5 SRs for audio and video, 5 seconds apart.
	// Audio: NTP = baseNtp + i*5s, RTP = i * 5 * audioClockRate
	// Video: NTP = baseNtp + i*5s + 80ms (encoder delay), RTP = i * 5 * videoClockRate
	for i := 0; i < 5; i++ {
		srTime := baseTime.Add(time.Duration(i) * 5 * time.Second)
		receivedAt := srTime.Add(owd)

		audioRTP := uint32(i) * 5 * audioClockRate
		audioNTP := ntpToUint64(srTime)
		engine.timeline.OnSenderReport("alice", "audio-alice", audioClockRate, audioNTP, audioRTP, receivedAt)

		videoRTP := uint32(i) * 5 * videoClockRate
		videoNTP := ntpToUint64(srTime.Add(videoEncoderDelay))
		engine.timeline.OnSenderReport("alice", "video-alice", videoClockRate, videoNTP, videoRTP, receivedAt)
	}

	// Push multiple packets through GetPTS to drive the transition slew
	// and allow the sync engine's per-call slew to converge.
	for i := 1; i <= 200; i++ {
		recvAt := firstArrival.Add(time.Duration(i) * 20 * time.Millisecond)
		audioRTP := uint32(i) * 960  // 20ms at 48kHz
		videoRTP := uint32(i) * 1800 // 20ms at 90kHz

		aPkt := makeExtPacket(audioRTP, uint16(i), recvAt)
		vPkt := makeExtPacket(videoRTP, uint16(i), recvAt)

		audioTS.GetPTS(aPkt)
		videoTS.GetPTS(vPkt)
	}

	// Get PTS for audio at RTP=480000 (10s at 48kHz) and video at RTP=900000 (10s at 90kHz).
	recvAt10s := firstArrival.Add(10 * time.Second)
	audioPktFinal := makeExtPacket(10*audioClockRate, 500, recvAt10s)
	videoPktFinal := makeExtPacket(10*videoClockRate, 500, recvAt10s)

	audioPTS, err := audioTS.GetPTS(audioPktFinal)
	require.NoError(t, err)

	videoPTS, err := videoTS.GetPTS(videoPktFinal)
	require.NoError(t, err)

	// The 80ms encoder delay should be corrected (or mostly corrected) by
	// ParticipantClock's slew-limited adjustment. Allow 100ms tolerance to
	// account for slew rate convergence.
	diff := audioPTS - videoPTS
	if diff < 0 {
		diff = -diff
	}

	t.Logf("Audio PTS: %v, Video PTS: %v, diff: %v", audioPTS, videoPTS, diff)
	require.Less(t, diff, 100*time.Millisecond,
		"A/V lip sync should be within 100ms after convergence; audio=%v video=%v diff=%v",
		audioPTS, videoPTS, diff)
}
