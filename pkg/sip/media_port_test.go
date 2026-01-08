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
	"sync/atomic"
	"testing"
	"time"

	msdk "github.com/livekit/media-sdk"
	"github.com/livekit/media-sdk/rtp"
	"github.com/livekit/media-sdk/sdp"
	"github.com/livekit/mediatransportutil/pkg/rtcconfig"
	"github.com/livekit/protocol/logger"
	"github.com/stretchr/testify/require"
)

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

func TestMediaPort(t *testing.T) {
	codecs := msdk.Codecs()
	disableAll := func() {
		for _, codec := range codecs {
			msdk.CodecSetEnabled(codec.Info().SDPName, false)
		}
	}
	defer func() {
		for _, codec := range codecs {
			info := codec.Info()
			msdk.CodecSetEnabled(info.SDPName, !info.Disabled)
		}
	}()
	for _, codec := range codecs {
		info := codec.Info()
		t.Run(info.SDPName, func(t *testing.T) {
			disableAll()
			msdk.CodecSetEnabled(info.SDPName, true)

			sub := strings.SplitN(info.SDPName, "/", 2)
			name := sub[0]
			nativeRate, err := strconv.Atoi(sub[1])
			require.NoError(t, err)
			switch name {
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

					m1, err := NewMediaPortWith(1, log.WithName("one"), nil, c1, &MediaOptions{
						IP:              newIP("1.1.1.1"),
						Ports:           rtcconfig.PortRange{Start: 10000},
						NoInputResample: true,
					}, tconf.Rate)
					require.NoError(t, err)
					defer m1.Close()

					m2, err := NewMediaPortWith(2, log.WithName("two"), nil, c2, &MediaOptions{
						IP:    newIP("2.2.2.2"),
						Ports: rtcconfig.PortRange{Start: 20000},
					}, tconf.Rate)
					require.NoError(t, err)
					defer m2.Close()

					offer, err := m1.NewOffer(tconf.Encrypted)
					require.NoError(t, err)
					offerData, err := offer.SDP.Marshal()
					require.NoError(t, err)

					t.Logf("SDP offer:\n%s", string(offerData))

					answer, conf, err := m2.SetOffer(offerData, tconf.Encrypted)
					require.NoError(t, err)
					answerData, err := answer.SDP.Marshal()
					require.NoError(t, err)

					t.Logf("SDP answer:\n%s", string(answerData))

					mc, err := m1.SetAnswer(offer, answerData, tconf.Encrypted)
					require.NoError(t, err)

					err = m1.SetConfig(mc)
					require.NoError(t, err)

					err = m2.SetConfig(conf)
					require.NoError(t, err)

					codec1 := m1.Config().Audio.Codec
					codec2 := m2.Config().Audio.Codec
					require.Equal(t, info.SDPName, codec1.Info().SDPName)
					require.Equal(t, info.SDPName, codec2.Info().SDPName)

					var buf1 msdk.PCM16Sample
					bw1 := msdk.NewPCM16BufferWriter(&buf1, codec1.Info().SampleRate)
					m1.WriteAudioTo(bw1)

					var buf2 msdk.PCM16Sample
					bw2 := msdk.NewPCM16BufferWriter(&buf2, tconf.Rate)
					m2.WriteAudioTo(bw2)

					w1 := m1.GetAudioWriter()
					w2 := m2.GetAudioWriter()

					packetSize := uint32(tconf.Rate / int(time.Second/rtp.DefFrameDur))
					sample1 := make(msdk.PCM16Sample, packetSize)
					sample2 := make(msdk.PCM16Sample, packetSize)
					for i := range packetSize {
						sample1[i] = +5116
						sample2[i] = -5116
					}

					writes1 := 1
					writes2 := 1
					if tconf.Rate == nativeRate {
						expChain := fmt.Sprintf("Switch(%d) -> %s(encode) -> RTP(%d)", nativeRate, name, nativeRate)
						require.Equal(t, expChain, w1.String())
						require.Equal(t, expChain, w2.String())

						expChain = fmt.Sprintf("SilenceFiller(25) -> RTP(%d) -> %s(decode) -> Switch(%d) -> Buffer(%d)", nativeRate, name, nativeRate, nativeRate)
						require.Equal(t, expChain, PrintAudioInWriter(m1))
						require.Equal(t, expChain, PrintAudioInWriter(m2))
					} else {
						expChain := fmt.Sprintf("Switch(48000) -> Resample(48000->%d) -> %s(encode) -> RTP(%d)", nativeRate, name, nativeRate)
						require.Equal(t, expChain, w1.String())
						require.Equal(t, expChain, w2.String())

						// This side does not resample the received audio, it uses sample rate of the RTP source.
						expChain1 := fmt.Sprintf("SilenceFiller(25) -> RTP(%d) -> %s(decode) -> Switch(%d) -> Buffer(%d)", nativeRate, name, nativeRate, nativeRate)
						// This side resamples the received audio to the expected sample rate.
						expChain2 := fmt.Sprintf("SilenceFiller(25) -> RTP(%d) -> %s(decode) -> Resample(%d->48000) -> Switch(48000) -> Buffer(48000)", nativeRate, name, nativeRate)
						require.Equal(t, expChain1, PrintAudioInWriter(m1))
						require.Equal(t, expChain2, PrintAudioInWriter(m2))

						// resampler will buffer a few frames
						writes1 += 2
						writes2 += 2
						// a few more because of higher resample quality required
						if nativeRate == 8000 {
							writes1 += 3
							writes2 += 5
						}
						if strings.HasPrefix(info.SDPName, "G722/") {
							writes2 += 1
						}
					}

					for range writes1 {
						err = w1.WriteSample(sample1)
						require.NoError(t, err)
					}
					for range writes2 {
						err = w2.WriteSample(sample2)
						require.NoError(t, err)
					}

					time.Sleep(time.Second / 4)

					// Cut buffers earlier, otherwise we might get extra samples
					// that we added to push resampler forward.
					bw1.Close()
					bw2.Close()

					m1.Close()
					m2.Close()

					checkPCM(t, sample1, buf2)
					checkPCM(t, sample2, buf1)
				})
			}
		})
	}

}

func checkPCM(t testing.TB, exp, got msdk.PCM16Sample) {
	t.Helper()
	require.Equal(t, len(exp), len(got))
	expSamples := slices.Clone(exp)
	slices.Sort(expSamples)
	median := expSamples[len(expSamples)/2]
	const perc = 0.1
	delta := int16(math.Abs(float64(median) * perc))

	hits := 0
	for _, v := range got {
		if v >= median-delta && v <= median+delta {
			hits++
		}
	}
	const percHit = 0.90
	expHit := int(float64(len(expSamples)) * percHit)
	require.True(t, hits >= expHit, "min=%v, max=%v\ngot:\n%v", slices.Min(got), slices.Max(got), got)
}

func newMediaPair(t testing.TB, opt1, opt2 *MediaOptions) (m1, m2 *MediaPort) {
	if opt1 == nil {
		opt1 = &MediaOptions{}
	}
	if opt2 == nil {
		opt2 = &MediaOptions{}
	}
	c1, c2 := newUDPPipe()

	opt1.IP = newIP("1.1.1.1")
	opt1.Ports = rtcconfig.PortRange{Start: 10000}
	opt1.NoInputResample = true

	opt2.IP = newIP("2.2.2.2")
	opt2.Ports = rtcconfig.PortRange{Start: 20000}

	const rate = 16000

	log := logger.GetLogger()

	var err error

	m1, err = NewMediaPortWith(1, log.WithName("one"), nil, c1, opt1, rate)
	require.NoError(t, err)
	t.Cleanup(m1.Close)

	m2, err = NewMediaPortWith(2, log.WithName("two"), nil, c2, opt2, rate)
	require.NoError(t, err)
	t.Cleanup(m2.Close)

	offer, err := m1.NewOffer(sdp.EncryptionNone)
	require.NoError(t, err)
	offerData, err := offer.SDP.Marshal()
	require.NoError(t, err)

	answer, mc2, err := m2.SetOffer(offerData, sdp.EncryptionNone)
	require.NoError(t, err)
	answerData, err := answer.SDP.Marshal()
	require.NoError(t, err)

	mc1, err := m1.SetAnswer(offer, answerData, sdp.EncryptionNone)
	require.NoError(t, err)

	err = m1.SetConfig(mc1)
	require.NoError(t, err)

	err = m2.SetConfig(mc2)
	require.NoError(t, err)

	w2 := m2.GetAudioWriter()
	require.Equal(t, "Switch(16000) -> G722(encode) -> RTP(16000)", w2.String())

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

	t.Run("reset timeout", func(t *testing.T) {
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
