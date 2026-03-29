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

package publisher

import (
	"io"
	"net"
	"sync"
	"sync/atomic"

	"github.com/frostbyte73/core"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"

	"github.com/livekit/protocol/logger"
	lksdk "github.com/livekit/server-sdk-go/v2"

	"github.com/livekit/sip/pkg/videobridge/stats"
)

// RTPSink receives RTP packets to be sent to the SIP endpoint.
type RTPSink interface {
	WriteRTP(pkt *rtp.Packet) error
}

// UDPRTPSink sends RTP packets over UDP to a remote SIP endpoint.
type UDPRTPSink struct {
	conn       *net.UDPConn
	remoteAddr *net.UDPAddr
}

// NewUDPRTPSink creates a new UDP RTP sink.
func NewUDPRTPSink(conn *net.UDPConn, remoteAddr *net.UDPAddr) *UDPRTPSink {
	return &UDPRTPSink{conn: conn, remoteAddr: remoteAddr}
}

// WriteRTP marshals and sends an RTP packet over UDP.
func (s *UDPRTPSink) WriteRTP(pkt *rtp.Packet) error {
	data, err := pkt.Marshal()
	if err != nil {
		return err
	}
	_, err = s.conn.WriteToUDP(data, s.remoteAddr)
	return err
}

// Subscriber subscribes to video and audio tracks in a LiveKit room
// and forwards them as RTP to the SIP endpoint (reverse direction).
type Subscriber struct {
	log  logger.Logger
	room *lksdk.Room

	mu        sync.RWMutex
	videoSink RTPSink
	audioSink RTPSink

	videoPacketsFwd atomic.Uint64
	audioPacketsFwd atomic.Uint64

	closed core.Fuse
}

// NewSubscriber creates a subscriber that reads tracks from the LiveKit room.
func NewSubscriber(log logger.Logger, room *lksdk.Room) *Subscriber {
	return &Subscriber{
		log:  log,
		room: room,
	}
}

// SetVideoSink sets the RTP sink for outbound video to the SIP endpoint.
func (s *Subscriber) SetVideoSink(sink RTPSink) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.videoSink = sink
}

// SetAudioSink sets the RTP sink for outbound audio to the SIP endpoint.
func (s *Subscriber) SetAudioSink(sink RTPSink) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.audioSink = sink
}

// Start begins subscribing to remote participant tracks.
func (s *Subscriber) Start() {
	// Subscribe to all existing tracks
	for _, rp := range s.room.GetRemoteParticipants() {
		for _, pub := range rp.TrackPublications() {
			if remotePub, ok := pub.(*lksdk.RemoteTrackPublication); ok {
				s.subscribeTo(remotePub, rp)
			}
		}
	}
}

func (s *Subscriber) subscribeTo(pub *lksdk.RemoteTrackPublication, rp *lksdk.RemoteParticipant) {
	log := s.log.WithValues("participant", rp.Identity(), "track", pub.SID(), "kind", pub.Kind())

	if pub.IsSubscribed() {
		return
	}

	log.Infow("subscribing to remote track")
	if err := pub.SetSubscribed(true); err != nil {
		log.Errorw("failed to subscribe to track", err)
	}
}

// HandleTrackSubscribed is called when a remote track is subscribed.
// Wire this to the room callback's OnTrackSubscribed.
func (s *Subscriber) HandleTrackSubscribed(track *webrtc.TrackRemote, pub *lksdk.RemoteTrackPublication, rp *lksdk.RemoteParticipant) {
	log := s.log.WithValues(
		"participant", rp.Identity(),
		"trackID", track.ID(),
		"codec", track.Codec().MimeType,
		"kind", track.Kind(),
	)
	log.Infow("remote track subscribed, starting forward loop")

	go s.forwardTrack(log, track)
}

func (s *Subscriber) forwardTrack(log logger.Logger, track *webrtc.TrackRemote) {
	isVideo := track.Kind() == webrtc.RTPCodecTypeVideo

	buf := make([]byte, 1500)
	for !s.closed.IsBroken() {
		n, _, err := track.Read(buf)
		if err != nil {
			if err == io.EOF {
				log.Infow("remote track ended")
			} else {
				log.Warnw("error reading remote track", err)
			}
			return
		}

		var pkt rtp.Packet
		if err := pkt.Unmarshal(buf[:n]); err != nil {
			log.Debugw("failed to unmarshal RTP from remote track", "error", err)
			continue
		}

		s.mu.RLock()
		var sink RTPSink
		if isVideo {
			sink = s.videoSink
		} else {
			sink = s.audioSink
		}
		s.mu.RUnlock()

		if sink == nil {
			continue
		}

		if err := sink.WriteRTP(&pkt); err != nil {
			log.Debugw("failed to forward RTP to SIP", "error", err)
			continue
		}

		if isVideo {
			s.videoPacketsFwd.Add(1)
			stats.RTPPacketsSent.WithLabelValues("video_reverse").Inc()
		} else {
			s.audioPacketsFwd.Add(1)
			stats.RTPPacketsSent.WithLabelValues("audio_reverse").Inc()
		}
	}
}

// Stats returns subscriber forwarding statistics.
func (s *Subscriber) Stats() SubscriberStats {
	return SubscriberStats{
		VideoPacketsForwarded: s.videoPacketsFwd.Load(),
		AudioPacketsForwarded: s.audioPacketsFwd.Load(),
	}
}

// SubscriberStats holds subscriber statistics.
type SubscriberStats struct {
	VideoPacketsForwarded uint64
	AudioPacketsForwarded uint64
}

// Close stops the subscriber.
func (s *Subscriber) Close() {
	s.closed.Break()
}
