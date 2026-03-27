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

package session

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/frostbyte73/core"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/livekit/protocol/logger"

	"github.com/livekit/sip/pkg/videobridge/codec"
	"github.com/livekit/sip/pkg/videobridge/config"
	"github.com/livekit/sip/pkg/videobridge/ingest"
	"github.com/livekit/sip/pkg/videobridge/publisher"
	"github.com/livekit/sip/pkg/videobridge/resilience"
	"github.com/livekit/sip/pkg/videobridge/signaling"
	"github.com/livekit/sip/pkg/videobridge/stats"
)

var tracer = otel.Tracer("github.com/livekit/sip/videobridge/session")

// Session manages the lifecycle of a single SIP video call bridged to a LiveKit room.
// Uses an embedded StateMachine for atomic state transitions.
// Media handlers check sm.IsActive() before forwarding — no locks in the hot path.
type Session struct {
	log  logger.Logger
	conf *config.Config
	sm   *StateMachine
	ff   *resilience.FeatureFlags

	// Identity (immutable after creation)
	ID       string
	CallID   string
	RoomName string
	FromURI  string
	ToURI    string

	// Components (set via SetComponents before Start, read-only after)
	receiver    *ingest.RTPReceiver
	router      *codec.Router
	pub         *publisher.Publisher
	audioBridge *ingest.AudioBridge
	rtcpHandler *ingest.RTCPHandler

	// Resilience
	publishCB     *resilience.CircuitBreaker
	onCircuitTrip func(sessionID string) // bridge-level callback when publisher CB trips

	// Lifecycle enforcement
	lifecycle *LifecycleMonitor

	// Lifecycle
	startTime time.Time
	closed    core.Fuse
	cancelFn  context.CancelFunc

	// Per-session tracing
	rootSpan trace.Span

	// Per-session stats
	videoNALs   atomic.Uint64
	audioFrames atomic.Uint64
	errors      atomic.Uint64
}

// NewSession creates a new bridging session for an inbound SIP video call.
func NewSession(
	log logger.Logger,
	conf *config.Config,
	call *signaling.InboundCall,
	roomName string,
) (*Session, error) {
	sessionID := fmt.Sprintf("svb_%s", call.CallID)
	log = log.WithValues("sessionID", sessionID, "callID", call.CallID, "room", roomName)

	codecMode := codec.ModePassthrough
	if conf.Video.DefaultCodec == "vp8" {
		codecMode = codec.ModeTranscode
	}

	s := &Session{
		log:      log,
		conf:     conf,
		ID:       sessionID,
		CallID:   call.CallID,
		RoomName: roomName,
		FromURI:  call.FromURI,
		ToURI:    call.ToURI,
		router:   codec.NewRouter(log, codecMode),
	}
	s.sm = NewStateMachine()

	// Circuit breaker for publisher writes
	s.publishCB = resilience.NewCircuitBreaker(log, resilience.CircuitBreakerConfig{
		Name:                "publisher",
		MaxFailures:         10,
		OpenDuration:        5 * time.Second,
		HalfOpenMaxAttempts: 3,
		OnStateChange: func(from, to resilience.CircuitState) {
			if to == resilience.StateOpen {
				stats.SessionErrors.WithLabelValues("publisher_circuit_open").Inc()
				log.Warnw("publisher circuit breaker opened — media will be dropped", nil)
				// Notify bridge-level callback (feeds global circuit breaker)
				if s.onCircuitTrip != nil {
					s.onCircuitTrip(s.ID)
				}
			}
		},
	})

	return s, nil
}

// SetOnCircuitTrip sets a callback invoked when this session's publisher circuit breaker trips.
// The bridge uses this to feed the global circuit breaker.
func (s *Session) SetOnCircuitTrip(fn func(sessionID string)) {
	s.onCircuitTrip = fn
}

// State returns the current session state (lock-free).
func (s *Session) State() State {
	return s.sm.Current()
}

// SetComponents wires the external components into the session.
func (s *Session) SetComponents(
	receiver *ingest.RTPReceiver,
	pub *publisher.Publisher,
	audioBridge *ingest.AudioBridge,
	rtcpFwd *ingest.RTCPHandler,
) {
	s.receiver = receiver
	s.pub = pub
	s.audioBridge = audioBridge
	s.rtcpHandler = rtcpFwd
}

// Start transitions the session to READY and begins media bridging.
// Components must be set via SetComponents before calling Start.
func (s *Session) Start(ctx context.Context) error {
	// Strict state transition: INIT → READY
	if err := s.sm.Transition(StateInit, StateReady); err != nil {
		return err
	}
	s.startTime = time.Now()

	ctx, cancel := context.WithCancel(ctx)
	s.cancelFn = cancel

	// Log every state transition
	s.sm.SetOnTransition(func(from, to State) {
		s.log.Infow("session state transition", "from", from.String(), "to", to.String())
	})

	// Start OTel span for the entire session lifetime
	ctx, s.rootSpan = tracer.Start(ctx, "session",
		trace.WithAttributes(
			attribute.String("session.id", s.ID),
			attribute.String("sip.call_id", s.CallID),
			attribute.String("session.room", s.RoomName),
			attribute.String("sip.from", s.FromURI),
		),
	)

	// Wire media pipeline
	s.router.SetPassthroughSink(s)

	setupDuration := time.Since(s.startTime)
	stats.CallSetupLatencyMs.Observe(float64(setupDuration.Milliseconds()))
	if s.router.Mode() == codec.ModePassthrough {
		stats.CodecPassthrough.Inc()
	} else {
		stats.CodecTranscode.Inc()
	}

	s.rootSpan.SetAttributes(attribute.Int64("setup_ms", setupDuration.Milliseconds()))

	s.log.Infow("session active",
		"setupMs", setupDuration.Milliseconds(),
		"codecMode", s.router.Mode().String(),
	)

	go s.monitor(ctx)
	return nil
}

// Media handlers are in media_pipe.go:
//   HandleVideoNALs, HandleAudioRTP, WriteOpusPCM, WriteNAL, WriteRawFrame

// --- Ports ---

func (s *Session) VideoPort() int {
	if s.receiver == nil {
		return 0
	}
	return s.receiver.VideoPort()
}

func (s *Session) AudioPort() int {
	if s.receiver == nil {
		return 0
	}
	return s.receiver.AudioPort()
}

// --- Lifecycle ---

// Close terminates the session with strict state transitions.
func (s *Session) Close() {
	s.closed.Once(func() {
		s.sm.ForceClosing()

		if s.cancelFn != nil {
			s.cancelFn()
		}

		// Teardown in reverse order: stop ingest first, then publisher
		var errs []error
		if s.rtcpHandler != nil {
			s.rtcpHandler.Close()
		}
		if s.receiver != nil {
			if err := s.receiver.Close(); err != nil {
				errs = append(errs, err)
			}
		}
		if s.pub != nil {
			if err := s.pub.Close(); err != nil {
				errs = append(errs, err)
			}
		}

		s.sm.ForceClosed()

		duration := time.Since(s.startTime)

		if s.rootSpan != nil {
			s.rootSpan.SetAttributes(
				attribute.Int64("duration_sec", int64(duration.Seconds())),
				attribute.Int64("video_nals", int64(s.videoNALs.Load())),
				attribute.Int64("audio_frames", int64(s.audioFrames.Load())),
				attribute.Int64("errors", int64(s.errors.Load())),
			)
			if len(errs) > 0 {
				s.rootSpan.RecordError(errors.Join(errs...))
				s.rootSpan.SetStatus(codes.Error, "session closed with errors")
			}
			s.rootSpan.End()
		}

		s.log.Infow("session closed",
			"durationSec", int(duration.Seconds()),
			"videoNALs", s.videoNALs.Load(),
			"audioFrames", s.audioFrames.Load(),
			"errors", s.errors.Load(),
			"publisherCB", s.publishCB.Stats().State,
		)
	})
}

// Closed returns a channel that is closed when the session is terminated.
func (s *Session) Closed() <-chan struct{} {
	return s.closed.Watch()
}

// Stats returns per-session statistics.
func (s *Session) Stats() SessionStats {
	return SessionStats{
		State:       s.sm.Current().String(),
		VideoNALs:   s.videoNALs.Load(),
		AudioFrames: s.audioFrames.Load(),
		Errors:      s.errors.Load(),
		PublisherCB: s.publishCB.Stats(),
		Duration:    time.Since(s.startTime),
	}
}

// SessionStats holds per-session statistics.
type SessionStats struct {
	State       string                         `json:"state"`
	VideoNALs   uint64                         `json:"video_nals"`
	AudioFrames uint64                         `json:"audio_frames"`
	Errors      uint64                         `json:"errors"`
	PublisherCB resilience.CircuitBreakerStats `json:"publisher_cb"`
	Duration    time.Duration                  `json:"duration"`
}

func (s *Session) monitor(ctx context.Context) {
	defer s.Close()

	if s.receiver == nil || s.pub == nil {
		s.log.Warnw("monitor: missing components", nil)
		return
	}

	select {
	case <-ctx.Done():
		s.log.Infow("session context cancelled")
	case <-s.receiver.Closed():
		s.log.Infow("RTP receiver closed, ending session")
	case <-s.pub.Closed():
		s.log.Infow("publisher disconnected, ending session")
	}
}
