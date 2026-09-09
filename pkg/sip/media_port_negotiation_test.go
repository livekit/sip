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
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	msdk "github.com/livekit/media-sdk"
	"github.com/livekit/media-sdk/dtmf"
	"github.com/livekit/media-sdk/g711"
	"github.com/livekit/media-sdk/g722"
	"github.com/livekit/media-sdk/rtp"
	"github.com/livekit/media-sdk/sdp"
	"github.com/livekit/mediatransportutil/pkg/rtcconfig"
	"github.com/livekit/protocol/logger"
)

// recvBuffer counts samples arriving from the RTP read loop. It only records the count,
// so tests can poll it while the pipeline is still writing.
type recvBuffer struct {
	mu      sync.Mutex
	samples int
}

func (b *recvBuffer) String() string  { return "recvBuffer" }
func (b *recvBuffer) SampleRate() int { return RoomSampleRate }
func (b *recvBuffer) Close() error    { return nil }

func (b *recvBuffer) WriteSample(sample msdk.PCM16Sample) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.samples += len(sample)
	return nil
}

func (b *recvBuffer) count() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.samples
}

func roomFrame() msdk.PCM16Sample {
	frame := make(msdk.PCM16Sample, RoomSampleRate/int(time.Second/rtp.DefFrameDur))
	for i := range frame {
		if (i/40)%2 == 0 {
			frame[i] = 8000
		} else {
			frame[i] = -8000
		}
	}
	return frame
}

// writeFrames pushes room audio into the port. Write errors are ignored: the in-memory
// UDP pipe is bounded, and a peer that is mid-renegotiation may not be draining it.
func writeFrames(m *mediaPort, frames int) {
	w := m.GetOutboundAudioWriter()
	frame := roomFrame()
	for range frames {
		_ = w.WriteSample(frame)
	}
}

// requireAudioFlows asserts that audio written to src is observed by dst's room writer.
func requireAudioFlows(t testing.TB, src *mediaPort, dst *recvBuffer) {
	t.Helper()
	before := dst.count()
	require.Eventually(t, func() bool {
		writeFrames(src, 5)
		return dst.count() > before
	}, 5*time.Second, 50*time.Millisecond, "no audio received")
}

func testCodecSet(names ...string) *msdk.CodecSet {
	set := msdk.NewCodecSet()
	for _, name := range names {
		set.SetEnabled(name, true)
		rate := strings.Split(name, "/")[1]
		set.SetEnabled(fmt.Sprintf("%s/%s", dtmf.SDPNameOnly, rate), true)
	}
	return set
}

func enabledAudioCodecs() []msdk.Codec {
	var audio []msdk.Codec
	for _, c := range msdk.GlobalCodecs().ListEnabled() {
		if _, ok := c.(msdk.AudioCodec); !ok {
			continue // telephone-event and other non-audio codecs
		}
		audio = append(audio, c)
	}
	return audio
}

func isAudioCodec(c msdk.Codec) bool {
	_, ok := c.(msdk.AudioCodec)
	return ok
}

func allAudioCodecs() []msdk.Codec {
	var audio []msdk.Codec
	for _, c := range msdk.Codecs() {
		if !isAudioCodec(c) {
			continue // telephone-event and other non-audio codecs
		}
		audio = append(audio, c)
	}
	return audio
}

func answerCodec(t testing.TB, answerData []byte) string {
	t.Helper()
	answer, err := parseAnswerWith(logger.NewTestLogger(t), nil, defaultCodecs, answerData)
	require.NoError(t, err)
	for _, c := range answer.Codecs {
		if !isAudioCodec(c.Codec) {
			continue // telephone-event and other non-audio codecs
		}
		return c.Codec.Info().SDPName
	}
	t.Fatal("no audio codec in answer")
	return ""
}

// A port only offers and accepts the codecs it was configured with.
func TestMediaPortCodecSet(t *testing.T) {
	newLocked := func(t *testing.T, names ...string) *mediaPort {
		return newTestPort(t, logger.NewTestLogger(t), newTestConn(1), &MediaOptions{
			IP:     newIP("127.0.0.1"),
			Codecs: testCodecSet(names...),
		}, RoomSampleRate)
	}

	t.Run("offer lists only enabled codecs", func(t *testing.T) {
		m := newLocked(t, g711.ALawSDPNameAndRate)

		offerData, err := m.GenerateOffer()
		require.NoError(t, err)

		offer, err := parseOfferWith(logger.NewTestLogger(t), nil, defaultCodecs, offerData)
		require.NoError(t, err)

		var names []string
		for _, c := range offer.Codecs {
			if !isAudioCodec(c.Codec) {
				continue // telephone-event and other non-audio codecs
			}
			names = append(names, c.Codec.Info().SDPName)
		}
		assert.Equal(t, []string{g711.ALawSDPNameAndRate}, names)
		assert.NotNil(t, offer.DTMF)
		assert.NotZero(t, offer.DTMF[0].Type, "DTMF type should still be offered")
	})

	t.Run("answer picks an enabled codec", func(t *testing.T) {
		m := newLocked(t, g711.ALawSDPNameAndRate)

		// Peer offers both, only PCMA is enabled here.
		offer := sdpWithMedia("m=audio 5004 RTP/AVP 0 8",
			"a=rtpmap:0 PCMU/8000", "a=rtpmap:8 PCMA/8000")
		answerData, err := m.GenerateAnswer(offer)
		require.NoError(t, err)
		assert.Equal(t, g711.ALawSDPNameAndRate, answerCodec(t, answerData))
	})

	t.Run("offer without an enabled codec is rejected", func(t *testing.T) {
		m := newLocked(t, g711.ALawSDPNameAndRate)

		offer := sdpWithMedia("m=audio 5004 RTP/AVP 0", "a=rtpmap:0 PCMU/8000")
		_, err := m.GenerateAnswer(offer)
		require.ErrorIs(t, err, sdp.ErrNoCommonMedia)
	})
}

func TestMediaPortRejectsDifferentCodecOffer(t *testing.T) {
	t.Skip("renegotiation is disabled: GenerateAnswer returns the prior answer when one already exists")
	// TODO: change this test to confirm renegotiation when it's enabled
	m := newTestPort(t, logger.NewTestLogger(t), newTestConn(1), &MediaOptions{
		IP:     newIP("127.0.0.1"),
		Codecs: testCodecSet(g711.ULawSDPNameAndRate, g722.SDPNameAndRate),
	}, RoomSampleRate)

	sdpA := sdpWithMedia("m=audio 5004 RTP/AVP 0", "a=rtpmap:0 PCMU/8000")
	sdpB := sdpWithMedia("m=audio 5004 RTP/AVP 9", "a=rtpmap:9 G722/8000")

	// Offer codec A
	answer, err := m.GenerateAnswer(sdpA)
	require.NoError(t, err)
	require.Equal(t, g711.ULawSDPNameAndRate, answerCodec(t, answer))

	// Attempt to offer only codec B, expect failure
	answer, err = m.GenerateAnswer(sdpB)
	require.ErrorIs(t, err, sdp.ErrNoCommonMedia)

	// Offer codec A again, expect success
	answer, err = m.GenerateAnswer(sdpA)
	require.NoError(t, err)
	require.Equal(t, g711.ULawSDPNameAndRate, answerCodec(t, answer))
}

// Renegotiation rebuilds the pipeline under the same port and keeps audio flowing,
// including across a codec change that moves the encoder's sample rate.
func TestMediaPortRenegotiation(t *testing.T) {
	t.Skip("renegotiation is disabled: GenerateAnswer returns the prior answer when one already exists")
	t.Run("repeated", func(t *testing.T) {
		m1, m2 := newMediaPair(t, nil, nil, "", RoomSampleRate)

		recv2 := &recvBuffer{}
		m2.WriteInboundAudioTo(recv2)
		requireAudioFlows(t, m1, recv2)

		for range 3 {
			negotiate(t, m1, m2)

			local, err := m1.GetLocalSDP()
			require.NoError(t, err)
			assert.NotEmpty(t, local)

			// The room-facing writers survive the rebuild, and the new pipeline sends.
			assert.NotNil(t, m1.audioOut.Get())
			requireAudioFlows(t, m1, recv2)
		}
	})

	t.Run("codec change", func(t *testing.T) {
		c1, c2 := newUDPPipe()
		log := logger.NewTestLogger(t)

		m1 := newTestPort(t, log.WithName("one"), c1, &MediaOptions{
			IP:    newIP("1.1.1.1"),
			Ports: rtcconfig.PortRange{Start: 10000},
		}, RoomSampleRate)
		m2 := newTestPort(t, log.WithName("two"), c2, &MediaOptions{
			IP:     newIP("2.2.2.2"),
			Ports:  rtcconfig.PortRange{Start: 20000},
			Codecs: testCodecSet(g711.ULawSDPNameAndRate),
		}, RoomSampleRate)

		answerData := negotiate(t, m1, m2)
		require.Equal(t, g711.ULawSDPNameAndRate, answerCodec(t, answerData))

		recv2 := &recvBuffer{}
		m2.WriteInboundAudioTo(recv2)
		requireAudioFlows(t, m1, recv2)

		// G722 samples at 16k, so the encode leaf changes sample rate under the same
		// room-facing switch.
		m2.codecs = testCodecSet(g722.SDPNameAndRate)

		answerData = negotiate(t, m1, m2)
		require.Equal(t, g722.SDPNameAndRate, answerCodec(t, answerData))

		requireAudioFlows(t, m1, recv2)
	})
}

// A peer that will not receive (RFC 3264 a=sendonly, or the legacy c=0.0.0.0) stops our
// media without stopping theirs, and resumes on the next offer.
func TestMediaPortHold(t *testing.T) {
	t.Skip("hold requires renegotiation: configure returns without rebuilding when a pipeline already exists")
	cases := []struct {
		name string
		hold func(t *testing.T, offer string) string
	}{
		{
			name: "sendonly",
			hold: func(t *testing.T, offer string) string {
				held := strings.Replace(offer, "a=sendrecv", "a=sendonly", 1)
				require.NotEqual(t, offer, held)
				return held
			},
		},
		{
			name: "zero connection address",
			hold: func(t *testing.T, offer string) string {
				held := strings.ReplaceAll(offer, "c=IN IP4 2.2.2.2", "c=IN IP4 0.0.0.0")
				require.NotEqual(t, offer, held)
				return held
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m1, m2 := newMediaPair(t, nil, nil, "", RoomSampleRate)

			recv1 := &recvBuffer{}
			m1.WriteInboundAudioTo(recv1)
			recv2 := &recvBuffer{}
			m2.WriteInboundAudioTo(recv2)

			// Baseline: m1 sends to m2.
			require.NotNil(t, m1.audioOut.Get())
			requireAudioFlows(t, m1, recv2)

			// m2 re-INVITEs with the hold form of its offer.
			base, err := m2.GenerateOffer()
			require.NoError(t, err)
			_, err = m1.GenerateAnswer([]byte(tc.hold(t, string(base))))
			require.NoError(t, err)

			// m1 no longer sends: no destination to write to, and the room-facing
			// writer is detached. Room audio is dropped rather than erroring.
			dst := m1.port.dst.Load()
			if assert.NotNil(t, dst) {
				assert.True(t, dst.Addr().IsUnspecified(), "held port kept a destination: %v", dst)
			}
			assert.Nil(t, m1.audioOut.Get(), "held port still accepts room audio")
			assert.NoError(t, m1.GetOutboundAudioWriter().WriteSample(roomFrame()))

			sent := recv2.count()
			writeFrames(m1, 10)
			time.Sleep(100 * time.Millisecond)
			assert.Equal(t, sent, recv2.count(), "held port kept sending")

			// ...while m2's media still reaches us.
			requireAudioFlows(t, m2, recv1)

			// Resume with the original offer.
			_, err = m1.GenerateAnswer(base)
			require.NoError(t, err)

			dst = m1.port.dst.Load()
			if assert.NotNil(t, dst) {
				assert.False(t, dst.Addr().IsUnspecified(), "destination not restored")
				assert.Equal(t, m2.Port(), int(dst.Port()))
			}
			assert.NotNil(t, m1.audioOut.Get())

			requireAudioFlows(t, m1, recv2)
		})
	}
}

var policyToString = map[sdp.Encryption]string{
	sdp.EncryptionNone:    "none",
	sdp.EncryptionAllow:   "allow",
	sdp.EncryptionRequire: "require",
}

func TestMediaPortEncryptionPolicy(t *testing.T) {
	encryptionPolicies := []sdp.Encryption{
		sdp.EncryptionNone,
		sdp.EncryptionAllow,
		sdp.EncryptionRequire,
	}

	forEach := func(t *testing.T, negotiate func(t *testing.T, mp *mediaPort, policy sdp.Encryption) (*sdp.MediaConfig, error)) {
		for _, portEncryptionPolicy := range encryptionPolicies {
			name := "port=" + policyToString[portEncryptionPolicy]
			t.Run(name, func(t *testing.T) {
				for _, remoteEncryptionPolicy := range encryptionPolicies {
					name := "remote=" + policyToString[remoteEncryptionPolicy]
					t.Run(name, func(t *testing.T) {
						opts := &MediaOptions{
							IP:         netip.MustParseAddr("1.1.1.1"),
							Ports:      rtcconfig.PortRange{Start: 10000},
							Encryption: portEncryptionPolicy,
						}
						conn := newTestConn(1)
						mp := newTestPort(t, logger.NewTestLogger(t), conn, opts, RoomSampleRate)

						mc, err := negotiate(t, mp, remoteEncryptionPolicy)
						if portEncryptionPolicy != sdp.EncryptionAllow && remoteEncryptionPolicy != sdp.EncryptionAllow && portEncryptionPolicy != remoteEncryptionPolicy {
							// Expect failue
							assert.Error(t, err)
							assert.ErrorIs(t, err, sdp.ErrNoCommonCrypto)
							return
						}
						// Expect success
						assert.NoError(t, err)
						if portEncryptionPolicy == sdp.EncryptionNone || remoteEncryptionPolicy == sdp.EncryptionNone {
							assert.Nil(t, mc.Crypto)
						} else {
							assert.NotNil(t, mc.Crypto)
						}
					})
				}
			})
		}
	}

	t.Run("inbound", func(t *testing.T) { // Receive offer
		negotiate := func(t *testing.T, mp *mediaPort, policy sdp.Encryption) (*sdp.MediaConfig, error) {
			offer, err := sdp.NewOfferWith(defaultCodecs, newIP("127.0.0.1"), 5004, policy)
			require.NoError(t, err)
			offerData, err := offer.SDP.Marshal()
			require.NoError(t, err)
			answerData, err := mp.GenerateAnswer(offerData)
			if err != nil {
				return nil, err
			}
			answer, err := parseAnswerWith(logger.NewTestLogger(t), nil, defaultCodecs, answerData)
			require.NoError(t, err)
			mc, _, err := answer.ApplyWithLocal(offer, policy)
			return mc, err
		}
		forEach(t, negotiate)
	})

	t.Run("outbound", func(t *testing.T) { // Send offer, receive answer
		negotiate := func(t *testing.T, mp *mediaPort, policy sdp.Encryption) (*sdp.MediaConfig, error) {
			offerData, err := mp.GenerateOffer()
			require.NoError(t, err)
			offer, err := parseOfferWith(logger.NewTestLogger(t), nil, defaultCodecs, offerData)
			require.NoError(t, err)
			answer, mc, err := offer.Answer(newIP("127.0.0.1"), 5004, policy)
			if errors.Is(err, sdp.ErrNoCommonCrypto) {
				return mc, err
			}
			require.NoError(t, err)
			answerData, err := answer.SDP.Marshal()
			require.NoError(t, err)
			return mc, mp.ProcessAnswer(answerData)
		}
		forEach(t, negotiate)
	})
}
