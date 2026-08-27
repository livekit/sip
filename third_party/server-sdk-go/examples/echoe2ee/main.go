package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"

	"github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	lksdk "github.com/livekit/server-sdk-go/v2"
)

var (
	host, apiKey, apiSecret, roomName, identity, passphrase string
	key                                                     []byte
	room                                                    *lksdk.Room
	closeTrack                                              chan struct{} = make(chan struct{}, 1)
)

func init() {
	flag.StringVar(&host, "host", "", "livekit server host")
	flag.StringVar(&apiKey, "api-key", "", "livekit api key")
	flag.StringVar(&apiSecret, "api-secret", "", "livekit api secret")
	flag.StringVar(&roomName, "room-name", "", "room name")
	flag.StringVar(&identity, "identity", "", "participant identity")
	flag.StringVar(&passphrase, "passphrase", "", "participant identity")
}

func main() {
	logger.InitFromConfig(&logger.Config{Level: "debug"}, "echoe2ee")
	lksdk.SetLogger(logger.GetLogger())
	flag.Parse()
	if host == "" || apiKey == "" || apiSecret == "" || roomName == "" || identity == "" {
		fmt.Println("invalid arguments.")
		return
	}

	if passphrase != "" {
		var err error
		key, err = lksdk.DeriveKeyFromString(passphrase)
		if err != nil {
			panic(err)
		}
	}

	echoTrack, err := lksdk.NewLocalTrack(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus})
	if err != nil {
		panic(err)
	}

	room = lksdk.NewRoom(&lksdk.RoomCallback{
		ParticipantCallback: lksdk.ParticipantCallback{
			OnTrackSubscribed: func(track *webrtc.TrackRemote, publication *lksdk.RemoteTrackPublication, rp *lksdk.RemoteParticipant) {
				// Only provide echo for the first participant
				if track.Kind() == webrtc.RTPCodecTypeAudio {
					onTrackSubscribed(track, publication, echoTrack)
				}
			},
			OnTrackUnsubscribed: func(track *webrtc.TrackRemote, publication *lksdk.RemoteTrackPublication, rp *lksdk.RemoteParticipant) {
				if track.Kind() == webrtc.RTPCodecTypeAudio {
					closeTrack <- struct{}{}
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
		Name:       "echo",
		Encryption: livekit.Encryption_GCM,
	}); err != nil {
		panic(err)
	}

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT)

	<-sigChan
	room.Disconnect()
}

func onTrackSubscribed(track *webrtc.TrackRemote, publication *lksdk.RemoteTrackPublication, echoTrack *lksdk.LocalTrack) {
	for {

		select {
		case <-closeTrack:
			return
		default:
			pkt, _, err := track.ReadRTP()
			if err != nil {
				continue
			}
			receivedSample := pkt.Payload
			sendSample := pkt.Payload
			encryption_type := publication.TrackInfo().GetEncryption()

			if encryption_type == livekit.Encryption_GCM && key == nil {
				panic(errors.New("received encrypted track but passphrase is not provided"))
			}

			if encryption_type == livekit.Encryption_GCM {
				receivedSample, err = lksdk.DecryptGCMAudioSample(receivedSample, key, room.SifTrailer())
				if err != nil {
					panic((err))
				}

				if receivedSample != nil {
					sendSample, err = lksdk.EncryptGCMAudioSample(receivedSample, key, 0)
					if err != nil {
						panic((err))
					}

				} else {
					sendSample = nil
				}

			}
			if sendSample != nil {
				echoTrack.WriteSample(media.Sample{Data: sendSample, Duration: 20 * time.Millisecond}, &lksdk.SampleWriteOptions{})
			}

		}

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
