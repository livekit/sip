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
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"

	"github.com/livekit/media-sdk/dtmf"
	"github.com/livekit/media-sdk/g711"

	"github.com/livekit/sip/pkg/siptest"
)

var (
	sipServer    = flag.String("sip-server", "", "SIP server to connect to")
	to           = flag.String("to", "+15550100000", "number to dial")
	from         = flag.String("from", "+15550100001", "client number")
	username     = flag.String("username", "", "username for INVITE")
	password     = flag.String("password", "", "password for INVITE")
	sipUri       = flag.String("sip-uri", "example.pstn.twilio.com", "SIP server URI")
	filePathPlay = flag.String("play", "audio.mkv", "play audio")
	filePathSave = flag.String("save", "save.mkv", "save incoming audio to file")
	sendDTMF     = flag.String("dtmf", "", "send DTMF sequence")
	codec        = flag.String("codec", g711.ULawSDPName, "audio codec")
)

func main() {
	flag.Parse()

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	cli, err := siptest.NewClient("", siptest.ClientConfig{
		Number:   *from,
		AuthUser: *username,
		AuthPass: *password,
		Codec:    *codec,
		OnBye: func() {
			cancel()
		},
		OnDTMF: func(ev dtmf.Event) {
			fmt.Println("DTMF C:", ev.Code, " D:", string(ev.Digit))
		},
	})
	if err != nil {
		panic(err)
	}
	defer cli.Close()

	if *sipServer == "" {
		*sipServer = cli.LocalIP() + ":5060"
	}

	if *filePathSave != "" {
		f, err := os.Create(*filePathSave)
		if err != nil {
			panic(err)
		}
		defer f.Close()
		cli.Record(f)
	}

	if err = cli.Dial(*sipServer, *sipUri, *to, nil); err != nil {
		panic(err)
	}

	go func() {
		<-ctx.Done()
		cli.Close()
	}()

	if dtmf := *sendDTMF; dtmf != "" {
		if err = cli.SendDTMF(dtmf); err != nil {
			panic(err)
		}
	}

	if err = cli.SendAudio(*filePathPlay); err != nil {
		if !errors.Is(err, net.ErrClosed) {
			panic(err)
		}
	}
}
