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
	"fmt"
	"io"
	"math"
	"net"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	msdk "github.com/livekit/media-sdk"
	"github.com/livekit/media-sdk/rtp"
	"github.com/livekit/media-sdk/sdp"
	"github.com/livekit/mediatransportutil/pkg/rtcconfig"
	"github.com/livekit/protocol/logger"

	"github.com/livekit/sip/pkg/config"
	"github.com/livekit/sip/pkg/stats"
)

func newTestCallMonitor(t testing.TB) *stats.CallMonitor {
	mon, err := stats.NewMonitor(&config.Config{})
	require.NoError(t, err)
	require.NoError(t, mon.Start(&config.Config{}))
	t.Cleanup(mon.Stop)
	return mon.NewCall(stats.Inbound, "test", "test")
}

type testUDPConn struct {
	addr   netip.AddrPort
	closed chan struct{}
	buf    chan []byte
	peer   atomic.Pointer[testUDPConn]
}

func (c *testUDPConn) Read(b []byte) (int, error) {
	n, _, err := c.ReadFromUDPAddrPort(b)
	return n, err
}

func (c *testUDPConn) Write(b []byte) (int, error) {
	return c.WriteToUDPAddrPort(b, netip.AddrPort{})
}

func (c *testUDPConn) RemoteAddr() net.Addr {
	p := c.peer.Load()
	if p == nil {
		return &net.UDPAddr{}
	}
	return p.LocalAddr()
}

func (c *testUDPConn) SetDeadline(t time.Time) error {
	return nil
}

func (c *testUDPConn) SetReadDeadline(t time.Time) error {
	return nil
}

func (c *testUDPConn) SetWriteDeadline(t time.Time) error {
	return nil
}

func (c *testUDPConn) ReadFromUDPAddrPort(buf []byte) (int, netip.AddrPort, error) {
	peer := c.peer.Load()
	if peer == nil {
		return 0, netip.AddrPort{}, io.ErrClosedPipe
	}
	select {
	case <-c.closed:
		return 0, netip.AddrPort{}, io.ErrClosedPipe
	case data := <-c.buf:
		n := copy(buf, data)
		var err error
		if n < len(data) {
			err = io.ErrShortBuffer
		}
		return n, peer.addr, err
	}
}

func (c *testUDPConn) WriteToUDPAddrPort(buf []byte, addr netip.AddrPort) (int, error) {
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

func (c *testUDPConn) LocalAddr() net.Addr {
	return &net.UDPAddr{
		IP:   c.addr.Addr().AsSlice(),
		Port: int(c.addr.Port()),
	}
}

func (c *testUDPConn) Close() error {
	if c.peer.Swap(nil) != nil {
		close(c.closed)
	}
	return nil
}

func newTestConn(i int) *testUDPConn {
	return &testUDPConn{
		addr: netip.AddrPortFrom(
			netip.AddrFrom4([4]byte{byte(i), byte(i), byte(i), byte(i)}),
			uint16(10000*i),
		),
		buf:    make(chan []byte, 10),
		closed: make(chan struct{}),
	}
}

func newUDPPipe() (c1, c2 *testUDPConn) {
	c1 = newTestConn(1)
	c2 = newTestConn(2)
	c1.peer.Store(c2)
	c2.peer.Store(c1)
	return
}

func PrintAudioInWriter(p *MediaPort) string {
	return p.audioInHandler.(fmt.Stringer).String()
}

func newIP(v string) netip.Addr {
	ip, err := netip.ParseAddr(v)
	if err != nil {
		panic(err)
	}
	return ip
}

func TestMediaPortUpdateRemote(t *testing.T) {
	log := logger.GetLogger()
	mon := newTestCallMonitor(t)

	// newUDPPipe wires two in-memory testUDPConn together.
	c1, _ := newUDPPipe()
	mp, err := NewMediaPortWith(1, log, mon, c1, &MediaOptions{
		IP: netip.MustParseAddr("127.0.0.1"),
	}, 8000)
	require.NoError(t, err)
	defer mp.Close()

	// Initially no destination is set.
	require.False(t, mp.RemoteAddr().IsValid(), "RemoteAddr should be invalid before any update")

	// Update to a valid address.
	addr := netip.MustParseAddrPort("9.8.7.6:12345")
	mp.UpdateRemote(addr)
	require.Equal(t, addr, mp.RemoteAddr(), "RemoteAddr should reflect the updated address")

	// UpdateRemote with invalid addr should be a no-op.
	mp.UpdateRemote(netip.AddrPort{})
	require.Equal(t, addr, mp.RemoteAddr(), "UpdateRemote with invalid addr should not change RemoteAddr")
}

func TestMediaPort(t *testing.T) {
	// Main resampler has unpredictable (although tiny) output delay
	// and other randomness in the generated samples.
	// Enable a predictable resampler to avoid flaky tests.
	prevOpts := msdk.DefaultResampleOptions
	msdk.DefaultResampleOptions = []msdk.ResampleOption{
		msdk.WithPredictableResample(true),
	}
	defer func() {
		msdk.DefaultResampleOptions = prevOpts
	}()
	codecList := msdk.Codecs()
	for _, codec := range codecList {
		info := codec.Info()
		tname := strings.ReplaceAll(info.SDPName, "/", "-")
		t.Run(tname, func(t *testing.T) {
			codecs := msdk.NewCodecSet()
			codecs.SetEnabled(info.SDPName, true)

			sub := strings.SplitN(info.SDPName, "/", 2)
			codecName := sub[0]
			nativeRateSDP, err := strconv.Atoi(sub[1])
			nativeRate := nativeRateSDP
			require.NoError(t, err)
			switch codecName {
			case "telephone-event":
				t.SkipNow()
			case "G722":
				nativeRate *= 2 // error in RFC
			}

			for _, tconf := range []struct {
				Rate      int
				Encrypted sdp.Encryption
			}{
				{nativeRate, sdp.EncryptionNone},
				{48000, sdp.EncryptionRequire},
			} {
				suff := ""
				if tconf.Encrypted != sdp.EncryptionNone {
					suff = " srtp"
				}
				t.Run(fmt.Sprintf("%d%s", tconf.Rate, suff), func(t *testing.T) {
					c1, c2 := newUDPPipe()

					log := logger.GetLogger()

					const (
						ip1   = "1.1.1.1"
						ip2   = "2.2.2.2"
						port1 = 10000
						port2 = 20000
					)
					testRate := tconf.Rate
					bobToAliceNoResample := testRate == 8000

					alicePort, err := NewMediaPortWith(1, log.WithName("Alice"), newTestCallMonitor(t), c1, &MediaOptions{
						IP:              newIP(ip1),
						Ports:           rtcconfig.PortRange{Start: port1},
						NoInputResample: bobToAliceNoResample,
					}, testRate)
					require.NoError(t, err)
					defer alicePort.Close()

					bobPort, err := NewMediaPortWith(2, log.WithName("Bob"), newTestCallMonitor(t), c2, &MediaOptions{
						IP:    newIP(ip2),
						Ports: rtcconfig.PortRange{Start: port2},
					}, testRate)
					require.NoError(t, err)
					defer bobPort.Close()

					// Alice sends an offer to Bob

					offer, err := alicePort.NewOffer(codecs, tconf.Encrypted)
					require.NoError(t, err)
					offerData, err := offer.SDP.Marshal()
					require.NoError(t, err)

					t.Logf("SDP offer:\n%s", string(offerData))

					answer, bobConf, err := bobPort.SetOffer(offerData, codecs, tconf.Encrypted)
					require.NoError(t, err)
					answerData, err := answer.SDP.Marshal()
					require.NoError(t, err)

					t.Logf("SDP answer:\n%s", string(answerData))

					aliceConf, _, err := alicePort.SetAnswer(offer, answerData, codecs, tconf.Encrypted)
					require.NoError(t, err)

					err = alicePort.SetConfig(aliceConf)
					require.NoError(t, err)

					err = bobPort.SetConfig(bobConf)
					require.NoError(t, err)

					aliceAudio := alicePort.Config().Audio
					bobAudio := bobPort.Config().Audio

					aliceCodec := aliceAudio.Codec
					bobCodec := bobAudio.Codec

					require.Equal(t, info.SDPName, aliceCodec.Info().SDPName)
					require.Equal(t, info.SDPName, bobCodec.Info().SDPName)

					// Buffers should match the rate of the samples we write.

					var aliceRecvBuf msdk.PCM16Sample
					aliceHandler := msdk.NewPCM16BufferWriter(&aliceRecvBuf, testRate)
					alicePort.WriteAudioTo(aliceHandler)

					var bobRecvBuf msdk.PCM16Sample
					bobHandler := msdk.NewPCM16BufferWriter(&bobRecvBuf, testRate)
					bobPort.WriteAudioTo(bobHandler)

					aliceToBob := alicePort.GetAudioWriter()
					bobToAlice := bobPort.GetAudioWriter()

					aliceToBobWriteChain := aliceToBob.String()
					bobToAliceWriteChain := bobToAlice.String()

					bobToAliceHandleChain := PrintAudioInWriter(alicePort)
					aliceToBobHandleChain := PrintAudioInWriter(bobPort)

					t.Log("A -> B (write)", aliceToBobWriteChain)
					t.Log("B -> A (write)", bobToAliceWriteChain)

					t.Log("B -> A (handle)", bobToAliceHandleChain)
					t.Log("A -> B (handle)", aliceToBobHandleChain)

					t.Log("resample", !bobToAliceNoResample)

					packetSize := testRate / int(time.Second/rtp.DefFrameDur)
					aliceToBobSamples := make(msdk.PCM16Sample, packetSize)
					bobToAliceSamples := make(msdk.PCM16Sample, packetSize)
					const (
						amp1 = 10000
						amp2 = 5000
						freq = 10
					)
					for i := range packetSize {
						aliceToBobSamples[i] = int16(amp1 * math.Sin(freq*2*math.Pi*float64(i)/float64(packetSize)))
						bobToAliceSamples[i] = int16(amp2 * math.Sin(freq*2*math.Pi*float64(i)/float64(packetSize)))
					}

					aliceToBobWrites := 1
					bobToAliceWrites := 1
					if tconf.Rate == nativeRate {
						expChainBase := fmt.Sprintf("Switch(%d) -> LatencyEntry -> %s(encode) -> ByteEncoder(%d) -> StatsWriter(%s/%d) -> LatencyExit",
							nativeRate, codecName, nativeRate, codecName, nativeRateSDP)
						require.Equal(t, fmt.Sprintf("%s -> RTPWriteStream(%s:%d)", expChainBase, ip2, port2), aliceToBobWriteChain)
						require.Equal(t, fmt.Sprintf("%s -> RTPWriteStream(%s:%d)", expChainBase, ip1, port1), bobToAliceWriteChain)

						expChainBase = fmt.Sprintf("SilenceFiller(25) -> RTP(%%d) -> ByteDecoder -> %s(decode) -> LatencyExit -> Switch(%d) -> Buffer(%d)", codecName, nativeRate, nativeRate)
						require.Equal(t, fmt.Sprintf(expChainBase, aliceAudio.Type), bobToAliceHandleChain)
						require.Equal(t, fmt.Sprintf(expChainBase, bobAudio.Type), aliceToBobHandleChain)
					} else {
						expChain := fmt.Sprintf("Switch(48000) -> Resample(48000->%d) -> LatencyEntry -> %s(encode) -> ByteEncoder(%d) -> StatsWriter(%s/%d) -> LatencyExit -> SRTPWriteStream",
							nativeRate, codecName, nativeRate, codecName, nativeRateSDP)
						require.Equal(t, expChain, aliceToBobWriteChain)
						require.Equal(t, expChain, bobToAliceWriteChain)

						// This side does not resample the received audio, it uses sample rate of the RTP source.
						var expChainAlice string
						if bobToAliceNoResample {
							expChainAlice = fmt.Sprintf("SilenceFiller(25) -> RTP(%d) -> ByteDecoder -> %s(decode) -> LatencyExit -> Switch(%d) -> Buffer(%d)", aliceAudio.Type, codecName, nativeRate, nativeRate)
						} else {
							expChainAlice = fmt.Sprintf("SilenceFiller(25) -> RTP(%d) -> ByteDecoder -> %s(decode) -> Resample(%d->48000) -> LatencyExit -> Switch(48000) -> Buffer(48000)", aliceAudio.Type, codecName, nativeRate)
						}

						// This side resamples the received audio to the expected sample rate.
						expChainBob := fmt.Sprintf("SilenceFiller(25) -> RTP(%d) -> ByteDecoder -> %s(decode) -> Resample(%d->48000) -> LatencyExit -> Switch(48000) -> Buffer(48000)", bobAudio.Type, codecName, nativeRate)

						require.Equal(t, expChainAlice, bobToAliceHandleChain)
						require.Equal(t, expChainBob, aliceToBobHandleChain)
					}
					// Ramp-up time for the codec.
					// Some codecs have "inertia" and cannot immediately represent the sound exactly.
					// This is shy we write signal multiple times to give it some time to adapt.
					// We will also cut the ramp-up part from the destination buffer before comparing.
					// This variable is in full frames, so that we clearly see where frames start to calculate the offset below.
					rampUpFrames := 0
					// Some codecs have an extra buffering internally, and we have to offset the compared sample
					// by this number of sampled values.
					offsetSamples := 0

					switch codecName {
					case "G722":
						rampUpFrames += 1
						offsetSamples += 22
					case "AMR-WB":
						rampUpFrames += 1
						offsetSamples += 14 + 16
					}
					aliceToBobWrites += rampUpFrames
					bobToAliceWrites += rampUpFrames
					discard := rampUpFrames * packetSize

					resampleMult := testRate / nativeRate
					offsetSamples *= resampleMult

					var wg sync.WaitGroup
					wg.Add(2)
					go func() {
						defer wg.Done()
						for range aliceToBobWrites {
							err := aliceToBob.WriteSample(aliceToBobSamples)
							require.NoError(t, err)
						}
					}()
					go func() {
						defer wg.Done()
						for range bobToAliceWrites {
							err := bobToAlice.WriteSample(bobToAliceSamples)
							require.NoError(t, err)
						}
					}()
					wg.Wait()

					time.Sleep(time.Second / 4)

					// Cut buffers earlier, otherwise we might get extra samples
					// that we added to push resampler forward.
					aliceHandler.Close()
					bobHandler.Close()

					alicePort.Close()
					bobPort.Close()

					checkPCM(t, "A -> B", aliceToBobSamples[:packetSize-offsetSamples], bobRecvBuf[discard+offsetSamples:])
					checkPCM(t, "B -> A", bobToAliceSamples[:packetSize-offsetSamples], aliceRecvBuf[discard+offsetSamples:])
				})
			}
		})
	}

}

func checkPCM(t testing.TB, name string, exp, got msdk.PCM16Sample) {
	t.Helper()
	require.Equal(t, len(exp), len(got))

	minV := slices.Min(exp)
	maxV := slices.Max(exp)

	// Allow 10% of deviation from original.
	const perc = 0.1
	delta := int16(math.Abs(float64(maxV-minV) * perc))

	hits := 0

	var minD, maxD int16 = math.MaxInt16, 0
	for i, v := range got {
		dv := v - exp[i]
		if dv < 0 {
			dv = -dv
		}
		if dv < delta {
			hits++
		}
		minD = min(minD, dv)
		maxD = max(maxD, dv)
	}

	// 90% of the samples should match.
	const percHit = 0.90
	expHit := int(float64(len(exp)) * percHit)
	require.True(t, hits >= expHit, "%s: insufficient number of good samples: %v/%v\nminD=%v, maxD=%v, allowed=%v\nmin=%v, max=%v\nexp:\n%v\ngot:\n%v",
		name,
		hits, expHit,
		minD, maxD, delta,
		slices.Min(got), slices.Max(got),
		exp, got,
	)
}

func newMediaPair(t testing.TB, opt1, opt2 *MediaOptions) (m1, m2 *MediaPort) {
	return newMediaPairWithAddr(t, newIP("1.1.1.1"), newIP("2.2.2.2"), opt1, opt2)
}

func newMediaPairWithAddr(t testing.TB, ip1, ip2 netip.Addr, opt1, opt2 *MediaOptions) (m1, m2 *MediaPort) {
	if opt1 == nil {
		opt1 = &MediaOptions{}
	}
	if opt2 == nil {
		opt2 = &MediaOptions{}
	}
	c1, c2 := newUDPPipe()

	codecs := defaultCodecs

	opt1.IP = ip1
	opt1.Ports = rtcconfig.PortRange{Start: 10000}
	opt1.NoInputResample = true

	opt2.IP = ip2
	opt2.Ports = rtcconfig.PortRange{Start: 20000}

	const rate = 16000

	log := logger.GetLogger()

	var err error

	m1, err = NewMediaPortWith(1, log.WithName("one"), newTestCallMonitor(t), c1, opt1, rate)
	require.NoError(t, err)
	t.Cleanup(m1.Close)

	m2, err = NewMediaPortWith(2, log.WithName("two"), newTestCallMonitor(t), c2, opt2, rate)
	require.NoError(t, err)
	t.Cleanup(m2.Close)

	offer, err := m1.NewOffer(codecs, sdp.EncryptionNone)
	require.NoError(t, err)
	offerData, err := offer.SDP.Marshal()
	require.NoError(t, err)

	answer, mc2, err := m2.SetOffer(offerData, codecs, sdp.EncryptionNone)
	require.NoError(t, err)
	answerData, err := answer.SDP.Marshal()
	require.NoError(t, err)

	mc1, _, err := m1.SetAnswer(offer, answerData, codecs, sdp.EncryptionNone)
	require.NoError(t, err)

	err = m1.SetConfig(mc1)
	require.NoError(t, err)

	err = m2.SetConfig(mc2)
	require.NoError(t, err)

	w2 := m2.GetAudioWriter()
	require.Equal(t, "Switch(16000) -> LatencyEntry -> G722(encode) -> ByteEncoder(16000) -> StatsWriter(G722/8000) -> LatencyExit -> RTPWriteStream(1.1.1.1:10000)", w2.String())

	return m1, m2
}

func TestMediaTimeout(t *testing.T) {
	const (
		timeout = time.Second / 4
		initial = timeout * 2
		dt      = timeout / 4
	)

	t.Run("initial", func(t *testing.T) {
		m1, _ := newMediaPair(t, &MediaOptions{
			MediaTimeoutInitial: initial,
			MediaTimeout:        timeout,
		}, nil)

		m1.EnableTimeout(true)

		targ := time.Now().Add(initial)
		select {
		case <-m1.Timeout():
			t.Fatal("initial timeout ignored")
		case <-time.After(initial / 2):
		}

		select {
		case <-time.After(time.Until(targ) + dt):
			t.Fatal("timeout didn't trigger")
		case <-m1.Timeout():
		}
	})

	t.Run("regular", func(t *testing.T) {
		m1, m2 := newMediaPair(t, &MediaOptions{
			MediaTimeoutInitial: initial,
			MediaTimeout:        timeout,
		}, nil)
		m1.EnableTimeout(true)

		w2 := m2.GetAudioWriter()
		err := w2.WriteSample(msdk.PCM16Sample{0, 0})
		require.NoError(t, err)

		select {
		case <-time.After(dt):
			t.Fatal("no media received")
		case <-m1.Received():
		}

		select {
		case <-time.After(2*timeout + dt):
			t.Fatal("timeout didn't trigger")
		case <-m1.Timeout():
		}
	})

	t.Run("no timeout", func(t *testing.T) {
		m1, m2 := newMediaPair(t, &MediaOptions{
			MediaTimeoutInitial: initial,
			MediaTimeout:        timeout,
		}, nil)
		m1.EnableTimeout(true)

		w2 := m2.GetAudioWriter()

		for i := 0; i < 10; i++ {
			err := w2.WriteSample(msdk.PCM16Sample{0, 0})
			require.NoError(t, err)

			select {
			case <-time.After(timeout / 2):
			case <-m1.Timeout():
				t.Fatal("timeout")
			}
		}
	})

	t.Run("reset timeout after media", func(t *testing.T) {
		m1, m2 := newMediaPair(t, &MediaOptions{
			MediaTimeoutInitial: initial,
			MediaTimeout:        timeout,
		}, nil)
		m1.EnableTimeout(true)

		w2 := m2.GetAudioWriter()

		for i := 0; i < 5; i++ {
			err := w2.WriteSample(msdk.PCM16Sample{0, 0})
			require.NoError(t, err)

			select {
			case <-time.After(timeout / 2):
			case <-m1.Timeout():
				t.Fatal("timeout")
			}
		}

		// Once media has flowed, SetTimeout does not re-enter the initial window —
		// the general timeout applies relative to the last received RTP packet.
		// Last packet arrived at most timeout/2 ago, so the timeout should fire
		// within ~timeout from now, well before initial would elapse.
		m1.SetTimeout(initial, timeout)

		select {
		case <-time.After(timeout + dt):
			t.Fatal("timeout didn't trigger")
		case <-m1.Timeout():
		}
	})

	t.Run("reset timeout before any media", func(t *testing.T) {
		m1, _ := newMediaPair(t, &MediaOptions{
			MediaTimeoutInitial: initial,
			MediaTimeout:        timeout,
		}, nil)
		m1.EnableTimeout(true)

		// No media has ever arrived. SetTimeout re-arms startTime, and since the
		// port has never seen an RTP packet, the new initial window applies from
		// the moment of the SetTimeout call.
		time.Sleep(initial / 2)
		m1.SetTimeout(initial, timeout)

		targ := time.Now().Add(initial)
		select {
		case <-m1.Timeout():
			t.Fatal("initial timeout fired too early")
		case <-time.After(initial / 2):
		}

		select {
		case <-time.After(time.Until(targ) + dt):
			t.Fatal("timeout didn't trigger")
		case <-m1.Timeout():
		}
	})

	t.Run("reset", func(t *testing.T) {
		m1, m2 := newMediaPair(t, &MediaOptions{
			MediaTimeoutInitial: initial,
			MediaTimeout:        timeout,
		}, nil)
		m1.EnableTimeout(true)

		w2 := m2.GetAudioWriter()

		for i := 0; i < 5; i++ {
			err := w2.WriteSample(msdk.PCM16Sample{0, 0})
			require.NoError(t, err)

			select {
			case <-time.After(timeout / 2):
			case <-m1.Timeout():
				t.Fatal("timeout")
			}
		}

		m1.SetTimeout(initial, timeout)

		for i := 0; i < 5; i++ {
			err := w2.WriteSample(msdk.PCM16Sample{0, 0})
			require.NoError(t, err)

			select {
			case <-time.After(timeout / 2):
			case <-m1.Timeout():
				t.Fatal("timeout")
			}
		}
	})
}

func TestSymmetricRTP(t *testing.T) {
	t.Run("disabled", func(t *testing.T) {
		m1, m2 := newMediaPair(t, &MediaOptions{SymmetricRTP: false}, nil)
		dstPtr := m1.port.dst.Load()
		require.NotNil(t, dstPtr)
		dst := *dstPtr
		require.True(t, dst.IsValid())

		c2 := m2.port.UDPConn.(*testUDPConn)
		newAddr := netip.AddrPortFrom(newIP("9.9.9.9"), 9999)
		c2.addr = newAddr

		err := m2.GetAudioWriter().WriteSample(msdk.PCM16Sample{0, 0})
		require.NoError(t, err)

		select {
		case <-m1.Received():
		case <-time.After(time.Second):
			t.Fatal("no media received")
		}

		curDstPtr := m1.port.dst.Load()
		require.NotNil(t, curDstPtr)
		require.Equal(t, dst, *curDstPtr)
	})

	t.Run("enabled", func(t *testing.T) {
		m1, m2 := newMediaPair(t, &MediaOptions{SymmetricRTP: true}, nil)
		dstPtr := m1.port.dst.Load()
		require.NotNil(t, dstPtr)
		require.True(t, dstPtr.IsValid())

		c2 := m2.port.UDPConn.(*testUDPConn)
		newAddr := netip.AddrPortFrom(newIP("9.9.9.9"), 9999)
		c2.addr = newAddr

		err := m2.GetAudioWriter().WriteSample(msdk.PCM16Sample{0, 0})
		require.NoError(t, err)

		select {
		case <-m1.Received():
		case <-time.After(time.Second):
			t.Fatal("no media received")
		}

		curDstPtr := m1.port.dst.Load()
		require.NotNil(t, curDstPtr)
		require.Equal(t, newAddr, *curDstPtr)
	})

	t.Run("auto", func(t *testing.T) {
		m1, m2 := newMediaPairWithAddr(t,
			newIP("1.1.1.1"), newIP("10.10.10.10"),
			&MediaOptions{IgnoreLocalAddrInSDP: true}, nil,
		)
		dstPtr := m1.port.dst.Load()
		require.NotNil(t, dstPtr)
		require.True(t, dstPtr.IsValid())
		symmetric := m1.port.symmetric.Load()
		require.True(t, symmetric)

		c2 := m2.port.UDPConn.(*testUDPConn)
		newAddr := netip.AddrPortFrom(newIP("3.3.3.3"), 9999)
		c2.addr = newAddr

		err := m2.GetAudioWriter().WriteSample(msdk.PCM16Sample{0, 0})
		require.NoError(t, err)

		select {
		case <-m1.Received():
		case <-time.After(time.Second):
			t.Fatal("no media received")
		}

		curDstPtr := m1.port.dst.Load()
		require.NotNil(t, curDstPtr)
		require.Equal(t, newAddr.String(), curDstPtr.String())
	})
}
