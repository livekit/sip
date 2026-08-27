// videotracke2ee publishes or subscribes to an H.264 video track with
// end-to-end encryption applied at the frame layer.
//
// The publish side derives a key from a shared passphrase, wraps each H.264
// sample with lksdk.EncryptGCMH264Sample-equivalent logic via NewFrameEncryptor,
// and advertises Encryption_GCM on the TrackPublicationOptions.
//
// The subscribe side mirrors that setup: same passphrase, matching
// NewFrameDecryptor, and TrackDecryptor to reassemble RTP into complete
// frames before handing them to the decryptor.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	lksdk "github.com/livekit/server-sdk-go/v2"
	"github.com/livekit/server-sdk-go/v2/e2ee"
	e2eetypes "github.com/livekit/server-sdk-go/v2/e2ee/types"
)

const (
	host       = "ws://localhost:7880"
	apiKey     = "devkey"
	apiSecret  = "secret"
	roomName   = "test"
	passphrase = "helloworld"
)

var mode string

func init() {
	flag.StringVar(&mode, "mode", "publish", "publish or subscribe")
}

func main() {
	flag.Parse()

	logger.InitFromConfig(&logger.Config{Level: "info"}, "videotracke2ee")
	lksdk.SetLogger(logger.GetLogger())

	publish := mode == "publish"
	identity := "go-sdk-videotracke2ee"
	if publish {
		identity += "-publisher"
	} else {
		identity += "-subscriber"
	}

	keyProvider := e2ee.NewExternalKeyProvider()
	if err := keyProvider.SetKeyFromPassphrase(passphrase, 0); err != nil {
		panic(err)
	}

	var sifTrailer []byte
	cb := subscribeCallback(keyProvider, &sifTrailer)
	if publish {
		cb = &lksdk.RoomCallback{} // no-op for publishers
	}

	room, err := lksdk.ConnectToRoom(host, lksdk.ConnectInfo{
		APIKey:              apiKey,
		APISecret:           apiSecret,
		RoomName:            roomName,
		ParticipantIdentity: identity,
	}, cb)
	if err != nil {
		panic(err)
	}
	defer room.Disconnect()
	sifTrailer = room.SifTrailer()

	if publish {
		go handlePublish(room, keyProvider)
	}

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT)
	<-sigChan
}

func handlePublish(room *lksdk.Room, kp e2eetypes.KeyProvider) {
	encryptor, err := lksdk.NewFrameEncryptor(kp, lksdk.CodecH264)
	if err != nil {
		panic(err)
	}

	track, err := lksdk.NewLocalTrack(webrtc.RTPCodecCapability{
		MimeType:  webrtc.MimeTypeH264,
		ClockRate: 90000,
	})
	if err != nil {
		panic(err)
	}

	provider := newEncryptedH264Provider(encryptor)
	track.OnBind(func() {
		if err := track.StartWrite(provider, func() {}); err != nil {
			logger.Errorw("start write failed", err)
		}
	})

	if _, err = room.LocalParticipant.PublishTrack(track, &lksdk.TrackPublicationOptions{
		Name:        "video-e2ee",
		Encryption:  livekit.Encryption_GCM,
		VideoWidth:  2,
		VideoHeight: 2,
	}); err != nil {
		panic(err)
	}
}

// subscribeCallback builds the RoomCallback used on the subscriber side: for
// every H.264 track it sees, it constructs a matching FrameDecryptor and a
// TrackDecryptor, then reads decrypted samples in a background goroutine.
func subscribeCallback(kp e2eetypes.KeyProvider, sifTrailer *[]byte) *lksdk.RoomCallback {
	return &lksdk.RoomCallback{
		ParticipantCallback: lksdk.ParticipantCallback{
			OnTrackSubscribed: func(track *webrtc.TrackRemote, pub *lksdk.RemoteTrackPublication, rp *lksdk.RemoteParticipant) {
				if track.Codec().MimeType != webrtc.MimeTypeH264 {
					return
				}
				if enc := pub.TrackInfo().GetEncryption(); enc != livekit.Encryption_GCM {
					logger.Infow("track not GCM-encrypted, skipping", "encryption", enc.String())
					return
				}

				decryptor, err := lksdk.NewFrameDecryptor(kp, lksdk.CodecH264, *sifTrailer)
				if err != nil {
					logger.Errorw("build frame decryptor", err)
					return
				}
				td := e2ee.NewTrackDecryptor(track, decryptor)
				go readDecryptedFrames(td, rp.Identity())
			},
		},
	}
}

func readDecryptedFrames(td *e2ee.TrackDecryptor, fromIdentity string) {
	for {
		sample, err := td.ReadSample()
		if err != nil {
			if err == io.EOF {
				logger.Infow("track ended", "from", fromIdentity)
				return
			}
			logger.Warnw("read decrypted sample", err, "from", fromIdentity)
			return
		}
		if sample == nil {
			continue
		}
		logger.Infow("decrypted frame", "from", fromIdentity, "bytes", len(sample.Data))
	}
}

// encryptedH264Provider feeds the LocalTrack a minimal looping H.264 key frame,
// encrypted per-frame with the supplied FrameEncryptor.
type encryptedH264Provider struct {
	lksdk.BaseSampleProvider
	encryptor e2eetypes.FrameEncryptor
}

func newEncryptedH264Provider(enc e2eetypes.FrameEncryptor) *encryptedH264Provider {
	return &encryptedH264Provider{encryptor: enc}
}

// Minimal H.264 Annex-B keyframe (2x2). Same bytes used by integration_test.go.
var h264KeyFrame = [][]byte{
	{0x67, 0x42, 0xc0, 0x1f, 0x0f, 0xd9, 0x1f, 0x88, 0x88, 0x84,
		0x00, 0x00, 0x03, 0x00, 0x04, 0x00, 0x00, 0x03, 0x00, 0xc8, 0x3c, 0x60, 0xc9, 0x20}, // SPS
	{0x68, 0x87, 0xcb, 0x83, 0xcb, 0x20},                                                     // PPS
	{0x65, 0x88, 0x84, 0x0a, 0xf2, 0x62, 0x80, 0x00, 0xa7, 0xbe},                             // IDR slice
}

func (p *encryptedH264Provider) NextSample(_ context.Context) (media.Sample, error) {
	var frame []byte
	for _, nalu := range h264KeyFrame {
		frame = append(frame, 0x00, 0x00, 0x00, 0x01)
		frame = append(frame, nalu...)
	}

	encrypted, err := p.encryptor.EncryptFrame(frame)
	if err != nil {
		return media.Sample{}, fmt.Errorf("encrypt frame: %w", err)
	}

	return media.Sample{
		Data:     encrypted,
		Duration: 100 * time.Millisecond,
	}, nil
}
