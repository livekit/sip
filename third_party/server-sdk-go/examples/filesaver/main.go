// Copyright 2023 LiveKit, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"

	"github.com/pion/rtp/codecs"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
	"github.com/pion/webrtc/v4/pkg/media/h264writer"
	"github.com/pion/webrtc/v4/pkg/media/h265writer"
	"github.com/pion/webrtc/v4/pkg/media/ivfwriter"
	"github.com/pion/webrtc/v4/pkg/media/oggwriter"

	"github.com/livekit/protocol/logger"
	lksdk "github.com/livekit/server-sdk-go/v2"
	"github.com/livekit/server-sdk-go/v2/pkg/samplebuilder"
)

var (
	host, apiKey, apiSecret, roomName, identity string

	writeWG sync.WaitGroup
)

func init() {
	flag.StringVar(&host, "host", "", "livekit server host")
	flag.StringVar(&apiKey, "api-key", "", "livekit api key")
	flag.StringVar(&apiSecret, "api-secret", "", "livekit api secret")
	flag.StringVar(&roomName, "room-name", "", "room name")
	flag.StringVar(&identity, "identity", "", "participant identity")
}

func main() {
	logger.InitFromConfig(&logger.Config{Level: "debug"}, "filesaver")
	lksdk.SetLogger(logger.GetLogger())
	flag.Parse()
	if host == "" || apiKey == "" || apiSecret == "" || roomName == "" || identity == "" {
		fmt.Println("invalid arguments.")
		return
	}
	room, err := lksdk.ConnectToRoom(host, lksdk.ConnectInfo{
		APIKey:              apiKey,
		APISecret:           apiSecret,
		RoomName:            roomName,
		ParticipantIdentity: identity,
	}, &lksdk.RoomCallback{
		ParticipantCallback: lksdk.ParticipantCallback{
			OnTrackSubscribed: onTrackSubscribed,
		},
	})
	if err != nil {
		panic(err)
	}

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT)

	<-sigChan
	room.Disconnect()
	writeWG.Wait()
}

func onTrackSubscribed(track *webrtc.TrackRemote, publication *lksdk.RemoteTrackPublication, rp *lksdk.RemoteParticipant) {
	fileName := fmt.Sprintf("%s-%s", rp.Identity(), track.ID())
	fmt.Println("write track to file ", fileName)
	NewTrackWriter(track, rp.WritePLI, fileName)
}

const (
	maxVideoLate = 1000 // nearly 2s for fhd video
	maxAudioLate = 200  // 4s for audio
)

type TrackWriter struct {
	sb     *samplebuilder.SampleBuilder
	writer media.Writer
	track  *webrtc.TrackRemote
}

func NewTrackWriter(track *webrtc.TrackRemote, pliWriter lksdk.PLIWriter, fileName string) (*TrackWriter, error) {
	var (
		sb     *samplebuilder.SampleBuilder
		writer media.Writer
		err    error
	)
	switch {
	case strings.EqualFold(track.Codec().MimeType, "video/vp8"):
		sb = samplebuilder.New(maxVideoLate, &codecs.VP8Packet{}, track.Codec().ClockRate, samplebuilder.WithPacketDroppedHandler(func() {
			pliWriter(track.SSRC())
		}))
		// ivfwriter use frame count as PTS, that might cause video played in a incorrect framerate(fast or slow)
		writer, err = ivfwriter.New(fileName + ".ivf")

	case strings.EqualFold(track.Codec().MimeType, "video/h264"):
		sb = samplebuilder.New(maxVideoLate, &codecs.H264Packet{}, track.Codec().ClockRate, samplebuilder.WithPacketDroppedHandler(func() {
			pliWriter(track.SSRC())
		}))
		writer, err = h264writer.New(fileName + ".h264")

	case strings.EqualFold(track.Codec().MimeType, "video/h265"):
		sb = samplebuilder.New(maxVideoLate, &codecs.H265Packet{}, track.Codec().ClockRate, samplebuilder.WithPacketDroppedHandler(func() {
			pliWriter(track.SSRC())
		}))
		writer, err = h265writer.New(fileName + ".h265")

	case strings.EqualFold(track.Codec().MimeType, "audio/opus"):
		sb = samplebuilder.New(maxAudioLate, &codecs.OpusPacket{}, track.Codec().ClockRate)
		writer, err = oggwriter.New(fileName+".ogg", 48000, track.Codec().Channels)

	default:
		return nil, errors.New("unsupported codec type")
	}

	if err != nil {
		return nil, err
	}

	t := &TrackWriter{
		sb:     sb,
		writer: writer,
		track:  track,
	}
	writeWG.Add(1)
	go t.start()
	return t, nil
}

func (t *TrackWriter) start() {
	defer func() {
		t.writer.Close()
		writeWG.Done()
	}()

	for {
		pkt, _, err := t.track.ReadRTP()
		if err != nil {
			logger.Errorw("failed to read rtp packet", err)
			break
		}
		t.sb.Push(pkt)

		for _, p := range t.sb.PopPackets() {
			if err := t.writer.WriteRTP(p); err != nil {
				logger.Errorw("failed to write rtp packet", err)
			}
		}
	}
}
