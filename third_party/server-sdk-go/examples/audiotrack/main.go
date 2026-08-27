package main

import (
	"flag"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/pion/webrtc/v4"

	"github.com/livekit/media-sdk/res"
	"github.com/livekit/media-sdk/res/testdata"
	"github.com/livekit/media-sdk/webm"
	"github.com/livekit/protocol/logger"
	lksdk "github.com/livekit/server-sdk-go/v2"
	lkmedia "github.com/livekit/server-sdk-go/v2/pkg/media"
)

const (
	host      = "ws://localhost:7880"
	apiKey    = "devkey"
	apiSecret = "secret"
	roomName  = "test"
)

var (
	participantIdentity = "go-sdk"
	mode                string
	subscribePCMTrack   *lkmedia.PCMRemoteTrack
	subscribeFileWriter *os.File
)

func init() {
	flag.StringVar(&mode, "mode", "publish", "publish or subscribe")
}

func connectToRoom(cb *lksdk.RoomCallback) (*lksdk.Room, error) {
	room, err := lksdk.ConnectToRoom(host, lksdk.ConnectInfo{
		APIKey:              apiKey,
		APISecret:           apiSecret,
		RoomName:            roomName,
		ParticipantIdentity: participantIdentity,
	}, cb)
	if err != nil {
		return nil, err
	}
	return room, nil
}

func getCbForRoom(publish bool) *lksdk.RoomCallback {
	if !publish {
		return &lksdk.RoomCallback{
			ParticipantCallback: lksdk.ParticipantCallback{
				OnTrackSubscribed: func(track *webrtc.TrackRemote, publication *lksdk.RemoteTrackPublication, rp *lksdk.RemoteParticipant) {
					if track.Codec().MimeType == webrtc.MimeTypeOpus {
						subscribePCMTrack, subscribeFileWriter = handleSubscribe(track, 1)
					}
				},
				OnTrackUnsubscribed: func(track *webrtc.TrackRemote, publication *lksdk.RemoteTrackPublication, rp *lksdk.RemoteParticipant) {
					// delay to ensure the final read before track is closed
					// is written to the writer (and the file in this case)
					time.Sleep(1 * time.Second)
					if subscribePCMTrack != nil {
						subscribePCMTrack.Close()
					}
					if subscribeFileWriter != nil {
						subscribeFileWriter.Close()
					}
				},
			},
		}
	}
	return nil
}

func main() {
	flag.Parse()

	logger.InitFromConfig(&logger.Config{Level: "info"}, "pcmopus")
	lksdk.SetLogger(logger.GetLogger())

	publish := mode == "publish"
	if publish {
		participantIdentity += "-publisher"
	} else {
		participantIdentity += "-subscriber"
	}

	room, err := connectToRoom(getCbForRoom(publish))
	if err != nil {
		panic(err)
	}
	defer room.Disconnect()

	if publish {
		go handlePublish(room)
	}

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT)

	<-sigChan
}

func handlePublish(room *lksdk.Room) {
	publishTrack, err := lkmedia.NewPCMLocalTrack(lkmedia.DefaultOpusSampleRate, 1, logger.GetLogger())
	if err != nil {
		panic(err)
	}
	defer func() {
		publishTrack.ClearQueue()
		publishTrack.Close()
	}()

	if _, err = room.LocalParticipant.PublishTrack(publishTrack, &lksdk.TrackPublicationOptions{
		Name: "test",
	}); err != nil {
		panic(err)
	}

	pcmSamples := res.ReadOggAudioFile(testdata.TestAudioOgg, 48000, 1)
	for {
		for _, sample := range pcmSamples {
			err = publishTrack.WriteSample(sample)
			if err != nil {
				logger.Errorw("error writing sample", err)
			}
			// temp: some delay before writing next sample
			time.Sleep(15 * time.Millisecond)
		}
	}
}

func handleSubscribe(track *webrtc.TrackRemote, targetChannels int) (*lkmedia.PCMRemoteTrack, *os.File) {
	fileWriter, err := os.Create("test.mka")
	if err != nil {
		panic(err)
	}

	webmWriter := webm.NewPCM16Writer(fileWriter, lkmedia.DefaultOpusSampleRate, targetChannels, lkmedia.DefaultOpusSampleDuration)
	pcmTrack, err := lkmedia.NewPCMRemoteTrack(track, webmWriter)
	if err != nil {
		panic(err)
	}

	return pcmTrack, fileWriter
}
