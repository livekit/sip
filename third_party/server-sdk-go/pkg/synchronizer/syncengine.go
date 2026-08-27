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
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v4"

	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/utils/rtputil"
)

const (
	// defaultOldPacketThreshold is the default age after which packets are dropped.
	defaultOldPacketThreshold = 500 * time.Millisecond

	// defaultNtpTrustThreshold is the maximum allowed divergence between NTP-derived PTS
	// and wall-clock PTS. If NTP disagrees with wall clock by more than this,
	// the NTP data is suspect (bad SRs, clock jumps, nonsensical timing) and
	// we clamp to wall clock. This prevents bad publishers from dragging PTS far
	// from reality.
	defaultNtpTrustThreshold = 500 * time.Millisecond
)

// SyncEngineOption configures a SyncEngine.
type SyncEngineOption func(*SyncEngine)

// WithSyncEngineOnStarted sets a callback invoked once the first track is initialized.
func WithSyncEngineOnStarted(f func()) SyncEngineOption {
	return func(e *SyncEngine) {
		e.onStarted = f
	}
}

// WithSyncEngineStartGate enables the burst-estimation start gate on all tracks.
func WithSyncEngineStartGate() SyncEngineOption {
	return func(e *SyncEngine) {
		e.enableStartGate = true
	}
}

// WithSyncEngineOldPacketThreshold sets the age after which packets are dropped.
// Zero disables the check.
func WithSyncEngineOldPacketThreshold(d time.Duration) SyncEngineOption {
	return func(e *SyncEngine) {
		e.oldPacketThreshold = d
	}
}

// WithSyncEngineNtpTrustThreshold sets the max divergence between NTP-derived
// PTS and wall-clock PTS before clamping to wall clock.
func WithSyncEngineNtpTrustThreshold(d time.Duration) SyncEngineOption {
	return func(e *SyncEngine) {
		e.ntpTrustThreshold = d
	}
}

// WithSyncEngineMediaRunningTime sets the initial media running time provider and max delay.
// If a track's PTS falls behind the deadline by more than maxDelay for >10s, PTS is force-corrected.
func WithSyncEngineMediaRunningTime(mediaRunningTime func() (time.Duration, bool), maxDelay time.Duration) SyncEngineOption {
	return func(e *SyncEngine) {
		e.mediaRunningTime = mediaRunningTime
		e.maxMediaRunningTimeDelay = maxDelay
	}
}

// WithSyncEngineLogger sets the logger for the sync engine and all sub-components.
func WithSyncEngineLogger(l logger.Logger) SyncEngineOption {
	return func(e *SyncEngine) {
		e.logger = l
	}
}

// WithSyncEngineAudioDriftCompensated signals that audio drift is handled
// externally (e.g., by a tempo controller) and the sync engine should not
// apply NTP PTS corrections to audio tracks. NTP regression still runs for
// drift measurement and reporting.
func WithSyncEngineAudioDriftCompensated() SyncEngineOption {
	return func(e *SyncEngine) {
		e.audioDriftCompensated = true
	}
}

// SyncEngine orchestrates NtpEstimator, ParticipantClock, and SessionTimeline
// to provide cross-participant alignment and per-participant A/V lip sync.
// It implements the Sync interface.
type SyncEngine struct {
	mu       sync.Mutex
	timeline *SessionTimeline
	tracks   map[uint32]*syncEngineTrack // keyed by SSRC
	trackIDs map[string]*syncEngineTrack // keyed by track ID

	startedAt atomic.Int64
	endedAt   atomic.Int64
	// ended is the authoritative "End() has been called" signal. It is
	// distinct from endedAt: endedAt is the PTS-derived end timestamp (zero
	// if End was called before any track started), whereas ended is set
	// unconditionally on End() so post-End AddTrack can detect the sealed
	// state even when the session never produced media.
	ended atomic.Bool

	// high-water mark for removed tracks, so End() includes their PTS
	maxRemovedPTS time.Duration

	logger                logger.Logger
	enableStartGate       bool
	oldPacketThreshold    time.Duration
	ntpTrustThreshold     time.Duration // max NTP-vs-wall divergence before clamping to wall clock
	audioDriftCompensated bool          // audio drift handled externally (e.g., tempo controller)
	onStarted             func()

	mediaRunningTime         func() (time.Duration, bool)
	maxMediaRunningTimeDelay time.Duration
	mediaRunningTimeLock     sync.RWMutex
}

// NewSyncEngine creates a new SyncEngine with the given options.
func NewSyncEngine(opts ...SyncEngineOption) *SyncEngine {
	e := &SyncEngine{
		tracks:             make(map[uint32]*syncEngineTrack),
		trackIDs:           make(map[string]*syncEngineTrack),
		oldPacketThreshold: defaultOldPacketThreshold,
		ntpTrustThreshold:  defaultNtpTrustThreshold,
	}
	for _, opt := range opts {
		opt(e)
	}
	e.timeline = NewSessionTimeline(e.logger)
	return e
}

// AddTrack registers a new track and returns a TrackSync handle.
//
// If the track's SSRC or track ID collides with an existing entry (common with
// browser/SFU renegotiation), the existing entry is closed and cleaned up
// first — this prevents leaked tracks, missed maxRemovedPTS contributions, and
// stale NtpEstimator state in the shared ParticipantClock.
//
// If End() has already been called on the engine, the returned track is
// immediately closed so the caller's pipeline drains cleanly without emitting
// post-end media.
func (e *SyncEngine) AddTrack(track TrackRemote, participantID string) TrackSync {
	ssrc := uint32(track.SSRC())
	trackID := track.ID()
	clockRate := track.Codec().ClockRate

	e.mu.Lock()
	defer e.mu.Unlock()

	// Defensively clean up colliding entries. The two lookups may resolve to
	// the same track (exact re-add) or to two distinct tracks (partial reuse);
	// removeTrackLocked deletes from both maps, so the second check naturally
	// sees nil for the same-track case.
	if existing := e.tracks[ssrc]; existing != nil {
		e.removeTrackLocked(existing)
	}
	if existing := e.trackIDs[trackID]; existing != nil {
		e.removeTrackLocked(existing)
	}

	// Ensure the participant exists in the timeline.
	e.timeline.GetOrAddParticipant(participantID)

	st := &syncEngineTrack{
		engine:        e,
		track:         track,
		participantID: participantID,
		logger:        e.getTrackLogger(track),
		converter:     rtputil.NewRTPConverter(int64(clockRate)),
	}

	if e.enableStartGate {
		st.startGate = newStartGate(clockRate, track.Kind(), nil)
	}

	if e.ended.Load() {
		// Post-End: the session is sealed. Mark the track closed so every
		// GetPTS returns io.EOF without emitting media. Use the dedicated
		// `ended` flag rather than endedAt: when End() ran with no tracks
		// initialized yet (startedAt == 0), endedAt stays at 0 since there
		// is no meaningful PTS-derived timestamp.
		st.closed = true
		st.logger.Warnw("AddTrack called after End(); returning closed track", nil)
	}

	e.tracks[ssrc] = st
	e.trackIDs[trackID] = st

	return st
}

// RemoveTrack removes a track by track ID.
func (e *SyncEngine) RemoveTrack(trackID string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	st, ok := e.trackIDs[trackID]
	if !ok {
		return
	}
	e.removeTrackLocked(st)
}

// removeTrackLocked closes a track, snapshots its final PTS into the engine's
// high-water mark, removes it from the lookup maps, and tears down the
// associated timeline state. Caller must hold e.mu.
//
// Performing all of this under e.mu (rather than dropping the lock partway as
// the original implementation did) guarantees three properties:
//   - No concurrent GetPTS can advance lastPTSAdjusted between the snapshot
//     and the close (st.closed is set under st.mu inside the same critical
//     section that reads lastPTSAdjusted).
//   - The hasTracksForParticipantLocked → RemoveParticipant decision is made
//     against a consistent map state, eliminating the TOCTOU window in which
//     a concurrent AddTrack for the same participant could have its freshly
//     inserted ParticipantClock deleted out from under it.
//   - There is no opportunity for End() to interleave and observe a stale
//     maxRemovedPTS.
func (e *SyncEngine) removeTrackLocked(st *syncEngineTrack) {
	st.mu.Lock()
	st.closeLocked()
	lastPTS := st.lastPTSAdjusted
	participantID := st.participantID
	trackID := st.track.ID()
	ssrc := uint32(st.track.SSRC())
	st.mu.Unlock()

	if lastPTS > e.maxRemovedPTS {
		e.maxRemovedPTS = lastPTS
	}
	delete(e.tracks, ssrc)
	delete(e.trackIDs, trackID)

	if pc := e.timeline.GetParticipantClock(participantID); pc != nil {
		pc.RemoveTrack(trackID)
	}
	if !e.hasTracksForParticipantLocked(participantID) {
		e.timeline.RemoveParticipant(participantID)
	}

	st.logger.Infow("track removed", "lastPTS", lastPTS)
}

// OnRTCP processes an RTCP packet, dispatching sender reports to the appropriate
// track's NTP estimator and ParticipantClock.
func (e *SyncEngine) OnRTCP(packet rtcp.Packet) {
	sr, ok := packet.(*rtcp.SenderReport)
	if !ok {
		return
	}

	e.mu.Lock()
	st, ok := e.tracks[sr.SSRC]
	if !ok {
		e.mu.Unlock()
		return
	}
	participantID := st.participantID
	trackID := st.track.ID()
	clockRate := st.track.Codec().ClockRate
	kind := st.track.Kind()
	e.mu.Unlock()

	now := time.Now()

	// Feed the SR to the session timeline UNDER st.mu so that a concurrent
	// removeTrackLocked (which takes st.mu via closeLocked) cannot delete the
	// track's NtpEstimator from pc.tracks between our lookup and our update —
	// otherwise pc.OnSenderReport would auto-create a zombie estimator that
	// nothing will ever clean up (it lives in pc.tracks until the participant
	// itself is removed, which only happens when the participant's LAST
	// track is removed).
	//
	// We also drop the SR entirely if the track was closed before we got
	// here: a removed track's SRs should not influence the timeline.
	st.mu.Lock()
	if st.closed {
		st.mu.Unlock()
		return
	}
	e.timeline.OnSenderReport(participantID, trackID, clockRate, sr.NTPTime, sr.RTPTime, now)
	onSR := st.onSR
	st.mu.Unlock()

	if onSR == nil {
		return
	}

	startedAt := e.startedAt.Load()
	if startedAt == 0 {
		return
	}

	sessionPTS, err := e.timeline.GetSessionPTS(participantID, trackID, sr.RTPTime)
	if err != nil {
		return
	}

	var drift time.Duration
	if e.audioDriftCompensated && kind == webrtc.RTPCodecTypeAudio {
		// Audio PTS is emitted using wall-clock (NTP corrections skipped); the
		// tempo controller closes the gap between what we emit and the NTP
		// regression. Report `wallPTS - sessionPTS`: positive = wall PTS ahead
		// (slow down), negative = behind (speed up).
		wallPTS, ok := st.wallClockPTSForRTP(sr.RTPTime, now)
		if !ok {
			return
		}
		drift = wallPTS - sessionPTS
	} else {
		// Diagnostic: how far the NTP regression is from wall clock since
		// session start. Not used for closed-loop correction.
		sessionStart := time.Unix(0, startedAt)
		drift = sessionPTS - now.Sub(sessionStart)
	}

	st.logger.Debugw("sender report",
		"drift", drift,
		"sessionPTS", sessionPTS,
	)
	onSR(drift)
}

// End signals the end of the session. After End() returns, all currently
// registered tracks will return io.EOF from GetPTS for any packet whose PTS
// would exceed the high-water mark of emissions seen so far, and any future
// AddTrack will return an already-closed track.
//
// End() is safe to call multiple times: the ended flag and endedAt are stable
// once set, and drain ceilings on already-drained tracks are idempotent.
func (e *SyncEngine) End() {
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.ended.Load() {
		// Idempotent: second End() call has nothing to do. lastPTSAdjusted
		// on surviving tracks cannot have advanced past the drain ceiling
		// set by the first call (GetPTS would have returned io.EOF), so
		// recomputing maxPTS would just yield the same value and rewriting
		// endedAt would be redundant.
		return
	}
	e.ended.Store(true)

	// Acquire all per-track locks for a single atomic snapshot + drain pass.
	// Holding every st.mu through both the max-PTS read AND the drain-ceiling
	// write closes the TOCTOU in which a track could advance lastPTSAdjusted
	// between the snapshot loop and the drain-set loop, emitting a packet
	// past what endedAt promised. GetPTS is the only other st.mu holder, and
	// it never acquires more than one st.mu at a time, so there is no
	// deadlock risk in lock-everything order.
	for _, st := range e.tracks {
		st.mu.Lock()
	}

	maxPTS := e.maxRemovedPTS
	for _, st := range e.tracks {
		if st.lastPTSAdjusted > maxPTS {
			maxPTS = st.lastPTSAdjusted
		}
	}

	startedAt := e.startedAt.Load()
	if startedAt > 0 {
		e.endedAt.Store(startedAt + int64(maxPTS))
	}
	// else: leave endedAt = 0. End() was called before any track started;
	// there is no meaningful PTS-derived end timestamp. The `ended` flag
	// above is what tells AddTrack to return closed tracks.

	// Per-track disposition: tracks that have already emitted media get a
	// drain ceiling so their in-flight packets can flush up to maxPTS; tracks
	// that have not yet emitted are closed outright. The latter handles two
	// distinct edge cases under one rule:
	//
	//   1. End() called before any track started. All hasEmitted are false,
	//      so all tracks close — no media leaks via the post-init first
	//      packet that would otherwise slip through `pts > maxPTS` when
	//      maxPTS == 0.
	//
	//   2. End() called after some tracks started but a sibling track is
	//      still buffered in the start gate (initialized via PrimeForStart
	//      but not yet emitted via GetPTS). Without this rule, the sibling
	//      track would inherit a maxPTS == 0 drain ceiling (since it never
	//      contributed to lastPTSAdjusted) — but maxPTS gets the global max
	//      from the active tracks, so its first packet's pts (typically
	//      sessionOffset) could still slip through up to that global value.
	//      Closing here prevents post-End emission from a track that never
	//      produced media before the seal.
	for _, st := range e.tracks {
		if st.hasEmitted {
			st.maxPTS = maxPTS
			st.maxPTSSet = true
		} else {
			st.closeLocked()
		}
	}

	for _, st := range e.tracks {
		st.mu.Unlock()
	}
}

// GetStartedAt returns the start timestamp in nanoseconds, or 0 if not started.
func (e *SyncEngine) GetStartedAt() int64 {
	return e.startedAt.Load()
}

// GetEndedAt returns the end timestamp in nanoseconds, or 0 if not ended.
func (e *SyncEngine) GetEndedAt() int64 {
	return e.endedAt.Load()
}

// SetMediaRunningTime sets the external media running time provider.
func (e *SyncEngine) SetMediaRunningTime(mediaRunningTime func() (time.Duration, bool)) {
	e.mediaRunningTimeLock.Lock()
	e.mediaRunningTime = mediaRunningTime
	e.mediaRunningTimeLock.Unlock()
}

// getMediaDeadline returns the current pipeline deadline, or false if unavailable.
func (e *SyncEngine) getMediaDeadline() (time.Duration, bool) {
	e.mediaRunningTimeLock.RLock()
	fn := e.mediaRunningTime
	e.mediaRunningTimeLock.RUnlock()
	if fn == nil {
		return 0, false
	}
	return fn()
}

// initializeIfNeeded sets the session start time on the first track
// initialization and returns the resulting startedAt value alongside a
// non-nil onStarted callback IFF this call won the race (and an onStarted
// callback is configured).
//
// The timeline's sessionStart is established BEFORE startedAt is published
// atomically. This ordering matters: any code path that checks
// startedAt.Load() != 0 and then queries the timeline (e.g., OnRTCP →
// GetSessionPTS) must not see startedAt set without the timeline also being
// ready, or it would silently drop the SR callback / force wall-clock PTS
// on a concurrent GetPTS that observed startedAt before timeline.hasStart.
//
// The caller is responsible for invoking the returned callback AFTER releasing
// any per-track mutex it holds. Invoking the user-supplied onStarted while
// st.mu is held would expose the callback to a reentrancy deadlock if it
// touched the originating track (e.g., calling GetPTS, LastPTSAdjusted, or
// Close on it).
func (e *SyncEngine) initializeIfNeeded(receivedAt time.Time) (int64, func()) {
	nano := receivedAt.UnixNano()
	if e.timeline.SetSessionStartIfNotSet(receivedAt) {
		// We won. Publish startedAt only after the timeline is observably
		// ready, so callers gated on startedAt see consistent state.
		e.startedAt.Store(nano)
		return nano, e.onStarted
	}
	// Non-winner: another goroutine has already set the timeline, but may not
	// yet have published e.startedAt atomically. Read from the timeline to
	// get a stable value — reading e.startedAt.Load() here could return 0
	// during that brief window and cause the caller's sessionOffset to be
	// computed as receivedAt.UnixNano() (huge), permanently offsetting that
	// track from the session timeline.
	return e.timeline.GetSessionStartNanos(), nil
}

// hasTracksForParticipantLocked returns true if any remaining track belongs
// to the given participantID. Caller must hold e.mu.
func (e *SyncEngine) hasTracksForParticipantLocked(participantID string) bool {
	for _, st := range e.tracks {
		if st.participantID == participantID {
			return true
		}
	}
	return false
}

func (e *SyncEngine) getTrackLogger(track TrackRemote) logger.Logger {
	if e.logger != nil {
		return e.logger.WithValues("trackID", track.ID(), "kind", track.Kind().String())
	}
	return logger.GetLogger().WithValues("trackID", track.ID(), "kind", track.Kind().String(), "syncEngine", true)
}
