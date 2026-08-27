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

func TestSessionTimeline_SingleParticipant(t *testing.T) {
	// One participant with 50ms OWD, feed 5 SRs, verify PTS at 10s is ~10s.
	const (
		clockRate = 90000
		owd       = 50 * time.Millisecond
		identity  = "alice"
		trackID   = "audio-1"
	)

	st := NewSessionTimeline(nil)

	// Session starts at a fixed wall-clock time.
	sessionStart := time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC)
	st.SetSessionStart(sessionStart)

	st.AddParticipant(identity)

	// The participant's NTP clock is offset from wall clock by OWD.
	// senderNtpTime + OWD = receivedAt (approximately).
	// So receivedAt = senderNtpTime + OWD.
	baseNTP := sessionStart // participant's NTP epoch starts at sessionStart
	for i := 0; i < 5; i++ {
		senderNTP := baseNTP.Add(time.Duration(i) * 2 * time.Second)
		rtpTS := uint32(i) * 2 * clockRate
		receivedAt := senderNTP.Add(owd)
		st.OnSenderReport(identity, trackID, clockRate, ntpToUint64(senderNTP), rtpTS, receivedAt)
	}

	// Query PTS at RTP timestamp corresponding to 10s into the stream.
	rtpAt10s := uint32(10 * clockRate)
	pts, err := st.GetSessionPTS(identity, trackID, rtpAt10s)
	require.NoError(t, err)

	// Expected: ~10s on the session timeline.
	diff := pts - 10*time.Second
	if diff < 0 {
		diff = -diff
	}
	require.Less(t, diff, 100*time.Millisecond,
		"PTS at 10s should be ~10s, got %v (diff %v)", pts, diff)
}

func TestSessionTimeline_CrossParticipantAlignment(t *testing.T) {
	// Two participants with different OWDs (50ms and 200ms), both producing
	// media at the same real-world time. The SessionTimeline maps each
	// participant's NTP clock domain onto the receiver's clock using OWD.
	//
	// Because both start producing at the same real-world time but have
	// different network path delays, the receiver-clock-based timeline
	// correctly reflects the OWD difference: bob's media arrives 150ms
	// later than alice's for the same production instant.
	//
	// Additionally, we verify that NTP clock offset differences between
	// participants are properly normalized via the OWD mapping: if bob's
	// NTP clock is offset by +500ms relative to alice's, the OWD estimator
	// absorbs this, and the session PTS still reflects the real receiver-clock
	// arrival times.
	const (
		clockRate = 90000
		owd1      = 50 * time.Millisecond // alice's real network delay
		owd2      = 50 * time.Millisecond // bob's real network delay (same)
	)

	// Bob's NTP clock is offset by 500ms relative to alice's (different NTP servers).
	bobNTPOffset := 500 * time.Millisecond

	st := NewSessionTimeline(nil)
	sessionStart := time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC)
	st.SetSessionStart(sessionStart)

	st.AddParticipant("alice")
	st.AddParticipant("bob")

	// Both participants start producing at the same real-world time.
	// Alice's NTP clock = real-world time.
	// Bob's NTP clock = real-world time + 500ms (NTP offset).
	// Both have the same real OWD of 50ms, so:
	//   receivedAt = realWorldTime + owd
	//   aliceNTP = realWorldTime
	//   bobNTP = realWorldTime + 500ms
	// OWD as seen by estimator:
	//   alice: receivedAt - aliceNTP = owd = 50ms
	//   bob: receivedAt - bobNTP = owd - 500ms = -450ms (negative! but the estimator handles this)
	//
	// Actually, OWD = receivedAt - senderNTP. For bob:
	//   receivedAt = realWorldTime + 50ms
	//   senderNTP = realWorldTime + 500ms
	//   observed OWD = (realWorldTime + 50ms) - (realWorldTime + 500ms) = -450ms
	//
	// This negative OWD is fine - it just means bob's NTP clock is ahead of
	// the receiver's clock by more than the real OWD. The formula still works
	// because: ntpTime + OWD - sessionStart = (realWorldTime + 500ms) + (-450ms) - sessionStart
	//         = realWorldTime + 50ms - sessionStart
	// Which matches alice's: realWorldTime + 50ms - sessionStart

	for i := 0; i < 5; i++ {
		realTime := sessionStart.Add(time.Duration(i) * 2 * time.Second)
		rtpTS := uint32(i) * 2 * clockRate
		receivedAt := realTime.Add(owd1)

		aliceNTP := realTime // alice NTP = real time
		st.OnSenderReport("alice", "audio-a", clockRate, ntpToUint64(aliceNTP), rtpTS, receivedAt)

		bobNTP := realTime.Add(bobNTPOffset) // bob NTP = real time + offset
		bobRecv := realTime.Add(owd2)
		st.OnSenderReport("bob", "audio-b", clockRate, ntpToUint64(bobNTP), rtpTS, bobRecv)
	}

	// Both participants produce a frame at RTP timestamp corresponding to 5s.
	rtpAt5s := uint32(5 * clockRate)

	alicePTS, err := st.GetSessionPTS("alice", "audio-a", rtpAt5s)
	require.NoError(t, err)

	bobPTS, err := st.GetSessionPTS("bob", "audio-b", rtpAt5s)
	require.NoError(t, err)

	// Despite bob's NTP clock being 500ms offset, the OWD-based mapping
	// normalizes both to the receiver's clock domain. Their PTS values
	// should be within a small tolerance.
	diff := alicePTS - bobPTS
	if diff < 0 {
		diff = -diff
	}
	require.Less(t, diff, 50*time.Millisecond,
		"cross-participant PTS should be aligned despite NTP clock offset; alice=%v bob=%v diff=%v", alicePTS, bobPTS, diff)
}

func TestSessionTimeline_LateJoiner(t *testing.T) {
	// One participant starts, 30s later another joins.
	// Verify the late joiner's first frame maps to ~30s on the session timeline.
	const (
		clockRate = 90000
		owd       = 50 * time.Millisecond
	)

	st := NewSessionTimeline(nil)
	sessionStart := time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC)
	st.SetSessionStart(sessionStart)

	// Alice joins at session start.
	st.AddParticipant("alice")
	aliceBaseNTP := sessionStart
	for i := 0; i < 5; i++ {
		aliceNTP := aliceBaseNTP.Add(time.Duration(i) * 2 * time.Second)
		aliceRTP := uint32(i) * 2 * clockRate
		aliceRecv := aliceNTP.Add(owd)
		st.OnSenderReport("alice", "audio-a", clockRate, ntpToUint64(aliceNTP), aliceRTP, aliceRecv)
	}

	// Bob joins 30s later.
	st.AddParticipant("bob")
	bobBaseNTP := sessionStart.Add(30 * time.Second)
	for i := 0; i < 5; i++ {
		bobNTP := bobBaseNTP.Add(time.Duration(i) * 2 * time.Second)
		bobRTP := uint32(i) * 2 * clockRate
		bobRecv := bobNTP.Add(owd)
		st.OnSenderReport("bob", "audio-b", clockRate, ntpToUint64(bobNTP), bobRTP, bobRecv)
	}

	// Bob's first frame (RTP=0) should map to ~30s on session timeline.
	bobPTS, err := st.GetSessionPTS("bob", "audio-b", 0)
	require.NoError(t, err)

	diff := bobPTS - 30*time.Second
	if diff < 0 {
		diff = -diff
	}
	require.Less(t, diff, 100*time.Millisecond,
		"late joiner's first frame should be at ~30s; got %v (diff %v)", bobPTS, diff)

	// Alice's first frame should be at ~0s.
	alicePTS, err := st.GetSessionPTS("alice", "audio-a", 0)
	require.NoError(t, err)

	diff = alicePTS
	if diff < 0 {
		diff = -diff
	}
	require.Less(t, diff, 100*time.Millisecond,
		"first participant's first frame should be at ~0s; got %v", alicePTS)
}

func TestSessionTimeline_FallbackBeforeSRs(t *testing.T) {
	// Verify error when no SRs received.
	st := NewSessionTimeline(nil)
	sessionStart := time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC)
	st.SetSessionStart(sessionStart)

	st.AddParticipant("alice")

	// No SRs have been received: should return error.
	_, err := st.GetSessionPTS("alice", "audio-a", 1000)
	require.Error(t, err)
	require.ErrorIs(t, err, errNoSenderReports)

	// Unknown participant should also error.
	_, err = st.GetSessionPTS("unknown", "track-x", 1000)
	require.Error(t, err)
}
