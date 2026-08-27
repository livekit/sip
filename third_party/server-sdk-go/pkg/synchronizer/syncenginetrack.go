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
	"io"
	"sync"
	"time"

	"github.com/pion/webrtc/v4"

	"github.com/livekit/media-sdk/jitter"

	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/utils/rtputil"
)

const (
	// transitionSlewRatePerSecond is the rate at which the wall-clock→NTP
	// transition correction is absorbed: 5ms per second of real time.
	transitionSlewRatePerSecond = 5 * time.Millisecond

	// wallClockSanityThreshold is the maximum divergence between RTP-derived PTS
	// and wall-clock PTS before falling back to wall clock in wallClockPTS().
	wallClockSanityThreshold = 5 * time.Second

	// maxTimelyPacketAge is how long a track can be behind the pipeline deadline
	// before its PTS is force-corrected forward.
	maxTimelyPacketAge = 10 * time.Second

	// slewRatePerSecond is the maximum rate at which PTS corrections are absorbed.
	slewRatePerSecond = 5 * time.Millisecond

	// deadbandThreshold is the minimum |correction| before slew smoothing kicks in.
	deadbandThreshold = 5 * time.Millisecond

	// wallSlewAlpha bounds publisher sample-clock skew: steady-state |drift| = R·δ·(1-α)/α ≈ 10ms for R=20ms, δ=±500ppm, α=0.01.
	wallSlewAlpha = 0.01
)

// syncEngineTrack implements TrackSync for a single track within a SyncEngine.
type syncEngineTrack struct {
	engine        *SyncEngine
	track         TrackRemote
	participantID string
	logger        logger.Logger
	converter     *rtputil.RTPConverter
	startGate     startGate // from start_gate.go, nil if not enabled

	mu            sync.Mutex
	startTime     time.Time
	sessionOffset time.Duration // offset from session start to this track's start
	lastTS        uint32

	lastWallPTSSlewed   time.Duration // emission baseline: slew-adjusted, sanity-clamped
	lastWallPTSUnslewed time.Duration // drift-sensor baseline: pure rtpDerived

	lastPTSAdjusted time.Duration // last emitted PTS (post-NTP-correction, post-slew, post-monotonicity)
	initialized     bool          // initializeLocked has run (startTime/sessionOffset/lastTS are set)
	hasEmitted      bool          // GetPTS has returned at least one PTS; used to distinguish "no prior emission" from "prior emission was zero-valued"
	closed          bool

	// NTP transition and smoothing
	ntpTransitioned bool
	transitionSlew  time.Duration
	lastSlewPTS     time.Duration // PTS at which slew was last updated
	lastNtpPTS      time.Duration // last raw NTP PTS (before corrections), for jump detection
	hasLastNtpPTS   bool          // lastNtpPTS holds a baseline from the current NTP regression (cleared when NTP becomes unavailable or on RTP discontinuity)
	ntpCorrection   time.Duration // smoothing correction for SR-induced NTP jumps

	clampedPackets int
	clampStartPTS  time.Duration
	clampMaxDiff   time.Duration

	// pipeline time feedback
	lastTimelyPacket time.Time

	// drain
	maxPTS    time.Duration
	maxPTSSet bool

	onSR func(drift time.Duration)
}

// PrimeForStart implements TrackSync. It buffers packets through the optional
// start gate and initializes the track on the first valid packet.
func (st *syncEngineTrack) PrimeForStart(pkt jitter.ExtPacket) ([]jitter.ExtPacket, int, bool) {
	// onStarted (if any) is invoked after st.mu is released to avoid reentrancy
	// deadlock; see initializeIfNeeded for the rationale.
	var onStarted func()
	defer func() {
		if onStarted != nil {
			onStarted()
		}
	}()

	st.mu.Lock()
	defer st.mu.Unlock()

	if st.closed {
		// Track was closed (either via Close, removeTrackLocked, or post-End
		// AddTrack). Skip initialization entirely — initializeLocked would
		// otherwise spuriously fire onStarted for a track that will only
		// ever return io.EOF from GetPTS. Signal done with no packets.
		return nil, 0, true
	}

	if st.initialized || st.startGate == nil {
		if !st.initialized {
			onStarted = st.initializeLocked(pkt)
		}
		return []jitter.ExtPacket{pkt}, 0, true
	}

	ready, dropped, done := st.startGate.Push(pkt)
	if !done {
		return nil, dropped, false
	}

	if len(ready) == 0 {
		ready = []jitter.ExtPacket{pkt}
	}

	if !st.initialized {
		onStarted = st.initializeLocked(ready[0])
	}

	return ready, dropped, true
}

// initializeLocked sets the track's start time and registers with the engine.
// Caller must hold st.mu. Returns a non-nil onStarted callback IFF this call
// was the first track initialization for the engine; the caller MUST invoke
// the returned callback only after releasing st.mu.
func (st *syncEngineTrack) initializeLocked(pkt jitter.ExtPacket) func() {
	receivedAt := pkt.ReceivedAt
	if receivedAt.IsZero() {
		receivedAt = time.Now()
	}

	st.startTime = receivedAt
	st.lastTS = pkt.Timestamp
	st.lastTimelyPacket = receivedAt
	st.initialized = true

	// Initialize the engine's session start time.
	sessionStart, onStarted := st.engine.initializeIfNeeded(receivedAt)
	st.sessionOffset = time.Duration(receivedAt.UnixNano() - sessionStart)
	// Seed to sessionOffset, not 0: late-joining tracks would else emit wallPTS from 0 and get NTP-trust-clamped to the wrong baseline
	st.lastWallPTSSlewed = st.sessionOffset
	st.lastWallPTSUnslewed = st.sessionOffset

	st.logger.Infow("initialized track",
		"startTime", st.startTime,
		"sessionOffset", st.sessionOffset,
		"rtpTS", pkt.Timestamp,
	)
	return onStarted
}

// GetPTS implements TrackSync. It computes the presentation timestamp for a packet
// using the NTP-grounded timeline when available, falling back to wall clock otherwise.
func (st *syncEngineTrack) GetPTS(pkt jitter.ExtPacket) (time.Duration, error) {
	// onStarted (if any) is invoked after st.mu is released to avoid reentrancy
	// deadlock; see initializeIfNeeded for the rationale.
	var onStarted func()
	defer func() {
		if onStarted != nil {
			onStarted()
		}
	}()

	st.mu.Lock()
	defer st.mu.Unlock()

	if st.closed {
		return 0, io.EOF
	}

	if !st.initialized {
		onStarted = st.initializeLocked(pkt)
	}

	ts := pkt.Timestamp

	// Same RTP timestamp as last packet: return same PTS (same frame).
	// hasEmitted distinguishes "we've previously emitted for this lastTS" from
	// the first-call case where lastTS was just set by initializeLocked.
	if st.hasEmitted && ts == st.lastTS {
		return st.lastPTSAdjusted, nil
	}

	// Drop packets older than threshold.
	if st.engine.oldPacketThreshold > 0 && !pkt.ReceivedAt.IsZero() {
		age := time.Since(pkt.ReceivedAt)
		if age > st.engine.oldPacketThreshold {
			st.logger.Infow("dropping old packet",
				"age", age,
				"threshold", st.engine.oldPacketThreshold,
				"rtpTS", ts,
			)
			return 0, ErrPacketTooOld
		}
	}

	// Step 1: Try NTP-grounded PTS from SessionTimeline.
	rawNtpPTS, ntpErr := st.engine.timeline.GetSessionPTS(st.participantID, st.track.ID(), ts)

	wallPTS, wallPTSUnslewed := st.wallClockPTS(pkt)

	// Audio tracks with external drift compensation (e.g., tempo controller) skip
	// NTP PTS corrections — drift is handled by resampling, not PTS adjustment.
	// NTP regression still runs (via OnSenderReport) for drift measurement.
	useWallClockOnly := st.engine.audioDriftCompensated && st.track.Kind() == webrtc.RTPCodecTypeAudio

	// Step 2: Detect discontinuities and NTP regression jumps on RAW NTP PTS.
	// This operates before any corrections to avoid feedback loops.
	//
	// Signed delta distinguishes forward gaps from backward (reordered) packets.
	// Unsigned uint32 subtraction would wrap a one-frame backward delta into
	// ~24 hours, falsely triggering the discontinuity branch and wiping all
	// NTP state. The jitter buffer should normally hand us in-order packets,
	// but defense-in-depth is cheap.
	signedRTPDelta := int32(ts - st.lastTS)
	rtpDelta := ts - st.lastTS
	rtpDeltaDuration := st.converter.ToDuration(rtpDelta)

	// ntpNowReady tracks whether the NTP regression is the source for THIS
	// packet's PTS. This is the unified gate for jump detection, transition
	// computation, and lastNtpPTS tracking — and the trigger for clearing
	// smoothing state when NTP goes away. Anything that invalidates the
	// regression mid-session (estimator outlier rebuild via the timeline,
	// ResetTrack from a discontinuity below, useWallClockOnly mode) flows
	// through this single gate.
	ntpNowReady := ntpErr == nil && !useWallClockOnly

	// signedRTPDelta > 0 implicitly skips the first call (delta is 0 there because
	// initializeLocked set lastTS to this packet's ts) and any backward reorders.
	discontinuity := signedRTPDelta > 0 && rtpDeltaDuration >= 30*time.Second
	if discontinuity {
		// Stream restart, SSRC reuse with new RTP offset, or massive gap.
		// Reset the timeline-side regression. rawNtpPTS we already have was
		// computed against the pre-reset regression; treat NTP as unavailable
		// for this packet so we fall back to wallPTS and clear smoothing state
		// in the unified branch below.
		st.engine.timeline.ResetTrack(st.participantID, st.track.ID())
		ntpNowReady = false
		// NTP smoothing state (lastNtpPTS, transitionSlew, ntpCorrection) is
		// cleared by the !ntpNowReady branch below. lastSlewPTS does not need
		// to be cleared: the end-of-iter update overwrites it with the
		// pre-slew PTS for this packet, providing a valid baseline for the
		// next iteration's slewPTSDelta.
		// Re-anchor unslewed; the 30s+ delta would otherwise persist as phantom drift
		wallPTSUnslewed = wallPTS
		st.logger.Warnw("stream discontinuity detected, resetting NTP state", nil,
			"rtpDelta", rtpDelta,
			"rtpDeltaDuration", rtpDeltaDuration,
		)
	}

	if !ntpNowReady {
		st.endClampEpisodeLocked(wallPTS)

		// NTP is not the source for this packet — either it was never ready,
		// the estimator was rebuilt internally (persistent outliers in the
		// timeline's NtpEstimator), the discontinuity branch above just reset
		// it, or we're in audio-drift-compensated mode. Forget any in-flight
		// NTP smoothing state so that a future NTP-ready packet triggers a
		// fresh transition against whatever regression is current then.
		//
		// Without this clear, ntpTransitioned would stay true from the prior
		// NTP session, the transition correction would never be recomputed
		// against the new regression, and lastNtpPTS would feed a stale
		// expectation into the jump-detection branch on the very first
		// post-rebuild packet — producing a large bogus ntpCorrection.
		if st.ntpTransitioned || st.hasLastNtpPTS || st.ntpCorrection != 0 || st.transitionSlew != 0 {
			st.ntpTransitioned = false
			st.transitionSlew = 0
			st.ntpCorrection = 0
			st.hasLastNtpPTS = false
			st.lastNtpPTS = 0
		}
	} else if st.hasLastNtpPTS && signedRTPDelta > 0 {
		// Steady NTP: detect regression jumps by comparing raw NTP PTS against
		// the previous packet's raw value extrapolated by RTP delta.
		expectedRawNtpPTS := st.lastNtpPTS + rtpDeltaDuration
		jump := rawNtpPTS - expectedRawNtpPTS
		if jump > deadbandThreshold || jump < -deadbandThreshold {
			st.ntpCorrection -= jump
			st.logger.Debugw("NTP regression jump detected",
				"jump", jump,
				"ntpCorrection", st.ntpCorrection,
			)
		}
	}

	// Track lastNtpPTS for forward (or first-NTP-packet) updates — using a
	// backward sample would corrupt the next iteration's expected-jump
	// baseline. signedRTPDelta >= 0 covers the first call (delta = 0, by way
	// of initializeLocked setting lastTS to this packet's ts) and forward
	// packets. hasLastNtpPTS is the canonical "have we recorded a baseline"
	// flag; lastNtpPTS being zero is also a valid raw NTP value at session
	// start so we never rely on it as a sentinel.
	if ntpNowReady && signedRTPDelta >= 0 {
		st.lastNtpPTS = rawNtpPTS
		st.hasLastNtpPTS = true
	}

	// Step 3: Compute final PTS with corrections. ntpNowReady (set above) is
	// the unified gate — covers ntpErr, useWallClockOnly, AND the discontinuity
	// reset that just invalidated rawNtpPTS by resetting the regression.
	var pts time.Duration
	if !ntpNowReady {
		pts = wallPTS
	} else {
		// Apply NTP jump correction.
		pts = rawNtpPTS + st.ntpCorrection

		// Clamp corrected PTS to within trust threshold of wall clock.
		clamped := false
		diff := pts - wallPTS
		if diff > st.engine.ntpTrustThreshold || diff < -st.engine.ntpTrustThreshold {
			if st.noteClampLocked(diff, wallPTS) {
				st.logger.Warnw("NTP PTS exceeds trust threshold, clamping to wall clock", nil,
					"rawNtpPTS", rawNtpPTS,
					"ntpCorrection", st.ntpCorrection,
					"wallPTS", wallPTS,
					"diff", diff,
				)
			}
			pts = wallPTS
			clamped = true
		} else {
			st.endClampEpisodeLocked(wallPTS)
		}

		// On first successful NTP PTS that is NOT clamped, compute the
		// transition correction. If clamped, we leave ntpTransitioned=false
		// so a future un-clamped packet can establish the transition fresh.
		// Setting ntpTransitioned=true here on a clamp would compute
		// transitionSlew = wallPTS - wallPTS = 0 and lock that in — when
		// rawNtpPTS later converges back within the trust threshold, pts
		// would jump by up to ntpTrustThreshold with no slew to absorb it.
		if !clamped && !st.ntpTransitioned {
			st.transitionSlew = wallPTS - pts
			st.ntpTransitioned = true
			st.logger.Infow("NTP transition",
				"wallPTS", wallPTS,
				"ntpPTS", rawNtpPTS,
				"transitionSlew", st.transitionSlew,
			)
		}
	}

	// Capture the pre-slew, pre-force-correction, pre-monotonicity PTS as the
	// next iteration's slew baseline. This is what tracks "media-time
	// progression" — using the post-correction emitted PTS instead (the
	// pre-#911 author's original choice and a regression in #911) freezes the
	// decay clock whenever a force-correction jump or monotonicity bump shifts
	// the output away from raw media time: slewPTSDelta collapses to ~1ms
	// (monotonicity step) per packet, dropping the decay step from the
	// intended 100µs to ~5µs, and stalling drain for tens of minutes.
	preSlewPTS := pts

	// Compute PTS delta for slew rate calculations.
	var slewPTSDelta time.Duration
	if st.hasEmitted {
		slewPTSDelta = pts - st.lastSlewPTS
	}

	// Step 4: Apply transition slew (absorb gradually toward zero).
	if st.transitionSlew != 0 {
		pts += st.transitionSlew

		if slewPTSDelta > 0 {
			maxStep := time.Duration(float64(transitionSlewRatePerSecond) * slewPTSDelta.Seconds())
			if st.transitionSlew > 0 {
				st.transitionSlew -= maxStep
				if st.transitionSlew < 0 {
					st.transitionSlew = 0
				}
			} else {
				st.transitionSlew += maxStep
				if st.transitionSlew > 0 {
					st.transitionSlew = 0
				}
			}
		}
	}

	// Decay ntpCorrection toward zero via slew.
	if st.ntpCorrection != 0 {
		if slewPTSDelta > 0 {
			maxStep := time.Duration(float64(slewRatePerSecond) * slewPTSDelta.Seconds())
			if st.ntpCorrection > 0 {
				st.ntpCorrection -= maxStep
				if st.ntpCorrection < 0 {
					st.ntpCorrection = 0
				}
			} else {
				st.ntpCorrection += maxStep
				if st.ntpCorrection > 0 {
					st.ntpCorrection = 0
				}
			}
		}
	}

	// Step 6: Pipeline time feedback — if the track has fallen behind the
	// pipeline's deadline for too long, force-correct PTS forward.
	if deadline, ok := st.engine.getMediaDeadline(); ok && st.engine.maxMediaRunningTimeDelay > 0 {
		limit := deadline - st.engine.maxMediaRunningTimeDelay
		if pts < limit {
			if time.Since(st.lastTimelyPacket) > maxTimelyPacketAge {
				oldPTS := pts
				pts = deadline - st.engine.maxMediaRunningTimeDelay/2
				correction := pts - oldPTS
				st.logger.Warnw("force-correcting PTS forward, track behind pipeline deadline", nil,
					"oldPTS", oldPTS,
					"newPTS", pts,
					"deadline", deadline,
					"behindBy", limit-oldPTS,
					"correction", correction,
				)
				// Emission-side only: lastWallPTSUnslewed and NTP state must keep seeing true media-vs-wall skew, not our synthetic catch-up.
				// wallPTS was captured pre-correction and is stored back into lastWallPTSSlewed below, so shift the local too or the bump is lost.
				st.sessionOffset += correction
				wallPTS += correction
				preSlewPTS += correction
				// Reset timeliness clock so a lingering transient deficit doesn't fire force-correction on every subsequent packet, collapsing them into the same newPTS at the mixer.
				st.lastTimelyPacket = time.Now()
			}
		} else {
			st.lastTimelyPacket = time.Now()
		}
	}

	// Step 7: Enforce monotonicity. hasEmitted distinguishes "we have a prior
	// emitted PTS to be greater than" from the first-emission case where
	// lastPTSAdjusted is zero by default.
	if st.hasEmitted && pts < st.lastPTSAdjusted+time.Millisecond {
		pts = st.lastPTSAdjusted + time.Millisecond
	}

	// Step 8: Enforce drain ceiling.
	if st.maxPTSSet && pts > st.maxPTS {
		return 0, io.EOF
	}

	// Anchor forward-only: backward packets would mismatch the next iter's expected-jump baselines and burn slew via a bogus slewPTSDelta
	if signedRTPDelta >= 0 {
		st.lastTS = ts
		st.lastWallPTSSlewed = wallPTS
		st.lastWallPTSUnslewed = wallPTSUnslewed
		st.lastSlewPTS = preSlewPTS
	}
	st.lastPTSAdjusted = pts
	st.hasEmitted = true

	return pts, nil
}

func (st *syncEngineTrack) noteClampLocked(diff, wallPTS time.Duration) bool {
	st.clampedPackets++
	if st.clampedPackets == 1 {
		st.clampStartPTS = wallPTS
		st.clampMaxDiff = diff
		return true
	}

	mag, curMax := diff, st.clampMaxDiff
	if mag < 0 {
		mag = -mag
	}
	if curMax < 0 {
		curMax = -curMax
	}
	if mag > curMax {
		st.clampMaxDiff = diff
	}
	return false
}

func (st *syncEngineTrack) endClampEpisodeLocked(wallPTS time.Duration) {
	if st.clampedPackets == 0 {
		return
	}

	st.logger.Infow("NTP clamping episode ended",
		"clampedPackets", st.clampedPackets,
		"episodeSpan", wallPTS-st.clampStartPTS,
		"maxDiff", st.clampMaxDiff,
	)
	st.clampedPackets = 0
	st.clampStartPTS = 0
	st.clampMaxDiff = 0
}

func (st *syncEngineTrack) wallClockPTS(pkt jitter.ExtPacket) (slewed, unslewed time.Duration) {
	return st.wallClockPTSForRTPLocked(pkt.Timestamp, pkt.ReceivedAt)
}

// wallClockPTSForRTP returns the sensor-side wall PTS for OnRTCP; !ok when the value would be a phantom (uninit, backward RTP, or 30s+ jump)
func (st *syncEngineTrack) wallClockPTSForRTP(ts uint32, receivedAt time.Time) (time.Duration, bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if !st.initialized || st.closed {
		return 0, false
	}
	signedDelta := int32(ts - st.lastTS)
	if signedDelta < 0 {
		return 0, false
	}
	if st.converter.ToDuration(uint32(signedDelta)) >= 30*time.Second {
		return 0, false
	}
	_, unslewed := st.wallClockPTSForRTPLocked(ts, receivedAt)
	return unslewed, true
}

// wallClockPTSForRTPLocked returns (slewed for emission, unslewed for drift sensor); slewing both would hide publisher skew from the tempo controller
func (st *syncEngineTrack) wallClockPTSForRTPLocked(ts uint32, receivedAt time.Time) (slewed, unslewed time.Duration) {
	// A zero receivedAt would make wallElapsed hugely negative and lock every downstream clamp onto a bogus baseline.
	if receivedAt.IsZero() {
		receivedAt = time.Now()
	}

	wallElapsed := receivedAt.Sub(st.startTime) + st.sessionOffset
	if wallElapsed < 0 {
		wallElapsed = 0
	}

	if !st.initialized {
		return wallElapsed, wallElapsed
	}

	// uint32 wraps for backward RTP; slewed path catches it via the 5s clamp, unslewed relies on caller-side filtering
	rtpDelta := ts - st.lastTS
	rtpDur := st.converter.ToDuration(rtpDelta)
	unslewed = st.lastWallPTSUnslewed + rtpDur

	slewedRTP := st.lastWallPTSSlewed + rtpDur
	diff := slewedRTP - wallElapsed
	if diff > wallClockSanityThreshold || diff < -wallClockSanityThreshold {
		return wallElapsed, unslewed
	}

	// Slew keeps emitted PTS near wallElapsed across the sanity band; see wallSlewAlpha for the steady-state math
	absorbed := time.Duration(float64(diff) * wallSlewAlpha)
	return slewedRTP - absorbed, unslewed
}

// OnSenderReport implements TrackSync. It stores a callback invoked on sender reports.
func (st *syncEngineTrack) OnSenderReport(f func(drift time.Duration)) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.onSR = f
}

// LastPTSAdjusted implements TrackSync.
func (st *syncEngineTrack) LastPTSAdjusted() time.Duration {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.lastPTSAdjusted
}

// Close implements TrackSync.
func (st *syncEngineTrack) Close() {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.closeLocked()
}

// closeLocked is the lock-held implementation of Close. Caller must hold st.mu.
// Used by SyncEngine.removeTrackLocked to inline the close action within the
// same critical section that snapshots lastPTSAdjusted, eliminating the race
// window between snapshot and close.
//
// Closing only stops FUTURE OnRTCP invocations (which check st.closed before
// the timeline call). An OnRTCP that has already acquired st.mu and read
// st.onSR into a local before closeLocked can run will still invoke that
// captured callback once after close — st.mu serializes the two but the
// callback function pointer is already copied. Consumers must therefore
// tolerate one final SR callback per RemoveTrack/Close. Tightening this
// would require holding st.mu across the user-supplied callback, which is
// an unacceptable constraint on what the callback may do.
func (st *syncEngineTrack) closeLocked() {
	st.endClampEpisodeLocked(st.lastPTSAdjusted)

	st.closed = true
	// Clear the field too: OnRTCP invocations that arrive AFTER close and
	// somehow pass the closed check would see nil. Also helps GC release
	// any state captured by the callback closure.
	st.onSR = nil
}
