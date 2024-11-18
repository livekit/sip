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

	"github.com/stretchr/testify/require"

	"github.com/livekit/mediatransportutil/pkg/rtcconfig"
	"github.com/livekit/protocol/logger"

	"github.com/livekit/sip/pkg/media"
	"github.com/livekit/sip/pkg/media/rtp"
)

type testUDPConn struct {
	addr   *net.UDPAddr
	closed chan struct{}
	buf    chan []byte
	peer   atomic.Pointer[testUDPConn]
}

func (c *testUDPConn) LocalAddr() net.Addr {
	return c.addr
}

func (c *testUDPConn) WriteToUDP(buf []byte, addr *net.UDPAddr) (int, error) {
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

func (c *testUDPConn) ReadFromUDP(buf []byte) (int, *net.UDPAddr, error) {
	peer := c.peer.Load()
	if peer == nil {
		return 0, nil, io.ErrClosedPipe
	}
	select {
	case <-c.closed:
		return 0, nil, io.ErrClosedPipe
	case data := <-c.buf:
		n := copy(buf, data)
		var err error
		if n < len(data) {
			err = io.ErrShortBuffer
		}
		return n, peer.addr, err
	}
}

func (c *testUDPConn) Close() error {
	if c.peer.Swap(nil) != nil {
		close(c.closed)
	}
	return nil
}

func newUDPConn(i int) *testUDPConn {
	return &testUDPConn{
		addr: &net.UDPAddr{
			IP:   net.IPv4(byte(i), byte(i), byte(i), byte(i)),
			Port: 10000 * i,
		},
		buf:    make(chan []byte, 10),
		closed: make(chan struct{}),
	}
}

func newUDPPipe() (c1, c2 *testUDPConn) {
	c1 = newUDPConn(1)
	c2 = newUDPConn(2)
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
	codecs := media.Codecs()
	disableAll := func() {
		for _, codec := range codecs {
			media.CodecSetEnabled(codec.Info().SDPName, false)
		}
	}
	defer func() {
		for _, codec := range codecs {
			info := codec.Info()
			media.CodecSetEnabled(info.SDPName, !info.Disabled)
		}
	}()
	for _, codec := range codecs {
		info := codec.Info()
		t.Run(info.SDPName, func(t *testing.T) {
			disableAll()
			media.CodecSetEnabled(info.SDPName, true)

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

			for _, rate := range []int{
				nativeRate,
				48000,
			} {
				t.Run(strconv.Itoa(rate), func(t *testing.T) {
					c1, c2 := newUDPPipe()

					log := logger.GetLogger()

					m1, err := NewMediaPortWith(log.WithName("one"), nil, c1, &MediaConfig{
						IP:    newIP("1.1.1.1"),
						Ports: rtcconfig.PortRange{Start: 10000},
					}, rate)
					require.NoError(t, err)
					defer m1.Close()

					m2, err := NewMediaPortWith(log.WithName("two"), nil, c2, &MediaConfig{
						IP:    newIP("2.2.2.2"),
						Ports: rtcconfig.PortRange{Start: 20000},
					}, rate)
					require.NoError(t, err)
					defer m2.Close()

					offer := m1.NewOffer()
					offerData, err := offer.SDP.Marshal()
					require.NoError(t, err)

					t.Logf("SDP offer:\n%s", string(offerData))

					answer, conf, err := m2.SetOffer(offerData)
					require.NoError(t, err)
					answerData, err := answer.SDP.Marshal()
					require.NoError(t, err)

					t.Logf("SDP answer:\n%s", string(answerData))

					mc, err := m1.SetAnswer(offer, answerData)
					require.NoError(t, err)

					err = m1.SetConfig(mc)
					require.NoError(t, err)

					err = m2.SetConfig(conf)
					require.NoError(t, err)

					require.Equal(t, info.SDPName, m1.Config().Audio.Codec.Info().SDPName)
					require.Equal(t, info.SDPName, m2.Config().Audio.Codec.Info().SDPName)

					var buf1 media.PCM16Sample
					m1.WriteAudioTo(media.NewPCM16BufferWriter(&buf1, rate))

					var buf2 media.PCM16Sample
					m2.WriteAudioTo(media.NewPCM16BufferWriter(&buf2, rate))

					w1 := m1.GetAudioWriter()
					w2 := m2.GetAudioWriter()

					packetSize := uint32(rate / int(time.Second/rtp.DefFrameDur))
					sample1 := make(media.PCM16Sample, packetSize)
					sample2 := make(media.PCM16Sample, packetSize)
					for i := range packetSize {
						sample1[i] = +5116
						sample2[i] = -5116
					}

					writes := 1
					if rate == nativeRate {
						expChain := fmt.Sprintf("Switch(%d) -> %s(encode) -> RTP(%d)", nativeRate, name, nativeRate)
						require.Equal(t, expChain, w1.String())
						require.Equal(t, expChain, w2.String())

						expChain = fmt.Sprintf("RTP(%d) -> %s(decode) -> Switch(%d) -> Buffer(%d)", nativeRate, name, nativeRate, nativeRate)
						require.Equal(t, expChain, PrintAudioInWriter(m1))
						require.Equal(t, expChain, PrintAudioInWriter(m2))
					} else {
						expChain := fmt.Sprintf("Switch(48000) -> Resample(48000->%d) -> %s(encode) -> RTP(%d)", nativeRate, name, nativeRate)
						require.Equal(t, expChain, w1.String())
						require.Equal(t, expChain, w2.String())

						expChain = fmt.Sprintf("RTP(%d) -> %s(decode) -> Resample(%d->48000) -> Switch(48000) -> Buffer(48000)", nativeRate, name, nativeRate)
						require.Equal(t, expChain, PrintAudioInWriter(m1))
						require.Equal(t, expChain, PrintAudioInWriter(m2))

						writes = 2 // resampler will buffer a few frames
						if nativeRate == 8000 {
							writes += 2 // a few more because of higher resample quality required
						}
					}

					for range writes {
						err = w1.WriteSample(sample1)
						require.NoError(t, err)

						err = w2.WriteSample(sample2)
						require.NoError(t, err)
					}

					time.Sleep(time.Second)

					m1.Close()
					m2.Close()

					checkPCM(t, sample1, buf2)
					checkPCM(t, sample2, buf1)
				})
			}
		})
	}

}

func checkPCM(t testing.TB, exp, got media.PCM16Sample) {
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
