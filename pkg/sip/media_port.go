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
	"fmt"
	"errors"
	"io"
	"net"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/frostbyte73/core"

	"github.com/livekit/mediatransportutil/pkg/rtcconfig"
	"github.com/livekit/protocol/logger"

	"github.com/livekit/sip/pkg/media"
	"github.com/livekit/sip/pkg/media/dtmf"
	"github.com/livekit/sip/pkg/media/rtp"
	"github.com/livekit/sip/pkg/media/sdp"
	"github.com/livekit/sip/pkg/media/srtp"
	"github.com/livekit/sip/pkg/mixer"
	"github.com/livekit/sip/pkg/stats"
)

type UDPConn interface {
	net.Conn
	ReadFromUDPAddrPort(b []byte) (n int, addr netip.AddrPort, err error)
	WriteToUDPAddrPort(b []byte, addr netip.AddrPort) (int, error)
}

func newUDPConn(conn UDPConn) *udpConn {
	return &udpConn{UDPConn: conn}
}

type udpConn struct {
	UDPConn
	src atomic.Pointer[netip.AddrPort]
	dst atomic.Pointer[netip.AddrPort]
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
		c.dst.Store(&addr)
	}
}

func (c *udpConn) Read(b []byte) (n int, err error) {
	n, addr, err := c.ReadFromUDPAddrPort(b)
	c.src.Store(&addr)
	return n, err
}

func (c *udpConn) Write(b []byte) (n int, err error) {
	dst := c.dst.Load()
	if dst == nil {
		return len(b), nil // ignore
	}
	return c.WriteToUDPAddrPort(b, *dst)
}

type MediaConf struct {
	sdp.MediaConfig
	Processor media.PCM16Processor
}

type MediaOptions struct {
	IP                  netip.Addr
	Ports               rtcconfig.PortRange
	MediaTimeoutInitial time.Duration
	MediaTimeout        time.Duration
	EnableJitterBuffer  bool
}

func NewMediaPort(log logger.Logger, mon *stats.CallMonitor, opts *MediaOptions, sampleRate int) (*MediaPort, error) {
	return NewMediaPortWith(log, mon, nil, opts, sampleRate)
}

func NewMediaPortWith(log logger.Logger, mon *stats.CallMonitor, conn UDPConn, opts *MediaOptions, sampleRate int) (*MediaPort, error) {
	if opts == nil {
		opts = &MediaOptions{}
	}
	if opts.MediaTimeoutInitial <= 0 {
		opts.MediaTimeoutInitial = 30 * time.Second
	}
	if opts.MediaTimeout <= 0 {
		opts.MediaTimeout = 15 * time.Second
	}
	if conn == nil {
		c, err := rtp.ListenUDPPortRange(opts.Ports.Start, opts.Ports.End, netip.AddrFrom4([4]byte{0, 0, 0, 0}))
		if err != nil {
			return nil, err
		}
		conn = c
	}
	mediaTimeout := make(chan struct{})
	p := &MediaPort{
		log:           log,
		opts:          opts,
		mon:           mon,
		externalIP:    opts.IP,
		mediaTimeout:  mediaTimeout,
		jitterEnabled: opts.EnableJitterBuffer,
		port:          newUDPConn(conn),
		audioOut:      media.NewSwitchWriter(sampleRate),
		audioIn:       media.NewSwitchWriter(sampleRate),
	}
	go p.timeoutLoop(func() {
		close(mediaTimeout)
	})
	p.log.Debugw("listening for media on UDP", "port", p.Port())
	return p, nil
}

// MediaPort combines all functionality related to sending and accepting SIP media.
type MediaPort struct {
	log              logger.Logger
	opts             *MediaOptions
	mon              *stats.CallMonitor
	externalIP       netip.Addr
	port             *udpConn
	mediaReceived    core.Fuse
	packetCount      atomic.Uint64
	mediaTimeout     <-chan struct{}
	timeoutStart     atomic.Pointer[time.Time]
	closed           core.Fuse
	dtmfAudioEnabled bool
	jitterEnabled    bool

	mu           sync.Mutex
	conf         *MediaConf
	sess         rtp.Session
	hnd          atomic.Pointer[rtp.Handler]
	dtmfOutRTP   *rtp.Stream
	dtmfOutAudio media.PCM16Writer

	audioOutRTP    *rtp.Stream
	audioOut       *media.SwitchWriter // LK PCM -> SIP RTP
	audioIn        *media.SwitchWriter // SIP RTP -> LK PCM
	audioInHandler rtp.Handler         // for debug only
	dtmfIn         atomic.Pointer[func(ev dtmf.Event)]
}

func (p *MediaPort) EnableTimeout(enabled bool) {
	if !enabled {
		p.timeoutStart.Store(nil)
		return
	}
	now := time.Now()
	p.timeoutStart.Store(&now)
}

func (p *MediaPort) timeoutLoop(timeoutCallback func()) {
	ticker := time.NewTicker(p.opts.MediaTimeout)
	defer ticker.Stop()

	var lastPackets uint64
	for {
		select {
		case <-p.closed.Watch():
			return
		case <-ticker.C:
			curPackets := p.packetCount.Load()
			if curPackets != lastPackets {
				lastPackets = curPackets
				continue
			}
			start := p.timeoutStart.Load()
			if start == nil {
				continue // temporary disabled
			}
			if lastPackets == 0 && time.Since(*start) < p.opts.MediaTimeoutInitial {
				continue
			}
			timeoutCallback()
			return
		}
	}
}

func (p *MediaPort) Close() {
	p.closed.Once(func() {
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
		_ = p.port.Close()
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
func (p *MediaPort) WriteAudioTo(w media.PCM16Writer) {
	if processor := p.conf.Processor; processor != nil {
		w = processor(w)
	}
	if pw := p.audioIn.Swap(w); pw != nil {
		_ = pw.Close()
	}
}

// GetAudioWriter returns audio writer that will send PCM to the destination via RTP.
func (p *MediaPort) GetAudioWriter() media.PCM16Writer {
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
		return nil, err
	}
	return &MediaConf{MediaConfig: *mc}, nil
}

// SetOffer decodes the offer from another party and returns encoded answer. To accept the offer, call SetConfig.
func (p *MediaPort) SetOffer(offerData []byte, enc sdp.Encryption) (*sdp.Answer, *MediaConf, error) {
	offer, err := sdp.ParseOffer(offerData)
	if err != nil {
		return nil, nil, fmt.Errorf("cannot parse sdp: %w", err)
	}
	answer, mc, err := offer.Answer(p.externalIP, p.Port(), enc)
	if err != nil {
		return nil, nil, err
	}
	return answer, &MediaConf{MediaConfig: *mc}, nil
}

func (p *MediaPort) SetConfig(c *MediaConf) error {
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

	if err = p.setupOutput(); err != nil {
		return err
	}
	p.setupInput()
	return nil
}

func (p *MediaPort) rtpLoop(sess rtp.Session) {
	// Need a loop to process all incoming packets.
	for {
		r, _, err := sess.AcceptStream()
		if err != nil {
			if !errors.Is(err, net.ErrClosed) && !strings.Contains(err.Error(), "closed") {
				p.log.Errorw("cannot accept RTP stream", err)
			}
			return
		}
		p.mediaReceived.Break()
		go p.rtpReadLoop(r)
	}
}

func (p *MediaPort) rtpReadLoop(r rtp.ReadStream) {
	buf := make([]byte, rtp.MTUSize+1)
	overflow := false
	var h rtp.Header
	for {
		h = rtp.Header{}
		n, err := r.ReadRTP(&h, buf)
		if err == io.EOF {
			return
		} else if err != nil {
			p.log.Errorw("read RTP failed", err)
			return
		}
		p.packetCount.Add(1)
		if n > rtp.MTUSize {
			overflow = true
			if !overflow {
				p.log.Errorw("RTP packet is larger than MTU limit", nil)
			}
			continue // ignore partial messages
		}

		ptr := p.hnd.Load()
		if ptr == nil {
			continue
		}
		hnd := *ptr
		if hnd == nil {
			continue
		}
		err = hnd.HandleRTP(&h, buf[:n])
		if err != nil {
			p.log.Errorw("handle RTP failed", err)
			continue
		}
	}
}

// Must be called holding the lock
func (p *MediaPort) setupOutput() error {
	go p.rtpLoop(p.sess)
	w, err := p.sess.OpenWriteStream()
	if err != nil {
		return err
	}

	// TODO: this says "audio", but actually includes DTMF too
	s := rtp.NewSeqWriter(newRTPStatsWriter(p.mon, "audio", w))
	p.audioOutRTP = s.NewStream(p.conf.Audio.Type, p.conf.Audio.Codec.Info().RTPClockRate)

	// Encoding pipeline (LK PCM -> SIP RTP)
	audioOut := p.conf.Audio.Codec.EncodeRTP(p.audioOutRTP)

	if p.conf.Audio.DTMFType != 0 {
		p.dtmfOutRTP = s.NewStream(p.conf.Audio.DTMFType, dtmf.SampleRate)
		if p.dtmfAudioEnabled {
			// Add separate mixer for DTMF audio.
			// TODO: optimize, if we'll ever need this code path
			mix := mixer.NewMixer(audioOut, rtp.DefFrameDur)
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
	audioHandler := p.conf.Audio.Codec.DecodeRTP(p.audioIn, p.conf.Audio.Type)
	p.audioInHandler = audioHandler

	mux := rtp.NewMux(nil)
	mux.SetDefault(newRTPStatsHandler(p.mon, "", nil))
	mux.Register(p.conf.Audio.Type, newRTPStatsHandler(p.mon, p.conf.Audio.Codec.Info().SDPName, audioHandler))
	if p.conf.Audio.DTMFType != 0 {
		mux.Register(p.conf.Audio.DTMFType, newRTPStatsHandler(p.mon, dtmf.SDPName, rtp.HandlerFunc(func(h *rtp.Header, payload []byte) error {
			ptr := p.dtmfIn.Load()
			if ptr == nil {
				return nil
			}
			fnc := *ptr
			if ev, ok := dtmf.DecodeRTP(h, payload); ok && fnc != nil {
				fnc(ev)
			}
			return nil
		})))
	}
	var hnd rtp.Handler = mux
	if p.jitterEnabled {
		hnd = rtp.HandleJitter(p.conf.Audio.Codec.Info().RTPClockRate, hnd)
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
