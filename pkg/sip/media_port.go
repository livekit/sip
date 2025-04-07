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
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"github.com/livekit/mediatransportutil/pkg/rtcconfig"
	"github.com/livekit/protocol/logger"

	"github.com/livekit/sip/pkg/media"
	"github.com/livekit/sip/pkg/media/dtmf"
	"github.com/livekit/sip/pkg/media/rtp"
	"github.com/livekit/sip/pkg/media/sdp"
	"github.com/livekit/sip/pkg/mixer"
	"github.com/livekit/sip/pkg/stats"
)

// MediaConf 媒体处理配置
type MediaConf struct {
	sdp.MediaConfig                      // 媒体配置
	Processor       media.PCM16Processor // 处理器
}

// MediaConfig 媒体配置
type MediaConfig struct {
	IP                  netip.Addr          // IP地址
	Ports               rtcconfig.PortRange // 端口范围
	MediaTimeoutInitial time.Duration       // 媒体超时初始时间
	MediaTimeout        time.Duration       // 媒体超时时间
	EnableJitterBuffer  bool                // 启用抖动缓冲区
}

// NewMediaPort 创建媒体端口
// 参数:
// - log: 日志记录器
// - mon: 呼叫监控
// - conf: 媒体配置
// - sampleRate: 采样率
func NewMediaPort(log logger.Logger, mon *stats.CallMonitor, conf *MediaConfig, sampleRate int) (*MediaPort, error) {
	return NewMediaPortWith(log, mon, nil, conf, sampleRate)
}

// NewMediaPortWith 创建媒体端口（使用已有的RTP连接）
// 参数:
// - log: 日志记录器
// - mon: 呼叫监控
// - conn: RTP连接
// - conf: 媒体配置
// - sampleRate: 采样率
func NewMediaPortWith(log logger.Logger, mon *stats.CallMonitor, conn rtp.UDPConn, conf *MediaConfig, sampleRate int) (*MediaPort, error) {
	mediaTimeout := make(chan struct{})
	p := &MediaPort{
		log:           log,
		mon:           mon,
		externalIP:    conf.IP,
		mediaTimeout:  mediaTimeout,
		jitterEnabled: conf.EnableJitterBuffer,
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
// 媒体端口结合了所有与发送和接受SIP媒体相关的功能。
type MediaPort struct {
	log              logger.Logger      // 日志
	mon              *stats.CallMonitor // 呼叫监控
	externalIP       netip.Addr         // 外部IP
	conn             *rtp.Conn          // 连接
	mediaTimeout     <-chan struct{}    // 媒体超时
	dtmfAudioEnabled bool               // DTMF音频启用
	jitterEnabled    bool               // 抖动缓冲区启用
	closed           atomic.Bool        // 关闭

	mu           sync.Mutex        // 互斥锁
	conf         *MediaConf        // 媒体配置
	dtmfOutRTP   *rtp.Stream       // DTMF输出RTP
	dtmfOutAudio media.PCM16Writer // DTMF输出音频

	audioOutRTP    *rtp.Stream                         // 音频输出RTP
	audioOut       *media.SwitchWriter                 // LK PCM -> SIP RTP // 音频输出
	audioIn        *media.SwitchWriter                 // SIP RTP -> LK PCM // 音频输入
	audioInHandler rtp.Handler                         // for debug only // 仅用于调试
	dtmfIn         atomic.Pointer[func(ev dtmf.Event)] // DTMF输入
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

// Received 返回一个通道，当收到至少一个RTP包时，通道会被关闭
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
func (p *MediaPort) NewOffer() *sdp.Offer {
	return sdp.NewOffer(p.externalIP, p.Port())
}

// SetAnswer decodes and applies SDP answer for offer from NewOffer. SetConfig must be called with the decoded configuration.
func (p *MediaPort) SetAnswer(offer *sdp.Offer, answerData []byte) (*MediaConf, error) {
	answer, err := sdp.ParseAnswer(answerData)
	if err != nil {
		return nil, err
	}
	mc, err := answer.Apply(offer)
	if err != nil {
		return nil, err
	}
	return &MediaConf{MediaConfig: *mc}, nil
}

// SetOffer decodes the offer from another party and returns encoded answer. To accept the offer, call SetConfig.
func (p *MediaPort) SetOffer(offerData []byte) (*sdp.Answer, *MediaConf, error) {
	offer, err := sdp.ParseOffer(offerData)
	if err != nil {
		return nil, nil, err
	}
	answer, mc, err := offer.Answer(p.externalIP, p.Port())
	if err != nil {
		return nil, nil, err
	}
	return answer, &MediaConf{MediaConfig: *mc}, nil
}

func (p *MediaPort) SetConfig(c *MediaConf) error {
	p.log.Infow("using codecs",
		"audio-codec", c.Audio.Codec.Info().SDPName, "audio-rtp", c.Audio.Type,
		"dtmf-rtp", c.Audio.DTMFType,
	)

	p.mu.Lock()
	defer p.mu.Unlock()
	if ip := c.Remote; ip.IsValid() {
		p.conn.SetDestAddr(&net.UDPAddr{
			IP:   ip.Addr().AsSlice(),
			Port: int(ip.Port()),
		})
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
}

func (p *MediaPort) setupInput() {
	// Decoding pipeline (SIP RTP -> LK PCM)
	audioHandler := p.conf.Audio.Codec.DecodeRTP(p.audioIn, p.conf.Audio.Type)
	p.audioInHandler = audioHandler

	mux := rtp.NewMux(nil)
	mux.SetDefault(newRTPStatsHandler(p.mon, "", nil))
	mux.Register(p.conf.Audio.Type, newRTPStatsHandler(p.mon, p.conf.Audio.Codec.Info().SDPName, audioHandler))
	if p.conf.Audio.DTMFType != 0 {
		mux.Register(p.conf.Audio.DTMFType, newRTPStatsHandler(p.mon, dtmf.SDPName, rtp.HandlerFunc(func(pck *rtp.Packet) error {
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

	if p.jitterEnabled {
		p.conn.OnRTP(rtp.HandleJitter(p.conf.Audio.Codec.Info().RTPClockRate, mux))
	} else {
		p.conn.OnRTP(mux)
	}
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
