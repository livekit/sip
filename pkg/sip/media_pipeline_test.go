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
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	msdk "github.com/livekit/media-sdk"
	"github.com/livekit/media-sdk/dtmf"
	"github.com/livekit/media-sdk/g711"
	msdkopus "github.com/livekit/media-sdk/opus"
	msrtp "github.com/livekit/media-sdk/rtp"
	"github.com/livekit/media-sdk/sdp"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
)

const (
	testAudioPT   = byte(0)  // PCMU
	testOpusPT    = byte(96) // dynamic
	testDTMFPT    = byte(101)
	testCodecRate = 8000
)

func mustAudioCodec(t testing.TB, name string) msdk.AudioCodec {
	t.Helper()
	codec, ok := sdp.CodecByName(name).(msdk.AudioCodec)
	require.True(t, ok, "codec %s", name)
	return codec
}

// Opus is not a registered SIP SDP codec; wrap media-sdk/opus so the pipeline
// can encode/decode at RoomSampleRate (no resample).
func testOpusCodec() msdk.AudioCodec {
	log := logger.GetLogger()
	return msdk.NewAudioCodec(msdk.CodecInfo{
		SDPName:      "opus/48000",
		SampleRate:   RoomSampleRate,
		RTPClockRate: RoomSampleRate,
	},
		func(w msdk.PCM16Writer) msdk.WriteCloser[msdkopus.Sample] {
			d, err := msdkopus.Decode(w, 1, log)
			if err != nil {
				panic(err)
			}
			return d
		},
		func(w msdk.WriteCloser[msdkopus.Sample]) msdk.PCM16Writer {
			e, err := msdkopus.Encode(w, 1, log)
			if err != nil {
				panic(err)
			}
			return e
		},
	)
}

type dtmfCollector struct {
	mu     sync.Mutex
	events []*livekit.SipDTMF
}

func (c *dtmfCollector) String() string { return "dtmfCollector" }

func (c *dtmfCollector) SampleRate() int { return dtmf.SampleRate }

func (c *dtmfCollector) Close() error { return nil }

func (c *dtmfCollector) WriteSample(sample *livekit.SipDTMF) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, sample)
	return nil
}

func (c *dtmfCollector) snapshot() []*livekit.SipDTMF {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]*livekit.SipDTMF, len(c.events))
	copy(out, c.events)
	return out
}

type pipelineHarness struct {
	t           *testing.T
	local       *testUDPConn
	remote      *testUDPConn
	port        *udpConn
	audioIn     msdk.WriteCloserSwitch[msdk.PCM16Sample]
	audioOut    msdk.WriteCloserSwitch[msdk.PCM16Sample]
	dtmfIn      msdk.WriteCloserSwitch[*livekit.SipDTMF]
	dtmfOut     msdk.WriteCloserSwitch[*livekit.SipDTMF]
	roomAudio   *msdk.PCM16Sample
	roomDTMF    *dtmfCollector
	pipeline    *mediaPortPipeline
	ssrcCount   atomic.Uint64
	packetCount atomic.Uint64
	codec       msdk.AudioCodec
	audioPT     byte
	dtmfPT      byte
}

func newPipelineHarness(t *testing.T, audioType byte, codec msdk.AudioCodec, dtmfType byte, dtmfAudio bool) *pipelineHarness {
	t.Helper()
	local, remote := newUDPPipe()

	log := logger.GetLogger()
	port := newUDPConn(log, local, false)
	h := &pipelineHarness{
		t:         t,
		local:     local,
		remote:    remote,
		port:      port,
		roomAudio: new(msdk.PCM16Sample),
		roomDTMF:  &dtmfCollector{},
		codec:     codec,
		audioPT:   audioType,
		dtmfPT:    dtmfType,
	}

	h.audioIn.Swap(msdk.NewPCM16BufferWriter(h.roomAudio, RoomSampleRate))
	h.dtmfIn.Swap(h.roomDTMF)

	pipelineConfig := &MediaPortPipelineConfig{
		log:   log,
		opts:  &MediaOptions{DTMFAudio: dtmfAudio},
		stats: &PortStats{},
		onNewSSRC: func() bool {
			h.ssrcCount.Add(1)
			return true
		},
		onPacket: func() {
			h.packetCount.Add(1)
		},
	}
	mc := &sdp.MediaConfig{
		Local:  local.addr,
		Remote: remote.addr,
		Audio: sdp.AudioConfig{
			Codec:    codec,
			Type:     audioType,
			DTMFType: dtmfType,
		},
	}
	pipe, err := NewMediaPortPipeline(pipelineConfig, mc, port, &h.audioIn, &h.dtmfIn, RoomSampleRate)
	require.NoError(t, err)
	audioToPort, dtmfToPort := pipe.GetConnectors()
	h.pipeline = pipe

	if old := h.audioOut.Swap(audioToPort); old != nil {
		_ = old.Close()
	}
	if old := h.dtmfOut.Swap(dtmfToPort); old != nil {
		_ = old.Close()
	}
	t.Cleanup(func() {
		_ = pipe.Close()
		_ = local.Close()
		_ = remote.Close()
	})
	return h
}

func (h *pipelineHarness) readRemotePacket(timeout time.Duration) (*rtp.Packet, bool) {
	h.t.Helper()
	select {
	case raw := <-h.remote.buf:
		var pkt rtp.Packet
		require.NoError(h.t, pkt.Unmarshal(raw))
		return &pkt, true
	case <-time.After(timeout):
		return nil, false
	}
}

func (h *pipelineHarness) injectRTP(pkt *rtp.Packet) {
	h.t.Helper()
	raw, err := pkt.Marshal()
	require.NoError(h.t, err)
	_, err = h.remote.WriteToUDPAddrPort(raw, h.local.addr)
	require.NoError(h.t, err)
}

func (h *pipelineHarness) injectAudio(ssrc uint32, seq uint16, ts uint32, pcm msdk.PCM16Sample) {
	h.t.Helper()
	var buf msrtp.Buffer
	stream := msrtp.NewSeqWriter(&buf).NewStream(h.audioPT, h.codec.Info().RTPClockRate)
	enc := msrtp.EncodePCM(stream, h.codec)
	require.NoError(h.t, enc.WriteSample(pcm))
	require.NoError(h.t, enc.Close())
	require.NotEmpty(h.t, buf, "codec produced no RTP")
	for i, pkt := range buf {
		pkt.Header.SSRC = ssrc
		pkt.Header.SequenceNumber = seq + uint16(i)
		if i == 0 {
			pkt.Header.Timestamp = ts
		}
		h.injectRTP(pkt)
	}
}

func (h *pipelineHarness) injectDTMFDigit(ssrc uint32, digit string, ts uint32) {
	h.t.Helper()
	var buf msrtp.Buffer
	w := msrtp.NewSeqWriter(&buf).NewStream(h.dtmfPT, dtmf.SampleRate)
	require.NoError(h.t, dtmf.Write(context.Background(), nil, w, ts, digit))
	for _, pkt := range buf {
		pkt.Header.SSRC = ssrc
		h.injectRTP(pkt)
	}
}

func tonePCM(rate, samples int, amp int16) msdk.PCM16Sample {
	out := make(msdk.PCM16Sample, samples)
	for i := range out {
		// Simple square-ish tone so PCMU round-trip keeps energy.
		if (i/(rate/400))%2 == 0 {
			out[i] = amp
		} else {
			out[i] = -amp
		}
	}
	return out
}

func pcmEnergy(s msdk.PCM16Sample) int64 {
	var sum int64
	for _, v := range s {
		if v < 0 {
			v = -v
		}
		sum += int64(v)
	}
	return sum
}

func TestMediaPipelinePermutations(t *testing.T) {
	pcmu := mustAudioCodec(t, g711.ULawSDPNameAndRate)
	opus := testOpusCodec()
	cases := []struct {
		name      string
		codec     msdk.AudioCodec
		audioPT   byte
		dtmfType  byte
		dtmfAudio bool
	}{
		{name: "dtmf_disabled", codec: pcmu, audioPT: testAudioPT, dtmfType: 0, dtmfAudio: false},
		{name: "dtmf_enabled", codec: pcmu, audioPT: testAudioPT, dtmfType: testDTMFPT, dtmfAudio: false},
		{name: "dtmf_enabled_with_audio", codec: pcmu, audioPT: testAudioPT, dtmfType: testDTMFPT, dtmfAudio: true},
		// PCMU is 8kHz: pipeline resamples 48kHz room PCM. Opus is 48kHz: ResampleWriter is a nop.
		{name: "resample", codec: pcmu, audioPT: testAudioPT, dtmfType: 0, dtmfAudio: false},
		{name: "no_resample", codec: opus, audioPT: testOpusPT, dtmfType: 0, dtmfAudio: false},
	}

	roomFrame := RoomSampleRate / int(time.Second/msrtp.DefFrameDur)

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newPipelineHarness(t, tc.audioPT, tc.codec, tc.dtmfType, tc.dtmfAudio)

			t.Run("audio_to_port", func(t *testing.T) {
				sample := tonePCM(RoomSampleRate, roomFrame, 10000)
				for range 5 {
					require.NoError(t, h.audioOut.WriteSample(sample))
				}
				deadline := time.Now().Add(time.Second)
				foundAudio := false
				for time.Now().Before(deadline) && !foundAudio {
					pkt, ok := h.readRemotePacket(50 * time.Millisecond)
					if !ok {
						continue
					}
					if pkt.PayloadType == h.audioPT && len(pkt.Payload) > 0 {
						foundAudio = true
					}
				}
				require.True(t, foundAudio, "expected RTP audio toward the peer")
			})

			t.Run("audio_to_room", func(t *testing.T) {
				before := len(*h.roomAudio)
				codecRate := tc.codec.Info().SampleRate
				codecFrame := codecRate / int(time.Second/msrtp.DefFrameDur)
				clock := uint32(tc.codec.Info().RTPClockRate / int(time.Second/msrtp.DefFrameDur))
				sample := tonePCM(codecRate, codecFrame, 12000)
				for i := uint16(0); i < 5; i++ {
					h.injectAudio(0xA11CE, 1+i, clock+uint32(i)*clock, sample)
				}
				require.Eventually(t, func() bool {
					return h.packetCount.Load() >= 5
				}, time.Second, 5*time.Millisecond, "RTP should be accepted")
				require.Eventually(t, func() bool {
					return len(*h.roomAudio) > before
				}, time.Second, 5*time.Millisecond, "decoded PCM should reach room (packets=%d input=%d failed=%d ignored=%d room=%d)",
					h.packetCount.Load(),
					h.pipeline.conf.stats.InputPackets.Load(),
					h.pipeline.conf.stats.FailedPackets.Load(),
					h.pipeline.conf.stats.IgnoredPackets.Load(),
					len(*h.roomAudio),
				)
				got := (*h.roomAudio)[before:]
				require.Greater(t, pcmEnergy(got), int64(0), "decoded room audio should carry energy")
			})

			t.Run("dtmf_to_port", func(t *testing.T) {
				// Drain while DTMF write runs (in-band audio can emit many RTP frames).
				var (
					mu       sync.Mutex
					dtmfPkts int
					stop     = make(chan struct{})
					wg       sync.WaitGroup
				)
				wg.Add(1)
				go func() {
					defer wg.Done()
					for {
						select {
						case <-stop:
							return
						default:
						}
						pkt, ok := h.readRemotePacket(20 * time.Millisecond)
						if !ok {
							continue
						}
						if tc.dtmfType != 0 && pkt.PayloadType == tc.dtmfType {
							mu.Lock()
							dtmfPkts++
							mu.Unlock()
						}
					}
				}()

				err := h.dtmfOut.WriteSample(&livekit.SipDTMF{Digit: "5", Code: 5})
				require.NoError(t, err, "DTMF write must not error when disabled or enabled")
				time.Sleep(50 * time.Millisecond) // allow final packets to flush
				close(stop)
				wg.Wait()

				mu.Lock()
				n := dtmfPkts
				mu.Unlock()
				if tc.dtmfType == 0 {
					require.Zero(t, n, "DTMF disabled: no telephone-event RTP")
				} else {
					require.NotZero(t, n, "DTMF enabled: expected telephone-event RTP")
				}
			})

			t.Run("dtmf_to_room", func(t *testing.T) {
				before := len(h.roomDTMF.snapshot())
				if tc.dtmfType == 0 {
					// Inject telephone-event anyway; mux should drop without error.
					h.dtmfPT = testDTMFPT
					h.injectDTMFDigit(0xD7DF, "7", 8000)
					h.dtmfPT = 0
					time.Sleep(50 * time.Millisecond)
					require.Equal(t, before, len(h.roomDTMF.snapshot()), "DTMF disabled: must not reach room")
					return
				}

				h.injectDTMFDigit(0xD7DF, "7", 8000)
				require.Eventually(t, func() bool {
					return len(h.roomDTMF.snapshot()) > before
				}, time.Second, 5*time.Millisecond)
				got := h.roomDTMF.snapshot()[before:]
				if assert.NotEmpty(t, got) {
					assert.Equal(t, "7", got[0].Digit)
				}
			})
		})
	}
}

func TestMediaPipelineTeardownMultiSSRC(t *testing.T) {
	h := newPipelineHarness(t, testAudioPT, mustAudioCodec(t, g711.ULawSDPNameAndRate), testDTMFPT, false)
	frame := testCodecRate / int(time.Second/msrtp.DefFrameDur)
	sample := tonePCM(testCodecRate, frame, 8000)

	h.injectAudio(0x11111111, 1, 160, sample)
	h.injectAudio(0x22222222, 1, 160, sample)

	require.Eventually(t, func() bool {
		return h.ssrcCount.Load() >= 2
	}, time.Second, 5*time.Millisecond, "expected AcceptStream for two SSRCs")
	assert.GreaterOrEqual(t, h.packetCount.Load(), uint64(2))

	done := make(chan error, 1)
	go func() {
		done <- h.pipeline.Close()
	}()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("pipeline.Close hung with multiple SSRCs")
	}
}

func TestMediaPipelineReuseUDPConn(t *testing.T) {
	local, remote := newUDPPipe()
	log := logger.GetLogger()
	port := newUDPConn(log, local, false)

	codec, ok := sdp.CodecByName(g711.ULawSDPNameAndRate).(msdk.AudioCodec)
	require.True(t, ok)
	frame := testCodecRate / int(time.Second/msrtp.DefFrameDur)
	sample := tonePCM(testCodecRate, frame, 9000)

	build := func(t *testing.T) (*mediaPortPipeline, *msdk.WriteCloserSwitch[msdk.PCM16Sample]) {
		t.Helper()
		var audioIn msdk.WriteCloserSwitch[msdk.PCM16Sample]
		var audioOut msdk.WriteCloserSwitch[msdk.PCM16Sample]
		var dtmfIn msdk.WriteCloserSwitch[*livekit.SipDTMF]
		roomBuf := new(msdk.PCM16Sample)
		audioIn.Swap(msdk.NewPCM16BufferWriter(roomBuf, RoomSampleRate))

		pipelineConfig := &MediaPortPipelineConfig{
			log:   log,
			opts:  &MediaOptions{},
			stats: &PortStats{},
		}
		mc := &sdp.MediaConfig{
			Local:  local.addr,
			Remote: remote.addr,
			Audio: sdp.AudioConfig{
				Codec:    codec,
				Type:     testAudioPT,
				DTMFType: testDTMFPT,
			},
		}
		pipe, err := NewMediaPortPipeline(pipelineConfig, mc, port, &audioIn, &dtmfIn, RoomSampleRate)
		require.NoError(t, err)
		audioToPort, dtmfToPort := pipe.GetConnectors()
		_ = audioOut.Swap(audioToPort)
		_ = dtmfToPort // unused in this test
		return pipe, &audioOut
	}

	// Generation 1
	pipe1, out1 := build(t)
	require.NoError(t, out1.WriteSample(sample))
	select {
	case raw := <-remote.buf:
		var pkt rtp.Packet
		require.NoError(t, pkt.Unmarshal(raw))
		assert.Equal(t, testAudioPT, pkt.PayloadType)
	case <-time.After(time.Second):
		t.Fatal("first pipeline produced no RTP")
	}
	require.NoError(t, pipe1.Close())
	if w := out1.Swap(nil); w != nil {
		_ = w.Close()
	}

	// Soft-closed port must be reopened before the next session.
	port.Reopen()

	// Generation 2 on the same udpConn / test pipe
	pipe2, out2 := build(t)
	require.NoError(t, out2.WriteSample(sample))
	select {
	case raw := <-remote.buf:
		var pkt rtp.Packet
		require.NoError(t, pkt.Unmarshal(raw))
		assert.Equal(t, testAudioPT, pkt.PayloadType)
	case <-time.After(time.Second):
		t.Fatal("second pipeline produced no RTP after Reopen")
	}
	require.NoError(t, pipe2.Close())

	_ = local.Close()
	_ = remote.Close()
}
