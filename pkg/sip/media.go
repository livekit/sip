// Copyright 2023 LiveKit, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package sip

import (
	"encoding/binary"
	"log"
	"net"
	"sync/atomic"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/webrtc/v3"
	"github.com/pion/webrtc/v3/pkg/media"
	"github.com/zaf/g711"
	"gopkg.in/hraban/opus.v2"

	"github.com/livekit/sip/pkg/mixer"

	lksdk "github.com/livekit/server-sdk-go"
)

const (
	channels   = 1
	sampleRate = 8000
)

type mediaData struct {
	conn  *net.UDPConn
	mix   *mixer.Mixer
	enc   *opus.Encoder
	dest  atomic.Pointer[net.UDPAddr]
	track atomic.Pointer[webrtc.TrackLocalStaticSample]
	room  atomic.Pointer[lksdk.Room]
	dtmf  chan byte
}

func (c *inboundCall) initMedia() {
	c.media.dtmf = make(chan byte, 10)
}

func (c *inboundCall) closeMedia() {
	if p := c.media.room.Load(); p != nil {
		p.Disconnect()
		c.media.room.Store(nil)
	}
	if p := c.media.track.Load(); p != nil {
		c.media.track.Store(nil)
	}
	c.media.mix.Stop()
	c.media.conn.Close()
	close(c.media.dtmf)
}

func (c *inboundCall) createMediaSession() (*net.UDPAddr, error) {
	conn, err := net.ListenUDP("udp", &net.UDPAddr{
		Port: 0,
		IP:   net.ParseIP("0.0.0.0"),
	})
	if err != nil {
		return nil, err
	}
	c.media.conn = conn

	mixerRtpPkt := &rtp.Packet{
		Header: rtp.Header{
			Version: 2,
			SSRC:    5000,
		},
	}
	c.media.mix = mixer.NewMixer(func(audioSample []byte) {
		dstAddr := c.media.dest.Load()
		if dstAddr == nil {
			return
		}

		mixerRtpPkt.Payload = g711.EncodeUlaw(audioSample)

		raw, err := mixerRtpPkt.Marshal()
		if err != nil {
			return
		}

		if _, err = c.media.conn.WriteTo(raw, dstAddr); err != nil {
			return
		}

		mixerRtpPkt.Header.Timestamp += 160
		mixerRtpPkt.Header.SequenceNumber += 1
	}, 8000)

	enc, err := opus.NewEncoder(sampleRate, channels, opus.AppVoIP)
	if err != nil {
		return nil, err
	}
	c.media.enc = enc

	go c.readMedia()
	return conn.LocalAddr().(*net.UDPAddr), nil
}

func (c *inboundCall) readMedia() {
	buff := make([]byte, 1500)
	var rtpPkt rtp.Packet
	for {
		n, srcAddr, err := c.media.conn.ReadFromUDP(buff)
		if err != nil {
			return
		}
		c.media.dest.Store(srcAddr)

		if err := rtpPkt.Unmarshal(buff[:n]); err != nil {
			continue
		}
		c.handleRTP(&rtpPkt)
	}
}

func (c *inboundCall) handleRTP(p *rtp.Packet) {
	if p.Marker && p.PayloadType == 101 {
		c.handleDTMF(p.Payload)
		return
	}
	// TODO: Audio data appears to be coming with PayloadType=0, so maybe enforce it?
	c.handleAudio(p.Payload)
}

var dtmfEventToChar = [256]byte{
	0: '0', 1: '1', 2: '2', 3: '3', 4: '4',
	5: '5', 6: '6', 7: '7', 8: '8', 9: '9',
	10: '*', 11: '#',
	12: 'a', 13: 'b', 14: 'c', 15: 'd',
}

func (c *inboundCall) handleDTMF(data []byte) { // RFC2833
	if len(data) < 4 {
		return
	}
	ev := data[0]
	b := dtmfEventToChar[ev]
	// We should have enough buffer here.
	select {
	case c.media.dtmf <- b:
	default:
	}
}

func (c *inboundCall) handleAudio(audioData []byte) {
	track := c.media.track.Load()
	if track == nil {
		return
	}
	decoded := g711.DecodeUlaw(audioData)

	var pcm []int16
	for i := 0; i < len(decoded); i += 2 {
		sample := binary.LittleEndian.Uint16(decoded[i:])
		pcm = append(pcm, int16(sample))
	}

	data := make([]byte, 1000)
	n, err := c.media.enc.Encode(pcm, data)
	if err != nil {
		return
	}
	if err = track.WriteSample(media.Sample{Data: data[:n], Duration: time.Millisecond * 20}); err != nil {
		return
	}
}

func (c *inboundCall) createLiveKitParticipant(roomName, participantIdentity string) error {
	roomCB := &lksdk.RoomCallback{
		ParticipantCallback: lksdk.ParticipantCallback{
			OnTrackSubscribed: func(track *webrtc.TrackRemote, publication *lksdk.RemoteTrackPublication, rp *lksdk.RemoteParticipant) {
				if track.Kind() == webrtc.RTPCodecTypeVideo {
					if err := publication.SetSubscribed(false); err != nil {
						log.Println(err)
					}
					return
				}

				decoder, err := opus.NewDecoder(8000, 1)
				if err != nil {
					return
				}

				input := c.media.mix.AddInput()
				samples := make([]int16, 1000)
				for {
					rtpPkt, _, err := track.ReadRTP()
					if err != nil {
						break
					}

					n, err := decoder.Decode(rtpPkt.Payload, samples)
					if err != nil {
						break
					}

					input.Push(samples[:n])
				}
				c.media.mix.RemoveInput(input)
			},
		},
	}

	room, err := lksdk.ConnectToRoom(c.s.conf.WsUrl,
		lksdk.ConnectInfo{
			APIKey:              c.s.conf.ApiKey,
			APISecret:           c.s.conf.ApiSecret,
			RoomName:            roomName,
			ParticipantIdentity: participantIdentity,
		},
		roomCB,
	)
	if err != nil {
		return err
	}

	track, err := webrtc.NewTrackLocalStaticSample(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus}, "audio", "pion")
	if err != nil {
		return err
	}

	if _, err = room.LocalParticipant.PublishTrack(track, &lksdk.TrackPublicationOptions{
		Name: participantIdentity,
	}); err != nil {
		return err
	}
	c.media.track.Store(track)
	c.media.room.Store(room)
	return nil
}

func (c *inboundCall) joinRoom(roomName, identity string) {
	log.Printf("Bridging SIP call %q -> %q to room %q (as %q)\n", c.from.Address.User, c.to.Address.User, roomName, identity)
	if err := c.createLiveKitParticipant(roomName, identity); err != nil {
		log.Println(err)
	}
}

func (c *inboundCall) playPleaseEnterPin() {
	// FIXME: play "Please enter room pin" audio
}
