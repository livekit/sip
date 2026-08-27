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
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	lksdk "github.com/livekit/server-sdk-go/v2"
)

var (
	host, apiKey, apiSecret, roomName, identity string
)

func init() {
	flag.StringVar(&host, "host", "", "livekit server host")
	flag.StringVar(&apiKey, "api-key", "", "livekit api key")
	flag.StringVar(&apiSecret, "api-secret", "", "livekit api secret")
	flag.StringVar(&roomName, "room-name", "", "room name")
	flag.StringVar(&identity, "identity", "", "participant identity")
}

func main() {
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
	}, &lksdk.RoomCallback{}, lksdk.WithAutoSubscribe(false))
	if err != nil {
		panic(err)
	}

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT)

	files, err := os.ReadDir(".")
	if err != nil {
		panic(err)
	}

	for _, file := range files {
		if file.IsDir() {
			continue
		} else if !strings.HasSuffix(file.Name(), ".h264") && !strings.HasSuffix(file.Name(), ".ivf") && !strings.HasSuffix(file.Name(), ".ogg") {
			continue
		}

		frameDuration := 33 * time.Millisecond
		if strings.HasSuffix(file.Name(), ".ogg") {
			frameDuration = 20 * time.Millisecond
		}

		track, err := lksdk.NewLocalFileTrack(file.Name(),
			lksdk.ReaderTrackWithFrameDuration(frameDuration),
			lksdk.ReaderTrackWithOnWriteComplete(func() { fmt.Println("track finished") }),
		)
		if err != nil {
			panic(err)
		}

		if _, err = room.LocalParticipant.PublishTrack(track, &lksdk.TrackPublicationOptions{
			VideoWidth:  640,
			VideoHeight: 480,
			Name:        file.Name(),
		}); err != nil {
			panic(err)
		}

	}

	<-sigChan
	room.Disconnect()
}
