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
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	msdk "github.com/livekit/media-sdk"
	"github.com/livekit/media-sdk/rtp"
	"github.com/livekit/media-sdk/sdp"
	"github.com/livekit/media-sdk/srtp"
	"github.com/livekit/mediatransportutil/pkg/rtcconfig"
	"github.com/livekit/protocol/logger"

	"github.com/livekit/sip/pkg/config"
	"github.com/livekit/sip/pkg/stats"
)

const (
	parsedMetric  = "livekit_sip_sdp_parsed_total"
	offeredMetric = "livekit_sip_codec_offered_total"
)

func newTestCallMonitor(t testing.TB) *stats.CallMonitor {
	mon, err := stats.NewMonitor(&config.Config{})
	require.NoError(t, err)
	require.NoError(t, mon.Start(&config.Config{}))
	t.Cleanup(mon.Stop)
	return mon.NewCall(stats.Inbound, "test", "test")
}

func newTestMediaPort(t testing.TB, provider string) MediaPort {
	t.Helper()
	mon := newTestCallMonitor(t)
	mon.SetProvider(provider)
	mp, err := NewMediaPortWith(logger.NewTestLogger(t), mon, nil, &MediaOptions{
		IP: netip.MustParseAddr("127.0.0.1"),
	}, 8000)
	require.NoError(t, err)
	t.Cleanup(func() { mp.Close() })
	return mp
}

type testUDPConn struct {
	addr   netip.AddrPort
	closed chan struct{}
	buf    chan []byte
	peer   atomic.Pointer[testUDPConn]

	// Deadlines follow net.Conn: the latest value wins, zero clears it. The value
	// lives in the guarded field and kick only wakes a parked reader, so coalescing
	// a wakeup can never drop a deadline - which would strand Close() forever.
	dmu      sync.Mutex
	deadline time.Time
	kick     chan struct{}
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
	c.SetReadDeadline(t)
	return nil
}

func (c *testUDPConn) SetReadDeadline(t time.Time) error {
	c.dmu.Lock()
	c.deadline = t
	c.dmu.Unlock()
	select {
	case c.kick <- struct{}{}:
	default:
	}
	return nil
}

func (c *testUDPConn) readDeadline() time.Time {
	c.dmu.Lock()
	defer c.dmu.Unlock()
	return c.deadline
}

func (c *testUDPConn) SetWriteDeadline(t time.Time) error {
	return nil
}

func (c *testUDPConn) ReadFromUDPAddrPort(buf []byte) (int, netip.AddrPort, error) {
	peer := c.peer.Load()
	if peer == nil {
		return 0, netip.AddrPort{}, io.ErrClosedPipe
	}

	for {
		var (
			deadlineCh <-chan time.Time
			timer      *time.Timer
		)
		if dl := c.readDeadline(); !dl.IsZero() {
			timer = time.NewTimer(time.Until(dl))
			deadlineCh = timer.C
		}

		select {
		case <-c.closed:
			stopTimer(timer)
			return 0, netip.AddrPort{}, io.ErrClosedPipe
		case <-deadlineCh:
			return 0, netip.AddrPort{}, os.ErrDeadlineExceeded
		case <-c.kick:
			stopTimer(timer) // deadline changed, re-arm
			continue
		case data := <-c.buf:
			stopTimer(timer)
			n := copy(buf, data)
			var err error
			if n < len(data) {
				err = io.ErrShortBuffer
			}
			return n, peer.addr, err
		}
	}
}

func stopTimer(t *time.Timer) {
	if t != nil {
		t.Stop()
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
		buf:    make(chan []byte, 256),
		closed: make(chan struct{}),
		kick:   make(chan struct{}, 1),
	}
}

func newUDPPipe() (c1, c2 *testUDPConn) {
	c1 = newTestConn(1)
	c2 = newTestConn(2)
	c1.peer.Store(c2)
	c2.peer.Store(c1)
	return
}

func newIP(v string) netip.Addr {
	ip, err := netip.ParseAddr(v)
	if err != nil {
		panic(err)
	}
	return ip
}

// newTestPort is NewMediaPortWith for tests: it keeps the concrete type, so tests can reach
// the udpConn and the timeout controls that are not part of the MediaPort interface.
func newTestPort(t testing.TB, log logger.Logger, conn UDPConn, opts *MediaOptions, rate int) *mediaPort {
	t.Helper()
	mp, err := NewMediaPortWith(log, newTestCallMonitor(t), conn, opts, rate)
	require.NoError(t, err)
	t.Cleanup(mp.Close)
	return mp.(*mediaPort)
}

func offerAt(t testing.TB, addr netip.AddrPort) []byte {
	t.Helper()
	return offerAtEnc(t, addr, sdp.EncryptionNone)
}

func offerAtEnc(t testing.TB, addr netip.AddrPort, enc sdp.Encryption) []byte {
	t.Helper()
	offer, err := sdp.NewOfferWith(defaultCodecs, addr.Addr(), int(addr.Port()), enc)
	require.NoError(t, err)
	data, err := offer.SDP.Marshal()
	require.NoError(t, err)
	return data
}

func TestMediaPortUpdateRemote(t *testing.T) {
	c1, _ := newUDPPipe()
	mp := newTestPort(t, logger.NewTestLogger(t), c1, &MediaOptions{
		IP: netip.MustParseAddr("127.0.0.1"),
	}, RoomSampleRate)

	require.False(t, mp.RemoteAddr().IsValid(), "RemoteAddr should be invalid before any offer")

	addr := netip.MustParseAddrPort("9.8.7.6:12345")
	_, err := mp.GenerateAnswer(offerAt(t, addr), true)
	require.NoError(t, err)
	require.Equal(t, addr, mp.RemoteAddr(), "GenerateAnswer should set RemoteAddr from the offer")

	// Body-less re-INVITE: empty offer returns the local SDP and must not change dest.
	_, err = mp.GenerateAnswer(nil, true)
	require.NoError(t, err)
	require.Equal(t, addr, mp.RemoteAddr(), "empty offer should not change RemoteAddr")

	// Hold form c=0.0.0.0 must not clobber dest once media is established.
	_, err = mp.GenerateAnswer(offerAt(t, netip.MustParseAddrPort("0.0.0.0:12345")), true)
	require.NoError(t, err)
	require.Equal(t, addr, mp.RemoteAddr(), "offer with unspecified addr should not change RemoteAddr")

	// successful re-INVITE update
	addr = netip.MustParseAddrPort("10.10.10.10:54321")
	_, err = mp.GenerateAnswer(offerAt(t, addr), true)
	require.NoError(t, err)
	require.Equal(t, addr, mp.RemoteAddr(), "re-INVITE offer should update RemoteAddr")
}

// Re-INVITE with the original offer SDP and crypto material must result in
// re-use of already-negotiated keys.
func TestMediaPortReinviteSameCrypto(t *testing.T) {
	c1, _ := newUDPPipe()
	mp := newTestPort(t, logger.NewTestLogger(t), c1, &MediaOptions{
		IP:         netip.MustParseAddr("127.0.0.1"),
		Encryption: sdp.EncryptionRequire,
	}, RoomSampleRate)

	addr := netip.MustParseAddrPort("9.8.7.6:12345")
	offer := offerAtEnc(t, addr, sdp.EncryptionRequire)

	_, err := mp.GenerateAnswer(offer, true)
	require.NoError(t, err)
	require.Equal(t, addr, mp.RemoteAddr())

	require.NotNil(t, mp.negotiated)
	require.NotNil(t, mp.negotiated.Crypto)
	localKey := slices.Clone(mp.negotiated.Crypto.Keys.LocalMasterKey)
	localSalt := slices.Clone(mp.negotiated.Crypto.Keys.LocalMasterSalt)
	require.NotEmpty(t, localKey)
	require.NotEmpty(t, localSalt)
	localSDP, err := mp.GetLocalSDP()
	require.NoError(t, err)
	require.NotEmpty(t, localSDP)

	// Same offer bytes: NewOfferWith would generate a new peer key.
	_, err = mp.GenerateAnswer(offer, true)
	require.NoError(t, err, "re-INVITE with the same offer must be accepted")
	require.Equal(t, addr, mp.RemoteAddr(), "same offer must not change dest")
	require.Equal(t, localKey, mp.negotiated.Crypto.Keys.LocalMasterKey, "local master key must not change")
	require.Equal(t, localSalt, mp.negotiated.Crypto.Keys.LocalMasterSalt, "local master salt must not change")
	gotSDP, err := mp.GetLocalSDP()
	require.NoError(t, err)
	require.Equal(t, localSDP, gotSDP, "local SDP (including a=crypto) must not change")
}

func TestMediaPortReofferSameCrypto(t *testing.T) {
	c1, _ := newUDPPipe()
	mp := newTestPort(t, logger.NewTestLogger(t), c1, &MediaOptions{
		IP:         netip.MustParseAddr("127.0.0.1"),
		Encryption: sdp.EncryptionRequire,
	}, RoomSampleRate)

	newOffer := func(t testing.TB, mp *mediaPort, localCrypto []srtp.Profile) (*sdp.Offer, *sdp.MediaConfig) {
		t.Helper()
		addr := netip.MustParseAddrPort("9.8.7.6:12345")
		offerData, err := mp.GenerateOffer()
		require.NoError(t, err)
		offer, err := sdp.ParseOfferWith(defaultCodecs, offerData)
		require.NoError(t, err)
		answer, mc, err := offer.Answer(addr.Addr(), int(addr.Port()), sdp.EncryptionRequire, sdp.WithLocalProfiles(localCrypto))
		require.NoError(t, err)
		answerData, err := answer.SDP.Marshal()
		require.NoError(t, err)
		err = mp.ProcessAnswer(answerData)
		require.NoError(t, err)
		require.Nil(t, mp.offer)
		return offer, mc
	}
	localCrypto, err := srtp.DefaultProfiles()
	require.NoError(t, err)
	offer1, mc1 := newOffer(t, mp, localCrypto)
	offer2, mc2 := newOffer(t, mp, localCrypto)

	// Offers must not regenerate keys
	require.Equal(t, offer1.CryptoProfiles, offer2.CryptoProfiles, "crypto profiles must not change")
	require.Equal(t, mc1.Crypto.Keys.RemoteMasterKey, mc2.Crypto.Keys.RemoteMasterKey, "remote master key must not change")
	require.Equal(t, mc1.Crypto.Keys.RemoteMasterSalt, mc2.Crypto.Keys.RemoteMasterSalt, "remote master salt must not change")
}

// negotiate runs a full offer/answer between two ports, m1 offering, and returns the answer.
func negotiate(t testing.TB, m1, m2 *mediaPort) []byte {
	t.Helper()
	offerData, err := m1.GenerateOffer()
	require.NoError(t, err)

	answerData, err := m2.GenerateAnswer(offerData, true)
	require.NoError(t, err)

	require.NoError(t, m1.ProcessAnswer(answerData))
	return answerData
}

func newMediaPair(t testing.TB, opt1, opt2 *MediaOptions, codec string, targetRate int) (m1, m2 *mediaPort) {
	return newMediaPairWithAddr(t, newIP("1.1.1.1"), newIP("2.2.2.2"), opt1, opt2, codec, targetRate)
}

func newMediaPairWithAddr(t testing.TB, ip1, ip2 netip.Addr, opt1, opt2 *MediaOptions, codec string, targetRate int) (m1, m2 *mediaPort) {
	if opt1 == nil {
		opt1 = &MediaOptions{}
	}
	if opt2 == nil {
		opt2 = &MediaOptions{}
	}
	c1, c2 := newUDPPipe()

	if targetRate <= 0 {
		targetRate = RoomSampleRate
	}

	opt1.IP = ip1
	opt1.Ports = rtcconfig.PortRange{Start: 10000}
	if codec != "" {
		opt1.Codecs = testCodecSet(codec)
	}

	opt2.IP = ip2
	opt2.Ports = rtcconfig.PortRange{Start: 20000}
	if codec != "" {
		opt2.Codecs = testCodecSet(codec)
	}

	log := logger.NewTestLogger(t)

	m1 = newTestPort(t, log.WithName("one"), c1, opt1, targetRate)
	m2 = newTestPort(t, log.WithName("two"), c2, opt2, targetRate)

	negotiate(t, m1, m2)
	return m1, m2
}

type codecConfig struct {
	rampUpFrames  int
	offsetSamples int
}

var codecConfigMap = map[string]codecConfig{
	"G722/8000":    {rampUpFrames: 1, offsetSamples: 22},
	"AMR-WB/16000": {rampUpFrames: 1, offsetSamples: 14 + 16},
}

func TestMediaPortAudioRoundTrip(t *testing.T) {
	// Production resampler delay is tiny but not deterministic; checkPCM needs a stable delay.
	prevOpts := msdk.DefaultResampleOptions
	msdk.DefaultResampleOptions = []msdk.ResampleOption{
		msdk.WithPredictableResample(true),
	}
	defer func() {
		msdk.DefaultResampleOptions = prevOpts
	}()

	for _, codec := range allAudioCodecs() {
		info := codec.Info()
		t.Run(strings.ReplaceAll(info.SDPName, "/", "-"), func(t *testing.T) {
			for _, resample := range []bool{true, false} {
				t.Run(fmt.Sprintf("resample=%t", resample), func(t *testing.T) {
					for _, enc := range []sdp.Encryption{sdp.EncryptionNone, sdp.EncryptionRequire} {
						t.Run("enc="+policyToString[enc], func(t *testing.T) {

							opts1 := &MediaOptions{Encryption: enc}
							opts2 := &MediaOptions{Encryption: enc}
							targetRate := RoomSampleRate
							if !resample {
								targetRate = info.SampleRate
							}
							m1, m2 := newMediaPair(t, opts1, opts2, info.SDPName, targetRate)

							var recv1, recv2 msdk.PCM16Sample
							h1 := msdk.NewPCM16BufferWriter(&recv1, targetRate)
							h2 := msdk.NewPCM16BufferWriter(&recv2, targetRate)
							m1.WriteInboundAudioTo(h1)
							m2.WriteInboundAudioTo(h2)

							w1 := m1.GetOutboundAudioWriter()
							w2 := m2.GetOutboundAudioWriter()

							packetSize := targetRate / int(time.Second/rtp.DefFrameDur)
							to2 := make(msdk.PCM16Sample, packetSize)
							to1 := make(msdk.PCM16Sample, packetSize)
							const (
								amp1 = 10000
								amp2 = 5000
								freq = 10
							)
							for i := range packetSize {
								to2[i] = int16(amp1 * math.Sin(freq*2*math.Pi*float64(i)/float64(packetSize)))
								to1[i] = int16(amp2 * math.Sin(freq*2*math.Pi*float64(i)/float64(packetSize)))
							}

							codecConfig := codecConfigMap[info.SDPName] // defaults to 0,0

							// Ramp-up time for the codec.
							// Some codecs have "inertia" and cannot immediately represent the sound exactly.
							// This is shy we write signal multiple times to give it some time to adapt.
							// We will also cut the ramp-up part from the destination buffer before comparing.
							// This variable is in full frames, so that we clearly see where frames start to calculate the offset below.
							rampUpFrames := codecConfig.rampUpFrames
							// Some codecs have an extra buffering internally, and we have to offset the compared sample
							// by this number of sampled values.
							offsetSamples := codecConfig.offsetSamples

							writes := 1 + rampUpFrames
							discard := rampUpFrames * packetSize
							resampleMult := targetRate / info.SampleRate
							offsetSamples *= resampleMult

							var wg sync.WaitGroup
							wg.Add(2)
							go func() {
								defer wg.Done()
								for range writes {
									require.NoError(t, w1.WriteSample(to2))
								}
							}()
							go func() {
								defer wg.Done()
								for range writes {
									require.NoError(t, w2.WriteSample(to1))
								}
							}()
							wg.Wait()

							time.Sleep(time.Second / 4)

							h1.Close()
							h2.Close()
							m1.Close()
							m2.Close()

							checkPCM(t, "A -> B", to2[:packetSize-offsetSamples], recv2[discard+offsetSamples:])
							checkPCM(t, "B -> A", to1[:packetSize-offsetSamples], recv1[discard+offsetSamples:])
						})
					}

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

func TestPipelineChains(t *testing.T) {
	for _, codec := range enabledAudioCodecs() {
		t.Run(codec.Info().SDPName, func(t *testing.T) {
			// Create new test media port
			// Process offer with a specific codec + dtmf
			codecs := testCodecSet(codec.Info().SDPName)
			opts := &MediaOptions{
				IP:     netip.MustParseAddr("1.1.1.1"),
				Ports:  rtcconfig.PortRange{Start: 10000},
				Codecs: codecs,
			}
			conn := newTestConn(1)
			mp := newTestPort(t, logger.NewTestLogger(t), conn, opts, RoomSampleRate)

			info := codec.Info()
			offer, err := sdp.NewOfferWith(codecs, netip.MustParseAddr("2.2.2.2"), 20000, sdp.EncryptionNone)
			require.NoError(t, err)
			answerData, err := offer.SDP.Marshal()
			require.NoError(t, err)
			_, err = mp.GenerateAnswer(answerData, true)
			require.NoError(t, err)

			codecName := strings.Split(info.SDPName, "/")[0]
			sampleRate := info.SampleRate
			clockRate := info.RTPClockRate
			payloadType := info.RTPDefType
			audioOutChain := fmt.Sprintf("WriteCloserSwitch(%d) -> LatencyEntry -> Resample(%d->%d) -> %s(encode) -> ByteEncoder(%d) -> StatsWriter(%s/%d) -> LatencyExit -> RTPWriteStream(:0)",
				RoomSampleRate, RoomSampleRate, sampleRate, codecName, sampleRate, codecName, clockRate)
			audioInChain := fmt.Sprintf("StatsHandler(%s/%d) -> SilenceFiller(25) -> RTP(%d) -> ByteDecoder -> %s(decode) -> Resample(%d->%d) -> LatencyExit -> WriteCloserSwitch(nil)",
				codecName, clockRate, payloadType, codecName, sampleRate, RoomSampleRate)
			dtmfOutChain := fmt.Sprintf("WriteCloserSwitch(%d) -> dtmfOutWriter(dtmfAudio: false)", clockRate)
			dtmfInChain := fmt.Sprintf("StatsHandler(telephone-event/%d) -> HandlerFunc", clockRate)
			assert.Equal(t, audioOutChain, mp.GetOutboundAudioWriter().String(), "out audio chain mismatch")
			assert.Equal(t, audioInChain, mp.pipeline.audioToRoom.String(), "in audio chain mismatch")
			assert.Equal(t, dtmfOutChain, mp.GetOutboundDTMFWriter().String(), "out dtmf chain mismatch")
			assert.Equal(t, dtmfInChain, mp.pipeline.dtmfToRoom.String(), "in dtmf chain mismatch")
		})
	}
}

// pushAudio writes two room-rate frames. The outbound resampler keeps a one-frame
// delay (soxr returns a short buffer on the first call), so a single WriteSample
// never produces RTP when the port runs at RoomSampleRate.
func pushAudio(t testing.TB, w msdk.PCM16Writer) {
	t.Helper()
	frame := roomFrame()
	require.NoError(t, w.WriteSample(frame))
	require.NoError(t, w.WriteSample(frame))
}

func TestMediaTimeout(t *testing.T) {
	const (
		codec   = "G722/8000"
		timeout = time.Second / 4
		initial = timeout * 2
		dt      = timeout / 4
	)

	t.Run("initial", func(t *testing.T) {
		m1, _ := newMediaPair(t, &MediaOptions{
			MediaTimeoutInitial: initial,
			MediaTimeout:        timeout,
		}, nil, codec, RoomSampleRate)

		targ := time.Now().Add(initial)
		select {
		case <-m1.MediaTimeout():
			t.Fatal("initial timeout ignored")
		case <-time.After(initial / 2):
		}

		select {
		case <-time.After(time.Until(targ) + dt):
			t.Fatal("timeout didn't trigger")
		case <-m1.MediaTimeout():
		}
	})

	t.Run("regular", func(t *testing.T) {
		m1, m2 := newMediaPair(t, &MediaOptions{
			MediaTimeoutInitial: initial,
			MediaTimeout:        timeout,
		}, nil, codec, RoomSampleRate)

		w2 := m2.GetOutboundAudioWriter()
		pushAudio(t, w2)

		select {
		case <-time.After(dt):
			t.Fatal("no media received")
		case <-m1.Received():
		}

		select {
		case <-time.After(2*timeout + dt):
			t.Fatal("timeout didn't trigger")
		case <-m1.MediaTimeout():
		}
	})

	t.Run("no timeout", func(t *testing.T) {
		m1, m2 := newMediaPair(t, &MediaOptions{
			MediaTimeoutInitial: initial,
			MediaTimeout:        timeout,
		}, nil, codec, RoomSampleRate)

		w2 := m2.GetOutboundAudioWriter()

		for i := 0; i < 10; i++ {
			pushAudio(t, w2)

			select {
			case <-time.After(timeout / 2):
			case <-m1.MediaTimeout():
				t.Fatal("timeout")
			}
		}
	})

	t.Run("reset timeout after media", func(t *testing.T) {
		m1, m2 := newMediaPair(t, &MediaOptions{
			MediaTimeoutInitial: initial,
			MediaTimeout:        timeout,
		}, nil, codec, RoomSampleRate)

		w2 := m2.GetOutboundAudioWriter()

		for i := 0; i < 5; i++ {
			pushAudio(t, w2)

			select {
			case <-time.After(timeout / 2):
			case <-m1.MediaTimeout():
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
		case <-m1.MediaTimeout():
		}
	})

	t.Run("reset timeout before any media", func(t *testing.T) {
		m1, _ := newMediaPair(t, &MediaOptions{
			MediaTimeoutInitial: initial,
			MediaTimeout:        timeout,
		}, nil, codec, RoomSampleRate)

		// No media has ever arrived. SetTimeout re-arms startTime, and since the
		// port has never seen an RTP packet, the new initial window applies from
		// the moment of the SetTimeout call.
		time.Sleep(initial / 2)
		m1.SetTimeout(initial, timeout)

		targ := time.Now().Add(initial)
		select {
		case <-m1.MediaTimeout():
			t.Fatal("initial timeout fired too early")
		case <-time.After(initial / 2):
		}

		select {
		case <-time.After(time.Until(targ) + dt):
			t.Fatal("timeout didn't trigger")
		case <-m1.MediaTimeout():
		}
	})

	t.Run("reset", func(t *testing.T) {
		m1, m2 := newMediaPair(t, &MediaOptions{
			MediaTimeoutInitial: initial,
			MediaTimeout:        timeout,
		}, nil, codec, RoomSampleRate)

		w2 := m2.GetOutboundAudioWriter()

		for i := 0; i < 5; i++ {
			pushAudio(t, w2)

			select {
			case <-time.After(timeout / 2):
			case <-m1.MediaTimeout():
				t.Fatal("timeout")
			}
		}

		for i := 0; i < 5; i++ {
			pushAudio(t, w2)

			select {
			case <-time.After(timeout / 2):
			case <-m1.MediaTimeout():
				t.Fatal("timeout")
			}
		}
	})
}

func TestSymmetricRTP(t *testing.T) {
	const codec = "G722/8000"

	t.Run("disabled", func(t *testing.T) {
		m1, m2 := newMediaPair(t, &MediaOptions{SymmetricRTP: false}, nil, codec, RoomSampleRate)
		dstPtr := m1.port.dst.Load()
		require.NotNil(t, dstPtr)
		dst := *dstPtr
		require.True(t, dst.IsValid())

		c2 := m2.port.UDPConn.(*testUDPConn)
		newAddr := netip.AddrPortFrom(newIP("9.9.9.9"), 9999)
		c2.addr = newAddr

		pushAudio(t, m2.GetOutboundAudioWriter())

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
		m1, m2 := newMediaPair(t, &MediaOptions{SymmetricRTP: true}, nil, codec, RoomSampleRate)
		dstPtr := m1.port.dst.Load()
		require.NotNil(t, dstPtr)
		require.True(t, dstPtr.IsValid())

		c2 := m2.port.UDPConn.(*testUDPConn)
		newAddr := netip.AddrPortFrom(newIP("9.9.9.9"), 9999)
		c2.addr = newAddr

		pushAudio(t, m2.GetOutboundAudioWriter())

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
			codec,
			RoomSampleRate,
		)
		dstPtr := m1.port.dst.Load()
		require.NotNil(t, dstPtr)
		require.True(t, dstPtr.IsValid())
		symmetric := m1.port.symmetric.Load()
		require.True(t, symmetric)

		c2 := m2.port.UDPConn.(*testUDPConn)
		newAddr := netip.AddrPortFrom(newIP("3.3.3.3"), 9999)
		c2.addr = newAddr

		pushAudio(t, m2.GetOutboundAudioWriter())

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

// Test util for incrementing prometheus counter metrics.
func gatherCounter(t testing.TB, name string, labels map[string]string) float64 {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	require.NoError(t, err)
	var total float64
	for _, f := range families {
		// Matching metric
		if f.GetName() != name {
			continue
		}
	metrics:
		for _, m := range f.GetMetric() {
			got := make(map[string]string, len(m.GetLabel()))
			for _, l := range m.GetLabel() {
				got[l.GetName()] = l.GetValue()
			}
			// Matching labels
			for k, v := range labels {
				if got[k] != v {
					continue metrics
				}
			}
			total += m.GetCounter().GetValue()
		}
	}
	return total
}

// Report codecs offered during SDP even when the offer fails to match any codecs
func TestSetOfferReportsCodecsBeforeFailing(t *testing.T) {
	mp := newTestMediaPort(t, "internal/somecarrier")

	parsed := map[string]string{"dir": "in", "provider": "internal/somecarrier"}
	other := map[string]string{"dir": "in", "provider": "internal/somecarrier", "codec": codecOther}
	pcmu := map[string]string{"dir": "in", "provider": "internal/somecarrier", "codec": "PCMU/8000"}

	parsedBefore := gatherCounter(t, parsedMetric, parsed)
	otherBefore := gatherCounter(t, offeredMetric, other)
	pcmuBefore := gatherCounter(t, offeredMetric, pcmu)

	offer := sdpWithMedia("m=audio 5004 RTP/AVP 96", "a=rtpmap:96 SPEEX/16000")
	_, err := mp.GenerateAnswer(offer, true)
	require.ErrorIs(t, err, sdp.ErrNoCommonMedia)

	// Codecs that are not part of the internal set are classified as "other"
	require.Equal(t, parsedBefore+1, gatherCounter(t, parsedMetric, parsed))
	require.Equal(t, otherBefore+1, gatherCounter(t, offeredMetric, other))
	require.Equal(t, pcmuBefore, gatherCounter(t, offeredMetric, pcmu))
}

func TestSetOfferReportsCodecsPerProvider(t *testing.T) {
	mp := newTestMediaPort(t, "internal/somecarrier")

	parsed := map[string]string{"dir": "in", "provider": "internal/somecarrier"}
	pcmu := map[string]string{"dir": "in", "provider": "internal/somecarrier", "codec": "PCMU/8000"}
	g722 := map[string]string{"dir": "in", "provider": "internal/somecarrier", "codec": "G722/8000"}

	parsedBefore := gatherCounter(t, parsedMetric, parsed)
	pcmuBefore := gatherCounter(t, offeredMetric, pcmu)
	g722Before := gatherCounter(t, offeredMetric, g722)

	offer := sdpWithMedia("m=audio 5004 RTP/AVP 0 9",
		"a=rtpmap:0 PCMU/8000", "a=rtpmap:9 G722/8000")
	_, err := mp.GenerateAnswer(offer, true)
	require.NoError(t, err)

	require.Equal(t, parsedBefore+1, gatherCounter(t, parsedMetric, parsed))
	require.Equal(t, pcmuBefore+1, gatherCounter(t, offeredMetric, pcmu))
	require.Equal(t, g722Before+1, gatherCounter(t, offeredMetric, g722))
}

// Without SetProvider - the pre-auth path - offers still land somewhere rather
// than being dropped.
func TestSetOfferReportsUnknownProvider(t *testing.T) {
	mp := newTestMediaPort(t, "")

	parsed := map[string]string{"dir": "in", "provider": stats.ProviderUnknown}
	before := gatherCounter(t, parsedMetric, parsed)

	offer := sdpWithMedia("m=audio 5004 RTP/AVP 0", "a=rtpmap:0 PCMU/8000")
	_, err := mp.GenerateAnswer(offer, true)
	require.NoError(t, err)

	require.Equal(t, before+1, gatherCounter(t, parsedMetric, parsed))
}
