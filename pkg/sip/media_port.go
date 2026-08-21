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
	"bytes"
	"errors"
	"io"
	"math"
	"net"
	"net/netip"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/frostbyte73/core"
	psdp "github.com/pion/sdp/v3"

	msdk "github.com/livekit/media-sdk"
	"github.com/livekit/media-sdk/rtp"
	"github.com/livekit/media-sdk/sdp"
	"github.com/livekit/media-sdk/srtp"
	"github.com/livekit/mediatransportutil/pkg/rtcconfig"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"

	"github.com/livekit/sip/pkg/config"
	"github.com/livekit/sip/pkg/stats"
)

const (
	defaultMediaTimeout        = 15 * time.Second
	defaultMediaTimeoutInitial = 30 * time.Second
	dstChangePrintInterval     = 10 * 1000 * 1000 * 1000 // 10 seconds, in nanoseconds
	srcChangePrintInterval     = dstChangePrintInterval
	holdEnabled                = false // Disabled in current code
)

var ErrRenegotiationDisabled = errors.New("renegotiation is not supported")

type PortStatsSnapshot struct {
	Streams        uint64 `json:"streams"`
	Packets        uint64 `json:"packets"`
	IgnoredPackets uint64 `json:"packets_ignored"`
	InputPackets   uint64 `json:"packets_input"`
	FailedPackets  uint64 `json:"packets_failed"`

	MuxPackets        uint64 `json:"mux_packets"`
	MuxBytes          uint64 `json:"mux_bytes"`
	MuxResets         uint64 `json:"mux_resets"`
	MuxGaps           uint64 `json:"mux_gaps"`
	MuxGapsSum        uint64 `json:"mux_gaps_sum"`
	MuxLate           uint64 `json:"mux_late"`
	MuxLateSum        uint64 `json:"mux_late_sum"`
	MuxRapidPackets   uint64 `json:"mux_rapid_packets"`
	MuxDelayedPackets uint64 `json:"mux_delayed_packets"`
	MuxDelayedSum     uint64 `json:"mux_delayed_sum"`

	AudioPackets uint64 `json:"audio_packets"`
	AudioBytes   uint64 `json:"audio_bytes"`

	AudioInFrames   uint64 `json:"audio_in_frames"`
	AudioInSamples  uint64 `json:"audio_in_samples"`
	AudioOutFrames  uint64 `json:"audio_out_frames"`
	AudioOutSamples uint64 `json:"audio_out_samples"`

	AudioRX float64 `json:"audio_rx"`
	AudioTX float64 `json:"audio_tx"`

	DTMFPackets uint64 `json:"dtmf_packets"`
	DTMFBytes   uint64 `json:"dtmf_bytes"`

	JitterBufferPacketsLost    uint64 `json:"jitter_buffer_packets_lost"`
	JitterBufferPacketsDropped uint64 `json:"jitter_buffer_packets_dropped"`

	LatencyInE2E LatencyStatsSnapshot `json:"latency_in_e2e"`
	LatencyOut   LatencyStatsSnapshot `json:"latency_out"`

	Closed bool `json:"closed"`
}

type PortStats struct {
	Streams        atomic.Uint64
	Packets        atomic.Uint64
	IgnoredPackets atomic.Uint64
	InputPackets   atomic.Uint64
	FailedPackets  atomic.Uint64

	MuxStats rtpCountingStats

	AudioPackets atomic.Uint64
	AudioBytes   atomic.Uint64

	AudioInFrames   atomic.Uint64
	AudioInSamples  atomic.Uint64
	AudioOutFrames  atomic.Uint64
	AudioOutSamples atomic.Uint64

	AudioRX atomic.Uint64 // based on AudioInSamples
	AudioTX atomic.Uint64 // based on AudioOutSamples

	DTMFPackets atomic.Uint64
	DTMFBytes   atomic.Uint64

	JitterBufferPacketsLost    atomic.Uint64
	JitterBufferPacketsDropped atomic.Uint64

	LatencyInE2E LatencyStats
	LatencyOut   LatencyStats

	Closed atomic.Bool

	mu   sync.Mutex
	last struct {
		Time            time.Time
		AudioInSamples  uint64
		AudioOutSamples uint64
	}
}

func (s *PortStats) Load() PortStatsSnapshot {
	return PortStatsSnapshot{
		Streams:                    s.Streams.Load(),
		Packets:                    s.Packets.Load(),
		IgnoredPackets:             s.IgnoredPackets.Load(),
		InputPackets:               s.InputPackets.Load(),
		FailedPackets:              s.FailedPackets.Load(),
		MuxPackets:                 s.MuxStats.packets.Load(),
		MuxBytes:                   s.MuxStats.bytes.Load(),
		MuxResets:                  s.MuxStats.resets.Load(),
		MuxGaps:                    s.MuxStats.gaps.Load(),
		MuxGapsSum:                 s.MuxStats.gapsSum.Load(),
		MuxLate:                    s.MuxStats.late.Load(),
		MuxLateSum:                 s.MuxStats.lateSum.Load(),
		MuxRapidPackets:            s.MuxStats.rapidPackets.Load(),
		MuxDelayedPackets:          s.MuxStats.delayedPackets.Load(),
		MuxDelayedSum:              s.MuxStats.delayedSum.Load(),
		AudioPackets:               s.AudioPackets.Load(),
		AudioBytes:                 s.AudioBytes.Load(),
		AudioInFrames:              s.AudioInFrames.Load(),
		AudioInSamples:             s.AudioInSamples.Load(),
		AudioOutFrames:             s.AudioOutFrames.Load(),
		AudioOutSamples:            s.AudioOutSamples.Load(),
		AudioRX:                    math.Float64frombits(s.AudioRX.Load()),
		AudioTX:                    math.Float64frombits(s.AudioTX.Load()),
		DTMFPackets:                s.DTMFPackets.Load(),
		DTMFBytes:                  s.DTMFBytes.Load(),
		JitterBufferPacketsLost:    s.JitterBufferPacketsLost.Load(),
		JitterBufferPacketsDropped: s.JitterBufferPacketsDropped.Load(),
		LatencyInE2E:               s.LatencyInE2E.Load(),
		LatencyOut:                 s.LatencyOut.Load(),
		Closed:                     s.Closed.Load(),
	}
}

func (s *PortStats) Update() {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := time.Now()
	lastTime := s.last.Time
	if lastTime.IsZero() {
		lastTime = t
	}
	dt := t.Sub(lastTime).Seconds()

	curAudioInSamples := s.AudioInSamples.Load()
	curAudioOutSamples := s.AudioOutSamples.Load()

	if dt > 0 {
		rxSamples := curAudioInSamples - s.last.AudioInSamples
		txSamples := curAudioOutSamples - s.last.AudioOutSamples

		rxRate := float64(rxSamples) / dt
		txRate := float64(txSamples) / dt

		s.AudioRX.Store(math.Float64bits(rxRate))
		s.AudioTX.Store(math.Float64bits(txRate))
	}

	s.last.Time = t
	s.last.AudioInSamples = curAudioInSamples
	s.last.AudioOutSamples = curAudioOutSamples
}

type UDPConn interface {
	net.Conn
	ReadFromUDPAddrPort(b []byte) (n int, addr netip.AddrPort, err error)
	WriteToUDPAddrPort(b []byte, addr netip.AddrPort) (int, error)
}

func newUDPConn(log logger.Logger, conn UDPConn, symmetric bool) *udpConn {
	c := &udpConn{
		UDPConn: conn,
		log:     log,
	}
	c.symmetric.Store(symmetric)
	return c
}

type udpConn struct {
	UDPConn
	closed         atomic.Bool
	discardStop    atomic.Bool
	discardWG      sync.WaitGroup
	log            logger.Logger
	symmetric      atomic.Bool // send packets to the same address we receive them from
	src            atomic.Pointer[netip.AddrPort]
	dst            atomic.Pointer[netip.AddrPort]
	srcChangeCount atomic.Uint64
	dstChangeCount atomic.Uint64
	lastSrcPrint   atomic.Int64
	lastDstPrint   atomic.Int64
}

func (c *udpConn) SetSymmetric(enabled bool) {
	c.symmetric.Store(enabled)
}

func (c *udpConn) GetSrc() (netip.AddrPort, bool) {
	ptr := c.src.Load()
	if ptr == nil {
		return netip.AddrPort{}, false
	}
	addr := *ptr
	return addr, addr.IsValid()
}

func (c *udpConn) SetDst(addr netip.AddrPort) {
	if addr.IsValid() {
		prev := c.dst.Swap(&addr)
		if prev == nil || !prev.IsValid() {
			c.log.Debugw("setting media destination", "addr", addr.String())
		} else if *prev != addr {
			changeCount := c.dstChangeCount.Add(1)
			now := time.Now().UnixNano()
			if now-c.lastDstPrint.Load() > dstChangePrintInterval {
				c.lastDstPrint.Store(now)
				c.log.Infow("changing media destination", "prev", (*prev).String(), "addr", addr.String(), "count", changeCount)
			}
		}
	}
}

func (c *udpConn) Read(b []byte) (n int, err error) {
	if c.closed.Load() {
		return 0, io.EOF
	}
	n, addr, err := c.ReadFromUDPAddrPort(b)
	if c.closed.Load() && errors.Is(err, os.ErrDeadlineExceeded) {
		return n, io.EOF
	}
	prev := c.src.Swap(&addr)
	if prev == nil || !prev.IsValid() {
		c.log.Debugw("setting media source", "addr", addr.String())
	} else if *prev != addr {
		changeCount := c.srcChangeCount.Add(1)
		now := time.Now().UnixNano()
		if now-c.lastSrcPrint.Load() > srcChangePrintInterval {
			c.lastSrcPrint.Store(now)
			c.srcChangeCount.Add(1)
			c.log.Infow("changing media source", "prev", (*prev).String(), "addr", addr.String(), "count", changeCount)
		}
	}
	if c.symmetric.Load() {
		dst := c.dst.Load()
		if dst != nil && dst.Addr().IsUnspecified() {
			// On hold: the peer may keep sending, but it doesn't want our media.
		} else if dst == nil || !dst.IsValid() || *dst != addr {
			c.SetDst(addr)
		}
	}
	return n, err
}

func (c *udpConn) Write(b []byte) (n int, err error) {
	dst := c.dst.Load()
	if dst == nil || dst.Addr().IsUnspecified() { // No remote or on hold
		return len(b), nil // ignore
	}
	return c.WriteToUDPAddrPort(b, *dst)
}

func (c *udpConn) discardLoop() error {
	defer c.discardWG.Done()

	var err error
	buf := make([]byte, 1024)
	packetsDiscarded := uint64(0)
	for !c.discardStop.Load() {
		err = c.UDPConn.SetReadDeadline(time.Now().Add(rtp.DefFrameDur))
		if err != nil {
			if !errors.Is(err, net.ErrClosed) {
				c.log.Warnw("error encountered while setting read deadline", err)
			}
			break
		}
		_, _, err = c.ReadFromUDPAddrPort(buf)
		if err != nil {
			if errors.Is(err, os.ErrDeadlineExceeded) {
				continue
			}
			if errors.Is(err, net.ErrClosed) {
				break
			}
			c.log.Warnw("error encountered while reading UDP packets", err)
			break
		}
		packetsDiscarded++
	}
	if err != nil || packetsDiscarded > 0 {
		c.log.Debugw("Stopped discarding packets", "packetsDiscarded", packetsDiscarded, "error", err)
	}
	err = c.UDPConn.SetReadDeadline(time.Time{}) // clear deadline
	if err != nil && !errors.Is(err, net.ErrClosed) {
		c.log.Warnw("error encountered while clearing read deadline", err)
	}
	return err
}

func (c *udpConn) startDiscarding() {
	c.discardWG.Add(1)
	go c.discardLoop()
}

func (c *udpConn) stopDiscarding() {
	c.discardStop.Store(true)
	c.discardWG.Wait()
}

func (c *udpConn) unwrap() UDPConn {
	c.Close()
	return c.UDPConn
}

func (c *udpConn) Reopen() {
	c.closed.Store(false)
	c.UDPConn.SetReadDeadline(time.Time{}) // Clear deadline, if set
}

func (c *udpConn) Close() error {
	c.stopDiscarding()
	c.closed.Store(true)
	c.UDPConn.SetReadDeadline(time.Now().Add(-time.Second)) // Kill ongoing reads
	return nil
}

type MediaOptions struct {
	IP                   netip.Addr
	Ports                rtcconfig.PortRange
	MediaTimeoutInitial  time.Duration
	MediaTimeout         time.Duration
	SymmetricRTP         bool
	IgnoreLocalAddrInSDP bool // enable symmetric RTP if local IP is specified in SDP
	Stats                *PortStats
	EnableJitterBuffer   bool
	LogSignalChanges     bool
	DrainingIdleTimeout  time.Duration
	DrainingDuration     time.Duration
	Codecs               *msdk.CodecSet
	Encryption           sdp.Encryption
	DTMFAudio            bool
}

func (o *MediaOptions) ApplyDefaults() {
	if o.MediaTimeoutInitial <= 0 {
		o.MediaTimeoutInitial = defaultMediaTimeoutInitial
	}
	if o.MediaTimeout <= 0 {
		o.MediaTimeout = defaultMediaTimeout
	}
	if o.Stats == nil {
		o.Stats = &PortStats{}
	}
	if o.Codecs == nil {
		o.Codecs = defaultCodecs
	}
	if o.Ports.Start == 0 {
		o.Ports.Start = config.DefaultRTPPortRange.Start
	}
	if o.Ports.End == 0 {
		o.Ports.End = config.DefaultRTPPortRange.End
	}
}

// MediaPort is the insulated media-plane API: UDP/RTP to the wire, SDP negotiation,
// and audio/DTMF endpoints. It does not know about calls, rooms, or SIP dialogs.
type MediaPort interface {
	Close()
	CloseWait()

	// GetOutboundAudioWriter returns the LK room -> SIP writer.
	GetOutboundAudioWriter() msdk.PCM16Writer
	// GetOutboundDTMFWriter returns the LK room -> SIP DTMF writer.
	GetOutboundDTMFWriter() msdk.WriteCloser[*livekit.SipDTMF]

	// WriteInboundAudioTo tells port where to write inbound SIP audio.
	WriteInboundAudioTo(w msdk.PCM16Writer) msdk.PCM16Writer
	// WriteInboundDTMFTo tells port where to write inbound SIP DTMF.
	WriteInboundDTMFTo(w msdk.WriteCloser[*livekit.SipDTMF]) msdk.WriteCloser[*livekit.SipDTMF]

	// If there is no offer, this generates an offer.
	// If there is an offer, this simply returns the SDP of that offer.
	// An offer is cleared once a negotiation is successful.
	GenerateOffer() ([]byte, error)

	// GenerateAnswer returns an encoded SDP answer for the given offer.
	// This does not arm the media timeout, use SetTimeout to do so.
	//
	// SIDE EFFECT: May cause a rebuild of the pipeline.
	GenerateAnswer(offer []byte) ([]byte, error)

	// ProcessAnswer processes an encoded SDP answer from the remote client. Returns an
	// error if the answer is invalid, the offer has not yet been generated, or
	// if media has already been negotiated.
	//
	// SIDE EFFECT: May cause a rebuild of the pipeline.
	ProcessAnswer(answer []byte) error

	GetLocalSDP() ([]byte, error)

	// NegotiatedAudio returns the audio configuration chosen by SDP negotiation.
	// Returns nil if media has not been negotiated yet.
	//
	// REQUIRES: The caller should not mutate the returned audio config.
	NegotiatedAudio() *sdp.AudioConfig

	// SetTimeout resets the media timeout with the given values.
	//
	// NOTE: This method is likely to go through additional changes.
	SetTimeout(initial, general time.Duration)

	Received() <-chan struct{}
	MediaTimeout() <-chan struct{}
}

func NewMediaPort(log logger.Logger, mon *stats.CallMonitor, opts *MediaOptions, targetSampleRate int) (MediaPort, error) {
	return NewMediaPortWith(log, mon, nil, opts, targetSampleRate)
}

func NewMediaPortWith(log logger.Logger, mon *stats.CallMonitor, conn UDPConn, opts *MediaOptions, targetSampleRate int) (MediaPort, error) {
	if opts == nil {
		opts = &MediaOptions{}
	}
	opts.ApplyDefaults()
	if conn == nil {
		// use an even RTP port (RFC 3550); some gateways misroute media when offered an odd one
		c, err := rtp.ListenUDPEvenPortRange(opts.Ports.Start, opts.Ports.End, netip.AddrFrom4([4]byte{0, 0, 0, 0}))
		if err != nil {
			return nil, err
		}
		conn = c
	}
	var localCrypto []srtp.Profile
	if opts.Encryption != sdp.EncryptionNone {
		var err error
		localCrypto, err = srtp.DefaultProfiles()
		if err != nil {
			return nil, err
		}
	}
	p := &mediaPort{
		log:         log,
		opts:        opts,
		mon:         mon,
		externalIP:  opts.IP,
		timeoutKick: make(chan struct{}, 1),
		port:        newUDPConn(log, conn, opts.SymmetricRTP),
		dtmfIn:      msdk.WriteCloserSwitch[*livekit.SipDTMF]{},
		dtmfOut:     msdk.WriteCloserSwitch[*livekit.SipDTMF]{},
		stats:       opts.Stats,
		codecs:      opts.Codecs,
		encryption:  opts.Encryption,
		localCrypto: localCrypto,
	}
	// Explicitly set sample rate. We manually create resamplers to include in latency
	p.audioOut = msdk.NewWriteCloserSwitch[msdk.PCM16Sample](targetSampleRate)
	p.audioIn = msdk.NewWriteCloserSwitch[msdk.PCM16Sample](targetSampleRate)

	p.port.startDiscarding()
	p.timeoutInitial.Store(&opts.MediaTimeoutInitial)
	p.timeoutGeneral.Store(&opts.MediaTimeout)
	p.wg.Go(p.mediaTimeoutLoop)
	p.log.Debugw("listening for media on UDP", "port", p.Port())
	return p, nil
}

// mediaPort is the concrete MediaPort implementation.
type mediaPort struct {
	log            logger.Logger
	wg             sync.WaitGroup
	opts           *MediaOptions
	mon            *stats.CallMonitor
	externalIP     netip.Addr
	port           *udpConn
	mediaReceived  core.Fuse
	packetCount    atomic.Uint64
	lastPacketTime atomic.Int64 // UnixNano of last RTP packet, 0 if none
	mediaTimeout   core.Fuse
	timeoutKick    chan struct{} // wakes timeoutLoop when the deadline may have changed
	timeoutStart   atomic.Pointer[time.Time]
	timeoutInitial atomic.Pointer[time.Duration]
	timeoutGeneral atomic.Pointer[time.Duration]
	closed         core.Fuse
	stats          *PortStats

	targetSampleRate int
	codecs           *msdk.CodecSet
	encryption       sdp.Encryption
	localCrypto      []srtp.Profile // our SRTP material, generated once per port

	mu         sync.RWMutex
	pipeline   *mediaPortPipeline
	localSDP   []byte
	offer      *sdp.Offer
	negotiated *sdp.MediaConfig

	audioIn  *msdk.WriteCloserSwitch[msdk.PCM16Sample] // SIP RTP -> LK PCM
	audioOut *msdk.WriteCloserSwitch[msdk.PCM16Sample] // LK PCM -> SIP RTP
	dtmfIn   msdk.WriteCloserSwitch[*livekit.SipDTMF]  // SIP DTMF -> LK DTMF
	dtmfOut  msdk.WriteCloserSwitch[*livekit.SipDTMF]  // LK DTMF -> SIP DTMF
}

func (p *mediaPort) SetTimeout(initial, general time.Duration) {
	if initial <= 0 || general <= 0 {
		p.log.Debugw("attempting to set zero media timeout", "initial", initial, "timeout", general, "fallbackInitial", p.opts.MediaTimeoutInitial, "fallbackTimeout", p.opts.MediaTimeout)
		if initial <= 0 {
			initial = p.opts.MediaTimeoutInitial
		}
		if general <= 0 {
			general = p.opts.MediaTimeout
		}
	}
	p.timeoutInitial.Store(&initial)
	p.timeoutGeneral.Store(&general)
	now := time.Now()
	p.timeoutStart.Store(&now)
	p.log.Debugw("media timeout enabled",
		"packets", p.packetCount.Load(),
		"initial", initial,
		"timeout", general,
	)
	select {
	case p.timeoutKick <- struct{}{}:
	default: // already pending
	}
}

func (p *mediaPort) mediaTimeoutLoop() {
	defer p.log.Infow("media timeout loop stopped")

	const disabledPark = time.Hour
	timer := time.NewTimer(disabledPark)
	defer timer.Stop()

	lastLog := time.Now()
	for {
		select {
		case <-p.closed.Watch():
			return
		case <-p.timeoutKick:
		case <-timer.C:
		}

		verbose := false
		if now := time.Now(); now.Sub(lastLog) > time.Hour {
			verbose = true
			lastLog = now
		}

		startPtr := p.timeoutStart.Load()
		if startPtr == nil {
			if verbose {
				p.log.Infow("media timeout disabled", "packets", p.packetCount.Load())
			}
			timer.Reset(disabledPark)
			continue
		}
		startTime := *startPtr

		var lastPacketTime time.Time
		if nano := p.lastPacketTime.Load(); nano > 0 {
			lastPacketTime = time.Unix(0, nano)
		}

		generalTimeout := p.opts.MediaTimeout
		if ptr := p.timeoutGeneral.Load(); ptr != nil {
			generalTimeout = *ptr
		}

		// Initial mode: no media has ever been received on this port. Once a single
		// RTP packet arrives, we switch to the general window regardless of any
		// subsequent SetTimeout re-arming the startTime.
		isInitial := lastPacketTime.IsZero()
		var (
			deadline time.Time
			timeout  time.Duration
		)
		if isInitial {
			timeout = p.opts.MediaTimeoutInitial
			if ptr := p.timeoutInitial.Load(); ptr != nil {
				timeout = *ptr
			}
			deadline = startTime.Add(timeout)
		} else {
			timeout = generalTimeout
			deadline = lastPacketTime.Add(timeout)
		}
		remaining := time.Until(deadline)

		var sinceLast time.Duration
		if !lastPacketTime.IsZero() {
			sinceLast = time.Since(lastPacketTime)
		}

		if verbose {
			log := p.log.WithValues(
				"packets", p.packetCount.Load(),
				"sinceStart", time.Since(startTime),
				"sinceLast", sinceLast,
				"remaining", remaining,
				"timeout", timeout,
				"isInitial", isInitial,
			)
			if isInitial {
				log.Warnw("media timeout is idle for a long time", nil)
			} else {
				log.Infow("media timeout stats")
			}
		}

		if remaining <= 0 {
			p.log.Infow("triggering media timeout",
				"packets", p.packetCount.Load(),
				"sinceStart", time.Since(startTime),
				"sinceLast", sinceLast,
				"timeout", timeout,
				"isInitial", isInitial,
			)
			p.mediaTimeout.Break()
			return
		}
		// Cap the wake-up at the general timeout so packet arrivals during a long
		// initial window get observed within one general interval, instead of
		// having to wait out the full initial deadline.
		timer.Reset(min(remaining, generalTimeout))
	}
}

func (p *mediaPort) closePipelineLocked() {
	// Lock must already be held

	// Close switch -> port
	if closer := p.audioOut.Swap(nil); closer != nil {
		_ = closer.Close()
	}
	if closer := p.dtmfOut.Swap(nil); closer != nil {
		_ = closer.Close()
	}
	// Close port -> switch
	if p.pipeline != nil {
		_ = p.pipeline.Close() // Waits until session terminates
		p.pipeline = nil
	}
}

func (p *mediaPort) Close() {
	p.closed.Once(func() {
		defer p.stats.Closed.Store(true)

		p.mu.Lock()
		defer p.mu.Unlock()
		p.closePipelineLocked()
		p.port.Close()
		conn := p.port.unwrap()
		if uc, ok := conn.(*net.UDPConn); ok {
			go DrainPort(p.log, uc, p.opts.DrainingIdleTimeout, p.opts.DrainingDuration, nil)
		} else {
			_ = conn.Close()
		}
		p.audioIn.Close()  // Propagate Close() to onwards to room
		p.dtmfIn.Close()   // Propagate Close() to onwards to room
		p.audioOut.Close() // No-op, but do anyway
		p.dtmfOut.Close()  // No-op, but do anyway
	})
}

func (p *mediaPort) CloseWait() {
	p.Close()
	<-p.closed.Watch()
	p.wg.Wait()
}

func (p *mediaPort) Port() int {
	return p.port.LocalAddr().(*net.UDPAddr).Port
}

func (p *mediaPort) RemoteAddr() netip.AddrPort {
	dst := p.port.dst.Load()
	if dst == nil {
		return netip.AddrPort{}
	}
	return *dst
}

// Reported for inbound (SetOffer) only since outbound (SetAnswer) only contains the
// codec picked by the end user, and not what they actually support
func (p *mediaPort) reportPeerCodecs(d sdp.MediaDesc, reinvite bool) {
	if p.mon == nil {
		return
	}
	p.mon.PeerSDP(peerCodecNames(d), reinvite)
}

// Plumbing

func (p *mediaPort) GetOutboundAudioWriter() msdk.PCM16Writer {
	return p.audioOut
}

// WriteInboundAudioTo sets audio writer that will receive decoded PCM from incoming RTP packets.
func (p *mediaPort) WriteInboundAudioTo(w msdk.PCM16Writer) msdk.PCM16Writer {
	return p.audioIn.Swap(w)
}

func (p *mediaPort) GetOutboundDTMFWriter() msdk.WriteCloser[*livekit.SipDTMF] {
	return &p.dtmfOut
}

func (p *mediaPort) WriteInboundDTMFTo(w msdk.WriteCloser[*livekit.SipDTMF]) msdk.WriteCloser[*livekit.SipDTMF] {
	return p.dtmfIn.Swap(w)
}

func (p *mediaPort) Received() <-chan struct{} {
	return p.mediaReceived.Watch()
}

func (p *mediaPort) MediaTimeout() <-chan struct{} {
	return p.mediaTimeout.Watch()
}

// SDP

func (p *mediaPort) GenerateOffer() ([]byte, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.offer != nil {
		return p.offer.SDP.Marshal()
	}

	offer, err := sdp.NewOfferWith(p.codecs, p.externalIP, p.Port(), p.encryption, sdp.WithLocalProfiles(p.localCrypto))
	if err != nil {
		return nil, err
	}
	p.offer = offer
	return offer.SDP.Marshal()
}

func (p *mediaPort) GenerateAnswer(offerData []byte) ([]byte, error) {
	if len(offerData) == 0 {
		return p.GetLocalSDP()
	}

	offer, err := sdp.ParseOfferWith(p.codecs, offerData)
	if err != nil {
		return nil, SDPError{Err: err}
	}
	p.mu.RLock()
	isReinvite := p.offer != nil
	p.mu.RUnlock()
	p.reportPeerCodecs(offer.MediaDesc, isReinvite)
	answer, mc, err := offer.Answer(p.externalIP, p.Port(), p.encryption, sdp.WithLocalProfiles(p.localCrypto))
	if err != nil {
		return nil, SDPError{Err: err}
	}

	answerData, err := answer.SDP.Marshal()
	if err != nil {
		return nil, err
	}
	err = p.configure(mc, answerData)
	if err != nil {
		return nil, err
	}
	return answerData, nil
}

func (p *mediaPort) ProcessAnswer(answerData []byte) error {
	if len(answerData) == 0 {
		return errors.New("no answer provided")
	}

	p.mu.RLock()
	offer := p.offer
	p.mu.RUnlock()

	if offer == nil {
		return errors.New("no offer generated")
	}

	answer, err := sdp.ParseAnswerWith(p.codecs, answerData)
	if err != nil {
		return SDPError{Err: err}
	}
	mc, localSDP, err := answer.ApplyWithLocal(offer, p.encryption)
	if err != nil {
		return SDPError{Err: err}
	}

	localSDPBytes, err := localSDP.Marshal()
	if err != nil {
		return err
	}

	err = p.configure(mc, localSDPBytes)
	if err != nil {
		return err
	}
	p.SetTimeout(p.opts.MediaTimeoutInitial, p.opts.MediaTimeout)
	return nil
}

func (p *mediaPort) GetLocalSDP() ([]byte, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.pipeline == nil || len(p.localSDP) == 0 {
		return nil, errors.New("no SDP provided, no local SDP available")
	}
	return p.localSDP, nil
}

func (p *mediaPort) NegotiatedAudio() *sdp.AudioConfig {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.negotiated == nil {
		return nil
	}
	return &p.negotiated.Audio
}

// Building pipeline

func (p *mediaPort) configure(c *sdp.MediaConfig, localSDP []byte) error {
	// Map the durable udpConn + WriteCloserSwitch anchors onto a fresh mediaPortPipeline.
	// Rebuild from scratch under mu: closePipelineLocked (soft-closes the session via udpConn),
	// Reopen the port, then Configure a new generation and Swap TX leaves into the anchors.

	if c.Audio.Codec == nil {
		return SDPError{Err: errors.New("no audio codec selected")}
	}

	p.mu.Lock() // No concurrent rebuilding of the pipeline
	defer p.mu.Unlock()

	p.offer = nil

	if p.closed.IsBroken() {
		return errors.New("media is already closed")
	}

	changeSetSummary := NewChangeSetSummary(p.negotiated, c)

	if changeSetSummary.includes(changeSetLocalAddr) {
		return errors.New("unexpected local address change")
	}

	audioToPort := p.audioOut.Swap(nil) // either nil or no-op closer
	defer func() { p.audioOut.Swap(audioToPort) }()
	dtmfToPort := p.dtmfOut.Swap(nil) // either nil or no-op closer
	defer func() { p.dtmfOut.Swap(dtmfToPort) }()

	hold := false

	if changeSetSummary.includes(changeSetRemoteAddr) {
		if c.Remote.Addr().IsUnspecified() {
			// Older hold semantics: c=0.0.0.0
			hold = true
		} else {
			p.port.SetDst(netip.AddrPortFrom(c.Remote.Addr(), c.Remote.Port()))
			p.negotiated.Remote = c.Remote
		}
	}
	if changeSetSummary.includes(changeSetPeerDirection) {
		// Newer hold semantics: a=sendonly
		// TODO: Support a=recvonly/inactive; requires toggling media timeout;
		//		maybe gate these on timers being active on the session to prevent dud calls
		hold = c.PeerDirection == psdp.DirectionSendOnly
	}
	if holdEnabled && hold {
		audioToPort = nil
		dtmfToPort = nil
		zero := netip.IPv4Unspecified()
		if !c.Remote.Addr().Is4() {
			zero = netip.IPv6Unspecified()
		}
		p.port.SetDst(netip.AddrPortFrom(zero, c.Remote.Port()))
		p.log.Infow("peer requested hold", "direction", c.PeerDirection.String(), "remote", c.Remote.String())
	}
	if changeSetSummary.shouldReconfigure() {
		if changeSetSummary != changeSetNew {
			// Explicitly disable renegotiation for now
			// Compatibility to todays behavior: return 200 OK, but don't reconfigure the pipeline
			return nil
		}

		p.closePipelineLocked()
		audioToPort = nil
		dtmfToPort = nil
		p.port.stopDiscarding() // Needs readDeadline. Must be ahead of Reopen() and NewMediaPortPipeline()
		p.port.Reopen()         // Allow reads from socket again

		pipelineConfig := &MediaPortPipelineConfig{
			log:       p.log,
			opts:      p.opts,
			mon:       p.mon,
			stats:     p.stats,
			onNewSSRC: p.mediaReceived.Break,
			onPacket:  p.onNewMediaPacket,
		}
		newPipeline, err := NewMediaPortPipeline(
			pipelineConfig,
			c,
			p.port,
			p.audioIn,
			&p.dtmfIn,
			p.audioOut.SampleRate(),
		)
		if err != nil {
			return err
		}

		audioToPort, dtmfToPort = newPipeline.GetConnectors() // These are not propagating Close()
		p.pipeline = newPipeline

		p.localSDP = localSDP // TODO: Move to end of function when reconfiguring is supported
	}
	p.negotiated = c
	return nil
}

func (p *mediaPort) onNewMediaPacket() {
	p.packetCount.Add(1)
	p.lastPacketTime.Store(time.Now().UnixNano())
}

type changeSetSummary uint

const (
	changeSetNew changeSetSummary = 1 << iota // 1 << 0 = 1
	changeSetAudioCodec
	changeSetDTMF
	changeSetCrypto
	changeSetLocalAddr
	changeSetRemoteAddr
	changeSetPeerDirection
)

func NewChangeSetSummary(current, new *sdp.MediaConfig) changeSetSummary {
	if current == nil {
		return changeSetNew
	}
	var changeSetSummary changeSetSummary
	if current.Audio.Codec.Info().SDPName != new.Audio.Codec.Info().SDPName || current.Audio.Type != new.Audio.Type {
		changeSetSummary |= changeSetAudioCodec
	}
	if current.Audio.DTMFType != new.Audio.DTMFType {
		changeSetSummary |= changeSetDTMF
	}
	a, b := current.Crypto, new.Crypto
	if a == nil || b == nil {
		if a != b {
			changeSetSummary |= changeSetCrypto
		}
	} else { // Profile exists on both
		if a.Profile != b.Profile ||
			!bytes.Equal(a.Keys.LocalMasterKey, b.Keys.LocalMasterKey) ||
			!bytes.Equal(a.Keys.LocalMasterSalt, b.Keys.LocalMasterSalt) ||
			!bytes.Equal(a.Keys.RemoteMasterKey, b.Keys.RemoteMasterKey) ||
			!bytes.Equal(a.Keys.RemoteMasterSalt, b.Keys.RemoteMasterSalt) {
			changeSetSummary |= changeSetCrypto
		}
	}
	if current.Local != new.Local {
		changeSetSummary |= changeSetLocalAddr
	}
	if current.Remote != new.Remote {
		changeSetSummary |= changeSetRemoteAddr
	}
	if current.PeerDirection != new.PeerDirection {
		changeSetSummary |= changeSetPeerDirection
	}
	return changeSetSummary
}

func (c changeSetSummary) shouldReconfigure() bool {
	return c&(changeSetNew|changeSetAudioCodec|changeSetDTMF|changeSetCrypto) != 0
}

func (c changeSetSummary) includes(feature changeSetSummary) bool {
	return c&feature != 0
}
