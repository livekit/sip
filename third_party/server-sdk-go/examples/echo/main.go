package main

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"

	"github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/logger"
	lksdk "github.com/livekit/server-sdk-go/v2"
)

var (
	host, apiKey, apiSecret, roomName, identity string
	firstParticipantSubscribed                  = false
)

func init() {
	flag.StringVar(&host, "host", "", "livekit server host")
	flag.StringVar(&apiKey, "api-key", "", "livekit api key")
	flag.StringVar(&apiSecret, "api-secret", "", "livekit api secret")
	flag.StringVar(&roomName, "room-name", "", "room name")
	flag.StringVar(&identity, "identity", "", "participant identity")
}

func main() {
	logger.InitFromConfig(&logger.Config{Level: "debug"}, "echo")
	lksdk.SetLogger(logger.GetLogger())
	flag.Parse()
	if host == "" || apiKey == "" || apiSecret == "" || roomName == "" || identity == "" {
		fmt.Println("invalid arguments.")
		return
	}

	echoTrack, err := lksdk.NewLocalTrack(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus})
	if err != nil {
		panic(err)
	}

	room := lksdk.NewRoom(&lksdk.RoomCallback{
		ParticipantCallback: lksdk.ParticipantCallback{
			OnTrackSubscribed: func(track *webrtc.TrackRemote, publication *lksdk.RemoteTrackPublication, rp *lksdk.RemoteParticipant) {
				// Only provide echo for the first participant
				if !firstParticipantSubscribed && track.Kind() == webrtc.RTPCodecTypeAudio {
					firstParticipantSubscribed = true
					onTrackSubscribed(track, echoTrack)
				}
			},
		},
	})

	token, err := newAccessToken(apiKey, apiSecret, roomName, identity)
	if err != nil {
		panic(err)
	}

	// not required. warm up the connection for a participant that may join later.
	if err := room.PrepareConnection(host, token); err != nil {
		panic(err)
	}

	if err := room.JoinWithToken(host, token); err != nil {
		panic(err)
	}

	if _, err = room.LocalParticipant.PublishTrack(echoTrack, &lksdk.TrackPublicationOptions{
		Name: "echo",
	}); err != nil {
		panic(err)
	}

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT)

	<-sigChan
	room.Disconnect()
}

func onTrackSubscribed(track *webrtc.TrackRemote, echoTrack *lksdk.LocalTrack) {
	for {
		pkt, _, err := track.ReadRTP()
		if err != nil {
			continue
		}
		echoTrack.WriteSample(media.Sample{Data: pkt.Payload, Duration: 20 * time.Millisecond}, &lksdk.SampleWriteOptions{})
	}
}

func newAccessToken(apiKey, apiSecret, roomName, pID string) (string, error) {
	at := auth.NewAccessToken(apiKey, apiSecret)
	grant := &auth.VideoGrant{
		RoomJoin: true,
		Room:     roomName,
	}
	at.SetVideoGrant(grant).
		SetIdentity(pID).
		SetName(pID)

	return at.ToJWT()
}
