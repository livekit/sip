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
	"time"

	"github.com/livekit/protocol/logger"
)

// ParticipantClock holds NTP estimation state for a single participant. OWD
// is tracked per-track inside each NtpEstimator (see NtpEstimator comment for
// why a participant-wide OWD estimator is incorrect when a participant has
// both audio and video tracks).
type ParticipantClock struct {
	mu            sync.Mutex
	logger        logger.Logger
	participantID string
	tracks        map[string]*NtpEstimator
	seenFirstSR   map[string]bool
	outliers      map[string]int
}

// NewParticipantClock creates a new ParticipantClock.
func NewParticipantClock(l logger.Logger, participantID string) *ParticipantClock {
	return &ParticipantClock{
		logger:        l,
		participantID: participantID,
		tracks:        make(map[string]*NtpEstimator),
		seenFirstSR:   make(map[string]bool),
		outliers:      make(map[string]int),
	}
}

// OnSenderReport processes an RTCP sender report for a track.
// It updates the track's NTP estimator (which in turn updates the per-track
// OWD estimator on accepted SRs).
func (pc *ParticipantClock) OnSenderReport(trackID string, clockRate uint32, ntpTime uint64, rtpTimestamp uint32, receivedAt time.Time) {
	pc.mu.Lock()
	defer pc.mu.Unlock()

	est, ok := pc.tracks[trackID]
	if !ok {
		est = NewNtpEstimator(clockRate)
		pc.tracks[trackID] = est
	}

	result := est.OnSenderReport(ntpTime, rtpTimestamp, receivedAt)
	switch result {
	case SROutlier:
		pc.outliers[trackID]++
		if pc.logger != nil {
			if pc.outliers[trackID] == 1 {
				pc.logger.Warnw("sender report rejected as outlier", nil,
					"trackID", trackID,
					"rtpTimestamp", rtpTimestamp,
					"ntpTime", ntpTime,
				)
			} else {
				pc.logger.Debugw("sender report rejected as outlier (suppressed)",
					"trackID", trackID,
					"outliersTotal", pc.outliers[trackID],
				)
			}
		}
	case SRRebuilt:
		if pc.logger != nil {
			pc.logger.Infow("NTP estimator rebuilt after persistent outliers",
				"participantID", pc.participantID,
				"trackID", trackID,
				"outliersTotal", pc.outliers[trackID],
			)
		}
		delete(pc.outliers, trackID)
		// A rebuild starts a new regression — the next SR is effectively a first.
		delete(pc.seenFirstSR, trackID)
	case SRAccepted:
		if n := pc.outliers[trackID]; n > 0 {
			if pc.logger != nil {
				pc.logger.Infow("sender report accepted after outliers",
					"participantID", pc.participantID,
					"trackID", trackID,
					"rejected", n,
				)
			}
			delete(pc.outliers, trackID)
		}
		if !pc.seenFirstSR[trackID] {
			pc.seenFirstSR[trackID] = true
			if pc.logger != nil {
				pc.logger.Infow("first sender report accepted",
					"participantID", pc.participantID,
					"trackID", trackID,
				)
			}
		}
	}
}

// RtpToReceiverClock maps an RTP timestamp to a time on the receiver's clock.
// The result is ntpTime + estimatedOWD, which places the sender's NTP time
// into the receiver's clock domain.
func (pc *ParticipantClock) RtpToReceiverClock(trackID string, rtpTimestamp uint32) (time.Time, error) {
	pc.mu.Lock()
	defer pc.mu.Unlock()

	est, ok := pc.tracks[trackID]
	if !ok {
		return time.Time{}, errNoSenderReports
	}

	if !est.IsReady() {
		return time.Time{}, errNotReady
	}

	ntpTime, err := est.RtpToNtp(rtpTimestamp)
	if err != nil {
		return time.Time{}, err
	}

	return ntpTime.Add(est.EstimatedOWD()), nil
}

// ResetTrack clears the NTP estimator for a track, forcing it to rebuild
// from new sender reports. Used when a stream discontinuity is detected.
func (pc *ParticipantClock) ResetTrack(trackID string) {
	pc.mu.Lock()
	defer pc.mu.Unlock()

	if est, ok := pc.tracks[trackID]; ok {
		est.Reset()
		delete(pc.outliers, trackID)
	}
}

// RemoveTrack removes a track.
func (pc *ParticipantClock) RemoveTrack(trackID string) {
	pc.mu.Lock()
	defer pc.mu.Unlock()

	delete(pc.tracks, trackID)
	delete(pc.seenFirstSR, trackID)
	delete(pc.outliers, trackID)
}

// HasTrack returns true if the participant has a track with the given ID.
func (pc *ParticipantClock) HasTrack(trackID string) bool {
	pc.mu.Lock()
	defer pc.mu.Unlock()

	_, ok := pc.tracks[trackID]
	return ok
}
