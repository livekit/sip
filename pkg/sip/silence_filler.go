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

	msdk "github.com/livekit/media-sdk"
	"github.com/livekit/media-sdk/rtp"
	"github.com/livekit/protocol/logger"
)

// silenceFiller detects RTP timestamp discontinuities (silence suppression)
// and generates silence samples to fill the gaps before passing packets to the decoder.
type silenceFiller struct {
	maxGapSize      int
	encodedSink     rtp.Handler
	pcmSink         msdk.PCM16Writer
	samplesPerFrame int
	log             logger.Logger
	lastTS          uint32
	lastSeq         uint16
	initialized     bool
	gapCount        uint64
	gapSizeSum      uint64
	lastPrintTime   time.Time
}

type SilenceSuppressionOption func(*silenceFiller)

func WithMaxGapSize(maxGapSize int) SilenceSuppressionOption {
	return func(h *silenceFiller) {
		if maxGapSize > 0 {
			h.maxGapSize = maxGapSize
		}
	}
}

func newSilenceFiller(encodedSink rtp.Handler, pcmSink msdk.PCM16Writer, clockRate int, log logger.Logger, options ...SilenceSuppressionOption) rtp.Handler {
	// TODO: We assume 20ms frame. We would need to adjust this when:
	// - When we add support for other frame durations.
	// - When we add support for re-INVITE sdp renegotiation (maybe, if we don't destroy this and start over).
	h := &silenceFiller{
		maxGapSize:      25, // Default max gap size
		encodedSink:     encodedSink,
		pcmSink:         pcmSink,
		samplesPerFrame: clockRate / rtp.DefFramesPerSec,
		log:             log,
		initialized:     false,
		lastSeq:         0,
		lastTS:          0,
		gapCount:        0,
		gapSizeSum:      0,
		lastPrintTime:   time.Time{},
	}
	for _, option := range options {
		option(h)
	}
	return h
}

func (h *silenceFiller) String() string {
	return fmt.Sprintf("SilenceFiller(%d) -> %s", h.maxGapSize, h.encodedSink.String())
}

func (h *silenceFiller) isSilenceSuppression(header *rtp.Header) (bool, int) {
	if !h.initialized {
		h.initialized = true
		h.lastSeq = header.SequenceNumber
		h.lastTS = header.Timestamp
		return false, 0
	}

	currentTS := header.Timestamp
	currentSeq := header.SequenceNumber

	expectedSeq := h.lastSeq + 1
	expectedTS := h.lastTS + uint32(h.samplesPerFrame)

	seqDiff := currentSeq - expectedSeq
	tsDiff := currentTS - expectedTS

	h.lastTS = currentTS
	h.lastSeq = currentSeq

	// A key characteristic of DTX or silence supression is no sequence gaps, but >1 frame TS gaps

	if seqDiff != 0 || tsDiff == 0 {
		// This also filters out out-of-order packets, due to seqDiff != 0
		return false, 0
	}

	// Silence supression happened - sequential packets (no loss), but with a gap in timestamp
	missedFrames := int(tsDiff) / int(h.samplesPerFrame)
	return true, missedFrames
}

func (h *silenceFiller) fillWithSilence(framesToFill int) error {
	for ; framesToFill > 0; framesToFill-- {
		silence := make(msdk.PCM16Sample, h.samplesPerFrame)
		if err := h.pcmSink.WriteSample(silence); err != nil {
			return err
		}
	}
	return nil
}

func (h *silenceFiller) HandleRTP(header *rtp.Header, payload []byte) error {
	isSilenceSupression, missingFrameCount := h.isSilenceSuppression(header)

	if isSilenceSupression {
		h.gapCount++
		h.gapSizeSum += uint64(missingFrameCount)
		if time.Since(h.lastPrintTime) > 15*time.Second {
			h.lastPrintTime = time.Now()
			h.log.Infow("timestamp gap",
				"rtpSeq", header.SequenceNumber,
				"rtpTimestamp", header.Timestamp,
				"rtpMarker", header.Marker,
				"gapCount", h.gapCount,
				"gapSize", missingFrameCount,
				"gapSizeSum", h.gapSizeSum, // Used to get averages and figure out outliers between prints
			)
		}

		// Don't cause a flood of silence packets on too large of a gap (large loss, RTP TS or Seq reset)
		if missingFrameCount <= h.maxGapSize {
			err := h.fillWithSilence(missingFrameCount)
			if err != nil {
				return err
			}
		}
	}
	return h.encodedSink.HandleRTP(header, payload)
}
