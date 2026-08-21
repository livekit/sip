// Copyright 2026 LiveKit, Inc.
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
	"fmt"
	"io"
	"math"
	"os"
	"strings"
	"sync"
	"sync/atomic"

	msdk "github.com/livekit/media-sdk"
	"github.com/livekit/media-sdk/dtmf"
	"github.com/livekit/media-sdk/jitter"
	"github.com/livekit/media-sdk/mixer"
	"github.com/livekit/media-sdk/rtp"
	"github.com/livekit/media-sdk/sdp"
	"github.com/livekit/media-sdk/srtp"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/sip/pkg/stats"
)

type MediaPortPipelineConfig struct {
	log       logger.Logger
	opts      *MediaOptions
	mon       *stats.CallMonitor
	stats     *PortStats
	onNewSSRC func() bool
	onPacket  func()
}

func NewMediaPortPipeline(
	conf *MediaPortPipelineConfig,
	mc *sdp.MediaConfig,
	port *udpConn,
	audioToRoom msdk.PCM16Writer,
	dtmfToRoom msdk.WriteCloser[*livekit.SipDTMF],
	incomingSampleRate int,
) (*mediaPortPipeline, error) {
	p := &mediaPortPipeline{
		conf: conf,
	}
	err := p.init(mc, port, audioToRoom, dtmfToRoom, incomingSampleRate)
	if err != nil {
		p.Close()
		return nil, err
	}
	return p, nil
}

// A data structure owning the implementation of everything between a udpConn and two output Switches
// Has two directions, with both audio and optionally DTMF for each.
// Constructed once per negotiation, possibly N times in the lifetime of udpConn/Switch anchors.
type mediaPortPipeline struct {
	conf *MediaPortPipelineConfig // Expected to be owned by caller, not managed

	// Owned by pipeline
	ctx               context.Context
	cancel            context.CancelFunc
	sess              rtp.Session
	rtpLoopWG         sync.WaitGroup
	muxToRoom         atomic.Pointer[rtp.HandlerCloser]
	dtmfMixer         *mixer.Mixer
	audioToRoom       rtp.HandlerCloser
	dtmfToRoom        rtp.HandlerCloser
	dtmfHandler       msdk.WriteCloser[*livekit.SipDTMF] // Reference, not closed
	audioToPort       msdk.PCM16Writer                   // post-mixer chain towards port
	mixerToPort       msdk.PCM16Writer                   // Reference, not closed
	dtmfToPort        msdk.WriteCloser[*livekit.SipDTMF]
	lastDTMFTimestamp atomic.Uint32 // rtp timestamp of last DTMF packet seen
}

// Returns insulated (nopCloser) connectors, preventing anchor close from closing pipeline.
func (p *mediaPortPipeline) GetConnectors() (msdk.PCM16Writer, msdk.WriteCloser[*livekit.SipDTMF]) {
	if p.audioToPort == nil {
		return nil, nil
	}
	if p.dtmfToPort == nil {
		return msdk.NopCloser(p.mixerToPort), nil
	}
	return msdk.NopCloser(p.mixerToPort), msdk.NopCloser(p.dtmfToPort)
}

// Build pipeline between a udpConn and two output Switches.
// Requires fields to be set:
// - log
// - opts
// - mon
// - stats
// - onNewSSRC
// - onPacket
func (p *mediaPortPipeline) init(
	mc *sdp.MediaConfig,
	port *udpConn,
	audioToRoom msdk.PCM16Writer,
	dtmfToRoom msdk.WriteCloser[*livekit.SipDTMF],
	incomingSampleRate int,
) error {
	p.ctx, p.cancel = context.WithCancel(context.Background())

	var crypto string
	if mc.Crypto != nil {
		crypto = mc.Crypto.Profile.String()
	}
	p.conf.log.Infow("using codecs",
		"audio-codec", mc.Audio.Codec.Info().SDPName, "audio-rtp", mc.Audio.Type,
		"dtmf-rtp", mc.Audio.DTMFType,
		"srtp", crypto,
	)

	port.SetDst(mc.Remote)
	if p.conf.opts.IgnoreLocalAddrInSDP && mc.Remote.Addr().IsPrivate() {
		port.SetSymmetric(true) // Already initialized with opts, turn on for edge case
	}
	p.lastDTMFTimestamp.Store(math.MaxUint32)

	var err error
	if mc.Crypto != nil {
		p.sess, err = srtp.NewSession(p.conf.log, port, mc.Crypto)
	} else {
		p.sess = rtp.NewSession(p.conf.log, port)
	}
	if err != nil {
		return fmt.Errorf("failed to setup pipeline: %w", err)
	}

	err = p.setupInput(mc, audioToRoom, dtmfToRoom)
	if err != nil {
		return fmt.Errorf("failed to setup pipeline: %w", err)
	}
	err = p.setupOutput(mc, incomingSampleRate)
	if err != nil {
		return fmt.Errorf("failed to setup pipeline: %w", err)
	}
	return nil
}

// Construct the Audio and optionally DTMF pipline from SIP RTP to LK PCM, in reverse order.
func (p *mediaPortPipeline) setupInput(mc *sdp.MediaConfig, audioToRoom msdk.PCM16Writer, dtmfToRoom msdk.WriteCloser[*livekit.SipDTMF]) error {
	var err error
	var inboundLatencyEntry atomic.Int64
	sink := msdk.NopCloser(audioToRoom) // Prevent pipeline close from closing room
	sink = newLatencyPCMExit(sink, &inboundLatencyEntry, &p.conf.stats.LatencyInE2E)

	codecInfo := mc.Audio.Codec.Info()
	sink = msdk.ResampleWriter(sink, codecInfo.SampleRate)

	if p.conf.stats != nil {
		sink = newMediaWriterCount(sink, &p.conf.stats.AudioInFrames, &p.conf.stats.AudioInSamples)
	}

	if p.conf.opts.LogSignalChanges {
		sink, err = NewSignalLogger(p.conf.log, "input", sink)
		if err != nil {
			sink.Close()
			return err
		}
	}

	audioHandler := rtp.DecodePCM(sink, mc.Audio.Codec, mc.Audio.Type)

	// SilenceFiller injects silence after decoding, but it needs access to RTP headers
	// And these are only available before decoding, hence it wraps both audioHandler & sink
	audioHandler = newSilenceFiller(audioHandler, sink, codecInfo.RTPClockRate, codecInfo.SampleRate, p.conf.log)

	mux := rtp.NewMux(nil)
	mux.SetDefault(newRTPStatsHandler(p.conf.mon, "", nil))

	audioType := newRTPHandlerCount(
		newRTPStatsHandler(p.conf.mon, codecInfo.SDPName, audioHandler),
		&p.conf.stats.AudioPackets, &p.conf.stats.AudioBytes,
	)
	p.audioToRoom = audioType
	mux.Register(mc.Audio.Type, audioType)

	if mc.Audio.DTMFType != 0 {
		p.dtmfHandler = dtmfToRoom // Close doesn't propagate through rtp.HandlerFunc
		dtmfType := newRTPHandlerCount(
			newRTPStatsHandler(p.conf.mon, dtmf.SDPNameAndRate, rtp.HandlerFunc(p.handleEventRTP)),
			&p.conf.stats.DTMFPackets, &p.conf.stats.DTMFBytes,
		)
		p.dtmfToRoom = dtmfType
		mux.Register(mc.Audio.DTMFType, dtmfType)
	}

	var hnd rtp.HandlerCloser = newRTPStreamStats(mux, &p.conf.stats.MuxStats)
	if p.conf.opts.EnableJitterBuffer {
		hnd = rtp.HandleJitter(hnd, jitter.WithPacketLossHandler(func(packetsLost, packetsDropped uint64) {
			p.conf.stats.JitterBufferPacketsLost.Store(packetsLost)
			p.conf.stats.JitterBufferPacketsDropped.Store(packetsDropped)
		}))
	}
	hnd = newLatencyRTPEntry(hnd, &inboundLatencyEntry)

	p.muxToRoom.Store(&hnd)
	return nil
}

// Processes an incoming telephony-event packet, turns into SipDTMF, and forwards it.
func (p *mediaPortPipeline) handleEventRTP(h *rtp.Header, payload []byte) error {
	// RFC 4733 requires all packets of a given digit to share identical timestamps.
	// The marker bit could be used instead, but it is prone to occasional loss.
	if h.Timestamp == p.lastDTMFTimestamp.Load() {
		return nil
	}
	ev, err := dtmf.Decode(payload)
	if err != nil {
		return nil
	}
	p.lastDTMFTimestamp.Store(h.Timestamp)
	return p.dtmfHandler.WriteSample(&livekit.SipDTMF{
		Code:  uint32(ev.Code),
		Digit: string([]byte{ev.Digit}),
	})
}

// Construct the Audio and optionally DTMF pipline from LK PCM to SIP RTP
// Returns the insulated (nopCloser) connectors, and an error.
func (p *mediaPortPipeline) setupOutput(mc *sdp.MediaConfig, incomingSampleRate int) error {
	p.rtpLoopWG.Go(p.rtpLoop)
	w, err := p.sess.OpenWriteStream()
	if err != nil {
		return fmt.Errorf("failed to open write stream: %w", err)
	}

	// Latency measurement: shared timestamp between entry (PCM writer) and exit (RTP writer).
	var outboundLatencyEntry atomic.Int64

	codecInfo := mc.Audio.Codec.Info()
	w = newLatencyRTPExit(w, &outboundLatencyEntry, &p.conf.stats.LatencyOut)
	w = newRTPStatsWriter(p.conf.mon, mc.Audio.Type, mc.Audio.DTMFType, codecInfo.SDPName, dtmf.SDPName, w)
	s := rtp.NewSeqWriter(w)
	audioOutRTP := s.NewStream(mc.Audio.Type, codecInfo.RTPClockRate)

	audioOut := rtp.EncodePCM(audioOutRTP, mc.Audio.Codec)

	audioOut = newMediaWriterCount(audioOut, &p.conf.stats.AudioOutFrames, &p.conf.stats.AudioOutSamples)

	if p.conf.opts.LogSignalChanges {
		audioOut, err = NewSignalLogger(p.conf.log, "mixed", audioOut)
		if err != nil {
			audioOut.Close() // need to close since it's not linked to the port yet
			return err
		}
	}

	audioOut = msdk.ResampleWriter(audioOut, incomingSampleRate)

	audioOut = newLatencyPCMEntry(audioOut, &outboundLatencyEntry)

	p.audioToPort = audioOut
	p.mixerToPort = audioOut

	if mc.Audio.DTMFType != 0 {
		var dtmfAudio msdk.PCM16Writer = nil
		if p.conf.opts.DTMFAudio {
			// Add separate mixer for DTMF audio.
			// TODO: optimize, if we'll ever need this code path
			mix, err := mixer.NewMixer(audioOut, rtp.DefFrameDur, 1, mixer.WithOutputChannel())
			if err != nil {
				return err
			}
			audioOut = mix.NewInput()
			dtmfAudio = mix.NewInput()
			p.dtmfMixer = mix
			p.mixerToPort = audioOut
		}

		p.dtmfToPort = &dtmfOutWriter{
			log:          p.conf.log,
			ctx:          p.ctx,
			dtmfEvents:   s.NewStream(mc.Audio.DTMFType, dtmf.SampleRate),
			dtmfAudio:    dtmfAudio,
			getTimestamp: audioOutRTP.GetCurrentTimestamp,
		}
	}
	return nil
}

func (p *mediaPortPipeline) rtpLoop() {
	// Need a loop to process all incoming packets.
	for {
		r, ssrc, err := p.sess.AcceptStream()
		if err != nil {
			if !errors.Is(err, io.EOF) && !errors.Is(err, os.ErrDeadlineExceeded) && !strings.Contains(err.Error(), "closed") {
				p.conf.log.Errorw("cannot accept RTP stream", err)
			}
			return
		}
		p.conf.stats.Streams.Add(1)
		if p.conf.onNewSSRC != nil {
			p.conf.onNewSSRC()
		}
		log := p.conf.log.WithValues("ssrc", ssrc)
		log.Debugw("accepting RTP stream")
		p.rtpLoopWG.Go(func() { p.rtpReadLoop(log, r) })
	}
}

func (p *mediaPortPipeline) rtpReadLoop(log logger.Logger, r rtp.ReadStream) {
	const maxErrors = 50 // 1 sec, given 20 ms frames
	buf := make([]byte, rtp.MTUSize+1)
	overflow := false
	var (
		h        rtp.Header
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
		if p.conf.onPacket != nil {
			p.conf.onPacket()
		}
		p.conf.stats.Packets.Add(1)
		if n > rtp.MTUSize {
			if !overflow {
				overflow = true
				log.Errorw("RTP packet is larger than MTU limit", nil, "payloadSize", n)
			}
			p.conf.stats.IgnoredPackets.Add(1)
			continue // ignore partial messages
		}

		ptr := p.muxToRoom.Load()
		if ptr == nil {
			p.conf.stats.IgnoredPackets.Add(1)
			continue
		}
		hnd := *ptr
		if hnd == nil {
			p.conf.stats.IgnoredPackets.Add(1)
			continue
		}
		err = hnd.HandleRTP(&h, buf[:n])
		if err != nil {
			log := log.WithValues(
				"payloadSize", n,
				"rtpHeader", h,
				"pipeline", hnd.String(),
				"errorCount", errorCnt,
			)
			log.Debugw("handle RTP failed", "error", err)
			errorCnt++
			p.conf.stats.FailedPackets.Add(1)
			if errorCnt >= maxErrors {
				log.Errorw("killing RTP loop due to persisted errors", err)
				return
			}
			continue
		}
		p.conf.stats.InputPackets.Add(1)
		errorCnt = 0
	}
}

func (p *mediaPortPipeline) Close() error {
	if p.cancel != nil {
		p.cancel() // stop active DTMF digit send
	}
	var errs []error
	if p.sess != nil {
		errs = append(errs, p.sess.Close())
		p.rtpLoopWG.Wait()
	}
	if closer := p.muxToRoom.Swap(nil); closer != nil {
		(*closer).Close() // Doesn't propagate onwards
	}
	if p.audioToRoom != nil {
		p.audioToRoom.Close()
	}
	if p.dtmfToRoom != nil {
		p.dtmfToRoom.Close()
	}
	if p.dtmfMixer != nil {
		p.dtmfMixer.Stop()
	}
	if p.audioToPort != nil {
		errs = append(errs, p.audioToPort.Close())
	}
	if p.dtmfToPort != nil {
		errs = append(errs, p.dtmfToPort.Close())
	}
	return errors.Join(errs...)
}

// dtmfOutWriter sends SipDTMF as RFC 4733 telephone-events (optional in-band audio).
type dtmfOutWriter struct {
	log logger.Logger

	mu           sync.Mutex
	ctx          context.Context // canceled by pipeline Close, aborts an in-flight digit train
	dtmfEvents   *rtp.Stream
	dtmfAudio    msdk.PCM16Writer
	getTimestamp func() uint32
}

func (w *dtmfOutWriter) String() string {
	return fmt.Sprintf("dtmfOutWriter(dtmfAudio: %v)", w.dtmfAudio != nil)
}

func (w *dtmfOutWriter) SampleRate() int {
	return dtmf.SampleRate
}

func (w *dtmfOutWriter) Close() error {
	if w == nil || w.dtmfAudio == nil {
		return nil
	}
	return w.dtmfAudio.Close()
}

func (w *dtmfOutWriter) WriteSample(sample *livekit.SipDTMF) error {
	if sample == nil || sample.Code >= 0x10 || (len(sample.Digit) == 0 && sample.Code == 0) {
		return fmt.Errorf("invalid DTMF sample: %v", sample)
	}
	digits := sample.Digit
	if len(digits) == 0 {
		digit := dtmf.CodeToChar(byte(sample.Code))
		if digit == 0 {
			return fmt.Errorf("code %d not supoported", sample.Code)
		}
		digits = string([]byte{digit})
	} else if sample.Code > 0 {
		// We can't distinguish between a code0 and no code, but better have something here
		w.log.Debugw("code payload detected, ignored due to explicit digits", "code", sample.Code, "digits", sample.Digit)
	}

	w.mu.Lock()
	defer w.mu.Unlock()
	var rtpTs uint32
	if w.dtmfEvents != nil {
		rtpTs = w.getTimestamp() // TODO: Maybe time to introduce the auto timestamp feature?
	}
	err := dtmf.Write(w.ctx, w.dtmfAudio, w.dtmfEvents, rtpTs, digits)
	if err != nil {
		return err
	}
	return nil
}
