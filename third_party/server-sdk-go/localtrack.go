// Copyright 2023 LiveKit, Inc.
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

package lksdk

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"strings"
	"sync"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
	"github.com/pion/sdp/v3"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
	"go.uber.org/atomic"

	protoCodecs "github.com/livekit/protocol/codecs"
	"github.com/livekit/protocol/livekit"
	protoLogger "github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/utils/guid"
	e2eetypes "github.com/livekit/server-sdk-go/v2/e2ee/types"
)

const (
	rtpOutboundMTU    = 1200
	rtpInboundMTU     = 1500
	absCaptureTimeURI = "http://www.webrtc.org/experiments/rtp-hdrext/abs-capture-time"
)

var (
	errInvalidDurationSample = errors.New("invalid duration sample")
	errOutOfOrderSample      = errors.New("out-of-order sample")
)

type SampleWriteOptions struct {
	AudioLevel *uint8
}

// LocalTrack is a local track that simplifies writing samples.
// It handles timing and publishing of things, so as long as a SampleProvider is provided, the class takes care of
// publishing tracks at the right frequency
// This extends webrtc.TrackLocalStaticSample, and adds the ability to write RTP extensions
type LocalTrack struct {
	log                      protoLogger.Logger
	packetizer               rtp.Packetizer
	sequencer                rtp.Sequencer
	transceiver              *webrtc.RTPTransceiver
	rtpTrack                 *webrtc.TrackLocalStaticRTP
	ssrc                     webrtc.SSRC
	ssrcAcked                bool
	clockRate                float64
	bound                    atomic.Bool
	lock                     sync.RWMutex
	writeStartupLock         sync.Mutex
	audioLevelID             uint8
	sdesMidID                uint8
	sdesRtpStreamID          uint8
	incomingAbsCaptureTimeID uint8
	absCaptureTimeID         uint8
	lastTS                   time.Time
	lastRTPTimestamp         uint32
	simulcastID              string
	videoLayer               *livekit.VideoLayer
	onRTCP                   func(rtcp.Packet)

	muted          atomic.Bool
	disabled       atomic.Bool
	disconnected   atomic.Bool
	cancelWrite    func()
	writeClosed    chan struct{}
	provider       SampleProvider
	frameEncryptor e2eetypes.FrameEncryptor // nil = no encryption
	onBind         func()
	onUnbind       func()
	// notify when sample provider responds with EOF
	onWriteComplete func()
}
type LocalSampleTrack = LocalTrack

type LocalTrackOptions func(s *LocalTrack)
type LocalSampleTrackOptions = LocalTrackOptions

// WithSimulcast marks the current track for simulcasting.
// In order to use simulcast, simulcastID must be identical across all layers.
func WithSimulcast(simulcastID string, layer *livekit.VideoLayer) LocalTrackOptions {
	return func(s *LocalTrack) {
		s.videoLayer = layer
		s.simulcastID = simulcastID
	}
}

// WithFrameEncryptor sets a frame-level encryptor for the track.
// When set, all samples written via WriteSample are encrypted before
// RTP packetization. Use NewAudioFrameEncryptor, NewH264FrameEncryptor,
// or NewH265FrameEncryptor to create a suitable encryptor.
func WithFrameEncryptor(enc e2eetypes.FrameEncryptor) LocalTrackOptions {
	return func(s *LocalTrack) {
		s.frameEncryptor = enc
	}
}

// WithRTCPHandler sets a callback to receive RTCP packets for the track.
func WithRTCPHandler(cb func(rtcp.Packet)) LocalTrackOptions {
	return func(s *LocalTrack) {
		s.onRTCP = cb
	}
}

func NewLocalTrack(c webrtc.RTPCodecCapability, opts ...LocalTrackOptions) (*LocalTrack, error) {
	s := &LocalTrack{log: logger}
	for _, o := range opts {
		o(s)
	}
	rid := ""
	if s.videoLayer != nil {
		switch s.videoLayer.Quality {
		case livekit.VideoQuality_HIGH:
			rid = "f"
		case livekit.VideoQuality_MEDIUM:
			rid = "h"
		case livekit.VideoQuality_LOW:
			rid = "q"
		}
	}
	trackID := guid.New("TR_")
	streamID := guid.New("ST_")
	if s.simulcastID != "" {
		trackID = s.simulcastID
		streamID = s.simulcastID
	}
	rtpTrack, err := webrtc.NewTrackLocalStaticRTP(c, trackID, streamID, webrtc.WithRTPStreamID(rid))
	if err != nil {
		return nil, err
	}
	s.rtpTrack = rtpTrack
	return s, nil
}

func NewLocalSampleTrack(c webrtc.RTPCodecCapability, opts ...LocalTrackOptions) (*LocalTrack, error) {
	return NewLocalTrack(c, opts...)
}

func NewLocalTrackWithCodec(c *livekit.Codec, opts ...LocalTrackOptions) (*LocalTrack, error) {
	return NewLocalTrack(protoCodecs.ToWebrtcCodecParameters(c).RTPCodecCapability, opts...)
}

func NewLocalSampleTrackWithCodec(c *livekit.Codec, opts ...LocalTrackOptions) (*LocalTrack, error) {
	return NewLocalTrack(protoCodecs.ToWebrtcCodecParameters(c).RTPCodecCapability, opts...)
}

// SetLogger overrides default logger for this track.
func (s *LocalTrack) SetLogger(l protoLogger.Logger) {
	s.log = l
}

// SetTransceiver sets the RTP transceiver for this track.
func (s *LocalTrack) SetTransceiver(transceiver *webrtc.RTPTransceiver) {
	s.lock.Lock()
	defer s.lock.Unlock()

	s.transceiver = transceiver
}

// ID returns the unique identifier for this Track. This should be unique for the
// stream, but doesn't have to globally unique. A common example would be 'audio' or 'video'
// and StreamID would be 'desktop' or 'webcam'.
func (s *LocalTrack) ID() string { return s.rtpTrack.ID() }

// RID returns the RTP stream identifier.
func (s *LocalTrack) RID() string {
	return s.rtpTrack.RID()
}

// StreamID returns the group this track belongs to. This must be unique.
func (s *LocalTrack) StreamID() string { return s.rtpTrack.StreamID() }

// Kind returns whether this TrackLocal is audio or video.
func (s *LocalTrack) Kind() webrtc.RTPCodecType { return s.rtpTrack.Kind() }

// Codec returns the codec capability of the track.
func (s *LocalTrack) Codec() webrtc.RTPCodecCapability {
	return s.rtpTrack.Codec()
}

// IsBound returns whether the track is currently bound to a peer connection.
func (s *LocalTrack) IsBound() bool {
	return s.bound.Load()
}

// Bind is an interface for TrackLocal, not for external consumption.
func (s *LocalTrack) Bind(t webrtc.TrackLocalContext) (webrtc.RTPCodecParameters, error) {
	codec, err := s.rtpTrack.Bind(t)
	if err != nil {
		return codec, err
	}

	payloader, err := payloaderForCodec(codec.RTPCodecCapability)
	if err != nil {
		return codec, err
	}

	s.lock.Lock()
	s.ssrcAcked = false
	s.ssrc = t.SSRC()
	for _, ext := range t.HeaderExtensions() {
		if ext.URI == sdp.AudioLevelURI {
			s.audioLevelID = uint8(ext.ID)
		}

		if ext.URI == sdp.SDESMidURI {
			s.sdesMidID = uint8(ext.ID)
		}

		if ext.URI == sdp.SDESRTPStreamIDURI {
			s.sdesRtpStreamID = uint8(ext.ID)
		}

		if ext.URI == absCaptureTimeURI {
			s.absCaptureTimeID = uint8(ext.ID)
		}
	}
	s.sequencer = rtp.NewRandomSequencer()
	s.packetizer = rtp.NewPacketizer(
		rtpOutboundMTU,
		0, // Value is handled when writing
		0, // Value is handled when writing
		payloader,
		s.sequencer,
		codec.ClockRate,
	)
	s.clockRate = float64(codec.ClockRate)
	onBind := s.onBind
	provider := s.provider
	onWriteComplete := s.onWriteComplete
	s.bound.Store(true)
	s.lock.Unlock()

	if provider != nil {
		err = provider.OnBind()
		go s.writeWorker(provider, onWriteComplete)
	}

	go s.rtcpWorker(t.RTCPReader())

	// notify callbacks last
	if onBind != nil {
		go onBind()
	}
	return codec, err
}

// Unbind is an interface for TrackLocal, not for external consumption.
func (s *LocalTrack) Unbind(t webrtc.TrackLocalContext) error {
	s.lock.Lock()
	provider := s.provider
	onUnbind := s.onUnbind
	s.bound.Store(false)
	cancel := s.cancelWrite
	s.lock.Unlock()

	var err error

	if provider != nil {
		err = provider.OnUnbind()
	}
	if cancel != nil {
		cancel()
	}
	if onUnbind != nil {
		go onUnbind()
	}
	unbindErr := s.rtpTrack.Unbind(t)
	if unbindErr != nil {
		return unbindErr
	}
	return err
}

// StartWrite starts writing samples from the provided SampleProvider.
// The onComplete callback is called when writing finishes.
func (s *LocalTrack) StartWrite(provider SampleProvider, onComplete func()) error {
	s.lock.Lock()
	defer s.lock.Unlock()
	if s.provider == provider {
		return nil
	}

	// when bound and already writing, ignore
	if s.IsBound() {
		// unbind previous provider
		if s.provider != nil {
			if err := s.provider.OnUnbind(); err != nil {
				return err
			}
		}
		if err := provider.OnBind(); err != nil {
			return err
		}
		// start new writer
		go s.writeWorker(provider, onComplete)
	}
	s.provider = provider
	s.onWriteComplete = onComplete
	return nil
}

// OnBind sets a callback to be called when the track has been negotiated for publishing and bound to a peer connection.
func (s *LocalTrack) OnBind(f func()) {
	s.lock.Lock()
	s.onBind = f
	s.lock.Unlock()
}

// OnUnbind sets a callback to be called after the track is removed from a peer connection.
func (s *LocalTrack) OnUnbind(f func()) {
	s.lock.Lock()
	s.onUnbind = f
	s.lock.Unlock()
}

// This is used to translate abs-capture-time extension before writing to a peer connection when source provided
// extension ID does not match negotiated extension ID on the peer connection
func (s *LocalTrack) SetIncomingAbsCaptureTimeExtensionID(id uint8) {
	s.lock.Lock()
	s.incomingAbsCaptureTimeID = id
	s.lock.Unlock()
}

// WriteRTP writes an RTP packet to the track with optional sample write options.
func (s *LocalTrack) WriteRTP(p *rtp.Packet, opts *SampleWriteOptions) error {
	s.lock.RLock()
	transceiver := s.transceiver
	ssrcAcked := s.ssrcAcked
	audioLevelID := s.audioLevelID
	incomingAbsCaptureTimeID := s.incomingAbsCaptureTimeID
	absCaptureTimeID := s.absCaptureTimeID
	s.lock.RUnlock()

	if audioLevelID != 0 && opts != nil && opts.AudioLevel != nil {
		ext := rtp.AudioLevelExtension{
			Level: *opts.AudioLevel,
		}
		data, err := ext.Marshal()
		if err != nil {
			return err
		}
		if err := p.SetExtension(audioLevelID, data); err != nil {
			return err
		}
	}

	if incomingAbsCaptureTimeID != 0 && absCaptureTimeID != 0 && incomingAbsCaptureTimeID != absCaptureTimeID && p.Extension {
		if data := p.GetExtension(incomingAbsCaptureTimeID); len(data) != 0 {
			_ = p.DelExtension(incomingAbsCaptureTimeID)
			_ = p.SetExtension(absCaptureTimeID, data)
		}
	}

	if s.RID() != "" && transceiver != nil && transceiver.Mid() != "" && !ssrcAcked {
		s.lock.RLock()
		sdesMidID := s.sdesMidID
		sdesRtpStreamID := s.sdesRtpStreamID
		s.lock.RUnlock()

		if sdesMidID != 0 {
			midValue := transceiver.Mid()
			if err := p.SetExtension(sdesMidID, []byte(midValue)); err != nil {
				return err
			}
		}

		if sdesRtpStreamID != 0 {
			ridValue := s.RID()
			if err := p.SetExtension(sdesRtpStreamID, []byte(ridValue)); err != nil {
				return err
			}
		}
	}

	return s.rtpTrack.WriteRTP(p)
}

// WriteSample writes a media sample to the track with optional write options.
// This handles timing, RTP packetization, and sample ordering automatically.
func (s *LocalTrack) WriteSample(sample media.Sample, opts *SampleWriteOptions) error {
	s.lock.Lock()
	if s.packetizer == nil {
		s.lock.Unlock()
		return nil
	}

	//
	// A few different cases for time stamp
	//   1. Publishers sending too fast, like from a file or some other source which has faster than real time data.
	//   2. Publishing after a long gap.
	//
	// Publishers could provide one or more of
	//   1. Timestamp -> wall clock time
	//   2. Duration -> duration of given sample
	//   3. PacketTimestamp -> RTP packet time stamp
	//   4. PrevDroppedPackets -> number of dropped packets before this sample
	//
	// The goal here is to calculate RTP time stamp of provided sample.
	// Priority of what is used (even if multiple are provided)
	//   1. PacketTimestamp
	//   2. Timestamp
	//   3. Duration
	//
	sampleDurationSeconds := sample.Duration.Seconds()
	elapsedDurationSeconds := float64(sample.PrevDroppedPackets+1) * sampleDurationSeconds // +1 to include given sample
	elapsedDurationSamples := uint32(elapsedDurationSeconds * s.clockRate)
	currentRTPTimestamp := uint32(0)
	if s.lastRTPTimestamp == 0 {
		// first sample, NOTE: 0 is a valid time stamp, but chances of it are 1 / (1 << 32)

		// negative duration is invalid
		if sampleDurationSeconds < 0.0 {
			s.lock.Unlock()
			return errInvalidDurationSample
		}

		if sample.PacketTimestamp != 0 {
			s.lastRTPTimestamp = sample.PacketTimestamp - elapsedDurationSamples
			currentRTPTimestamp = sample.PacketTimestamp
		} else {
			// start with a random one
			s.lastRTPTimestamp = uint32(rand.Intn(1<<30)) + uint32(1<<31) // in third quartile of timestamp space
			currentRTPTimestamp = s.lastRTPTimestamp + elapsedDurationSamples
		}

		s.lastTS = sample.Timestamp
		if !s.lastTS.IsZero() {
			s.lastTS = s.lastTS.Add(time.Duration(-elapsedDurationSeconds * float64(time.Second)))
		}
	} else {
		// reject samples that move backward in time
		switch {
		case sample.PacketTimestamp != 0:
			if (sample.PacketTimestamp - s.lastRTPTimestamp) > (1 << 31) {
				s.lock.Unlock()
				return errOutOfOrderSample
			}

			currentRTPTimestamp = sample.PacketTimestamp

		case !sample.Timestamp.IsZero():
			if !s.lastTS.IsZero() && sample.Timestamp.Before(s.lastTS) {
				s.lock.Unlock()
				return errOutOfOrderSample
			}

			if s.lastTS.IsZero() {
				// negative duration is invalid
				if sampleDurationSeconds < 0.0 {
					s.lock.Unlock()
					return errInvalidDurationSample
				}

				s.lastTS = sample.Timestamp.Add(time.Duration(-elapsedDurationSeconds * float64(time.Second)))
			}

			currentRTPTimestamp = s.lastRTPTimestamp + uint32(sample.Timestamp.Sub(s.lastTS).Seconds()*s.clockRate)
		}
	}

	if currentRTPTimestamp == 0 {
		// PacketTimestamp AND Timestamp not in sample, use Duration
		if sampleDurationSeconds < 0.0 {
			s.lock.Unlock()
			return errInvalidDurationSample
		}

		// assume all dropped packets have the given Duration
		currentRTPTimestamp = s.lastRTPTimestamp + elapsedDurationSamples
	}

	samples := currentRTPTimestamp - s.lastRTPTimestamp
	skippedSamples := uint32(0)
	if samples < elapsedDurationSamples {
		// possible that wall clock time based samples are sent too close,
		// lower bound Duration if necessary
		samples = elapsedDurationSamples
		currentRTPTimestamp = s.lastRTPTimestamp + elapsedDurationSamples
	} else if samples > elapsedDurationSamples {
		if elapsedDurationSamples == 0 {
			// could have 0 duration samples, for example SPS/PPS NALUs in H.264
			currentRTPTimestamp = s.lastRTPTimestamp
		} else {
			// writing a sample after a gap
			skippedSamples = samples - elapsedDurationSamples
		}
		samples = elapsedDurationSamples
	}

	// skip packets by the number of previously dropped packets
	for i := uint16(0); i < sample.PrevDroppedPackets; i++ {
		s.sequencer.NextSequenceNumber()
	}

	samplesPerPacket := samples / uint32(sample.PrevDroppedPackets+1)
	if sample.PrevDroppedPackets > 0 {
		s.packetizer.SkipSamples(samplesPerPacket * uint32(sample.PrevDroppedPackets))
	}
	if skippedSamples != 0 {
		s.packetizer.SkipSamples(skippedSamples)
	}

	data := sample.Data
	if s.frameEncryptor != nil {
		var err error
		data, err = s.frameEncryptor.EncryptFrame(data)
		if err != nil {
			s.lock.Unlock()
			return fmt.Errorf("frame encryption: %w", err)
		}
	}

	packets := s.packetizer.Packetize(data, samplesPerPacket)

	s.lastTS = sample.Timestamp
	s.lastRTPTimestamp = currentRTPTimestamp
	s.lock.Unlock()

	if s.disconnected.Load() {
		return nil
	}

	var writeErrs []error
	for _, p := range packets {
		if err := s.WriteRTP(p, opts); err != nil {
			writeErrs = append(writeErrs, err)
		}
	}

	if len(writeErrs) > 0 {
		return writeErrs[0]
	}

	return nil
}

// Close stops the track and cleans up resources.
func (s *LocalTrack) Close() error {
	s.lock.Lock()
	cancelWrite := s.cancelWrite
	provider := s.provider
	s.lock.Unlock()
	if cancelWrite != nil {
		cancelWrite()
	}
	if provider != nil {
		provider.Close()
	}
	return nil
}

// SSRC returns the Synchronization Source identifier for this track.
func (s *LocalTrack) SSRC() webrtc.SSRC {
	s.lock.Lock()
	defer s.lock.Unlock()

	return s.ssrc
}

func (s *LocalTrack) setMuted(muted bool) {
	s.muted.Store(muted)
}

func (s *LocalTrack) setDisabled(disabled bool) {
	s.disabled.Store(disabled)
}

func (s *LocalTrack) setDisconnected(disconnected bool) {
	s.disconnected.Store(disconnected)
}

func (s *LocalTrack) rtcpWorker(rtcpReader interceptor.RTCPReader) {
	// read incoming rtcp packets, interceptors require this
	b := make([]byte, rtpInboundMTU)
	rtcpCB := s.onRTCP

	for {
		var a interceptor.Attributes
		i, _, err := rtcpReader.Read(b, a)
		if err != nil {
			// pipe closed
			return
		}

		pkts, err := rtcp.Unmarshal(b[:i])
		if err != nil {
			s.log.Warnw("could not unmarshal rtcp", err)
			return
		}
		for _, packet := range pkts {
			s.lock.Lock()
			if !s.ssrcAcked {
				switch p := packet.(type) {
				case *rtcp.ReceiverReport:
					for _, r := range p.Reports {
						if webrtc.SSRC(r.SSRC) == s.ssrc {
							s.ssrcAcked = true
							break
						}
					}
				}
			}
			s.lock.Unlock()
			if rtcpCB != nil {
				rtcpCB(packet)
			}
		}
	}
}

func (s *LocalTrack) writeWorker(provider SampleProvider, onComplete func()) {
	s.writeStartupLock.Lock()

	s.lock.RLock()
	previousCancel := s.cancelWrite
	previousWriteClosed := s.writeClosed
	s.lock.RUnlock()

	if previousCancel != nil {
		previousCancel()
		// wait for previous write to finish to prevent multi-threaded provider reading
		<-previousWriteClosed
	}

	s.lock.Lock()
	var ctx context.Context
	ctx, s.cancelWrite = context.WithCancel(context.Background())
	writeClosed := make(chan struct{})
	s.writeClosed = writeClosed
	s.lock.Unlock()
	s.writeStartupLock.Unlock()
	if onComplete != nil {
		defer onComplete()
	}
	defer close(writeClosed)

	audioProvider, isAudioProvider := provider.(AudioSampleProvider)

	nextSampleTime := time.Now()

	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()

	for {
		// Be mindful that NextSample is not thread-safe
		sample, err := provider.NextSample(ctx)
		if err == io.EOF {
			return
		}
		if err != nil {
			s.log.Errorw("could not get sample from provider", err)
			return
		}

		if !s.muted.Load() && !s.disabled.Load() {
			var opts *SampleWriteOptions
			if isAudioProvider {
				level := audioProvider.CurrentAudioLevel()
				opts = &SampleWriteOptions{
					AudioLevel: &level,
				}
			}

			sample.Timestamp = nextSampleTime
			if err := s.WriteSample(sample, opts); err != nil {
				s.log.Errorw("could not write sample", err)
				return
			}
		}

		// account for clock drift
		nextSampleTime = nextSampleTime.Add(sample.Duration)
		sleepDuration := time.Until(nextSampleTime)
		if sleepDuration <= 0 {
			continue
		}
		ticker.Reset(sleepDuration)

		select {
		case <-ticker.C:
			continue
		case <-ctx.Done():
			return
		}
	}
}

// duplicated from pion mediaengine.go
func payloaderForCodec(codec webrtc.RTPCodecCapability) (rtp.Payloader, error) {
	switch strings.ToLower(codec.MimeType) {
	case strings.ToLower(webrtc.MimeTypeH264):
		return &codecs.H264Payloader{}, nil
	case strings.ToLower(webrtc.MimeTypeH265):
		return &codecs.H265Payloader{}, nil
	case strings.ToLower(webrtc.MimeTypeOpus):
		return &codecs.OpusPayloader{}, nil
	case strings.ToLower(webrtc.MimeTypeVP8):
		return &codecs.VP8Payloader{
			EnablePictureID: true,
		}, nil
	case strings.ToLower(webrtc.MimeTypeVP9):
		return &codecs.VP9Payloader{}, nil
	case strings.ToLower(webrtc.MimeTypeAV1):
		return &codecs.AV1Payloader{}, nil
	case strings.ToLower(webrtc.MimeTypeG722):
		return &codecs.G722Payloader{}, nil
	case strings.ToLower(webrtc.MimeTypePCMU), strings.ToLower(webrtc.MimeTypePCMA):
		return &codecs.G711Payloader{}, nil
	default:
		return nil, webrtc.ErrNoPayloaderForCodec
	}
}
