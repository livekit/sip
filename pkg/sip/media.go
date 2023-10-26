package sip

import (
	"encoding/binary"
	"net"
	"sync/atomic"
	"time"

	"github.com/livekit/sip/pkg/config"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v3"
	"github.com/pion/webrtc/v3/pkg/media"
	"github.com/zaf/g711"
	"gopkg.in/hraban/opus.v2"

	lksdk "github.com/livekit/server-sdk-go"
)

const (
	demoRoom = "gz4h-1yff"

	channels   = 1
	sampleRate = 8000
)

func createRTPListener(conf *config.Config, participantIdentity string) (*net.UDPConn, error) {
	webrtcAudioSamples := make(chan []byte, 1500)

	roomCB := &lksdk.RoomCallback{
		ParticipantCallback: lksdk.ParticipantCallback{
			OnTrackSubscribed: func(track *webrtc.TrackRemote, publication *lksdk.RemoteTrackPublication, rp *lksdk.RemoteParticipant) {
				if track.Kind() == webrtc.RTPCodecTypeVideo {
					publication.SetSubscribed(false)
					return
				}

				defer func() {
					close(webrtcAudioSamples)
				}()

				decoder, err := opus.NewDecoder(8000, 1)
				if err != nil {
					return
				}

				samples := make([]int16, 1000)
				for {
					rtpPkt, _, err := track.ReadRTP()
					if err != nil {
						return
					}

					n, err := decoder.Decode(rtpPkt.Payload, samples)
					if err != nil {
						return
					}

					out := []byte{}
					for _, sample := range samples[:n] {
						out = append(out, byte(sample&0xff))
						out = append(out, byte(sample>>8))
					}

					webrtcAudioSamples <- g711.EncodeUlaw(out)
				}

			},
		},
	}

	room, err := lksdk.ConnectToRoom(conf.WsUrl,
		lksdk.ConnectInfo{
			APIKey:              conf.ApiKey,
			APISecret:           conf.ApiSecret,
			RoomName:            demoRoom,
			ParticipantIdentity: participantIdentity,
		},
		roomCB,
	)
	if err != nil {
		return nil, err
	}

	track, err := webrtc.NewTrackLocalStaticSample(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus}, "audio", "pion")
	if err != nil {
		return nil, err
	}

	if _, err = room.LocalParticipant.PublishTrack(track, &lksdk.TrackPublicationOptions{
		Name: participantIdentity,
	}); err != nil {
		return nil, err
	}

	enc, err := opus.NewEncoder(sampleRate, channels, opus.AppVoIP)
	if err != nil {
		return nil, err
	}

	conn, err := net.ListenUDP("udp", &net.UDPAddr{
		Port: 0,
		IP:   net.ParseIP("0.0.0.0"),
	})
	if err != nil {
		return nil, err
	}

	var rtpDestination atomic.Value

	go func() {

		rtpPkt := &rtp.Packet{
			Header: rtp.Header{
				Version: 2,
				SSRC:    5000,
			},
		}

		for {
			audioSample, ok := <-webrtcAudioSamples
			if !ok {
				return
			}

			dstAddr, ok := rtpDestination.Load().(*net.UDPAddr)
			if !ok || dstAddr == nil {
				continue
			}

			rtpPkt.Payload = audioSample

			raw, err := rtpPkt.Marshal()
			if err != nil {
				continue
			}

			if _, err = conn.WriteTo(raw, dstAddr); err != nil {
				continue
			}

			rtpPkt.Header.Timestamp += 160
			rtpPkt.Header.SequenceNumber += 1
		}
	}()

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
	}()

	return conn, nil
}
