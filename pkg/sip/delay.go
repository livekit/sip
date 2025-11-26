// Copyright 2024 LiveKit, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// 	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package sip

import (
	"fmt"
	"time"

	"github.com/livekit/media-sdk/rtp"
)

// // Snychronous measurement of rtp stream quality
type rtpGaugeData struct {
	minDelta time.Duration
	maxDelta time.Duration
	sumDelta time.Duration
	numDelta int
}

// DiffNN returns the signed difference between cur and prev, accounting for wrap-around in a set integer size
func Diff32(cur, prev uint32) int32 {
	return int32(cur - prev)
}

func Diff16(cur, prev uint16) int16 {
	return int16(cur - prev)
}

type rtpGauge struct {
	// Configuration
	timeHop        time.Duration // Time between packets. Must be > 0.
	sampleHop      int32         // Number of samples between packets. Must be > 0.
	resetThreshold time.Duration // Threshold for detecting stream resets

	// State
	projectedTime time.Time
	projectedSeq  uint16
	projectedTs   uint32

	data rtpGaugeData
}

func (g *rtpGauge) GetData() rtpGaugeData {
	return g.data
}

func (g *rtpGauge) Reset(header *rtp.Header) *rtpGaugeData {
	ret := g.data
	g.projectedTime = time.Now()
	g.projectedTs = header.Timestamp
	g.projectedSeq = header.SequenceNumber
	g.data = rtpGaugeData{}
	return &ret
}

// Process incoming RTP packet.
// If an incoming packet detects a reset, this function returns a snapshot of the previous data.
func (g *rtpGauge) Process(header *rtp.Header) *rtpGaugeData {
	const safetyMargin = -1 * time.Millisecond // prevents frequent resets when we're sub this threshold in difference

	if g.projectedTime.IsZero() {
		fmt.Println("DEBUG: No data, resetting")
		g.Reset(header) // No data, don't return anything
		return nil
	}

	// Project current packet by sequence
	if g.timeHop <= 0 {
		panic("timeHop should be positive")
	}
	if g.sampleHop <= 0 {
		panic("sampleHop should be positive")
	}
	now := time.Now()
	seqDiff := Diff16(header.SequenceNumber, g.projectedSeq)
	projectedSeqTime := g.projectedTime.Add(time.Duration(seqDiff) * g.timeHop)
	seqDrift := now.Sub(projectedSeqTime)
	tsDiff := Diff32(header.Timestamp, g.projectedTs)
	projectedTsTime := g.projectedTime.Add(time.Duration(tsDiff/g.sampleHop) * g.timeHop)
	tsDrift := now.Sub(projectedTsTime)
	//fmt.Printf("DEBUG: last projected: ts %d, seq %d, time %v\n", g.projectedTs, g.projectedSeq, g.projectedTime)

	fmt.Printf("DEBUG: diff projected: ts %d (%v), seq %d (%v)\n", tsDiff, seqDrift, seqDiff, tsDrift)

	// Detect resets
	fmt.Printf("DEBUG: seqDrift: %v\n", seqDrift)
	if seqDrift > g.resetThreshold || seqDrift < -g.resetThreshold {
		// Reset if the projected duration is too far from the actual duration in either direction
		fmt.Printf("DEBUG: Resetting due to seqDrift %v exceeding resetThreshold %v\n", seqDrift, g.resetThreshold)
		return g.Reset(header)
	}
	fmt.Printf("DEBUG: tsDrift: %v\n", tsDrift)
	if tsDrift > g.resetThreshold || tsDrift < -g.resetThreshold {
		fmt.Printf("DEBUG: Resetting due to tsDrift %v exceeding resetThreshold %v\n", tsDrift, g.resetThreshold)
		return g.Reset(header)
	}

	// Adjust baseline since we see that it's possible to be faster
	if seqDrift < safetyMargin {
		fmt.Println("DEBUG: Updating projection based on seq diff")
		g.Reset(header)
		return nil
	}
	if tsDrift < safetyMargin {
		fmt.Println("DEBUG: Updating projection based on TS diff")
		g.Reset(header)
		return nil
	}

	// Update stats
	sinceLastProjected := seqDrift + g.timeHop
	if g.data.numDelta == 0 {
		g.data.minDelta = sinceLastProjected
		g.data.maxDelta = sinceLastProjected
	} else {
		g.data.minDelta = min(g.data.minDelta, sinceLastProjected)
		g.data.maxDelta = max(g.data.maxDelta, sinceLastProjected)
	}
	g.data.sumDelta += sinceLastProjected
	g.data.numDelta++

	// Advance projection; Deliberately use projection and not actual data to catch accumulating drift
	if seqDiff > 0 {
		g.projectedTime = g.projectedTime.Add(g.timeHop)
		g.projectedSeq++
		g.projectedTs += uint32(g.sampleHop)
	}
	return nil
}
