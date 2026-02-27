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
	"context"
	"errors"
	"io"
	"math"
	"net"
	"net/netip"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/frostbyte73/core"

	msdk "github.com/livekit/media-sdk"
	"github.com/livekit/media-sdk/dtmf"
	"github.com/livekit/media-sdk/jitter"
	"github.com/livekit/media-sdk/mixer"
	"github.com/livekit/media-sdk/rtp"
	"github.com/livekit/media-sdk/sdp"
	"github.com/livekit/media-sdk/srtp"
	"github.com/livekit/mediatransportutil/pkg/rtcconfig"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/utils/traceid"

	"github.com/livekit/sip/pkg/stats"
)

const (
	defaultMediaTimeout        = 15 * time.Second
	defaultMediaTimeoutInitial = 30 * time.Second
)

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

func newUDPConn(log logger.Logger, conn UDPConn) *udpConn {
	return &udpConn{UDPConn: conn, log: log, stopped: make(chan struct{})}
}

type udpConn struct {
	UDPConn
	stopping core.Fuse
	stopped  chan struct{}
	log      logger.Logger
	src      atomic.Pointer[netip.AddrPort]
	dst      atomic.Pointer[netip.AddrPort]
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
			c.log.Infow("setting media destination", "addr", addr.String())
		} else if *prev != addr {
			c.log.Infow("changing media destination", "addr", addr.String())
		}
	}
}

func (c *udpConn) Read(b []byte) (n int, err error) {
	n, addr, err := c.ReadFromUDPAddrPort(b)
	prev := c.src.Swap(&addr)
	if prev == nil || !prev.IsValid() {
		c.log.Infow("setting media source", "addr", addr.String())
	} else if *prev != addr {
		c.log.Infow("changing media source", "addr", addr.String())
	}
	return n, err
}

func (c *udpConn) Write(b []byte) (n int, err error) {
	dst := c.dst.Load()
	if dst == nil {
		return len(b), nil // ignore
	}
	return c.WriteToUDPAddrPort(b, *dst)
}

func (c *udpConn) discardLoop() error {
	defer close(c.stopped)

	var err error
	buf := make([]byte, 1024)
	packetsDiscarded := uint64(0)
	for !c.stopping.IsBroken() {
		err = c.UDPConn.SetReadDeadline(time.Now().Add(rtp.DefFrameDur))
		if err != nil {
			c.log.Warnw("error encountered while setting read deadline", err)
			break
		}
		_, _, err = c.ReadFromUDPAddrPort(buf)
		if errors.Is(err, os.ErrDeadlineExceeded) {
			continue
		}
		if err != nil {
			c.log.Warnw("error encountered while reading UDP packets", err)
			break
		}
		packetsDiscarded++
	}
	if err != nil || packetsDiscarded > 0 {
		c.log.Infow("Stopped discarding packets", "packetsDiscarded", packetsDiscarded, "error", err)
	}
	err = c.UDPConn.SetReadDeadline(time.Time{}) // clear deadline
	if err != nil {
		c.log.Warnw("error encountered while clearing read deadline", err)
	}
	return err
}

func (c *udpConn) stopDiscarding() {
	c.stopping.Break()
	<-c.stopped
}

type MediaConf struct {
	sdp.MediaConfig
	Processor msdk.PCM16Processor
}

type MediaOptions struct {
	IP                  netip.Addr
	Ports               rtcconfig.PortRange
	MediaTimeoutInitial time.Duration
	MediaTimeout        time.Duration
	Stats               *PortStats
	EnableJitterBuffer  bool
	NoInputResample     bool
	IgnorePreanswerData bool
	LogSignalChanges    bool
}

func NewMediaPort(tid traceid.ID, log logger.Logger, mon *stats.CallMonitor, opts *MediaOptions, sampleRate int) (*MediaPort, error) {
	return NewMediaPortWith(tid, log, mon, nil, opts, sampleRate)
}

func NewMediaPortWith(tid traceid.ID, log logger.Logger, mon *stats.CallMonitor, conn UDPConn, opts *MediaOptions, sampleRate int) (*MediaPort, error) {
	if opts == nil {
		opts = &MediaOptions{}
	}
	if opts.MediaTimeoutInitial <= 0 {
		opts.MediaTimeoutInitial = defaultMediaTimeoutInitial
	}
	if opts.MediaTimeout <= 0 {
		opts.MediaTimeout = defaultMediaTimeout
	}
	if opts.Stats == nil {
		opts.Stats = &PortStats{}
	}
	if conn == nil {
		c, err := rtp.ListenUDPPortRange(opts.Ports.Start, opts.Ports.End, netip.AddrFrom4([4]byte{0, 0, 0, 0}))
		if err != nil {
			return nil, err
		}
		conn = c
	}
	mediaTimeout := make(chan struct{})
	inSampleRate := sampleRate
	if opts.NoInputResample {
		inSampleRate = -1 // set only after SDP is accepted
	}
	p := &MediaPort{
		tid:              tid,
		log:              log,
		opts:             opts,
		mon:              mon,
		externalIP:       opts.IP,
		mediaTimeout:     mediaTimeout,
		timeoutResetTick: make(chan time.Duration, 1),
		jitterEnabled:    opts.EnableJitterBuffer,
		logSignalChanges: opts.LogSignalChanges,
		port:             newUDPConn(log, conn),
		audioOut:         msdk.NewSwitchWriter(sampleRate),
		audioIn:          msdk.NewSwitchWriter(inSampleRate),
		stats:            opts.Stats,
	}
	if p.opts.IgnorePreanswerData {
		go p.port.discardLoop()
	}
	p.timeoutInitial.Store(&opts.MediaTimeoutInitial)
	p.timeoutGeneral.Store(&opts.MediaTimeout)
	go p.timeoutLoop(tid, func() {
		close(mediaTimeout)
	})
	p.log.Debugw("listening for media on UDP", "port", p.Port())
	return p, nil
}

// MediaPort combines all functionality related to sending and accepting SIP media.
type MediaPort struct {
	tid              traceid.ID
	log              logger.Logger
	opts             *MediaOptions
	mon              *stats.CallMonitor
	externalIP       netip.Addr
	port             *udpConn
	mediaReceived    core.Fuse
	packetCount      atomic.Uint64
	mediaTimeout     <-chan struct{}
	timeoutStart     atomic.Pointer[time.Time]
	timeoutResetTick chan time.Duration
	timeoutInitial   atomic.Pointer[time.Duration]
	timeoutGeneral   atomic.Pointer[time.Duration]
	closed           core.Fuse
	stats            *PortStats
	dtmfAudioEnabled bool
	jitterEnabled    bool
	logSignalChanges bool

	mu           sync.Mutex
	conf         *MediaConf
	sess         rtp.Session
	hnd          atomic.Pointer[rtp.HandlerCloser]
	dtmfOutRTP   *rtp.Stream
	dtmfOutAudio msdk.PCM16Writer

	audioOutRTP    *rtp.Stream
	audioOut       *msdk.SwitchWriter // LK PCM -> SIP RTP
	audioIn        *msdk.SwitchWriter // SIP RTP -> LK PCM
	audioInHandler rtp.Handler        // for debug only
	dtmfIn         atomic.Pointer[func(ev dtmf.Event)]
}

func (p *MediaPort) DisableOut() {
	p.audioOut.Disable()
}

func (p *MediaPort) EnableOut() {
	p.audioOut.Enable()
}

func (p *MediaPort) disableTimeout() {
	p.log.Debugw("media timeout disabled")
	p.timeoutStart.Store(nil)
}

func (p *MediaPort) enableTimeout(initial, general time.Duration) {
	if initial <= 0 || general <= 0 {
		p.log.Warnw("attempting to set zero media timeout", nil, "initial", initial, "timeout", general)
		if initial <= 0 {
			initial = defaultMediaTimeoutInitial
		}
		if general <= 0 {
			general = defaultMediaTimeout
		}
	}
	p.timeoutInitial.Store(&initial)
	p.timeoutGeneral.Store(&general)
	select {
	case p.timeoutResetTick <- general:
	default:
	}
	now := time.Now()
	p.timeoutStart.Store(&now)
	p.log.Debugw("media timeout enabled",
		"packets", p.packetCount.Load(),
		"initial", initial,
		"timeout", general,
	)
}

func (p *MediaPort) EnableTimeout(enabled bool) {
	if !enabled {
		p.disableTimeout()
		return
	}
	p.enableTimeout(p.opts.MediaTimeoutInitial, p.opts.MediaTimeout)
}

func (p *MediaPort) SetTimeout(initial, general time.Duration) {
	p.enableTimeout(initial, general)
}

func (p *MediaPort) timeoutLoop(tid traceid.ID, timeoutCallback func()) {
	defer p.log.Infow("media timeout loop stopped")
	ticker := time.NewTicker(p.opts.MediaTimeout)
	defer ticker.Stop()

	var (
		lastPackets  uint64
		startPackets uint64
		lastTime     time.Time
		lastLog      = time.Now()
	)
	for {
		select {
		case <-p.closed.Watch():
			return
		case tick := <-p.timeoutResetTick:
			ticker.Reset(tick)
			startPackets = p.packetCount.Load()
			lastTime = time.Now()
			lastLog = lastTime
			p.log.Infow("media timeout reset", "packets", startPackets, "tick", tick)
		case <-ticker.C:
			log := p.log
			curPackets := p.packetCount.Load()
			startPtr := p.timeoutStart.Load()
			var startTime time.Time
			if startPtr != nil {
				startTime = *startPtr
			}
			verbose := false
			if now := time.Now(); now.Sub(lastLog) > time.Hour {
				verbose = true
				lastLog = now
				log = log.WithValues(
					"startPackets", startPackets,
					"packets", curPackets,
					"lastPackets", lastPackets,
					"sinceLast", time.Since(lastTime),
					"sinceStart", time.Since(startTime),
				)
				if curPackets == startPackets {
					log.Warnw("media timout is idle for a long time", nil)
				} else {
					log.Infow("media timeout stats")
				}
			}
			if curPackets != lastPackets {
				lastPackets = curPackets
				lastTime = time.Now()
				if verbose {
					log.Infow("got a new packet")
				}
				continue // wait for the next tick
			}
			if startPtr == nil {
				if verbose {
					log.Infow("timeout is disabled")
				}
				continue // timeout disabled
			}
			isInitial := lastPackets == startPackets
			sinceStart := time.Since(*startPtr)
			sinceLast := time.Since(lastTime)
			var (
				since   time.Duration
				timeout time.Duration
			)
			// First timeout could be different. Usually it's longer to allow for a call setup.
			// In some cases it could be shorter (e.g. when we notice an issue with signaling and suspect media will fail).
			if isInitial {
				since = sinceStart
				timeout = p.opts.MediaTimeoutInitial
				if ptr := p.timeoutInitial.Load(); ptr != nil {
					timeout = *ptr
				}
			} else {
				since = sinceLast
				timeout = p.opts.MediaTimeout
				if ptr := p.timeoutGeneral.Load(); ptr != nil {
					timeout = *ptr
				}
			}

			// Ticker is allowed to fire earlier than the full timeout interval. Skip if it's not a full timeout yet.
			if since+timeout/10 < timeout {
				if verbose {
					log.Infow("too early to trigger", "since", since, "timeout", timeout)
				}
				continue
			}
			p.log.Infow("triggering media timeout",
				"packets", lastPackets,
				"startPackets", startPackets,
				"sinceStart", sinceStart,
				"sinceLast", sinceLast,
				"timeout", timeout,
				"isInitial", isInitial,
			)
			timeoutCallback()
			return
		}
	}
}

func (p *MediaPort) Close() {
	p.closed.Once(func() {
		defer p.stats.Closed.Store(true)

		p.mu.Lock()
		defer p.mu.Unlock()
		if w := p.audioOut.Swap(nil); w != nil {
			_ = w.Close()
		}
		if w := p.audioIn.Swap(nil); w != nil {
			_ = w.Close()
		}
		p.audioOutRTP = nil
		p.audioInHandler = nil
		p.dtmfOutRTP = nil
		if p.dtmfOutAudio != nil {
			p.dtmfOutAudio.Close()
			p.dtmfOutAudio = nil
		}
		p.dtmfIn.Store(nil)
		if p.sess != nil {
			_ = p.sess.Close()
		}
		_ = p.port.Close()

		hnd := p.hnd.Load()
		if hnd != nil {
			(*hnd).Close()
		}
	})
}

func (p *MediaPort) Port() int {
	return p.port.LocalAddr().(*net.UDPAddr).Port
}

func (p *MediaPort) Received() <-chan struct{} {
	return p.mediaReceived.Watch()
}

func (p *MediaPort) Timeout() <-chan struct{} {
	return p.mediaTimeout
}

func (p *MediaPort) Config() *MediaConf {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.conf
}

// WriteAudioTo sets audio writer that will receive decoded PCM from incoming RTP packets.
func (p *MediaPort) WriteAudioTo(w msdk.PCM16Writer) {
	if processor := p.conf.Processor; processor != nil {
		w = processor(w)
	}
	if pw := p.audioIn.Swap(w); pw != nil {
		_ = pw.Close()
	}
}

// GetAudioWriter returns audio writer that will send PCM to the destination via RTP.
func (p *MediaPort) GetAudioWriter() msdk.PCM16Writer {
	return p.audioOut
}

// NewOffer generates an SDP offer for the media.
func (p *MediaPort) NewOffer(encrypted sdp.Encryption) (*sdp.Offer, error) {
	return sdp.NewOffer(p.externalIP, p.Port(), encrypted)
}

// SetAnswer decodes and applies SDP answer for offer from NewOffer. SetConfig must be called with the decoded configuration.
func (p *MediaPort) SetAnswer(offer *sdp.Offer, answerData []byte, enc sdp.Encryption) (*MediaConf, error) {
	answer, err := sdp.ParseAnswer(answerData)
	if err != nil {
		return nil, err
	}
	mc, err := answer.Apply(offer, enc)
	if err != nil {
		return nil, SDPError{Err: err}
	}
	return &MediaConf{MediaConfig: *mc}, nil
}

// SetOffer decodes the offer from another party and returns encoded answer. To accept the offer, call SetConfig.
func (p *MediaPort) SetOffer(offerData []byte, enc sdp.Encryption) (*sdp.Answer, *MediaConf, error) {
	offer, err := sdp.ParseOffer(offerData)
	if err != nil {
		return nil, nil, err
	}
	answer, mc, err := offer.Answer(p.externalIP, p.Port(), enc)
	if err != nil {
		return nil, nil, err
	}
	return answer, &MediaConf{MediaConfig: *mc}, nil
}

func (p *MediaPort) SetConfig(c *MediaConf) error {
	if p.closed.IsBroken() {
		return errors.New("media is already closed")
	}
	var crypto string
	if c.Crypto != nil {
		crypto = c.Crypto.Profile.String()
	}
	p.log.Infow("using codecs",
		"audio-codec", c.Audio.Codec.Info().SDPName, "audio-rtp", c.Audio.Type,
		"dtmf-rtp", c.Audio.DTMFType,
		"srtp", crypto,
	)

	p.port.SetDst(c.Remote)
	var (
		sess rtp.Session
		err  error
	)
	if c.Crypto != nil {
		sess, err = srtp.NewSession(p.log, p.port, c.Crypto)
	} else {
		sess = rtp.NewSession(p.log, p.port)
	}
	if err != nil {
		return err
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	p.port.SetDst(c.Remote)
	p.conf = c
	p.sess = sess

	if err = p.setupOutput(p.tid); err != nil {
		return err
	}
	p.setupInput()
	return nil
}

func (p *MediaPort) rtpLoop(tid traceid.ID, sess rtp.Session) {
	// Need a loop to process all incoming packets.
	for {
		r, ssrc, err := sess.AcceptStream()
		if err != nil {
			if !errors.Is(err, net.ErrClosed) && !strings.Contains(err.Error(), "closed") {
				p.log.Errorw("cannot accept RTP stream", err)
			}
			return
		}
		p.stats.Streams.Add(1)
		p.mediaReceived.Break()
		log := p.log.WithValues("ssrc", ssrc)
		log.Infow("accepting RTP stream")
		go p.rtpReadLoop(tid, log, r)
	}
}

func (p *MediaPort) rtpReadLoop(tid traceid.ID, log logger.Logger, r rtp.ReadStream) {
	const maxErrors = 50 // 1 sec, given 20 ms frames
	buf := make([]byte, rtp.MTUSize+1)
	overflow := false
	var (
		h        rtp.Header
		pipeline string
		errorCnt int
	)
	for {
		h = rtp.Header{}
		n, err := r.ReadRTP(&h, buf)
		if err == io.EOF {
			return
		} else if err != nil {
			log.Errorw("read RTP failed", err)
			return
		}
		p.packetCount.Add(1)
		p.stats.Packets.Add(1)
		if n > rtp.MTUSize {
			if !overflow {
				overflow = true
				log.Errorw("RTP packet is larger than MTU limit", nil, "payloadSize", n)
			}
			p.stats.IgnoredPackets.Add(1)
			continue // ignore partial messages
		}

		ptr := p.hnd.Load()
		if ptr == nil {
			p.stats.IgnoredPackets.Add(1)
			continue
		}
		hnd := *ptr
		if hnd == nil {
			p.stats.IgnoredPackets.Add(1)
			continue
		}
		err = hnd.HandleRTP(&h, buf[:n])
		if err != nil {
			if pipeline == "" {
				pipeline = hnd.String()
			}
			log := log.WithValues(
				"payloadSize", n,
				"rtpHeader", h,
				"pipeline", pipeline,
				"errorCount", errorCnt,
			)
			log.Debugw("handle RTP failed", "error", err)
			errorCnt++
			p.stats.FailedPackets.Add(1)
			if errorCnt >= maxErrors {
				log.Errorw("killing RTP loop due to persisted errors", err)
				return
			}
			continue
		}
		p.stats.InputPackets.Add(1)
		errorCnt = 0
		pipeline = ""
	}
}

// Must be called holding the lock
func (p *MediaPort) setupOutput(tid traceid.ID) error {
	if p.closed.IsBroken() {
		return errors.New("media is already closed")
	}
	if p.opts.IgnorePreanswerData {
		p.port.stopDiscarding()
	}
	go p.rtpLoop(tid, p.sess)
	w, err := p.sess.OpenWriteStream()
	if err != nil {
		return err
	}

	codecInfo := p.conf.Audio.Codec.Info()
	w = newRTPStatsWriter(p.mon, p.conf.Audio.Type, p.conf.Audio.DTMFType, codecInfo.SDPName, dtmf.SDPName, w)
	s := rtp.NewSeqWriter(w)
	p.audioOutRTP = s.NewStream(p.conf.Audio.Type, codecInfo.RTPClockRate)

	// Encoding pipeline (LK PCM -> SIP RTP)
	audioOut := p.conf.Audio.Codec.EncodeRTP(p.audioOutRTP)
	if p.stats != nil {
		audioOut = newMediaWriterCount(audioOut, &p.stats.AudioOutFrames, &p.stats.AudioOutSamples)
	}
	if p.logSignalChanges {
		audioOut, err = NewSignalLogger(p.log, "mixed", audioOut)
		if err != nil {
			audioOut.Close() // need to close since it's not linked to the port yet
			return err
		}
	}

	if p.conf.Audio.DTMFType != 0 {
		p.dtmfOutRTP = s.NewStream(p.conf.Audio.DTMFType, dtmf.SampleRate)
		if p.dtmfAudioEnabled {
			// Add separate mixer for DTMF audio.
			// TODO: optimize, if we'll ever need this code path
			mix, err := mixer.NewMixer(audioOut, rtp.DefFrameDur, 1, mixer.WithOutputChannel())
			if err != nil {
				return err
			}
			audioOut = mix.NewInput()
			p.dtmfOutAudio = mix.NewInput()
		}
	}

	if w := p.audioOut.Swap(audioOut); w != nil {
		_ = w.Close()
	}
	return nil
}

func (p *MediaPort) setupInput() {
	// Decoding pipeline (SIP RTP -> LK PCM)
	codec := p.conf.Audio.Codec
	codecInfo := codec.Info()
	if p.opts.NoInputResample {
		p.audioIn.SetSampleRate(codecInfo.SampleRate)
	}

	var audioWriter msdk.PCM16Writer = p.audioIn
	if p.stats != nil {
		audioWriter = newMediaWriterCount(audioWriter, &p.stats.AudioInFrames, &p.stats.AudioInSamples)
	}
	if p.logSignalChanges {
		signalLogger, err := NewSignalLogger(p.log, "input", audioWriter)
		if err != nil {
			p.log.Errorw("failed to create signal logger", err)
		} else {
			audioWriter = signalLogger
		}
	}
	audioHandler := p.conf.Audio.Codec.DecodeRTP(audioWriter, p.conf.Audio.Type)
	// Wrap the decoder with silence suppression handler to fill gaps during silence suppression
	audioHandler = newSilenceFiller(audioHandler, audioWriter, codecInfo.RTPClockRate, codecInfo.SampleRate, p.log)
	p.audioInHandler = audioHandler

	mux := rtp.NewMux(nil)
	mux.SetDefault(newRTPStatsHandler(p.mon, "", nil))
	mux.Register(
		p.conf.Audio.Type, newRTPHandlerCount(
			newRTPStatsHandler(p.mon, codecInfo.SDPName, audioHandler),
			&p.stats.AudioPackets, &p.stats.AudioBytes,
		),
	)
	if p.conf.Audio.DTMFType != 0 {
		mux.Register(
			p.conf.Audio.DTMFType, newRTPHandlerCount(
				newRTPStatsHandler(p.mon, dtmf.SDPName, rtp.HandlerFunc(p.dtmfHandler)),
				&p.stats.DTMFPackets, &p.stats.DTMFBytes,
			),
		)
	}
	var hnd rtp.HandlerCloser = newRTPStreamStats(mux, &p.stats.MuxStats)
	if p.jitterEnabled {
		hnd = rtp.HandleJitter(hnd, jitter.WithPacketLossHandler(func(packetsLost, packetsDropped uint64) {
			p.stats.JitterBufferPacketsLost.Store(packetsLost)
			p.stats.JitterBufferPacketsDropped.Store(packetsDropped)
		}))
	}
	p.hnd.Store(&hnd)
}

// SetDTMFAudio forces SIP to generate audio dTMF tones in addition to digital signals.
func (p *MediaPort) SetDTMFAudio(enabled bool) {
	p.dtmfAudioEnabled = enabled
}

// HandleDTMF sets an incoming DTMF handler.
func (p *MediaPort) HandleDTMF(h func(ev dtmf.Event)) {
	if h == nil {
		p.dtmfIn.Store(nil)
	} else {
		p.dtmfIn.Store(&h)
	}
}

func (p *MediaPort) dtmfHandler(h *rtp.Header, payload []byte) error {
	ptr := p.dtmfIn.Load()
	if ptr == nil {
		return nil
	}
	fnc := *ptr
	if ev, ok := dtmf.DecodeRTP(h, payload); ok && fnc != nil {
		fnc(ev)
	}
	return nil
}

func (p *MediaPort) WriteDTMF(ctx context.Context, digits string) error {
	if len(digits) == 0 {
		return nil
	}
	p.mu.Lock()
	dtmfOut := p.dtmfOutRTP
	audioOut := p.dtmfOutAudio
	audioOutRTP := p.audioOutRTP
	p.mu.Unlock()
	if !p.dtmfAudioEnabled {
		audioOut = nil
	}
	if dtmfOut == nil && audioOut == nil {
		return nil
	}

	var rtpTs uint32
	if audioOutRTP != nil {
		rtpTs = audioOutRTP.GetCurrentTimestamp()
	}

	return dtmf.Write(ctx, audioOut, dtmfOut, rtpTs, digits)
}
