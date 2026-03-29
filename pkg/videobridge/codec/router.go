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

package codec

import (
	"fmt"
	"sync"

	"github.com/livekit/protocol/logger"

	"github.com/livekit/sip/pkg/videobridge/stats"
)

// CodecMode represents the video codec handling mode.
type CodecMode int

const (
	// ModePassthrough sends H.264 directly to LiveKit without transcoding.
	ModePassthrough CodecMode = iota
	// ModeTranscode decodes H.264 and re-encodes to VP8.
	ModeTranscode
)

func (m CodecMode) String() string {
	switch m {
	case ModePassthrough:
		return "passthrough"
	case ModeTranscode:
		return "transcode"
	default:
		return fmt.Sprintf("unknown(%d)", int(m))
	}
}

// VideoSink receives processed video data from the codec router.
type VideoSink interface {
	// WriteNAL receives a complete H.264 NAL unit (passthrough mode).
	WriteNAL(nal NALUnit, timestamp uint32) error
	// WriteRawFrame receives a decoded raw frame for re-encoding (transcode mode).
	WriteRawFrame(frame *RawFrame) error
}

// RawFrame represents a decoded video frame.
type RawFrame struct {
	Width     int
	Height    int
	Data      []byte // YUV420 planar
	Timestamp uint32
	Keyframe  bool
}

// Router decides whether to passthrough H.264 or transcode to VP8.
// It routes incoming NAL units to the appropriate output path.
type Router struct {
	mu   sync.RWMutex
	log  logger.Logger
	mode CodecMode

	// Passthrough sink receives NAL units directly
	passthroughSink VideoSink
	// Transcode sink receives decoded raw frames
	transcodeSink VideoSink

	// Parameter sets cached for passthrough prepending
	sps []byte
	pps []byte
}

// NewRouter creates a codec router with the specified initial mode.
func NewRouter(log logger.Logger, mode CodecMode) *Router {
	return &Router{
		log:  log,
		mode: mode,
	}
}

// SetPassthroughSink sets the sink for H.264 passthrough output.
func (r *Router) SetPassthroughSink(sink VideoSink) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.passthroughSink = sink
}

// SetTranscodeSink sets the sink for transcoded output.
func (r *Router) SetTranscodeSink(sink VideoSink) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.transcodeSink = sink
}

// Mode returns the current codec routing mode.
func (r *Router) Mode() CodecMode {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.mode
}

// SetMode switches the codec routing mode.
// This can be called dynamically when room participant capabilities change.
func (r *Router) SetMode(mode CodecMode) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.mode != mode {
		r.log.Infow("switching codec mode", "from", r.mode.String(), "to", mode.String())
		r.mode = mode
	}
}

// RouteNALs processes depacketized NAL units and routes them to the appropriate sink.
func (r *Router) RouteNALs(nals []NALUnit, timestamp uint32) error {
	r.mu.RLock()
	mode := r.mode
	passthroughSink := r.passthroughSink
	transcodeSink := r.transcodeSink
	r.mu.RUnlock()

	// Cache parameter sets regardless of mode
	for _, nal := range nals {
		switch nal.Type {
		case NALTypeSPS:
			r.mu.Lock()
			r.sps = make([]byte, len(nal.Data))
			copy(r.sps, nal.Data)
			r.mu.Unlock()
		case NALTypePPS:
			r.mu.Lock()
			r.pps = make([]byte, len(nal.Data))
			copy(r.pps, nal.Data)
			r.mu.Unlock()
		}
	}

	switch mode {
	case ModePassthrough:
		if passthroughSink == nil {
			r.log.Warnw("dropping NALs: passthrough sink not set", nil, "nalCount", len(nals))
			stats.SessionErrors.WithLabelValues("nil_passthrough_sink").Inc()
			return nil
		}
		for _, nal := range nals {
			if err := passthroughSink.WriteNAL(nal, timestamp); err != nil {
				return fmt.Errorf("passthrough write: %w", err)
			}
		}
		return nil

	case ModeTranscode:
		if transcodeSink == nil {
			r.log.Warnw("dropping NALs: transcode sink not set", nil, "nalCount", len(nals))
			stats.SessionErrors.WithLabelValues("nil_transcode_sink").Inc()
			return nil
		}
		for _, nal := range nals {
			if err := transcodeSink.WriteNAL(nal, timestamp); err != nil {
				return fmt.Errorf("transcode write: %w", err)
			}
		}
		return nil

	default:
		return fmt.Errorf("unknown codec mode: %d", mode)
	}
}

// GetParameterSets returns cached SPS and PPS.
func (r *Router) GetParameterSets() (sps, pps []byte) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.sps, r.pps
}
