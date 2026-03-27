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
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/frostbyte73/core"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"

	msdk "github.com/livekit/media-sdk"
	msdkrtp "github.com/livekit/media-sdk/rtp"
	"github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	lksdk "github.com/livekit/server-sdk-go/v2"

	"github.com/livekit/sip/pkg/media/opus"
	"github.com/livekit/sip/pkg/videobridge/codec"
	"github.com/livekit/sip/pkg/videobridge/stats"
)

const (
	videoTrackID  = "video"
	videoStreamID = "sip-video"
	audioTrackID  = "audio"
	audioStreamID = "sip-audio"
	h264ClockRate = 90000
	opusClockRate = 48000
)

// PublisherConfig configures the LiveKit room publisher.
type PublisherConfig struct {
	WsURL     string
	ApiKey    string
	ApiSecret string
	RoomName  string
	// Participant identity in the LiveKit room
	Identity string
	// Participant display name
	Name string
	// Participant metadata
	Metadata string
	// Participant attributes (e.g., SIP call info)
	Attributes map[string]string
	// Video codec to publish: "h264" (passthrough) or "vp8" (transcoded)
	VideoCodec string
	// Maximum video bitrate
	MaxBitrate int
}

// Publisher joins a LiveKit room and publishes video + audio tracks
// from a SIP video call.
type Publisher struct {
	log    logger.Logger
	config PublisherConfig

	room       *lksdk.Room
	videoTrack *webrtc.TrackLocalStaticRTP
	audioTrack *webrtc.TrackLocalStaticRTP

	// Opus audio pipeline: PCM16 → Opus encode → TrackLocalStaticSample
	audioSampleTrack *webrtc.TrackLocalStaticSample
	audioOpusWriter  msdk.PCM16Writer // accepts PCM16 samples, Opus-encodes and writes to track

	repacketizer *codec.H264Repacketizer

	// RTP sequence/timestamp management for video
	videoSeq atomic.Uint32
	videoTS  atomic.Uint32

	// RTP sequence/timestamp management for audio
	audioSeq atomic.Uint32

	// PLI callback: called when LiveKit requests a keyframe
	pliHandler atomic.Pointer[func()]

	// Stats
	videoPacketsSent atomic.Uint64
	audioPacketsSent atomic.Uint64
	audioSamplesSent atomic.Uint64

	closed core.Fuse
	mu     sync.Mutex
}

// NewPublisher creates a new LiveKit room publisher.
func NewPublisher(log logger.Logger, config PublisherConfig) *Publisher {
	return &Publisher{
		log:          log,
		config:       config,
		repacketizer: codec.NewH264Repacketizer(1200),
	}
}

// Connect joins the LiveKit room and creates video + audio tracks.
func (p *Publisher) Connect(ctx context.Context) error {
	p.log.Infow("connecting to LiveKit room",
		"room", p.config.RoomName,
		"identity", p.config.Identity,
		"videoCodec", p.config.VideoCodec,
	)

	roomCallback := &lksdk.RoomCallback{
		ParticipantCallback: lksdk.ParticipantCallback{
			OnTrackSubscribed: func(track *webrtc.TrackRemote, pub *lksdk.RemoteTrackPublication, rp *lksdk.RemoteParticipant) {
				p.log.Debugw("remote track subscribed",
					"participant", rp.Identity(),
					"track", track.ID(),
					"codec", track.Codec().MimeType,
				)
			},
		},
		OnDisconnected: func() {
			p.log.Infow("disconnected from LiveKit room")
			p.closed.Break()
		},
	}

	room := lksdk.NewRoom(roomCallback)
	room.SetLogger(p.log)

	token, err := p.generateToken()
	if err != nil {
		return fmt.Errorf("generating room token: %w", err)
	}

	err = room.JoinWithToken(p.config.WsURL, token,
		lksdk.WithAutoSubscribe(false),
	)
	if err != nil {
		return fmt.Errorf("joining room: %w", err)
	}

	p.room = room
	p.log.Infow("joined LiveKit room",
		"roomSID", room.SID(),
		"participantSID", room.LocalParticipant.SID(),
	)

	// Create and publish video track
	if err := p.createVideoTrack(); err != nil {
		room.Disconnect()
		return fmt.Errorf("creating video track: %w", err)
	}

	// Create and publish audio track
	if err := p.createAudioTrack(); err != nil {
		room.Disconnect()
		return fmt.Errorf("creating audio track: %w", err)
	}

	return nil
}

func (p *Publisher) createVideoTrack() error {
	var mimeType string
	switch p.config.VideoCodec {
	case "vp8":
		mimeType = webrtc.MimeTypeVP8
	default:
		mimeType = webrtc.MimeTypeH264
	}

	track, err := webrtc.NewTrackLocalStaticRTP(
		webrtc.RTPCodecCapability{
			MimeType:  mimeType,
			ClockRate: h264ClockRate,
		},
		videoTrackID,
		videoStreamID,
	)
	if err != nil {
		return fmt.Errorf("creating video track: %w", err)
	}

	opts := &lksdk.TrackPublicationOptions{
		Name:   p.config.Identity + "-video",
		Source: livekit.TrackSource_SCREEN_SHARE,
	}

	pub, err := p.room.LocalParticipant.PublishTrack(track, opts)
	if err != nil {
		return fmt.Errorf("publishing video track: %w", err)
	}

	p.videoTrack = track
	p.log.Infow("video track published",
		"trackSID", pub.SID(),
		"codec", mimeType,
	)

	return nil
}

func (p *Publisher) createAudioTrack() error {
	// Create a sample-based track for Opus audio (same pattern as existing SIP service room.go)
	sampleTrack, err := webrtc.NewTrackLocalStaticSample(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus},
		audioTrackID,
		audioStreamID,
	)
	if err != nil {
		return fmt.Errorf("creating audio sample track: %w", err)
	}

	pub, err := p.room.LocalParticipant.PublishTrack(sampleTrack, &lksdk.TrackPublicationOptions{
		Name: p.config.Identity + "-audio",
	})
	if err != nil {
		return fmt.Errorf("publishing audio track: %w", err)
	}

	p.audioSampleTrack = sampleTrack

	// Build Opus encoding pipeline: PCM16 → Opus encode → media.SampleWriter → track
	opusSampleWriter := msdk.FromSampleWriter[opus.Sample](sampleTrack, opusSampleRate, msdkrtp.DefFrameDur)
	opusEncoder, err := opus.Encode(opusSampleWriter, 1, p.log)
	if err != nil {
		return fmt.Errorf("creating opus encoder: %w", err)
	}
	p.audioOpusWriter = opusEncoder

	p.log.Infow("audio track published with Opus encoder", "trackSID", pub.SID(), "sampleRate", opusSampleRate)

	// Also create a raw RTP track as fallback for direct RTP forwarding
	rtpTrack, err := webrtc.NewTrackLocalStaticRTP(
		webrtc.RTPCodecCapability{
			MimeType:  webrtc.MimeTypeOpus,
			ClockRate: opusClockRate,
			Channels:  1,
		},
		audioTrackID+"-rtp",
		audioStreamID+"-rtp",
	)
	if err != nil {
		p.log.Debugw("RTP audio track creation skipped", "error", err)
	} else {
		p.audioTrack = rtpTrack
	}

	return nil
}

const opusSampleRate = 48000

// WriteVideoNAL writes an H.264 NAL unit to the video track (passthrough mode).
// The NAL is repacketized into WebRTC-compatible RTP packets.
func (p *Publisher) WriteVideoNAL(nal codec.NALUnit, timestamp uint32) error {
	if p.videoTrack == nil || p.closed.IsBroken() {
		return nil
	}

	payloads := p.repacketizer.Repacketize(nal)

	for i, payload := range payloads {
		seq := uint16(p.videoSeq.Add(1))
		marker := i == len(payloads)-1 // marker bit on last packet of the NAL

		pkt := &rtp.Packet{
			Header: rtp.Header{
				Version:        2,
				PayloadType:    96, // dynamic PT for H.264
				SequenceNumber: seq,
				Timestamp:      timestamp,
				Marker:         marker,
			},
			Payload: payload,
		}

		if err := p.videoTrack.WriteRTP(pkt); err != nil {
			return fmt.Errorf("writing video RTP: %w", err)
		}
		p.videoPacketsSent.Add(1)
		stats.RTPPacketsSent.WithLabelValues("video").Inc()
	}

	return nil
}

// WriteVideoRTP writes a pre-formed RTP packet to the video track (transcode mode).
func (p *Publisher) WriteVideoRTP(pkt *rtp.Packet) error {
	if p.videoTrack == nil || p.closed.IsBroken() {
		return nil
	}

	if err := p.videoTrack.WriteRTP(pkt); err != nil {
		return fmt.Errorf("writing video RTP: %w", err)
	}
	p.videoPacketsSent.Add(1)
	stats.RTPPacketsSent.WithLabelValues("video").Inc()
	return nil
}

// WriteAudioPCM writes PCM16 samples (48kHz, mono) through the Opus encoder pipeline.
// This is the primary audio path: G.711 → AudioBridge (PCM16 48kHz) → here → Opus → LiveKit.
func (p *Publisher) WriteAudioPCM(samples []int16) error {
	if p.audioOpusWriter == nil || p.closed.IsBroken() {
		return nil
	}

	if err := p.audioOpusWriter.WriteSample(msdk.PCM16Sample(samples)); err != nil {
		return fmt.Errorf("writing audio PCM to opus encoder: %w", err)
	}
	p.audioSamplesSent.Add(uint64(len(samples)))
	stats.RTPPacketsSent.WithLabelValues("audio").Inc()
	return nil
}

// WriteAudioRTP writes a raw audio RTP packet to the audio track (fallback/reverse path).
func (p *Publisher) WriteAudioRTP(pkt *rtp.Packet) error {
	if p.audioTrack == nil || p.closed.IsBroken() {
		return nil
	}

	if err := p.audioTrack.WriteRTP(pkt); err != nil {
		return fmt.Errorf("writing audio RTP: %w", err)
	}
	p.audioPacketsSent.Add(1)
	stats.RTPPacketsSent.WithLabelValues("audio").Inc()
	return nil
}

// SetPLIHandler sets the callback invoked when LiveKit requests a keyframe (PLI).
func (p *Publisher) SetPLIHandler(handler func()) {
	p.pliHandler.Store(&handler)
}

// RequestKeyframe triggers a PLI handler if set.
func (p *Publisher) RequestKeyframe() {
	ptr := p.pliHandler.Load()
	if ptr != nil {
		(*ptr)()
		stats.KeyframeRequests.Inc()
	}
}

// Close disconnects from the LiveKit room.
func (p *Publisher) Close() error {
	var err error
	p.closed.Once(func() {
		if p.audioOpusWriter != nil {
			_ = p.audioOpusWriter.Close()
		}
		if p.room != nil {
			p.room.Disconnect()
		}
		p.log.Infow("publisher closed",
			"videoPacketsSent", p.videoPacketsSent.Load(),
			"audioPacketsSent", p.audioPacketsSent.Load(),
			"audioSamplesSent", p.audioSamplesSent.Load(),
		)
	})
	return err
}

// Closed returns a channel that is closed when the publisher disconnects.
func (p *Publisher) Closed() <-chan struct{} {
	return p.closed.Watch()
}

// Stats returns publisher statistics.
func (p *Publisher) Stats() PublisherStats {
	return PublisherStats{
		VideoPacketsSent: p.videoPacketsSent.Load(),
		AudioPacketsSent: p.audioPacketsSent.Load(),
	}
}

// PublisherStats holds publisher statistics.
type PublisherStats struct {
	VideoPacketsSent uint64
	AudioPacketsSent uint64
}

func (p *Publisher) generateToken() (string, error) {
	at := auth.NewAccessToken(p.config.ApiKey, p.config.ApiSecret)
	grant := &auth.VideoGrant{
		RoomJoin: true,
		Room:     p.config.RoomName,
	}
	at.SetVideoGrant(grant).
		SetIdentity(p.config.Identity).
		SetName(p.config.Name).
		SetMetadata(p.config.Metadata).
		SetValidFor(24 * time.Hour)

	if p.config.Attributes != nil {
		at.SetAttributes(p.config.Attributes)
	}

	return at.ToJWT()
}
