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
	"os"
	"strings"
	"sync/atomic"
	"time"

	msdk "github.com/livekit/media-sdk"
	"github.com/livekit/media-sdk/dtmf"
	"github.com/livekit/media-sdk/jitter"
	"github.com/livekit/media-sdk/mixer"
	"github.com/livekit/media-sdk/rtp"
	"github.com/livekit/media-sdk/srtp"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/utils/traceid"
)

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

	symmetric := p.opts.SymmetricRTP || (p.opts.IgnoreLocalAddrInSDP && c.Remote.Addr().IsPrivate())
	p.port.SetDst(c.Remote)
	if symmetric {
		p.port.SetSymmetric(true)
	}
	if p.opts.IgnorePreanswerData {
		// this needs to happen before the SRTP session is created, otherwise the read deadline will be
		// overwritten and we may get stuck in the discard loop
		p.port.stopDiscarding()
	}
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
	p.conf = c
	p.sess = sess

	if err = p.setupOutput(p.tid); err != nil {
		return err
	}
	p.setupInput()
	return nil
}

func (p *MediaPort) setupInput() {
	// Decoding pipeline (SIP RTP -> LK PCM)
	codec := p.conf.Audio.Codec
	codecInfo := codec.Info()
	if p.opts.NoInputResample {
		p.audioIn.SetSampleRate(codecInfo.SampleRate)
	}

	// Latency measurement: shared timestamp between entry (RTP handler) and exit (PCM writer).
	var inboundLatencyEntry atomic.Int64

	var audioWriter msdk.PCM16Writer = p.audioIn
	audioWriter = newLatencyPCMExit(audioWriter, &inboundLatencyEntry, &p.stats.LatencyInE2E)
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
	audioHandler := rtp.DecodePCM(audioWriter, p.conf.Audio.Codec, p.conf.Audio.Type)
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
				newRTPStatsHandler(p.mon, dtmf.SDPNameAndRate, rtp.HandlerFunc(p.dtmfHandler)),
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

	hnd = newLatencyRTPEntry(hnd, &inboundLatencyEntry)
	p.hnd.Store(&hnd)
}

func (p *MediaPort) dtmfHandler(h *rtp.Header, payload []byte) error {
	ptr := p.dtmfIn.Load()
	if ptr == nil {
		return nil
	}
	fnc := *ptr
	if fnc == nil {
		return nil
	}
	ev, err := dtmf.Decode(payload)
	if err != nil {
		return nil
	}
	// RFC 4733 requires all packets of a given digit to share identical timestamps.
	// Some SIP devices or carriers may reuse the timestamp of the previous digit
	// for the next one, so we combine timestamp and event code for deduplication.
	// The marker bit could be used instead, but it is prone to occasional loss.
	eventID := uint64(h.Timestamp)<<8 | uint64(ev.Code)
	if eventID == p.lastDTMFEvent.Load() {
		return nil
	}
	p.lastDTMFEvent.Store(eventID)
	fnc(ev)
	return nil
}

// Must be called holding the lock
func (p *MediaPort) setupOutput(tid traceid.ID) error {
	if p.closed.IsBroken() {
		return errors.New("media is already closed")
	}
	p.rtpLoopWG.Add(1)
	go p.rtpLoop(tid, p.sess)
	w, err := p.sess.OpenWriteStream()
	if err != nil {
		return err
	}

	// Latency measurement: shared timestamp between entry (PCM writer) and exit (RTP writer).
	var outboundLatencyEntry atomic.Int64

	codecInfo := p.conf.Audio.Codec.Info()
	w = newLatencyRTPExit(w, &outboundLatencyEntry, &p.stats.LatencyOut)
	w = newRTPStatsWriter(p.mon, p.conf.Audio.Type, p.conf.Audio.DTMFType, codecInfo.SDPName, dtmf.SDPName, w)
	s := rtp.NewSeqWriter(w)
	p.audioOutRTP = s.NewStream(p.conf.Audio.Type, codecInfo.RTPClockRate)

	// Encoding pipeline (LK PCM -> SIP RTP)
	audioOut := rtp.EncodePCM(p.audioOutRTP, p.conf.Audio.Codec)
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

	audioOut = newLatencyPCMEntry(audioOut, &outboundLatencyEntry)

	if w := p.audioOut.Swap(audioOut); w != nil {
		_ = w.Close()
	}
	return nil
}

func (p *MediaPort) rtpLoop(tid traceid.ID, sess rtp.Session) {
	defer p.rtpLoopWG.Done()
	// Need a loop to process all incoming packets.
	for {
		r, ssrc, err := sess.AcceptStream()
		if err != nil {
			if !errors.Is(err, io.EOF) && !errors.Is(err, os.ErrDeadlineExceeded) && !strings.Contains(err.Error(), "closed") {
				p.log.Errorw("cannot accept RTP stream", err)
			}
			return
		}
		p.stats.Streams.Add(1)
		p.mediaReceived.Break()
		log := p.log.WithValues("ssrc", ssrc)
		log.Debugw("accepting RTP stream")
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
		p.lastPacketTime.Store(time.Now().UnixNano())
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
