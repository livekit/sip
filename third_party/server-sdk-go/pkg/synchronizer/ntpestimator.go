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
	"errors"
	"math"
	"sync"
	"time"

	"github.com/livekit/mediatransportutil/pkg/latency"
)

const (
	// maxSRSamples is the sliding window size for sender report pairs.
	maxSRSamples = 20

	// minSamplesReady is the minimum number of SR pairs needed before the
	// regression is considered ready. With only 2 points the slope is entirely
	// determined by SR timing jitter; 4 gives a much more stable fit.
	minSamplesReady = 4

	// outlierThresholdStdDevs is the number of standard deviations beyond which
	// a new SR is considered an outlier and excluded from the regression.
	outlierThresholdStdDevs = 3.0

	// maxConsecutiveOutliers is how many SRs may be rejected in a row before
	// the estimator decides the sender's NTP clock has stepped (or otherwise
	// jumped to a state inconsistent with the prior regression) and rebuilds
	// from scratch. At typical 1 Hz RTCP this is ~5 seconds of sustained
	// rejection.
	maxConsecutiveOutliers = 5

	// minOutlierStdDevNanos is the floor applied to residStd when computing
	// the outlier threshold. Without it, an exactly-fitting set of samples
	// would produce residStd = 0 (or a few ULPs), and outlier detection would
	// effectively disable — letting a sender NTP step through as if it were a
	// real measurement. 100µs corresponds to ~300µs at the 3σ threshold, which
	// is well below typical SR jitter on real networks but well above any
	// float-precision artifact.
	minOutlierStdDevNanos = 100_000 // 100µs in nanoseconds

	// ntpEpochOffset is the number of seconds between the NTP epoch (1900-01-01)
	// and the Unix epoch (1970-01-01).
	ntpEpochOffset = 2208988800
)

var errNotReady = errors.New("NtpEstimator: not enough sender reports for regression")

// srSample holds one sender report observation used in the regression.
type srSample struct {
	unwrappedRTP int64 // RTP timestamp unwrapped to 64-bit
	ntpNanos     int64 // NTP wall-clock in nanoseconds since Unix epoch
	receivedAt   time.Time
}

// NtpEstimator maintains a linear regression over a sliding window of RTCP
// sender report pairs to map RTP timestamps to NTP time. It is modeled after
// Chrome's RtpToNtpEstimator.
//
// Each NtpEstimator also owns a per-track OWDEstimator. OWD is per-track
// rather than per-participant because audio and video tracks of the same
// participant typically have different (senderNTP, receivedAt) relationships:
// video frames carry an encoder-delay-shifted NTP timestamp relative to the
// audio sample taken at the same real-world instant. Feeding both into one
// shared estimator produces a blended estimate biased toward whichever track
// happens to have lower raw OWD, and the OWDEstimator's path-change detector
// misfires on the sign-alternating input pattern.
type NtpEstimator struct {
	mu        sync.Mutex
	clockRate uint32

	samples    [maxSRSamples]srSample
	sampleLen  int // number of valid samples in the buffer (0..maxSRSamples)
	sampleHead int // index of the next write position

	// RTP unwrapping state
	lastRTP    uint32
	rtpOffset  int64 // cumulative offset from wraparounds
	hasLastRTP bool

	// Regression results (valid when sampleLen >= minSamplesReady)
	// The internal model is: ntpNanos = slopeNanos * (unwrappedRTP - meanX) + meanY
	// where slopeNanos is nanos per RTP tick.
	slopeNanos float64 // nanos of NTP time per RTP tick
	meanX      float64 // mean of unwrapped RTP values in the current window
	meanY      float64 // mean of NTP nanos values in the current window
	residStd   float64 // residual standard deviation in NTP nanos
	ready      bool

	consecutiveOutliers int

	// owdEstimator tracks the per-track propagation delay (receiver wall time
	// minus sender NTP at SR emission). Updated only on accepted SRs.
	owdEstimator *latency.OWDEstimator
}

// NewNtpEstimator creates an NtpEstimator for a codec with the given clock rate.
func NewNtpEstimator(clockRate uint32) *NtpEstimator {
	return &NtpEstimator{
		clockRate:    clockRate,
		owdEstimator: latency.NewOWDEstimator(latency.OWDEstimatorParamsDefault),
	}
}

// Reset clears all state including OWD. Called via SessionTimeline.ResetTrack for true stream discontinuities where every prior sample is stale.
func (e *NtpEstimator) Reset() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.resetLocked()
	e.owdEstimator = latency.NewOWDEstimator(latency.OWDEstimatorParamsDefault)
}

// resetLocked clears the regression only; OWD is preserved because outlier-triggered rebuilds don't imply a network change and re-converging OWD would thrash the tempo controller.
func (e *NtpEstimator) resetLocked() {
	e.samples = [maxSRSamples]srSample{}
	e.sampleLen = 0
	e.sampleHead = 0
	e.lastRTP = 0
	e.rtpOffset = 0
	e.hasLastRTP = false
	e.slopeNanos = 0
	e.meanX = 0
	e.meanY = 0
	e.residStd = 0
	e.ready = false
	e.consecutiveOutliers = 0
}

// SRResult indicates the outcome of processing a sender report.
type SRResult int

const (
	SRAccepted SRResult = iota
	SRDuplicate
	SROutlier
	SRRebuilt
)

// OnSenderReport ingests a new RTCP sender report observation.
// ntpTime is the 64-bit NTP timestamp from the SR, rtpTimestamp is the
// corresponding RTP timestamp, and receivedAt is the local wall-clock time
// when the SR was received.
func (e *NtpEstimator) OnSenderReport(ntpTime uint64, rtpTimestamp uint32, receivedAt time.Time) SRResult {
	e.mu.Lock()
	defer e.mu.Unlock()

	ntpNanos := ntpTimestampToNanos(ntpTime)
	unwrapped := e.unwrapRTP(rtpTimestamp)

	// Skip duplicate SRs (same NTP/RTP pair as the most recent sample).
	// This happens when the same SR is dispatched multiple times via
	// per-publication RTCP callbacks.
	if e.sampleLen > 0 {
		lastIdx := (e.sampleHead - 1 + maxSRSamples) % maxSRSamples
		last := e.samples[lastIdx]
		if last.unwrappedRTP == unwrapped && last.ntpNanos == ntpNanos {
			return SRDuplicate
		}
	}

	// Outlier rejection: if we already have a valid regression, check whether
	// this new sample deviates from the prediction by more than 3 standard
	// deviations. Persistent rejection (e.g., sender's NTP clock stepped)
	// triggers a full reset so the regression can rebuild from the new state.
	// residStd is floored to avoid disabling detection when the prior samples
	// happened to fit the line exactly.
	rebuilt := false
	if e.ready {
		std := e.residStd
		if std < minOutlierStdDevNanos {
			std = minOutlierStdDevNanos
		}
		predicted := e.slopeNanos*(float64(unwrapped)-e.meanX) + e.meanY
		residual := math.Abs(float64(ntpNanos) - predicted)
		if residual > outlierThresholdStdDevs*std {
			e.consecutiveOutliers++
			if e.consecutiveOutliers < maxConsecutiveOutliers {
				return SROutlier
			}
			// Persistent outliers: rebuild from scratch starting with this SR.
			e.resetLocked()
			unwrapped = e.unwrapRTP(rtpTimestamp)
			rebuilt = true
		}
	}
	e.consecutiveOutliers = 0

	// Write into circular buffer.
	e.samples[e.sampleHead] = srSample{
		unwrappedRTP: unwrapped,
		ntpNanos:     ntpNanos,
		receivedAt:   receivedAt,
	}
	e.sampleHead = (e.sampleHead + 1) % maxSRSamples
	if e.sampleLen < maxSRSamples {
		e.sampleLen++
	}

	// Recompute regression if we have enough samples. computeRegression
	// returns false on degenerate input (e.g., all RTP timestamps identical,
	// which yields sumDxDx == 0). In that case ready stays at its prior
	// value rather than flipping to true with stale/zero regression
	// coefficients — IsReady() must imply usable slope/mean.
	if e.sampleLen >= minSamplesReady && e.computeRegression() {
		e.ready = true
	}

	// Feed the OWD estimator only with accepted SRs — outliers would
	// contaminate the propagation-delay measurement.
	e.owdEstimator.Update(ntpNanos, receivedAt.UnixNano())

	if rebuilt {
		return SRRebuilt
	}
	return SRAccepted
}

// EstimatedOWD returns the current estimated propagation delay for this track.
// The value may be negative when the sender's NTP clock is ahead of the
// receiver's wall clock — that is by design; the cross-participant alignment
// formula relies on the OWD absorbing whatever clock offset exists.
func (e *NtpEstimator) EstimatedOWD() time.Duration {
	e.mu.Lock()
	defer e.mu.Unlock()
	return time.Duration(e.owdEstimator.EstimatedPropagationDelay())
}

// IsReady returns true once at least 2 sender reports have been processed
// and the regression is valid.
func (e *NtpEstimator) IsReady() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.ready
}

// RtpToNtp maps an RTP timestamp to wall-clock time using the current regression.
func (e *NtpEstimator) RtpToNtp(rtpTimestamp uint32) (time.Time, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.ready {
		return time.Time{}, errNotReady
	}

	unwrapped := e.unwrapRTPQuery(rtpTimestamp)
	ntpNanos := e.slopeNanos*(float64(unwrapped)-e.meanX) + e.meanY
	return nanosToTime(int64(math.Round(ntpNanos))), nil
}

// Slope returns the regression slope: seconds of NTP time per RTP tick.
// For a perfect clock this equals 1/clockRate.
func (e *NtpEstimator) Slope() float64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.slopeNanos / 1e9
}

// computeRegression performs ordinary least squares on the current samples
// using centered data to preserve float64 precision.
// Model: ntpNanos = slopeNanos * (unwrappedRTP - meanX) + meanY
//
// Returns false if the input is degenerate (all RTP timestamps identical,
// yielding sumDxDx == 0). The caller must NOT set ready=true in that case —
// the existing slope/mean values are stale and would produce wrong NTP times.
func (e *NtpEstimator) computeRegression() bool {
	n := float64(e.sampleLen)

	// First pass: compute means for centering.
	var sumX, sumY float64
	e.iterSamples(func(s srSample) {
		sumX += float64(s.unwrappedRTP)
		sumY += float64(s.ntpNanos)
	})
	mX := sumX / n
	mY := sumY / n

	// Second pass: compute centered sums for regression.
	var sumDxDx, sumDxDy float64
	e.iterSamples(func(s srSample) {
		dx := float64(s.unwrappedRTP) - mX
		dy := float64(s.ntpNanos) - mY
		sumDxDx += dx * dx
		sumDxDy += dx * dy
	})

	if sumDxDx == 0 {
		// Degenerate case: all RTP timestamps identical.
		return false
	}

	e.slopeNanos = sumDxDy / sumDxDx
	e.meanX = mX
	e.meanY = mY

	// Compute residual standard deviation.
	var sumResidSq float64
	e.iterSamples(func(s srSample) {
		predicted := e.slopeNanos*(float64(s.unwrappedRTP)-mX) + mY
		r := float64(s.ntpNanos) - predicted
		sumResidSq += r * r
	})

	if e.sampleLen > 2 {
		e.residStd = math.Sqrt(sumResidSq / (n - 2))
	} else {
		// With exactly 2 points the regression is exact; use a small positive
		// value so that the 3-sigma check is not trivially zero. (Currently
		// unreachable since the caller requires sampleLen >= minSamplesReady
		// (= 4) before invoking; left in place for robustness if minSamplesReady
		// is lowered.)
		e.residStd = math.Sqrt(sumResidSq / n)
	}
	return true
}

// iterSamples calls fn for each valid sample in the circular buffer.
func (e *NtpEstimator) iterSamples(fn func(srSample)) {
	start := 0
	if e.sampleLen == maxSRSamples {
		start = e.sampleHead // oldest entry is at head when buffer is full
	}
	for i := 0; i < e.sampleLen; i++ {
		idx := (start + i) % maxSRSamples
		fn(e.samples[idx])
	}
}

// unwrapRTP unwraps a 32-bit RTP timestamp to a 64-bit value, tracking
// forward/backward jumps via signed diff. This is used when ingesting SRs
// to maintain the running unwrap state.
func (e *NtpEstimator) unwrapRTP(rtpTS uint32) int64 {
	if !e.hasLastRTP {
		e.hasLastRTP = true
		e.lastRTP = rtpTS
		e.rtpOffset = 0
		return int64(rtpTS)
	}

	diff := int32(rtpTS - e.lastRTP)
	if diff > 0 && rtpTS < e.lastRTP {
		// Forward jump that crossed the uint32 boundary.
		e.rtpOffset += 1 << 32
	} else if diff < 0 && rtpTS > e.lastRTP {
		// Backward jump that crossed the uint32 boundary.
		e.rtpOffset -= 1 << 32
	}

	e.lastRTP = rtpTS
	return e.rtpOffset + int64(rtpTS)
}

// unwrapRTPQuery unwraps an RTP timestamp for a query (RtpToNtp) without
// mutating the unwrap state. It uses the current offset tracked from SRs.
func (e *NtpEstimator) unwrapRTPQuery(rtpTS uint32) int64 {
	if !e.hasLastRTP {
		return int64(rtpTS)
	}

	offset := e.rtpOffset
	diff := int32(rtpTS - e.lastRTP)
	if diff > 0 && rtpTS < e.lastRTP {
		offset += 1 << 32
	} else if diff < 0 && rtpTS > e.lastRTP {
		offset -= 1 << 32
	}
	return offset + int64(rtpTS)
}

// ntpTimestampToNanos converts a 64-bit NTP timestamp to nanoseconds since
// the Unix epoch.
func ntpTimestampToNanos(ntpTS uint64) int64 {
	secs := int64(ntpTS>>32) - ntpEpochOffset
	frac := ntpTS & 0xFFFFFFFF
	nanos := int64(frac) * 1e9 / (1 << 32)
	return secs*1e9 + nanos
}

// nanosToTime converts nanoseconds since the Unix epoch to a time.Time.
func nanosToTime(nanos int64) time.Time {
	return time.Unix(0, nanos)
}
