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

package ingest

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/livekit/protocol/logger"

	"github.com/livekit/sip/pkg/videobridge/stats"
)

// BitrateController implements adaptive bitrate based on REMB/TWCC feedback
// from the LiveKit side and packet loss observations from the SIP side.
// It adjusts the transcoder bitrate and can send TMMBR to the SIP endpoint.
type BitrateController struct {
	log logger.Logger

	// Configuration
	minBitrate int // minimum bitrate in bps
	maxBitrate int // maximum bitrate in bps

	// Current state
	currentBitrate atomic.Int64
	targetBitrate  atomic.Int64

	// Packet loss tracking
	mu             sync.Mutex
	lastLossCheck  time.Time
	packetsTotal   uint64
	packetsLost    uint64
	lossHistory    []float64 // rolling window of loss ratios

	// Callbacks
	onBitrateChange func(bps int)

	closed atomic.Bool
}

// BitrateControllerConfig configures the adaptive bitrate controller.
type BitrateControllerConfig struct {
	MinBitrate int // minimum bitrate in bps (default 200000 = 200kbps)
	MaxBitrate int // maximum bitrate in bps (default 2500000 = 2.5Mbps)
	InitBitrate int // initial bitrate in bps (default 1500000 = 1.5Mbps)
}

// NewBitrateController creates a new adaptive bitrate controller.
func NewBitrateController(log logger.Logger, conf BitrateControllerConfig) *BitrateController {
	if conf.MinBitrate <= 0 {
		conf.MinBitrate = 200_000
	}
	if conf.MaxBitrate <= 0 {
		conf.MaxBitrate = 2_500_000
	}
	if conf.InitBitrate <= 0 {
		conf.InitBitrate = 1_500_000
	}

	bc := &BitrateController{
		log:         log,
		minBitrate:  conf.MinBitrate,
		maxBitrate:  conf.MaxBitrate,
		lossHistory: make([]float64, 0, 10),
	}
	bc.currentBitrate.Store(int64(conf.InitBitrate))
	bc.targetBitrate.Store(int64(conf.InitBitrate))

	return bc
}

// SetOnBitrateChange sets the callback invoked when the bitrate should change.
func (bc *BitrateController) SetOnBitrateChange(fn func(bps int)) {
	bc.onBitrateChange = fn
}

// OnREMB handles a REMB (Receiver Estimated Maximum Bitrate) value from LiveKit.
// This is the primary signal for downlink bandwidth estimation.
func (bc *BitrateController) OnREMB(estimatedBitrate uint64) {
	if bc.closed.Load() {
		return
	}

	target := int(estimatedBitrate)
	if target < bc.minBitrate {
		target = bc.minBitrate
	}
	if target > bc.maxBitrate {
		target = bc.maxBitrate
	}

	bc.targetBitrate.Store(int64(target))
	bc.applyBitrate(target)

	stats.VideoBitrateKbps.WithLabelValues("target", "vp8").Set(float64(target) / 1000)
}

// OnPacketLoss reports observed packet loss from the RTP receiver.
// Used as a secondary signal to reduce bitrate when loss is detected.
func (bc *BitrateController) OnPacketLoss(totalPackets, lostPackets uint64) {
	if bc.closed.Load() {
		return
	}

	bc.mu.Lock()
	defer bc.mu.Unlock()

	now := time.Now()
	if bc.lastLossCheck.IsZero() {
		bc.lastLossCheck = now
		bc.packetsTotal = totalPackets
		bc.packetsLost = lostPackets
		return
	}

	dt := now.Sub(bc.lastLossCheck)
	if dt < time.Second {
		return // sample at most once per second
	}

	deltaTotal := totalPackets - bc.packetsTotal
	deltaLost := lostPackets - bc.packetsLost
	bc.lastLossCheck = now
	bc.packetsTotal = totalPackets
	bc.packetsLost = lostPackets

	if deltaTotal == 0 {
		return
	}

	lossRatio := float64(deltaLost) / float64(deltaTotal)
	stats.RTPPacketsLost.Add(float64(deltaLost))

	// Rolling window
	bc.lossHistory = append(bc.lossHistory, lossRatio)
	if len(bc.lossHistory) > 10 {
		bc.lossHistory = bc.lossHistory[1:]
	}

	// Calculate average loss
	var avgLoss float64
	for _, l := range bc.lossHistory {
		avgLoss += l
	}
	avgLoss /= float64(len(bc.lossHistory))

	current := int(bc.currentBitrate.Load())

	if avgLoss > 0.10 {
		// Heavy loss (>10%): reduce by 30%
		newBitrate := int(float64(current) * 0.70)
		if newBitrate < bc.minBitrate {
			newBitrate = bc.minBitrate
		}
		bc.log.Infow("reducing bitrate due to heavy packet loss",
			"loss", avgLoss, "from", current, "to", newBitrate)
		bc.applyBitrateUnlocked(newBitrate)
	} else if avgLoss > 0.02 {
		// Moderate loss (2-10%): reduce by 10%
		newBitrate := int(float64(current) * 0.90)
		if newBitrate < bc.minBitrate {
			newBitrate = bc.minBitrate
		}
		bc.applyBitrateUnlocked(newBitrate)
	} else if avgLoss < 0.005 {
		// Very low loss (<0.5%): probe up by 5%
		target := int(bc.targetBitrate.Load())
		if current < target {
			newBitrate := int(float64(current) * 1.05)
			if newBitrate > target {
				newBitrate = target
			}
			bc.applyBitrateUnlocked(newBitrate)
		}
	}
}

// CurrentBitrate returns the current bitrate in bps.
func (bc *BitrateController) CurrentBitrate() int {
	return int(bc.currentBitrate.Load())
}

// Close stops the bitrate controller.
func (bc *BitrateController) Close() {
	bc.closed.Store(true)
}

func (bc *BitrateController) applyBitrate(bps int) {
	bc.mu.Lock()
	defer bc.mu.Unlock()
	bc.applyBitrateUnlocked(bps)
}

func (bc *BitrateController) applyBitrateUnlocked(bps int) {
	old := bc.currentBitrate.Swap(int64(bps))
	if int(old) == bps {
		return
	}

	stats.VideoBitrateKbps.WithLabelValues("current", "vp8").Set(float64(bps) / 1000)

	if bc.onBitrateChange != nil {
		bc.onBitrateChange(bps)
	}
}
