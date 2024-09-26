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
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/sdp/v3"

	"github.com/livekit/mediatransportutil/pkg/rtcconfig"
	"github.com/livekit/protocol/logger"

	"github.com/livekit/sip/pkg/media"
	"github.com/livekit/sip/pkg/media/dtmf"
	"github.com/livekit/sip/pkg/media/rtp"
	"github.com/livekit/sip/pkg/mixer"
	"github.com/livekit/sip/pkg/stats"
)

type MediaConfig struct {
	IP                  string
	Ports               rtcconfig.PortRange
	MediaTimeoutInitial time.Duration
	MediaTimeout        time.Duration
}

func NewMediaPort(log logger.Logger, mon *stats.CallMonitor, conf *MediaConfig, sampleRate int) (*MediaPort, error) {
	return NewMediaPortWith(log, mon, nil, conf, sampleRate)
}

func NewMediaPortWith(log logger.Logger, mon *stats.CallMonitor, conn rtp.UDPConn, conf *MediaConfig, sampleRate int) (*MediaPort, error) {
	mediaTimeout := make(chan struct{})
	p := &MediaPort{
		log:          log,
		mon:          mon,
		externalIP:   conf.IP,
		mediaTimeout: mediaTimeout,
		conn: rtp.NewConnWith(conn, &rtp.ConnConfig{
			MediaTimeoutInitial: conf.MediaTimeoutInitial,
			MediaTimeout:        conf.MediaTimeout,
			TimeoutCallback: func() {
				close(mediaTimeout)
			},
		}),
		audioOut: media.NewSwitchWriter(sampleRate),
		audioIn:  media.NewSwitchWriter(sampleRate),
	}
	if err := p.conn.ListenAndServe(conf.Ports.Start, conf.Ports.End, "0.0.0.0"); err != nil {
		return nil, err
	}
	p.log.Debugw("listening for media on UDP", "port", p.Port())
	return p, nil
}

// MediaPort combines all functionality related to sending and accepting SIP media.
type MediaPort struct {
	log              logger.Logger
	mon              *stats.CallMonitor
	externalIP       string
	conn             *rtp.Conn
	mediaTimeout     <-chan struct{}
	dtmfAudioEnabled bool
	closed           atomic.Bool

	mu           sync.Mutex
	conf         *MediaConf
	dtmfOutRTP   *rtp.Stream
	dtmfOutAudio media.PCM16Writer

	audioOutRTP    *rtp.Stream
	audioOut       *media.SwitchWriter // SIP PCM -> LK RTP
	audioIn        *media.SwitchWriter // LK RTP -> SIP PCM
	audioInHandler rtp.Handler         // for debug only
	dtmfIn         atomic.Pointer[func(ev dtmf.Event)]
}

func (p *MediaPort) EnableTimeout(enabled bool) {
	p.conn.EnableTimeout(enabled)
}

func (p *MediaPort) Close() {
	if !p.closed.CompareAndSwap(false, true) {
		return
	}
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
	_ = p.conn.Close()
}

func (p *MediaPort) Port() int {
	return p.conn.LocalAddr().Port
}

func (p *MediaPort) Received() <-chan struct{} {
	return p.conn.Received()
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
	if pw := p.audioIn.Swap(w); pw != nil {
		_ = pw.Close()
	}
}

// GetAudioWriter returns audio writer that will send PCM to the destination via RTP.
func (p *MediaPort) GetAudioWriter() media.PCM16Writer {
	return p.audioOut
}

// NewOffer generates an SDP offer for the media.
func (p *MediaPort) NewOffer() ([]byte, error) {
	return sdpGenerateOffer(p.externalIP, p.Port())
}

func (p *MediaPort) decodeSDP(data []byte) (*sdp.SessionDescription, *MediaConf, error) {
	desc := &sdp.SessionDescription{}
	if err := desc.Unmarshal(data); err != nil {
		return nil, nil, err
	}
	c, err := sdpGetAudioCodec(desc)
	if err != nil {
		p.log.Infow("SIP SDP failed", "error", err)
		return nil, nil, err
	}
	return desc, c, nil
}

// SetAnswer decodes and applies SDP answer for offer from NewOffer. It calls SetConfig with a decoded configuration.
func (p *MediaPort) SetAnswer(answer []byte) error {
	_, c, err := p.decodeSDP(answer)
	if err != nil {
		return err
	}
	return p.SetConfig(c)
}

// SetOffer decodes the offer from another party and returns encoded answer. To accept the offer, call SetConfig.
func (p *MediaPort) SetOffer(offer []byte) ([]byte, *MediaConf, error) {
	off, c, err := p.decodeSDP(offer)
	if err != nil {
		return nil, nil, err
	}
	answer, err := sdpGenerateAnswer(off, p.externalIP, p.Port(), c)
	if err != nil {
		return nil, nil, err
	}
	return answer, c, nil
}

func (p *MediaPort) SetConfig(c *MediaConf) error {
	p.log.Infow("using codecs",
		"audio-codec", c.Audio.Info().SDPName, "audio-rtp", c.AudioType,
		"dtmf-rtp", c.DTMFType,
	)

	p.mu.Lock()
	defer p.mu.Unlock()
	if c.Dest != nil {
		p.conn.SetDestAddr(c.Dest)
	}
	p.conf = c

	p.setupOutput()
	p.setupInput()
	return nil
}

// Must be called holding the lock
func (p *MediaPort) setupOutput() {
	// TODO: this says "audio", but actually includes DTMF too
	s := rtp.NewSeqWriter(newRTPStatsWriter(p.mon, "audio", p.conn))
	p.audioOutRTP = s.NewStream(p.conf.AudioType, p.conf.Audio.Info().RTPClockRate)
	// Encoding pipeline (LK -> SIP)
	audioOut := p.conf.Audio.EncodeRTP(p.audioOutRTP)

	if processor := p.conf.Processor; processor != nil {
		if audioOut.SampleRate() != processor.SampleRate() {
			audioOut = media.ResampleWriter(audioOut, processor.SampleRate())
		}
		processor.SetWriter(audioOut)
		audioOut = processor
	}

	if p.conf.DTMFType != 0 {
		p.dtmfOutRTP = s.NewStream(p.conf.DTMFType, dtmf.SampleRate)
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
}

func (p *MediaPort) setupInput() {
	// Decoding pipeline (SIP -> LK)
	audioHandler := p.conf.Audio.DecodeRTP(p.audioIn, p.conf.AudioType)
	p.audioInHandler = audioHandler
	audioHandler = rtp.HandleJitter(p.conf.Audio.Info().RTPClockRate, audioHandler)
	mux := rtp.NewMux(nil)
	mux.SetDefault(newRTPStatsHandler(p.mon, "", nil))
	mux.Register(p.conf.AudioType, newRTPStatsHandler(p.mon, p.conf.Audio.Info().SDPName, audioHandler))
	if p.conf.DTMFType != 0 {
		mux.Register(p.conf.DTMFType, newRTPStatsHandler(p.mon, dtmf.SDPName, rtp.HandlerFunc(func(pck *rtp.Packet) error {
			ptr := p.dtmfIn.Load()
			if ptr == nil {
				return nil
			}
			fnc := *ptr
			if ev, ok := dtmf.DecodeRTP(pck); ok && fnc != nil {
				fnc(ev)
			}
			return nil
		})))
	}
	p.conn.OnRTP(mux)
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
