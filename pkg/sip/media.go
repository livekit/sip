package sip

import (
	"encoding/binary"
	"net"
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
	room, err := lksdk.ConnectToRoom(conf.WsUrl,
		lksdk.ConnectInfo{
			APIKey:              conf.ApiKey,
			APISecret:           conf.ApiSecret,
			RoomName:            demoRoom,
			ParticipantIdentity: participantIdentity,
		},
		lksdk.NewRoomCallback(),
		lksdk.WithAutoSubscribe(false),
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

	go func() {
		buff := make([]byte, 1500)
		rtpPkt := &rtp.Packet{}

		for {
			n, _, err := conn.ReadFromUDP(buff)
			if err != nil {
				break
			}

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
