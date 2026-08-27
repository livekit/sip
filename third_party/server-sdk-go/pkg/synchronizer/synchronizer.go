// Copyright 2023 LiveKit, Inc.
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
)

const (
	DefaultMaxTsDiff                    = time.Minute
	DefaultMaxDriftAdjustment           = 5 * time.Millisecond
	DefaultDriftAdjustmentWindowPercent = 0.0 // 0 -> disables throttling
	DefaultOldPacketThreshold           = 500 * time.Millisecond
)

type SynchronizerOption func(*SynchronizerConfig)

type SenderReportSyncMode int

const (
	SenderReportSyncModeUnset SenderReportSyncMode = iota
	// SenderReportSyncModeWithoutRebase uses the legacy sender report path without
	// rebasing SR timestamps onto the local clock. This mode can still act on SR
	// drift unless combined with WithAudioPTSAdjustmentDisabled for audio tracks.
	SenderReportSyncModeWithoutRebase
	// SenderReportSyncModeRebase rebases sender reports onto the local clock and
	// applies drift using the gradual adjustment path.
	SenderReportSyncModeRebase
	// SenderReportSyncModeOneShot rebases sender reports onto the local clock but
	// applies drift only as threshold-triggered one-shot corrections.
	SenderReportSyncModeOneShot
)

func (m SenderReportSyncMode) String() string {
	switch m {
	case SenderReportSyncModeWithoutRebase:
		return "without_rebase"
	case SenderReportSyncModeRebase:
		return "rebase"
	case SenderReportSyncModeOneShot:
		return "one_shot"
	default:
		return "unset"
	}
}

// SynchronizerConfig holds configuration for the Synchronizer
type SynchronizerConfig struct {
	MaxTsDiff                    time.Duration
	MaxDriftAdjustment           time.Duration
	DriftAdjustmentWindowPercent float64
	SenderReportSyncMode         SenderReportSyncMode
	// Legacy compatibility control for audio in SenderReportSyncModeWithoutRebase.
	AudioPTSAdjustmentDisabled bool
	// Deprecated: use SenderReportSyncMode instead.
	RTCPSenderReportRebaseEnabled bool
	OldPacketThreshold            time.Duration
	EnableStartGate               bool

	OneShotDriftCorrectionThreshold time.Duration

	OnStarted func()

	MediaRunningTime         func() (time.Duration, bool)
	MaxMediaRunningTimeDelay time.Duration
}

// WithMaxTsDiff sets the maximum acceptable difference between RTP packets
// In case exceeded the synchronizer will adjust the PTS offset to keep the audio and video in sync
func WithMaxTsDiff(maxTsDiff time.Duration) SynchronizerOption {
	return func(config *SynchronizerConfig) {
		config.MaxTsDiff = maxTsDiff
	}
}

// WithMaxDriftAdjustment sets the maximum drift adjustment applied at a time
func WithMaxDriftAdjustment(maxDriftAdjustment time.Duration) SynchronizerOption {
	return func(config *SynchronizerConfig) {
		config.MaxDriftAdjustment = maxDriftAdjustment
	}
}

// WithDriftAdjustmentWinddowPercent controls throttling of how often drift adjustments are applied
// Throttles PTS adjustment to a limited amount in a time window.
// This setting determines how long a certain amount of adjustment
// throttles the next adjustment.
//
// For example, if a 1ms adjustment is appied at 1%, it means that
// 1ms is 1% of ajustment window, so the adjustment window is 100ms
// and next adjustment will not be applied till that time elapses
//
// With the settings of 5ms adjustment at 5%, a mamximum adjustment
// of 5ms per 100ms
func WithDriftAdjustmentWindowPercent(driftAdjustmentWindowPercent float64) SynchronizerOption {
	return func(config *SynchronizerConfig) {
		config.DriftAdjustmentWindowPercent = driftAdjustmentWindowPercent
	}
}

// WithAudioPTSAdjustmentDisabled - disables auto PTS adjustments after sender reports
// Use case: when media processing pipeline needs stable - monotonically increasing
// PTS sequence - small adjustments coming from RTCP sender reports could cause gaps in the audio
// Media processing pipeline could opt out of auto PTS adjustments and handle the gap
// by e.g modifying tempo to compensate instead.
//
// This option is still required if you want the legacy
// SenderReportSyncModeWithoutRebase path but do not want audio SR drift to drive
// gradual PTS offset changes.
func WithAudioPTSAdjustmentDisabled() SynchronizerOption {
	return func(config *SynchronizerConfig) {
		config.AudioPTSAdjustmentDisabled = true
	}
}

// WithSenderReportSyncMode explicitly selects how RTCP sender report drift is
// measured and applied.
func WithSenderReportSyncMode(mode SenderReportSyncMode) SynchronizerOption {
	return func(config *SynchronizerConfig) {
		config.SenderReportSyncMode = mode
	}
}

// Deprecated: use WithSenderReportSyncMode(SenderReportSyncModeRebase) instead.
//
// WithRTCPSenderReportRebaseEnabled - enables rebasing RTCP Sender Report to local clock
func WithRTCPSenderReportRebaseEnabled() SynchronizerOption {
	return func(config *SynchronizerConfig) {
		config.RTCPSenderReportRebaseEnabled = true
	}
}

// WithOldPacketThreshold sets the threshold at which a packet is considered old
func WithOldPacketThreshold(oldPacketThreshold time.Duration) SynchronizerOption {
	return func(config *SynchronizerConfig) {
		config.OldPacketThreshold = oldPacketThreshold
	}
}

// WithStartGate enabled will buffer incoming packets until pacing stabilizes before initializing tracks
func WithStartGate() SynchronizerOption {
	return func(config *SynchronizerConfig) {
		config.EnableStartGate = true
	}
}

// WithOnStarted sets the callback to be called when the synchronizer starts
func WithOnStarted(onStarted func()) SynchronizerOption {
	return func(config *SynchronizerConfig) {
		config.OnStarted = onStarted
	}
}

// WithMediaRunningTime sets the callback to be called to get the media running time
// maxMediaRunningTimeDelay is the maximum allowed latency a packet can be behind the media running time
func WithMediaRunningTime(mediaRunningTime func() (time.Duration, bool), maxMediaRunningTimeDelay time.Duration) SynchronizerOption {
	return func(config *SynchronizerConfig) {
		config.MediaRunningTime = mediaRunningTime
		config.MaxMediaRunningTimeDelay = maxMediaRunningTimeDelay
	}
}

// WithOneShotDriftCorrectionThreshold sets the threshold for one-shot PTS
// correction. It is used only when SenderReportSyncMode is
// SenderReportSyncModeOneShot. In that mode, OWD-normalized drift from RTCP
// Sender Reports is monitored, but gradual PTS adjustments are suppressed.
// Instead, a one-shot PTS correction is applied when the drift reaches this
// threshold.
//
// This is useful for pipelines with e.g live audio mixer where:
//   - Gradual PTS adjustments cause audible gaps (so they must be disabled)
//   - But the input stream may have unsignalled timing drift (e.g. SIP silence suppression)
//     that needs correction before the track falls out of the mixer's live window
//
// The corrected PTS is sanity-checked against the media live window to reject
// bogus SRs.
func WithOneShotDriftCorrectionThreshold(threshold time.Duration) SynchronizerOption {
	return func(config *SynchronizerConfig) {
		config.OneShotDriftCorrectionThreshold = threshold
	}
}

func (c *SynchronizerConfig) normalizeLegacySenderReportSyncMode() {
	if c.SenderReportSyncMode == SenderReportSyncModeUnset {
		switch {
		case c.RTCPSenderReportRebaseEnabled:
			c.SenderReportSyncMode = SenderReportSyncModeRebase
		default:
			c.SenderReportSyncMode = SenderReportSyncModeWithoutRebase
		}
	}

	switch c.SenderReportSyncMode {
	case SenderReportSyncModeOneShot:
		c.RTCPSenderReportRebaseEnabled = true
	case SenderReportSyncModeRebase:
		c.RTCPSenderReportRebaseEnabled = true
	case SenderReportSyncModeWithoutRebase:
		c.RTCPSenderReportRebaseEnabled = false
	}
}

// a single Synchronizer is shared between all audio and video writers
type Synchronizer struct {
	sync.RWMutex

	startedAt int64
	onStarted func()
	endedAt   int64
	config    SynchronizerConfig

	psByIdentity map[string]*participantSynchronizer
	psBySSRC     map[uint32]*participantSynchronizer
	ssrcByID     map[string]uint32

	// lock-free: read by track/participant code that may run under child locks
	mediaRunningTime atomic.Pointer[func() (time.Duration, bool)]
	// start time of the external live media (if used, nil otherwise)
	externalMediaStartTime atomic.Pointer[time.Time]
}

func NewSynchronizer(onStarted func()) *Synchronizer {
	config := SynchronizerConfig{
		MaxTsDiff:                    DefaultMaxTsDiff,
		MaxDriftAdjustment:           DefaultMaxDriftAdjustment,
		DriftAdjustmentWindowPercent: DefaultDriftAdjustmentWindowPercent,
		OldPacketThreshold:           DefaultOldPacketThreshold,
		OnStarted:                    onStarted,
		MediaRunningTime:             nil,
	}
	config.normalizeLegacySenderReportSyncMode()

	return &Synchronizer{
		onStarted:    config.OnStarted,
		psByIdentity: make(map[string]*participantSynchronizer),
		psBySSRC:     make(map[uint32]*participantSynchronizer),
		ssrcByID:     make(map[string]uint32),
		config:       config,
	}
}

func NewSynchronizerWithOptions(opts ...SynchronizerOption) *Synchronizer {
	config := SynchronizerConfig{
		MaxTsDiff:                    DefaultMaxTsDiff,
		MaxDriftAdjustment:           DefaultMaxDriftAdjustment,
		DriftAdjustmentWindowPercent: DefaultDriftAdjustmentWindowPercent,
		OldPacketThreshold:           DefaultOldPacketThreshold,
		OnStarted:                    nil,
		MediaRunningTime:             nil,
	}

	for _, opt := range opts {
		opt(&config)
	}
	config.normalizeLegacySenderReportSyncMode()

	s := &Synchronizer{
		onStarted:    config.OnStarted,
		psByIdentity: make(map[string]*participantSynchronizer),
		psBySSRC:     make(map[uint32]*participantSynchronizer),
		ssrcByID:     make(map[string]uint32),
		config:       config,
	}
	s.SetMediaRunningTime(config.MediaRunningTime)
	return s
}

func (s *Synchronizer) AddTrack(track TrackRemote, participantID string) *TrackSynchronizer {
	t := newTrackSynchronizer(s, track)

	s.Lock()
	p := s.psByIdentity[participantID]
	if p == nil {
		p = newParticipantSynchronizer()
		s.psByIdentity[participantID] = p
	}
	ssrc := uint32(track.SSRC())
	s.ssrcByID[track.ID()] = ssrc
	s.psBySSRC[ssrc] = p
	s.Unlock()

	p.Lock()
	p.tracks[ssrc] = t
	p.Unlock()

	return t
}

func (s *Synchronizer) RemoveTrack(trackID string) {
	s.Lock()
	ssrc := s.ssrcByID[trackID]
	p := s.psBySSRC[ssrc]
	delete(s.ssrcByID, trackID)
	delete(s.psBySSRC, ssrc)
	s.Unlock()
	if p == nil {
		return
	}

	p.removeTrack(ssrc)
}

func (s *Synchronizer) GetStartedAt() int64 {
	s.RLock()
	defer s.RUnlock()

	return s.startedAt
}

// SetMediaRunningTime updates the external media running time provider after the synchronizer has been created.
// Passing a nil provider clears the configuration.
func (s *Synchronizer) SetMediaRunningTime(mediaRunningTime func() (time.Duration, bool)) {
	if mediaRunningTime == nil {
		s.mediaRunningTime.Store(nil)
	} else {
		s.mediaRunningTime.Store(&mediaRunningTime)
	}
}

func (s *Synchronizer) getOrSetStartedAt(now int64) int64 {
	s.Lock()
	defer s.Unlock()

	if s.startedAt == 0 {
		s.startedAt = now

		if s.onStarted != nil {
			s.onStarted()
		}
	}

	return s.startedAt
}

// OnRTCP syncs a/v using sender reports
func (s *Synchronizer) OnRTCP(packet rtcp.Packet) {
	switch pkt := packet.(type) {
	case *rtcp.SenderReport:
		s.Lock()
		p := s.psBySSRC[pkt.SSRC]
		endedAt := s.endedAt
		s.Unlock()

		if endedAt != 0 || p == nil {
			return
		}

		p.onSenderReport(pkt)
	}
}

func (s *Synchronizer) End() {
	s.Lock()
	defer s.Unlock()

	// maxPTS is the drain ceiling: the maximum adjusted PTS after which tracks
	// return EOF. Use the furthest adjusted PTS any track has actually reached
	// so that all tracks can drain up to the same point in the output timeline.
	var maxPTS time.Duration
	for _, p := range s.psByIdentity {
		if m := p.getMaxPTSAdjusted(); m > maxPTS {
			maxPTS = m
		}
	}

	s.endedAt = s.startedAt + int64(maxPTS)

	// drain all
	for _, p := range s.psByIdentity {
		p.drain(maxPTS)
	}
}

func (s *Synchronizer) GetEndedAt() int64 {
	s.RLock()
	defer s.RUnlock()

	return s.endedAt
}

// SynchronizerAdapter wraps the legacy Synchronizer to implement the Sync interface.
// The Synchronizer's own AddTrack returns *TrackSynchronizer (concrete type); this
// adapter's AddTrack returns TrackSync so that *SynchronizerAdapter satisfies Sync.
type SynchronizerAdapter struct {
	*Synchronizer
}

func (a *SynchronizerAdapter) AddTrack(track TrackRemote, participantID string) TrackSync {
	return a.Synchronizer.AddTrack(track, participantID)
}

// AsSyncInterface returns a Sync-compatible wrapper around this Synchronizer.
func (s *Synchronizer) AsSyncInterface() Sync {
	return &SynchronizerAdapter{Synchronizer: s}
}

func (s *Synchronizer) getExternalMediaDeadline() (time.Duration, bool) {
	now := time.Now()

	startTime := s.externalMediaStartTime.Load()
	if startTime == nil {
		cb := s.mediaRunningTime.Load()
		if cb == nil {
			return 0, false
		}
		mediaRunningTime, ok := (*cb)()
		if !ok {
			return 0, false
		}
		st := now.Add(-mediaRunningTime)
		if !s.externalMediaStartTime.CompareAndSwap(nil, &st) {
			startTime = s.externalMediaStartTime.Load()
		} else {
			startTime = &st
		}
	}

	return now.Sub(*startTime) - s.config.MaxMediaRunningTimeDelay, true
}
