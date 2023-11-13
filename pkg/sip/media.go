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

	"github.com/livekit/sip/pkg/config"
	"github.com/livekit/sip/pkg/mixer"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v3"
	"github.com/pion/webrtc/v3/pkg/media"
	"github.com/zaf/g711"
	"gopkg.in/hraban/opus.v2"

	lksdk "github.com/livekit/server-sdk-go"
)

const (
	channels   = 1
	sampleRate = 8000
)

func createLiveKitParticipant(conf *config.Config, roomName, participantIdentity string, audioMixer *mixer.Mixer) (*webrtc.TrackLocalStaticSample, *lksdk.Room, error) {
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

				input := audioMixer.AddInput()
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

				audioMixer.RemoveInput(input)

			},
		},
	}

	room, err := lksdk.ConnectToRoom(conf.WsUrl,
		lksdk.ConnectInfo{
			APIKey:              conf.ApiKey,
			APISecret:           conf.ApiSecret,
			RoomName:            roomName,
			ParticipantIdentity: participantIdentity,
		},
		roomCB,
	)
	if err != nil {
		return nil, nil, err
	}

	track, err := webrtc.NewTrackLocalStaticSample(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus}, "audio", "pion")
	if err != nil {
		return nil, nil, err
	}

	if _, err = room.LocalParticipant.PublishTrack(track, &lksdk.TrackPublicationOptions{
		Name: participantIdentity,
	}); err != nil {
		return nil, nil, err
	}

	return track, room, nil
}

func createMediaSession(conf *config.Config, roomName, participantIdentity string) (*net.UDPConn, error) {
	var rtpDestination atomic.Value
	conn, err := net.ListenUDP("udp", &net.UDPAddr{
		Port: 0,
		IP:   net.ParseIP("0.0.0.0"),
	})
	if err != nil {
		return nil, err
	}

	mixerRtpPkt := &rtp.Packet{
		Header: rtp.Header{
			Version: 2,
			SSRC:    5000,
		},
	}
	audioMixer := mixer.NewMixer(func(audioSample []byte, sampleRate) {
		dstAddr, ok := rtpDestination.Load().(*net.UDPAddr)
		if !ok || dstAddr == nil {
			return
		}

		mixerRtpPkt.Payload = g711.EncodeUlaw(audioSample)

		raw, err := mixerRtpPkt.Marshal()
		if err != nil {
			return
		}

		if _, err = conn.WriteTo(raw, dstAddr); err != nil {
			return
		}

		mixerRtpPkt.Header.Timestamp += 160
		mixerRtpPkt.Header.SequenceNumber += 1
	}, 8000)

	track, room, err := createLiveKitParticipant(conf, roomName, participantIdentity, audioMixer)
	if err != nil {
		return nil, err
	}

	enc, err := opus.NewEncoder(sampleRate, channels, opus.AppVoIP)
	if err != nil {
		return nil, err
	}

	go func() {
		buff := make([]byte, 1500)
		rtpPkt := &rtp.Packet{}

		for {
			n, srcAddr, err := conn.ReadFromUDP(buff)
			if err != nil {
				break
			}

			rtpDestination.Store(srcAddr)

			if err := rtpPkt.Unmarshal(buff[:n]); err != nil {
				continue
			}

			decoded := g711.DecodeUlaw(rtpPkt.Payload)

			var pcm []int16
			for i := 0; i < len(decoded); i += 2 {
				sample := binary.LittleEndian.Uint16(decoded[i:])
				pcm = append(pcm, int16(sample))
			}

			data := make([]byte, 1000)
			n, err = enc.Encode(pcm, data)
			if err != nil {
				continue
			}

			if err = track.WriteSample(media.Sample{Data: data[:n], Duration: time.Millisecond * 20}); err != nil {
				continue
			}
		}

		room.Disconnect()
		audioMixer.Stop()
	}()

	return conn, nil
}
