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
	"github.com/pion/rtp"

	"github.com/livekit/sip/pkg/videobridge/codec"
)

// This file contains all media handler methods for Session.
// Extracted from session.go to keep it focused on identity + lifecycle.
// All handlers guard on sm.IsActive() + feature flags before forwarding.

// HandleVideoNALs implements ingest.MediaHandler — receives depacketized H.264 NALs from RTP receiver.
func (s *Session) HandleVideoNALs(nals []codec.NALUnit, timestamp uint32) error {
	if !s.sm.IsActive() {
		return nil
	}
	if s.ff != nil && !s.ff.VideoEnabled() {
		return nil
	}
	s.sm.MarkStreaming() // Active → Streaming on first media
	if s.lifecycle != nil {
		s.lifecycle.TouchVideo()
	}
	s.videoNALs.Add(uint64(len(nals)))
	return s.router.RouteNALs(nals, timestamp)
}

// HandleAudioRTP implements ingest.MediaHandler — receives raw G.711 audio RTP from RTP receiver.
func (s *Session) HandleAudioRTP(pkt *rtp.Packet) error {
	if !s.sm.IsActive() {
		return nil
	}
	if s.ff != nil && !s.ff.AudioEnabled() {
		return nil
	}
	if s.audioBridge == nil {
		return nil
	}
	if s.lifecycle != nil {
		s.lifecycle.TouchAudio()
	}
	s.audioFrames.Add(1)
	return s.audioBridge.HandleRTP(pkt)
}

// WriteOpusPCM implements ingest.AudioOpusWriter — receives PCM16 48kHz from audio bridge.
// Pipeline: G.711 RTP → AudioBridge → here → Publisher.WriteAudioPCM → Opus → LiveKit.
func (s *Session) WriteOpusPCM(samples []int16) error {
	if !s.sm.IsActive() || s.pub == nil {
		return nil
	}
	if s.ff != nil && !s.ff.AudioEnabled() {
		return nil
	}
	if !s.publishCB.Allow() {
		return nil
	}

	err := s.pub.WriteAudioPCM(samples)
	if err != nil {
		s.publishCB.RecordFailure(err)
		s.errors.Add(1)
		return err
	}
	s.publishCB.RecordSuccess()
	return nil
}

// WriteNAL implements codec.VideoSink — receives NALs for H.264 passthrough publishing.
func (s *Session) WriteNAL(nal codec.NALUnit, timestamp uint32) error {
	if !s.sm.IsActive() || s.pub == nil {
		return nil
	}
	if s.ff != nil && !s.ff.VideoEnabled() {
		return nil
	}
	if !s.publishCB.Allow() {
		return nil
	}

	err := s.pub.WriteVideoNAL(nal, timestamp)
	if err != nil {
		s.publishCB.RecordFailure(err)
		s.errors.Add(1)
		return err
	}
	s.publishCB.RecordSuccess()
	return nil
}

// WriteRawFrame implements codec.VideoSink — receives decoded frames for VP8 transcode path.
func (s *Session) WriteRawFrame(frame *codec.RawFrame) error {
	if !s.sm.IsActive() {
		return nil
	}
	s.log.Debugw("raw frame received (transcode path)",
		"width", frame.Width,
		"height", frame.Height,
		"keyframe", frame.Keyframe,
	)
	return nil
}
