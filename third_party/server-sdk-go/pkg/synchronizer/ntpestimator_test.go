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
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// ntpToUint64 converts a time.Time to a 64-bit NTP timestamp.
// Upper 32 bits = seconds since NTP epoch (1900-01-01),
// lower 32 bits = fractional seconds.
func ntpToUint64(t time.Time) uint64 {
	const ntpEpochOffset = 2208988800
	secs := uint64(t.Unix()) + ntpEpochOffset
	frac := uint64(t.Nanosecond()) * (1 << 32) / 1e9
	return secs<<32 | frac
}

func TestNtpEstimator_NotReadyBeforeEnoughSRs(t *testing.T) {
	e := NewNtpEstimator(90000)

	require.False(t, e.IsReady(), "should not be ready with 0 SRs")

	_, err := e.RtpToNtp(1000)
	require.Error(t, err, "RtpToNtp should error when not ready")

	// Feed SRs one at a time, checking readiness
	now := time.Now()
	for i := 0; i < minSamplesReady-1; i++ {
		srTime := now.Add(time.Duration(i) * time.Second)
		rtpTS := uint32(i+1) * 90000
		e.OnSenderReport(ntpToUint64(srTime), rtpTS, srTime)
		require.False(t, e.IsReady(), "should not be ready with %d SRs", i+1)
	}

	// One more SR makes it ready
	srTime := now.Add(time.Duration(minSamplesReady-1) * time.Second)
	rtpTS := uint32(minSamplesReady) * 90000
	e.OnSenderReport(ntpToUint64(srTime), rtpTS, srTime)
	require.True(t, e.IsReady(), "should be ready with %d SRs", minSamplesReady)

	_, err = e.RtpToNtp(135000)
	require.NoError(t, err, "RtpToNtp should succeed when ready")
}

func TestNtpEstimator_AccurateMapping(t *testing.T) {
	const clockRate = 90000
	e := NewNtpEstimator(clockRate)

	baseTime := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)

	// Feed 10 perfect SRs at 1-second intervals
	for i := 0; i < 10; i++ {
		wallTime := baseTime.Add(time.Duration(i) * time.Second)
		rtpTS := uint32(i) * clockRate
		e.OnSenderReport(ntpToUint64(wallTime), rtpTS, wallTime)
	}

	require.True(t, e.IsReady())

	// Verify mapping at intermediate points
	for _, tc := range []struct {
		name    string
		rtpTS   uint32
		wantNTP time.Time
	}{
		{"at SR 0", 0, baseTime},
		{"at SR 5", 5 * clockRate, baseTime.Add(5 * time.Second)},
		{"between SR 2 and 3", uint32(2.5 * clockRate), baseTime.Add(2500 * time.Millisecond)},
		{"at SR 9", 9 * clockRate, baseTime.Add(9 * time.Second)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := e.RtpToNtp(tc.rtpTS)
			require.NoError(t, err)
			diff := got.Sub(tc.wantNTP)
			if diff < 0 {
				diff = -diff
			}
			require.Less(t, diff, time.Millisecond,
				"mapping off by %v; got %v, want %v", diff, got, tc.wantNTP)
		})
	}
}

func TestNtpEstimator_OutlierRejection(t *testing.T) {
	const clockRate = 90000
	e := NewNtpEstimator(clockRate)

	baseTime := time.Date(2025, 6, 15, 10, 0, 0, 0, time.UTC)

	// Feed 5 good SRs at 1-second intervals
	for i := 0; i < 5; i++ {
		wallTime := baseTime.Add(time.Duration(i) * time.Second)
		rtpTS := uint32(i) * clockRate
		e.OnSenderReport(ntpToUint64(wallTime), rtpTS, wallTime)
	}

	require.True(t, e.IsReady())

	// Feed 1 wildly wrong SR: RTP says 5 seconds but NTP says 50 seconds
	badWallTime := baseTime.Add(50 * time.Second)
	badRTP := uint32(5) * clockRate
	e.OnSenderReport(ntpToUint64(badWallTime), badRTP, badWallTime)

	// Verify mapping is still accurate (outlier should have been rejected)
	got, err := e.RtpToNtp(uint32(2.5 * clockRate))
	require.NoError(t, err)
	want := baseTime.Add(2500 * time.Millisecond)
	diff := got.Sub(want)
	if diff < 0 {
		diff = -diff
	}
	require.Less(t, diff, time.Millisecond,
		"mapping should be accurate despite outlier; off by %v", diff)
}

func TestNtpEstimator_Wraparound(t *testing.T) {
	const clockRate = 90000
	e := NewNtpEstimator(clockRate)

	baseTime := time.Date(2025, 3, 1, 0, 0, 0, 0, time.UTC)

	// Start RTP near uint32 max so wraparound occurs
	// math.MaxUint32 - 5*clockRate puts us 5 seconds before wrap
	startRTP := uint32(math.MaxUint32 - 5*clockRate)

	for i := 0; i < 10; i++ {
		wallTime := baseTime.Add(time.Duration(i) * time.Second)
		rtpTS := startRTP + uint32(i)*clockRate // will wrap around uint32
		e.OnSenderReport(ntpToUint64(wallTime), rtpTS, wallTime)
	}

	require.True(t, e.IsReady())

	// Test mapping at points before and after the wraparound
	// SR at i=5 is exactly where RTP wraps past 0
	for _, tc := range []struct {
		name    string
		idx     int
		wantNTP time.Time
	}{
		{"before wrap (i=3)", 3, baseTime.Add(3 * time.Second)},
		{"at wrap (i=5)", 5, baseTime.Add(5 * time.Second)},
		{"after wrap (i=8)", 8, baseTime.Add(8 * time.Second)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rtpTS := startRTP + uint32(tc.idx)*clockRate
			got, err := e.RtpToNtp(rtpTS)
			require.NoError(t, err)
			diff := got.Sub(tc.wantNTP)
			if diff < 0 {
				diff = -diff
			}
			require.Less(t, diff, time.Millisecond,
				"mapping off by %v across wraparound; got %v, want %v", diff, got, tc.wantNTP)
		})
	}
}

func TestNtpEstimator_SlidingWindow(t *testing.T) {
	const clockRate = 90000
	e := NewNtpEstimator(clockRate)

	baseTime := time.Date(2025, 4, 1, 0, 0, 0, 0, time.UTC)

	// Feed 25 SRs (exceeds window of 20)
	for i := 0; i < 25; i++ {
		wallTime := baseTime.Add(time.Duration(i) * time.Second)
		rtpTS := uint32(i) * clockRate
		e.OnSenderReport(ntpToUint64(wallTime), rtpTS, wallTime)
	}

	require.True(t, e.IsReady())

	// Verify mapping still works accurately in the recent window
	got, err := e.RtpToNtp(uint32(22) * clockRate)
	require.NoError(t, err)
	want := baseTime.Add(22 * time.Second)
	diff := got.Sub(want)
	if diff < 0 {
		diff = -diff
	}
	require.Less(t, diff, time.Millisecond,
		"mapping should be accurate after sliding window overflow; off by %v", diff)
}

func TestNtpEstimator_Slope(t *testing.T) {
	const clockRate = 90000
	e := NewNtpEstimator(clockRate)

	baseTime := time.Date(2025, 5, 1, 0, 0, 0, 0, time.UTC)

	// Feed 5 perfect SRs
	for i := 0; i < 5; i++ {
		wallTime := baseTime.Add(time.Duration(i) * time.Second)
		rtpTS := uint32(i) * clockRate
		e.OnSenderReport(ntpToUint64(wallTime), rtpTS, wallTime)
	}

	require.True(t, e.IsReady())

	// Slope should be close to 1/clockRate (seconds per RTP tick)
	expectedSlope := 1.0 / float64(clockRate)
	gotSlope := e.Slope()

	relError := math.Abs(gotSlope-expectedSlope) / expectedSlope
	require.Less(t, relError, 1e-6,
		"slope should be ~%e, got %e (relative error %e)", expectedSlope, gotSlope, relError)
}

func TestNtpEstimator_PersistentOutliersRebuildRegression(t *testing.T) {
	// Simulates a sender NTP step correction: after a steady regression,
	// every SR arrives with the same large NTP offset. The estimator must
	// reject the first few outliers, then reset and rebuild from the new
	// state — and the rebuilt regression must accurately map the post-step
	// RTP/NTP relationship.
	const clockRate = 90000
	e := NewNtpEstimator(clockRate)

	baseTime := time.Date(2025, 6, 15, 10, 0, 0, 0, time.UTC)

	// Build a stable regression with 5 SRs.
	for i := 0; i < 5; i++ {
		wallTime := baseTime.Add(time.Duration(i) * time.Second)
		rtpTS := uint32(i) * clockRate
		e.OnSenderReport(ntpToUint64(wallTime), rtpTS, wallTime)
	}
	require.True(t, e.IsReady())

	// Step the sender's NTP clock forward by 500ms while RTP keeps ticking
	// at its normal rate. Every subsequent SR is now ~500ms off the old
	// regression — far above 3σ.
	const ntpStep = 500 * time.Millisecond
	for i := 5; i < 5+maxConsecutiveOutliers-1; i++ {
		wallTime := baseTime.Add(time.Duration(i)*time.Second + ntpStep)
		rtpTS := uint32(i) * clockRate
		result := e.OnSenderReport(ntpToUint64(wallTime), rtpTS, wallTime)
		require.Equal(t, SROutlier, result, "SR %d should still be rejected", i)
	}

	// The Nth outlier triggers reset; the SR is accepted as the new baseline.
	resetIdx := 5 + maxConsecutiveOutliers - 1
	wallTime := baseTime.Add(time.Duration(resetIdx)*time.Second + ntpStep)
	rtpTS := uint32(resetIdx) * clockRate
	result := e.OnSenderReport(ntpToUint64(wallTime), rtpTS, wallTime)
	require.Equal(t, SRRebuilt, result, "Nth consecutive outlier should trigger reset and acceptance")
	require.False(t, e.IsReady(), "regression should not be ready until minSamplesReady SRs accumulate")

	// Feed minSamplesReady more SRs at the new (stepped) NTP base.
	for i := resetIdx + 1; i < resetIdx+1+minSamplesReady; i++ {
		wallTime := baseTime.Add(time.Duration(i)*time.Second + ntpStep)
		rtpTS := uint32(i) * clockRate
		result := e.OnSenderReport(ntpToUint64(wallTime), rtpTS, wallTime)
		require.Equal(t, SRAccepted, result, "post-reset SR %d should be accepted", i)
	}
	require.True(t, e.IsReady(), "regression should be ready after rebuild")

	// The rebuilt regression should accurately map post-step RTP timestamps.
	queryIdx := resetIdx + 2
	got, err := e.RtpToNtp(uint32(queryIdx) * clockRate)
	require.NoError(t, err)
	want := baseTime.Add(time.Duration(queryIdx)*time.Second + ntpStep)
	diff := got.Sub(want)
	if diff < 0 {
		diff = -diff
	}
	require.Less(t, diff, time.Millisecond,
		"post-rebuild mapping should be accurate; got %v want %v (off by %v)", got, want, diff)
}

func TestNtpEstimator_IntermittentOutliersDoNotReset(t *testing.T) {
	// Network glitches that produce occasional outliers — but interleaved
	// with normal SRs — should not trigger the rebuild path.
	const clockRate = 90000
	e := NewNtpEstimator(clockRate)

	baseTime := time.Date(2025, 6, 15, 10, 0, 0, 0, time.UTC)

	// Build a stable regression.
	for i := 0; i < 5; i++ {
		wallTime := baseTime.Add(time.Duration(i) * time.Second)
		rtpTS := uint32(i) * clockRate
		e.OnSenderReport(ntpToUint64(wallTime), rtpTS, wallTime)
	}
	require.True(t, e.IsReady())

	// Alternate: 4 outliers spread across 8 SRs (never consecutive enough to
	// trigger reset). Each outlier resets the counter, so we never reach
	// maxConsecutiveOutliers.
	for i := 0; i < 8; i++ {
		idx := 5 + i
		wallTime := baseTime.Add(time.Duration(idx) * time.Second)
		rtpTS := uint32(idx) * clockRate

		if i%2 == 0 {
			// Outlier: same RTP slot but NTP wildly off.
			badNTP := baseTime.Add(time.Duration(idx+50) * time.Second)
			result := e.OnSenderReport(ntpToUint64(badNTP), rtpTS, wallTime)
			require.Equal(t, SROutlier, result, "outlier SR at idx %d should be rejected", idx)
		} else {
			// Normal SR.
			result := e.OnSenderReport(ntpToUint64(wallTime), rtpTS, wallTime)
			require.Equal(t, SRAccepted, result, "normal SR at idx %d should be accepted", idx)
		}
	}

	// Original regression should still be valid.
	got, err := e.RtpToNtp(uint32(2.5 * clockRate))
	require.NoError(t, err)
	want := baseTime.Add(2500 * time.Millisecond)
	diff := got.Sub(want)
	if diff < 0 {
		diff = -diff
	}
	require.Less(t, diff, 10*time.Millisecond,
		"original regression should survive intermittent outliers; off by %v", diff)
}
