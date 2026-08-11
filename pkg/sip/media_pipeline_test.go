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
	"io"
	"net"
	"net/netip"
	"os"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/stretchr/testify/require"

	msdk "github.com/livekit/media-sdk"
	"github.com/livekit/media-sdk/dtmf"
	"github.com/livekit/media-sdk/g711"
	msrtp "github.com/livekit/media-sdk/rtp"
	"github.com/livekit/media-sdk/sdp"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
)

const (
	testAudioPT   = byte(0) // PCMU
	testDTMFPT    = byte(101)
	testCodecRate = 8000
)

// In-memory UDP pipe for pipeline tests (same shape as media_port_test's testUDPConn).
type pipelineUDPConn struct {
	addr     netip.AddrPort
	closed   chan struct{}
	buf      chan []byte
	peer     atomic.Pointer[pipelineUDPConn]
	deadline chan time.Time
}

func (c *pipelineUDPConn) Read(b []byte) (int, error) {
	n, _, err := c.ReadFromUDPAddrPort(b)
	return n, err
}

func (c *pipelineUDPConn) Write(b []byte) (int, error) {
	return c.WriteToUDPAddrPort(b, netip.AddrPort{})
}

func (c *pipelineUDPConn) RemoteAddr() net.Addr {
	p := c.peer.Load()
	if p == nil {
		return &net.UDPAddr{}
	}
	return p.LocalAddr()
}

func (c *pipelineUDPConn) SetDeadline(t time.Time) error {
	return c.SetReadDeadline(t)
}

func (c *pipelineUDPConn) SetReadDeadline(t time.Time) error {
	select {
	case c.deadline <- t:
	default:
	}
	return nil
}

func (c *pipelineUDPConn) SetWriteDeadline(time.Time) error { return nil }

func (c *pipelineUDPConn) ReadFromUDPAddrPort(buf []byte) (int, netip.AddrPort, error) {
	peer := c.peer.Load()
	if peer == nil {
		return 0, netip.AddrPort{}, io.ErrClosedPipe
	}
	var curDeadline time.Time
	for {
		var deadlineCh <-chan time.Time
		if !curDeadline.IsZero() {
			deadlineCh = time.After(time.Until(curDeadline))
		}
		select {
		case <-c.closed:
			return 0, netip.AddrPort{}, io.ErrClosedPipe
		case <-deadlineCh:
			return 0, netip.AddrPort{}, os.ErrDeadlineExceeded
		case newDeadline := <-c.deadline:
			if !newDeadline.IsZero() && (newDeadline.Before(curDeadline) || curDeadline.IsZero()) {
				curDeadline = newDeadline
			}
			continue
		case data := <-c.buf:
			n := copy(buf, data)
			var err error
			if n < len(data) {
				err = io.ErrShortBuffer
			}
			return n, peer.addr, err
		}
	}
}

func (c *pipelineUDPConn) WriteToUDPAddrPort(buf []byte, addr netip.AddrPort) (int, error) {
	peer := c.peer.Load()
	if peer == nil {
		return 0, io.ErrClosedPipe
	} else if peer.addr.String() != addr.String() {
		panic("unexpected address")
	}
	buf = slices.Clone(buf)
	select {
	default:
		return 0, io.ErrShortWrite
	case <-peer.closed:
		return 0, io.ErrClosedPipe
	case peer.buf <- buf:
		return len(buf), nil
	}
}

func (c *pipelineUDPConn) LocalAddr() net.Addr {
	return &net.UDPAddr{
		IP:   c.addr.Addr().AsSlice(),
		Port: int(c.addr.Port()),
	}
}

func (c *pipelineUDPConn) Close() error {
	if c.peer.Swap(nil) != nil {
		close(c.closed)
	}
	return nil
}

func newPipelineUDPPipe() (c1, c2 *pipelineUDPConn) {
	c1 = &pipelineUDPConn{
		addr:     netip.AddrPortFrom(netip.AddrFrom4([4]byte{1, 1, 1, 1}), 10000),
		buf:      make(chan []byte, 256),
		closed:   make(chan struct{}),
		deadline: make(chan time.Time, 1),
	}
	c2 = &pipelineUDPConn{
		addr:     netip.AddrPortFrom(netip.AddrFrom4([4]byte{2, 2, 2, 2}), 20000),
		buf:      make(chan []byte, 256),
		closed:   make(chan struct{}),
		deadline: make(chan time.Time, 1),
	}
	c1.peer.Store(c2)
	c2.peer.Store(c1)
	return c1, c2
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
	local       *pipelineUDPConn
	remote      *pipelineUDPConn
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
	audioPT     byte
	dtmfPT      byte
}

func newPipelineHarness(t *testing.T, dtmfType byte, dtmfAudio bool) *pipelineHarness {
	t.Helper()
	local, remote := newPipelineUDPPipe()

	log := logger.GetLogger()
	port := newUDPConn(log, local, false)
	h := &pipelineHarness{
		t:         t,
		local:     local,
		remote:    remote,
		port:      port,
		roomAudio: new(msdk.PCM16Sample),
		roomDTMF:  &dtmfCollector{},
		audioPT:   testAudioPT,
		dtmfPT:    dtmfType,
	}

	codec, ok := sdp.CodecByName(g711.ULawSDPNameAndRate).(msdk.AudioCodec)
	require.True(t, ok)

	h.audioIn.Swap(msdk.NewPCM16BufferWriter(h.roomAudio, RoomSampleRate))
	h.dtmfIn.Swap(h.roomDTMF)

	pipe := &mediaPortPipeline{
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
			Type:     testAudioPT,
			DTMFType: dtmfType,
		},
	}
	audioToPort, dtmfToPort, err := pipe.Configure(mc, port, &h.audioIn, &h.dtmfIn)
	require.NoError(t, err)
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
	var ulaw g711.ULawSample
	ulaw.Encode(pcm)
	h.injectRTP(&rtp.Packet{
		Header: rtp.Header{
			Version:        2,
			PayloadType:    h.audioPT,
			SequenceNumber: seq,
			Timestamp:      ts,
			SSRC:           ssrc,
		},
		Payload: []byte(ulaw),
	})
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
	cases := []struct {
		name      string
		dtmfType  byte
		dtmfAudio bool
	}{
		{name: "dtmf_disabled", dtmfType: 0, dtmfAudio: false},
		{name: "dtmf_enabled", dtmfType: testDTMFPT, dtmfAudio: false},
		{name: "dtmf_enabled_with_audio", dtmfType: testDTMFPT, dtmfAudio: true},
	}

	frame := testCodecRate / int(time.Second/msrtp.DefFrameDur)

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newPipelineHarness(t, tc.dtmfType, tc.dtmfAudio)

			t.Run("audio_to_port", func(t *testing.T) {
				sample := tonePCM(testCodecRate, frame, 10000)
				require.NoError(t, h.audioOut.WriteSample(sample))
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
				sample := tonePCM(testCodecRate, frame, 12000)
				for i := uint16(0); i < 5; i++ {
					h.injectAudio(0xA11CE, 1+i, 160+uint32(i)*160, sample)
				}
				require.Eventually(t, func() bool {
					return h.packetCount.Load() >= 5
				}, time.Second, 5*time.Millisecond, "RTP should be accepted")
				require.Eventually(t, func() bool {
					return len(*h.roomAudio) > before
				}, time.Second, 5*time.Millisecond, "decoded PCM should reach room (packets=%d input=%d failed=%d ignored=%d room=%d)",
					h.packetCount.Load(),
					h.pipeline.stats.InputPackets.Load(),
					h.pipeline.stats.FailedPackets.Load(),
					h.pipeline.stats.IgnoredPackets.Load(),
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
				require.NotEmpty(t, got)
				require.Equal(t, "7", got[0].Digit)
			})
		})
	}
}

func TestMediaPipelineTeardownMultiSSRC(t *testing.T) {
	h := newPipelineHarness(t, testDTMFPT, false)
	frame := testCodecRate / int(time.Second/msrtp.DefFrameDur)
	sample := tonePCM(testCodecRate, frame, 8000)

	h.injectAudio(0x11111111, 1, 160, sample)
	h.injectAudio(0x22222222, 1, 160, sample)

	require.Eventually(t, func() bool {
		return h.ssrcCount.Load() >= 2
	}, time.Second, 5*time.Millisecond, "expected AcceptStream for two SSRCs")
	require.GreaterOrEqual(t, h.packetCount.Load(), uint64(2))

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
	local, remote := newPipelineUDPPipe()
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

		pipe := &mediaPortPipeline{
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
		audioToPort, dtmfToPort, err := pipe.Configure(mc, port, &audioIn, &dtmfIn)
		require.NoError(t, err)
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
		require.Equal(t, testAudioPT, pkt.PayloadType)
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
		require.Equal(t, testAudioPT, pkt.PayloadType)
	case <-time.After(time.Second):
		t.Fatal("second pipeline produced no RTP after Reopen")
	}
	require.NoError(t, pipe2.Close())

	_ = local.Close()
	_ = remote.Close()
}
