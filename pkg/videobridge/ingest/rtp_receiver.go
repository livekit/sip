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
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"github.com/frostbyte73/core"
	"github.com/pion/rtp"

	"github.com/livekit/protocol/logger"

	"github.com/livekit/sip/pkg/videobridge/codec"
	"github.com/livekit/sip/pkg/videobridge/stats"
)

const (
	rtpMTUSize       = 1500
	readBufferSize   = 2048
	maxSequenceGap   = 500
	videoClockRate   = 90000
)

// MediaHandler receives processed media from the RTP receiver.
type MediaHandler interface {
	// HandleVideoNALs is called with depacketized H.264 NAL units.
	HandleVideoNALs(nals []codec.NALUnit, timestamp uint32) error
	// HandleAudioRTP is called with raw audio RTP packets.
	HandleAudioRTP(pkt *rtp.Packet) error
}

// RTPReceiverConfig configures the RTP receiver.
type RTPReceiverConfig struct {
	// Port range for allocating RTP listen ports
	PortStart int
	PortEnd   int
	// Video payload type from SDP negotiation
	VideoPayloadType uint8
	// Audio payload type from SDP negotiation
	AudioPayloadType uint8
	// Enable jitter buffer
	JitterBuffer bool
	// Jitter buffer target latency
	JitterLatency time.Duration
	// Media timeout
	MediaTimeout        time.Duration
	MediaTimeoutInitial time.Duration
}

// RTPReceiver handles incoming RTP streams for a single SIP session.
// It manages separate video and audio streams, depacketizes H.264,
// and forwards processed media to the handler.
type RTPReceiver struct {
	log    logger.Logger
	config RTPReceiverConfig

	videoConn *net.UDPConn
	audioConn *net.UDPConn

	handler      MediaHandler
	depacketizer *codec.H264Depacketizer

	// Stream state
	videoSSRC atomic.Uint32
	audioSSRC atomic.Uint32
	remoteSrc atomic.Pointer[netip.AddrPort]

	// Sequencing
	lastVideoSeq atomic.Uint32
	lastAudioSeq atomic.Uint32

	// Stats
	videoPackets atomic.Uint64
	audioPackets atomic.Uint64
	videoBytes   atomic.Uint64
	audioBytes   atomic.Uint64

	// Keyframe tracking
	lastKeyframe atomic.Int64

	// Lifecycle
	closed core.Fuse
	mu     sync.Mutex
}

// NewRTPReceiver creates a new RTP receiver listening on allocated UDP ports.
func NewRTPReceiver(log logger.Logger, config RTPReceiverConfig) (*RTPReceiver, error) {
	videoConn, err := listenUDPInRange(config.PortStart, config.PortEnd)
	if err != nil {
		return nil, fmt.Errorf("allocating video RTP port: %w", err)
	}

	audioConn, err := listenUDPInRange(config.PortStart, config.PortEnd)
	if err != nil {
		videoConn.Close()
		return nil, fmt.Errorf("allocating audio RTP port: %w", err)
	}

	r := &RTPReceiver{
		log:          log,
		config:       config,
		videoConn:    videoConn,
		audioConn:    audioConn,
		depacketizer: codec.NewH264Depacketizer(),
	}

	log.Infow("RTP receiver created",
		"videoPort", r.VideoPort(),
		"audioPort", r.AudioPort(),
	)

	return r, nil
}

// VideoPort returns the local UDP port for video RTP.
func (r *RTPReceiver) VideoPort() int {
	return r.videoConn.LocalAddr().(*net.UDPAddr).Port
}

// AudioPort returns the local UDP port for audio RTP.
func (r *RTPReceiver) AudioPort() int {
	return r.audioConn.LocalAddr().(*net.UDPAddr).Port
}

// SetHandler sets the media handler for processed packets.
func (r *RTPReceiver) SetHandler(h MediaHandler) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.handler = h
}

// SetRemoteAddr sets the expected remote address for RTP packets.
func (r *RTPReceiver) SetRemoteAddr(addr netip.AddrPort) {
	r.remoteSrc.Store(&addr)
	r.log.Infow("RTP remote address set", "addr", addr.String())
}

// Start begins reading RTP packets on both video and audio ports.
func (r *RTPReceiver) Start() {
	go r.videoReadLoop()
	go r.audioReadLoop()
	go r.mediaTimeoutLoop()
}

// Close shuts down the receiver and releases ports.
func (r *RTPReceiver) Close() error {
	var errs []error
	r.closed.Once(func() {
		if r.videoConn != nil {
			errs = append(errs, r.videoConn.Close())
		}
		if r.audioConn != nil {
			errs = append(errs, r.audioConn.Close())
		}
		r.log.Infow("RTP receiver closed",
			"videoPackets", r.videoPackets.Load(),
			"audioPackets", r.audioPackets.Load(),
			"videoBytes", r.videoBytes.Load(),
			"audioBytes", r.audioBytes.Load(),
		)
	})
	return errors.Join(errs...)
}

// Closed returns a channel that is closed when the receiver is shut down.
func (r *RTPReceiver) Closed() <-chan struct{} {
	return r.closed.Watch()
}

func (r *RTPReceiver) videoReadLoop() {
	buf := make([]byte, readBufferSize)
	var pkt rtp.Packet

	for !r.closed.IsBroken() {
		_ = r.videoConn.SetReadDeadline(time.Now().Add(1 * time.Second))
		n, _, err := r.videoConn.ReadFromUDP(buf)
		if err != nil {
			if r.closed.IsBroken() {
				return
			}
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				continue
			}
			r.log.Warnw("video RTP read error", err)
			continue
		}

		if err := pkt.Unmarshal(buf[:n]); err != nil {
			r.log.Debugw("video RTP unmarshal error", "error", err, "size", n)
			continue
		}

		// Filter by payload type
		if pkt.PayloadType != r.config.VideoPayloadType {
			continue
		}

		r.videoPackets.Add(1)
		r.videoBytes.Add(uint64(n))
		stats.RTPPacketsReceived.WithLabelValues("video").Inc()

		// Track SSRC
		r.videoSSRC.Store(pkt.SSRC)

		// Depacketize H.264
		nals, err := r.depacketizer.Depacketize(pkt.Payload)
		if err != nil {
			r.log.Debugw("H.264 depacketize error", "error", err, "seq", pkt.SequenceNumber)
			continue
		}

		if len(nals) == 0 {
			continue // incomplete fragment
		}

		// Track keyframes
		for _, nal := range nals {
			if nal.IsKeyframe() {
				now := time.Now().UnixMilli()
				lastKF := r.lastKeyframe.Swap(now)
				if lastKF > 0 {
					interval := float64(now-lastKF) / 1000.0
					stats.KeyframeInterval.Observe(interval)
				}
			}
		}

		r.mu.Lock()
		handler := r.handler
		r.mu.Unlock()

		if handler != nil {
			if err := handler.HandleVideoNALs(nals, pkt.Timestamp); err != nil {
				r.log.Debugw("video handler error", "error", err)
			}
		}

		r.lastVideoSeq.Store(uint32(pkt.SequenceNumber))
	}
}

func (r *RTPReceiver) audioReadLoop() {
	buf := make([]byte, readBufferSize)
	var pkt rtp.Packet

	for !r.closed.IsBroken() {
		_ = r.audioConn.SetReadDeadline(time.Now().Add(1 * time.Second))
		n, _, err := r.audioConn.ReadFromUDP(buf)
		if err != nil {
			if r.closed.IsBroken() {
				return
			}
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				continue
			}
			r.log.Warnw("audio RTP read error", err)
			continue
		}

		if err := pkt.Unmarshal(buf[:n]); err != nil {
			r.log.Debugw("audio RTP unmarshal error", "error", err, "size", n)
			continue
		}

		// Filter by payload type
		if pkt.PayloadType != r.config.AudioPayloadType {
			continue
		}

		r.audioPackets.Add(1)
		r.audioBytes.Add(uint64(n))
		stats.RTPPacketsReceived.WithLabelValues("audio").Inc()

		r.audioSSRC.Store(pkt.SSRC)

		r.mu.Lock()
		handler := r.handler
		r.mu.Unlock()

		if handler != nil {
			pktCopy := pkt.Clone()
			if err := handler.HandleAudioRTP(pktCopy); err != nil {
				r.log.Debugw("audio handler error", "error", err)
			}
		}

		r.lastAudioSeq.Store(uint32(pkt.SequenceNumber))
	}
}

func (r *RTPReceiver) mediaTimeoutLoop() {
	timeout := r.config.MediaTimeoutInitial
	if timeout <= 0 {
		timeout = 30 * time.Second
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	var lastVideoPkts, lastAudioPkts uint64
	initial := true

	for {
		select {
		case <-r.closed.Watch():
			return
		case <-timer.C:
			curVideo := r.videoPackets.Load()
			curAudio := r.audioPackets.Load()

			if curVideo == lastVideoPkts && curAudio == lastAudioPkts {
				if initial {
					r.log.Warnw("no media received within initial timeout", nil,
						"timeout", timeout,
					)
				} else {
					r.log.Warnw("media timeout - no packets received", nil,
						"timeout", r.config.MediaTimeout,
						"lastVideoPackets", lastVideoPkts,
						"lastAudioPackets", lastAudioPkts,
					)
				}
				stats.SessionErrors.WithLabelValues("media_timeout").Inc()
				return
			}

			lastVideoPkts = curVideo
			lastAudioPkts = curAudio

			if initial {
				initial = false
				timeout = r.config.MediaTimeout
				if timeout <= 0 {
					timeout = 15 * time.Second
				}
			}
			timer.Reset(timeout)
		}
	}
}

// Stats returns current receiver statistics.
func (r *RTPReceiver) Stats() ReceiverStats {
	return ReceiverStats{
		VideoPackets: r.videoPackets.Load(),
		AudioPackets: r.audioPackets.Load(),
		VideoBytes:   r.videoBytes.Load(),
		AudioBytes:   r.audioBytes.Load(),
		VideoSSRC:    r.videoSSRC.Load(),
		AudioSSRC:    r.audioSSRC.Load(),
	}
}

// ReceiverStats holds RTP receiver statistics.
type ReceiverStats struct {
	VideoPackets uint64
	AudioPackets uint64
	VideoBytes   uint64
	AudioBytes   uint64
	VideoSSRC    uint32
	AudioSSRC    uint32
}

// listenUDPInRange allocates a UDP port within the specified range.
func listenUDPInRange(portStart, portEnd int) (*net.UDPConn, error) {
	for port := portStart; port <= portEnd; port++ {
		addr := &net.UDPAddr{IP: net.IPv4zero, Port: port}
		conn, err := net.ListenUDP("udp4", addr)
		if err == nil {
			return conn, nil
		}
	}
	return nil, fmt.Errorf("no available UDP port in range %d-%d", portStart, portEnd)
}
