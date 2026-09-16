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
	"math"
	"slices"
	"strings"
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
	"github.com/livekit/media-sdk/opus"
	msrtp "github.com/livekit/media-sdk/rtp"
	"github.com/livekit/media-sdk/sdp"
	"github.com/livekit/protocol/logger"
)

const testDTMFPT = byte(101)

func audioCodecByName(t testing.TB, name string) msdk.AudioCodec {
	t.Helper()
	for _, c := range msdk.Codecs() {
		if strings.EqualFold(c.Info().SDPName, name) {
			ac, ok := c.(msdk.AudioCodec)
			require.True(t, ok, "codec %s is not audio", name)
			return ac
		}
	}
	t.Skipf("codec %s is not registered", name)
	return nil
}

// Opus is not a registered SIP SDP codec; wrap media-sdk/opus so the pipeline
// can encode/decode at RoomSampleRate (no resample).
func testOpusCodec(t testing.TB) msdk.AudioCodec {
	t.Helper()
	log := logger.NewTestLogger(t)
	return msdk.NewAudioCodec(msdk.CodecInfo{
		SDPName:      "opus/48000",
		SampleRate:   RoomSampleRate,
		RTPClockRate: RoomSampleRate,
	},
		func(w msdk.PCM16Writer) msdk.WriteCloser[opus.Sample] {
			d, err := opus.Decode(w, 1, log)
			if err != nil {
				panic(err)
			}
			return d
		},
		func(w msdk.WriteCloser[opus.Sample]) msdk.PCM16Writer {
			e, err := opus.Encode(w, 1, log)
			if err != nil {
				panic(err)
			}
			return e
		},
	)
}

func testAudioPT(c msdk.AudioCodec) byte {
	info := c.Info()
	if info.RTPIsStatic {
		return info.RTPDefType
	}
	return 96
}

type dtmfCollector struct {
	mu     sync.Mutex
	events []string
}

func (c *dtmfCollector) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	res := ""
	for _, event := range c.events {
		res += event
	}
	return res
}

func (c *dtmfCollector) SampleRate() int { return dtmf.SampleRate }

func (c *dtmfCollector) Close() error { return nil }

func (c *dtmfCollector) WriteSample(sample string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, sample)
	return nil
}

func (c *dtmfCollector) snapshot() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, len(c.events))
	copy(out, c.events)
	return out
}

// pcmCollector accumulates decoded room audio. The pipeline writes from the RTP
// read goroutine while the test reads, so every access is guarded.
type pcmCollector struct {
	sampleRate int

	mu  sync.Mutex
	buf msdk.PCM16Sample
}

func (c *pcmCollector) String() string { return fmt.Sprintf("pcmCollector(%d)", c.sampleRate) }

func (c *pcmCollector) SampleRate() int { return c.sampleRate }

func (c *pcmCollector) Close() error { return nil }

func (c *pcmCollector) WriteSample(sample msdk.PCM16Sample) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.buf = append(c.buf, sample...)
	return nil
}

func (c *pcmCollector) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.buf)
}

// since returns a copy of everything written after the first n samples.
func (c *pcmCollector) since(n int) msdk.PCM16Sample {
	c.mu.Lock()
	defer c.mu.Unlock()
	if n >= len(c.buf) {
		return nil
	}
	return slices.Clone(c.buf[n:])
}

// pipelineHarness is the durable side of a mediaPort: UDP pipe, pipeline config,
// buffer anchors, and a synthesized MediaConfig. The pipeline itself is swapped
// on configure / reconfigure.
type pipelineHarness struct {
	t           *testing.T
	local       *testUDPConn
	remote      *testUDPConn
	port        *udpConn
	conf        *MediaPortPipelineConfig
	audioIn     *msdk.WriteCloserSwitch[msdk.PCM16Sample]
	audioOut    *msdk.WriteCloserSwitch[msdk.PCM16Sample]
	dtmfIn      *msdk.WriteCloserSwitch[string]
	dtmfOut     *msdk.WriteCloserSwitch[string]
	roomAudio   *pcmCollector
	roomDTMF    *dtmfCollector
	pipeline    *mediaPortPipeline
	ssrcCount   atomic.Uint64
	packetCount atomic.Uint64
	codec       msdk.AudioCodec
	audioPT     byte
	dtmfPT      byte
	jitter      bool // when true, configure() enables the jitter buffer
}

func newPipelineHarness(t *testing.T, sampleRate int) *pipelineHarness {
	t.Helper()
	local, remote := newUDPPipe()
	log := logger.NewTestLogger(t)
	h := &pipelineHarness{
		t:         t,
		local:     local,
		remote:    remote,
		port:      newUDPConn(log, local, false),
		audioIn:   msdk.NewWriteCloserSwitch[msdk.PCM16Sample](sampleRate),
		audioOut:  msdk.NewWriteCloserSwitch[msdk.PCM16Sample](sampleRate),
		dtmfIn:    msdk.NewWriteCloserSwitch[string](dtmf.SampleRate),
		dtmfOut:   msdk.NewWriteCloserSwitch[string](dtmf.SampleRate),
		roomAudio: &pcmCollector{sampleRate: sampleRate},
		roomDTMF:  &dtmfCollector{},
	}
	h.audioIn.Swap(h.roomAudio)
	h.dtmfIn.Swap(h.roomDTMF)
	h.conf = &MediaPortPipelineConfig{
		log:   log,
		opts:  &MediaOptions{},
		stats: &PortStats{},
		onNewSSRC: func() bool {
			h.ssrcCount.Add(1)
			return true
		},
		onPacket: func() {
			h.packetCount.Add(1)
		},
	}
	t.Cleanup(func() {
		if h.pipeline != nil {
			_ = h.pipeline.Close()
		}
		_ = local.Close()
		_ = remote.Close()
	})
	return h
}

func (h *pipelineHarness) mediaConfig() *sdp.MediaConfig {
	return &sdp.MediaConfig{
		Local:  h.local.addr,
		Remote: h.remote.addr,
		Audio: sdp.AudioConfig{
			Codec:    h.codec,
			Type:     h.audioPT,
			DTMFType: h.dtmfPT,
		},
	}
}

func (h *pipelineHarness) configure(codec msdk.AudioCodec, audioPT, dtmfPT byte, dtmfAudio bool) {
	h.t.Helper()
	h.codec = codec
	h.audioPT = audioPT
	h.dtmfPT = dtmfPT
	h.conf.opts = &MediaOptions{DTMFAudio: dtmfAudio, EnableJitterBuffer: h.jitter}

	pipe, err := NewMediaPortPipeline(h.conf, h.mediaConfig(), h.port, h.audioIn, h.dtmfIn, h.audioIn.SampleRate())
	require.NoError(h.t, err)
	audioToPort, dtmfToPort := pipe.GetConnectors()
	h.pipeline = pipe
	if old := h.audioOut.Swap(audioToPort); old != nil {
		_ = old.Close()
	}
	if old := h.dtmfOut.Swap(dtmfToPort); old != nil {
		_ = old.Close()
	}
}

func (h *pipelineHarness) reconfigure(codec msdk.AudioCodec, audioPT, dtmfPT byte, dtmfAudio bool) {
	h.t.Helper()
	if h.pipeline != nil {
		require.NoError(h.t, h.pipeline.Close())
	}
	h.port.Reopen()
	h.ssrcCount.Store(0)
	h.packetCount.Store(0)
	h.configure(codec, audioPT, dtmfPT, dtmfAudio)
}

func (h *pipelineHarness) drainRemote() {
	for {
		select {
		case <-h.remote.buf:
		default:
			return
		}
	}
}

func (h *pipelineHarness) roomFrame() msdk.PCM16Sample {
	sampleRate := h.audioOut.SampleRate()
	n := sampleRate / int(time.Second/msrtp.DefFrameDur)
	return tonePCM(sampleRate, n, 10000)
}

func (h *pipelineHarness) codecFrame() msdk.PCM16Sample {
	rate := h.codec.Info().SampleRate
	n := rate / int(time.Second/msrtp.DefFrameDur)
	return tonePCM(rate, n, 12000)
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
	clock := h.codec.Info().RTPClockRate
	if clock == 0 {
		clock = h.codec.Info().SampleRate
	}
	var buf msrtp.Buffer
	stream := msrtp.NewSeqWriter(&buf).NewStream(h.audioPT, clock)
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
	require.NotEmpty(h.t, digit)
	pt := h.dtmfPT
	if pt == 0 {
		pt = testDTMFPT
	}
	var payload [4]byte
	n, err := dtmf.Encode(payload[:], dtmf.Event{
		Digit:  digit[0],
		Volume: 10,
		Dur:    800,
		End:    true,
	})
	require.NoError(h.t, err)
	h.injectRTP(&rtp.Packet{
		Header: rtp.Header{
			Version:        2,
			PayloadType:    pt,
			SequenceNumber: 1,
			Timestamp:      ts,
			SSRC:           ssrc,
			Marker:         true,
		},
		Payload: payload[:n],
	})
}

func (h *pipelineHarness) runDirections(t *testing.T) {
	t.Run("audio_from_room", h.testAudioFromRoom)
	t.Run("audio_from_port", h.testAudioFromPort)
	t.Run("dtmf_from_room", h.testDTMFFromRoom)
	t.Run("dtmf_from_port", h.testDTMFFromPort)
}

func (h *pipelineHarness) testAudioFromRoom(t *testing.T) {
	h.drainRemote()
	sample := h.roomFrame()
	for range 5 {
		require.NoError(t, h.audioOut.WriteSample(sample))
	}
	deadline := time.Now().Add(time.Second)
	found := false
	for time.Now().Before(deadline) && !found {
		pkt, ok := h.readRemotePacket(50 * time.Millisecond)
		if !ok {
			continue
		}
		if pkt.PayloadType == h.audioPT && len(pkt.Payload) > 0 {
			found = true
		}
	}
	require.True(t, found, "expected RTP audio toward the peer")
}

func (h *pipelineHarness) testAudioFromPort(t *testing.T) {
	before := h.roomAudio.len()
	packetsBefore := h.packetCount.Load()
	clock := h.codec.Info().RTPClockRate
	if clock == 0 {
		clock = h.codec.Info().SampleRate
	}
	samplesPerFrame := uint32(clock / int(time.Second/msrtp.DefFrameDur))
	sample := h.codecFrame()
	for i := uint16(0); i < 5; i++ {
		h.injectAudio(0xA11CE, 1+i, samplesPerFrame+uint32(i)*samplesPerFrame, sample)
	}
	require.Eventually(t, func() bool {
		return h.packetCount.Load() >= packetsBefore+5
	}, time.Second, 5*time.Millisecond, "RTP should be accepted")
	require.Eventually(t, func() bool {
		return h.roomAudio.len() > before
	}, time.Second, 5*time.Millisecond, "decoded PCM should reach room (packets=%d input=%d failed=%d ignored=%d room=%d)",
		h.packetCount.Load(),
		h.pipeline.conf.stats.InputPackets.Load(),
		h.pipeline.conf.stats.FailedPackets.Load(),
		h.pipeline.conf.stats.IgnoredPackets.Load(),
		h.roomAudio.len(),
	)
	require.Greater(t, pcmEnergy(h.roomAudio.since(before)), int64(0), "decoded room audio should carry energy")
}

func (h *pipelineHarness) testDTMFFromRoom(t *testing.T) {
	h.drainRemote()
	if h.dtmfPT == 0 {
		require.NoError(t, h.dtmfOut.WriteSample("5"))
		h.drainRemote()
		return
	}

	// dtmf.Write paces a 250ms tone on a real ticker. Assert the first
	// telephone-event and let pipeline Close cancel the rest.
	go func() {
		_ = h.dtmfOut.WriteSample("5")
	}()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		pkt, ok := h.readRemotePacket(20 * time.Millisecond)
		if ok && pkt.PayloadType == h.dtmfPT {
			return
		}
	}
	t.Fatal("DTMF enabled: expected telephone-event RTP")
}

func (h *pipelineHarness) testDTMFFromPort(t *testing.T) {
	before := len(h.roomDTMF.snapshot())
	packetsBefore := h.packetCount.Load()
	h.injectDTMFDigit(0xD7DF, "7", 8000)
	require.Eventually(t, func() bool {
		return h.packetCount.Load() > packetsBefore
	}, time.Second, 5*time.Millisecond, "RTP should be accepted")
	if h.dtmfPT == 0 {
		require.Equal(t, before, len(h.roomDTMF.snapshot()), "DTMF disabled: must not reach room")
		return
	}
	require.Eventually(t, func() bool {
		return len(h.roomDTMF.snapshot()) > before
	}, time.Second, 5*time.Millisecond)
	got := h.roomDTMF.snapshot()[before:]
	if assert.NotEmpty(t, got) {
		assert.Equal(t, "7", got[0])
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

type testCodecSpec struct {
	name string
	sdp  string
}

type testDTMFSpec struct {
	name  string
	pt    byte
	audio bool
}

var (
	pipelineTestCodecs = allAudioCodecs()
	pipelineTestRates  = []int{8000, 16000, 48000}
	pipelineTestDTMF   = []testDTMFSpec{
		{name: "dtmf_disabled", pt: 0, audio: false},
		{name: "dtmf_event", pt: testDTMFPT, audio: false},
		{name: "dtmf_event_audio", pt: testDTMFPT, audio: true},
	}
)

func TestMediaPipelinePermutations(t *testing.T) {
	for _, spec := range pipelineTestCodecs {
		t.Run(spec.Info().SDPName, func(t *testing.T) {
			codec := spec.(msdk.AudioCodec)
			pt := testAudioPT(codec)
			for _, rate := range pipelineTestRates {
				for _, d := range pipelineTestDTMF {
					t.Run(fmt.Sprintf("%dHz/%s", rate, d.name), func(t *testing.T) {
						h := newPipelineHarness(t, rate)
						h.configure(codec, pt, d.pt, d.audio)
						h.runDirections(t)
					})
				}
			}
		})
	}
}

func TestMediaPipelineTeardownMultiSSRC(t *testing.T) {
	for _, jitter := range []bool{false, true} {
		t.Run(fmt.Sprintf("jitter=%v", jitter), func(t *testing.T) {
			codec := audioCodecByName(t, g711.ULawSDPNameAndRate)
			h := newPipelineHarness(t, RoomSampleRate)
			h.jitter = jitter
			h.configure(codec, testAudioPT(codec), testDTMFPT, false)
			sample := h.codecFrame()

			h.injectAudio(0x11111111, 1, 160, sample)
			h.injectAudio(0x22222222, 1, 160, sample)

			require.Eventually(t, func() bool {
				return h.ssrcCount.Load() >= 2 && h.packetCount.Load() >= 2
			}, time.Second, 5*time.Millisecond, "expected AcceptStream and HandleRTP for two SSRCs")
			assert.Equal(t, uint64(2), h.ssrcCount.Load())
			assert.Equal(t, uint64(2), h.packetCount.Load())

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
		})
	}
}

func TestMediaPipelineConcurrentSSRCPump(t *testing.T) {
	for _, jitter := range []bool{false, true} {
		t.Run(fmt.Sprintf("jitter=%v", jitter), func(t *testing.T) {
			const (
				ssrcCount = 3
				packets   = 30 // Currently the built-in limit of media-sdk's ssrc mux
			)
			codec := audioCodecByName(t, g711.ULawSDPNameAndRate)
			h := newPipelineHarness(t, RoomSampleRate)
			h.jitter = jitter
			h.configure(codec, testAudioPT(codec), testDTMFPT, false)

			silence := make(msdk.PCM16Sample, codec.Info().SampleRate/int(time.Second/msrtp.DefFrameDur))
			var encoded msrtp.Buffer
			clock := codec.Info().RTPClockRate
			if clock == 0 {
				clock = codec.Info().SampleRate
			}
			enc := msrtp.EncodePCM(msrtp.NewSeqWriter(&encoded).NewStream(h.audioPT, clock), h.codec)
			require.NoError(t, enc.WriteSample(silence))
			require.NoError(t, enc.Close())
			require.NotEmpty(t, encoded, "codec produced no RTP")
			payload := slices.Clone(encoded[0].Payload)

			pkt := &rtp.Packet{
				Header: rtp.Header{
					Version:     2,
					PayloadType: h.audioPT,
				},
				Payload: payload,
			}
			for i := range packets {
				pkt.SequenceNumber = uint16(i)
				pkt.SSRC = uint32(i % ssrcCount)
				h.injectRTP(pkt)
			}

			require.Eventually(t, func() bool { return h.ssrcCount.Load() == ssrcCount }, time.Second, time.Millisecond, "expected %d SSRCs", ssrcCount)
			assert.Eventually(t, func() bool { return h.packetCount.Load() == packets }, time.Second, time.Millisecond, "expected %d packets", packets)
			assert.Equal(t, uint64(ssrcCount), h.ssrcCount.Load())
			assert.Equal(t, uint64(packets), h.packetCount.Load())

			done := make(chan error, 1)
			go func() {
				done <- h.pipeline.Close()
			}()
			select {
			case err := <-done:
				require.NoError(t, err)
			case <-time.After(2 * time.Second):
				t.Fatal("pipeline.Close hung under concurrent SSRC pumps")
			}
		})
	}
}

// Reproduces a PBX transfer: the far end keeps the same 5-tuple but starts
// relaying a different source, so LiveKit sees a new SSRC whose sequence
// numbers are far *behind* the previous stream. Before the fix, every packet
// of the new stream was rejected by the shared jitter buffer as "expired".
func TestMediaPipelineJitterNewSSRCBackwardSeq(t *testing.T) {
	const (
		oldSSRC = uint32(0xA0000001)
		newSSRC = uint32(0xB0000002)
		oldSeq  = uint16(21280) // old stream ends near 21294 like the captured call
		newSeq  = uint16(12676) // new stream starts here: 8618 behind, inside the 32768 half-space
		perSSRC = 15
	)
	codec := audioCodecByName(t, g711.ULawSDPNameAndRate)
	h := newPipelineHarness(t, RoomSampleRate)
	h.jitter = true
	h.configure(codec, testAudioPT(codec), testDTMFPT, false)

	clock := codec.Info().RTPClockRate
	if clock == 0 {
		clock = codec.Info().SampleRate
	}
	samplesPerFrame := uint32(clock / int(time.Second/msrtp.DefFrameDur))
	sample := h.codecFrame()

	// Phase 1: original stream decodes.
	// Pace the injections: media-sdk's per-SSRC read stream buffers 10 packets
	// and drops on overflow, so a tight burst would lose packets at the socket.
	for i := uint16(0); i < perSSRC; i++ {
		h.injectAudio(oldSSRC, oldSeq+i, samplesPerFrame*uint32(i+1), sample)
		time.Sleep(2 * time.Millisecond)
	}
	require.Eventually(t, func() bool { return h.roomAudio.len() > 0 },
		2*time.Second, 5*time.Millisecond, "original stream should decode")
	afterOld := h.roomAudio.len()

	// Phase 2: transfer. New SSRC, sequence space restarts far behind prevSN.
	for i := uint16(0); i < perSSRC; i++ {
		h.injectAudio(newSSRC, newSeq+i, samplesPerFrame*uint32(i+1), sample)
		time.Sleep(2 * time.Millisecond)
	}
	require.Eventually(t, func() bool { return h.packetCount.Load() >= 2*perSSRC },
		2*time.Second, 5*time.Millisecond, "all RTP should be read from the socket")
	require.Eventually(t, func() bool { return h.roomAudio.len() > afterOld },
		2*time.Second, 5*time.Millisecond,
		"audio from the new SSRC must reach the room (dropped=%d, audioPackets=%d)",
		h.pipeline.conf.stats.JitterBufferPacketsDropped.Load(),
		h.pipeline.conf.stats.AudioPackets.Load())
	require.Eventually(t, func() bool { return h.pipeline.conf.stats.AudioPackets.Load() >= 2*perSSRC },
		2*time.Second, 5*time.Millisecond, "every packet of both streams should pass the jitter buffer")
	assert.Equal(t, uint64(0), h.pipeline.conf.stats.JitterBufferPacketsDropped.Load(),
		"a new SSRC must not be treated as expired packets of the old one")
	assert.Equal(t, uint64(2), h.ssrcCount.Load())
}

func TestMediaPipelineJitterStatsAccumulateAcrossSSRC(t *testing.T) {
	codec := audioCodecByName(t, g711.ULawSDPNameAndRate)
	h := newPipelineHarness(t, RoomSampleRate)
	h.jitter = true
	h.configure(codec, testAudioPT(codec), testDTMFPT, false)

	clock := codec.Info().RTPClockRate
	if clock == 0 {
		clock = codec.Info().SampleRate
	}
	spf := uint32(clock / int(time.Second/msrtp.DefFrameDur))
	sample := h.codecFrame()

	// Each stream: seq 1,2,3 then skip 4,5 then 6,7,8 => 2 lost per stream.
	inject := func(ssrc uint32) {
		for _, seq := range []uint16{1, 2, 3, 6, 7, 8} {
			h.injectAudio(ssrc, seq, spf*uint32(seq), sample)
			time.Sleep(2 * time.Millisecond) // per-SSRC read stream drops on a 10-packet burst
		}
	}
	inject(0x1111)
	inject(0x2222)

	require.Eventually(t, func() bool { return h.packetCount.Load() >= 12 },
		2*time.Second, 5*time.Millisecond)
	// Loss is only declared once the gap expires (60ms jitter latency), so wait.
	require.Eventually(t, func() bool {
		return h.pipeline.conf.stats.JitterBufferPacketsLost.Load() >= 4
	}, 2*time.Second, 5*time.Millisecond, "lost=%d should be the sum over both SSRCs, not the last buffer's count",
		h.pipeline.conf.stats.JitterBufferPacketsLost.Load())
	// >= not ==: a scheduler stall can add a socket-level drop that is counted as loss.
	assert.GreaterOrEqual(t, h.pipeline.conf.stats.JitterBufferPacketsLost.Load(), uint64(4))
	assert.Equal(t, uint64(0), h.pipeline.conf.stats.JitterBufferPacketsDropped.Load())
}

// Covers the defensive "pipeline already closed" path: once muxToRoom is
// cleared, newStreamHandler returns nil and a read loop that starts in that
// state drains its stream, counting packets as ignored, without touching the
// chain. Close() cannot reach this state today (it waits for the read loops
// first), so the test forces it directly.
func TestMediaPipelineReadLoopAfterHandlerCleared(t *testing.T) {
	codec := audioCodecByName(t, g711.ULawSDPNameAndRate)
	h := newPipelineHarness(t, RoomSampleRate)
	h.jitter = true
	h.configure(codec, testAudioPT(codec), testDTMFPT, false)

	old := h.pipeline.muxToRoom.Swap(nil)
	require.NotNil(t, old)
	require.Nil(t, h.pipeline.newStreamHandler(), "no handler once the pipeline is marked closed")

	before := h.roomAudio.len()
	h.injectAudio(0xDEAD, 1, 160, h.codecFrame())
	require.Eventually(t, func() bool {
		return h.pipeline.conf.stats.IgnoredPackets.Load() >= 1
	}, time.Second, 5*time.Millisecond, "packet should be drained and counted as ignored")
	assert.Equal(t, uint64(1), h.ssrcCount.Load())
	assert.Equal(t, before, h.roomAudio.len(), "nothing must reach the room without a handler")

	// Put the chain back so Close() releases it, then close with the draining loop still blocked in ReadRTP.
	h.pipeline.muxToRoom.Store(old)
	done := make(chan error, 1)
	go func() { done <- h.pipeline.Close() }()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("pipeline.Close hung with a draining read loop")
	}
}

func TestMediaPipelineReuseUDPConn(t *testing.T) {
	const rate = 48000
	d := pipelineTestDTMF[1] // event-only

	for _, from := range pipelineTestCodecs {
		t.Run("from_"+from.Info().SDPName, func(t *testing.T) {
			for _, to := range pipelineTestCodecs {
				t.Run("to_"+to.Info().SDPName, func(t *testing.T) {
					c1 := from.(msdk.AudioCodec)
					c2 := to.(msdk.AudioCodec)
					h := newPipelineHarness(t, rate)
					h.configure(c1, testAudioPT(c1), d.pt, d.audio)
					t.Run("gen1", h.runDirections)
					h.reconfigure(c2, testAudioPT(c2), d.pt, d.audio)
					t.Run("gen2", h.runDirections)
				})
			}
		})
	}
}

func generateDTMFPackets(t *testing.T, digits string) [][]*rtp.Packet {
	t.Helper()
	var buf msrtp.Buffer
	packets := make([][]*rtp.Packet, len(digits))
	last := len(buf)
	w := msrtp.NewSeqWriter(&buf).NewStream(101, dtmf.SampleRate)
	timestamp := uint32(1000)
	for i := range digits {
		err := dtmf.Write(context.Background(), nil, w, timestamp, digits[i:i+1])
		require.NoError(t, err)
		require.NotEmpty(t, buf)
		timestamp += uint32(dtmf.SampleRate / 2)
		packets[i] = slices.Clone(buf[last:])
		last = len(buf)
	}
	return packets
}

func dropPackets(t *testing.T, dropType string, packets []*rtp.Packet) []*rtp.Packet {
	t.Helper()
	switch dropType {
	case "none":
		return packets
	case "first":
		require.Greater(t, len(packets), 3)
		return packets[3:]
	case "last":
		require.Greater(t, len(packets), 3)
		return packets[:len(packets)-3]
	case "middle":
		require.Greater(t, len(packets), 6)
		ret := slices.Clone(packets[:3])
		ret = append(ret, packets[len(packets)-3:]...)
		return ret
	default:
		t.Fatal("unknown drop type: " + dropType)
		return nil
	}
}

func TestMediaPipelineDTMF(t *testing.T) {
	// Multi-digit test, including correct handling of lost packets
	digitCases := []string{"1", "12", "123"}
	lossCases := []string{"none", "first", "last", "middle"}

	for _, digits := range digitCases {
		packets := generateDTMFPackets(t, digits)
		for _, lossPackets := range lossCases {
			t.Run(fmt.Sprintf("digits=%s/loss=%s", digits, lossPackets), func(t *testing.T) {
				got := &dtmfCollector{}
				p := &mediaPortPipeline{dtmfHandler: got}
				p.lastDTMFEvent.Store(math.MaxUint64)
				for _, digitPackets := range packets {
					sendPackets := dropPackets(t, lossPackets, digitPackets)
					t.Logf("sending %d/%d packets", len(sendPackets), len(digitPackets))
					for _, pkt := range sendPackets {
						h := pkt.Header
						t.Logf("sending packet: seq=%d, ts=%d, marker=%t", h.SequenceNumber, h.Timestamp, h.Marker)
						require.NoError(t, p.handleEventRTP(&h, pkt.Payload))
					}
				}
				t.Logf("sent: %s", digits)
				t.Logf("got: %s", got.String())
				require.Equal(t, digits, got.String())
			})
		}
	}
}

func TestMediaPortDTMFSameTimestamp(t *testing.T) {
	// Verify that each distinct event code is still reported even when timestamps are shared.
	// The spec is clear on needing a separate timestamp, but some carriers don't adhere to it.
	got := &dtmfCollector{}
	p := &mediaPortPipeline{dtmfHandler: got}
	p.lastDTMFEvent.Store(math.MaxUint64)
	digits := "123"
	for i := range digits {
		var encoded [4]byte
		n, err := dtmf.Encode(encoded[:], dtmf.Event{
			Digit:  digits[i],
			Volume: 10,
			Dur:    800,
			End:    true,
		})
		require.NoError(t, err)
		h := &rtp.Header{
			Version:        2,
			PayloadType:    testDTMFPT,
			SequenceNumber: uint16(i),
			Timestamp:      uint32(100000), // Ruse this timestamp for all digits
			SSRC:           12345,
			Marker:         true,
		}
		require.NoError(t, p.handleEventRTP(h, encoded[:n]))
	}
	require.Equal(t, digits, got.String())
}
