// Copyright 2024 LiveKit, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// 	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"context"
	"flag"
	"time"

	"github.com/livekit/sip/test/lktest"
)

var (
	fOutURL    = flag.String("ws-out", "", "LiveKit WS URL (outbound)")
	fOutKey    = flag.String("key-out", "", "LiveKit API key (outbound)")
	fOutSecret = flag.String("secret-out", "", "LiveKit API secret (outbound)")
	fOutTrunk  = flag.String("trunk-out", "", "SIP Trunk ID (outbound)")
	fOutRoom   = flag.String("room-out", "sip-test-out", "room name (outbound)")

	fInURL    = flag.String("ws-in", "", "LiveKit WS URL (inbound)")
	fInKey    = flag.String("key-in", "", "LiveKit API key (inbound)")
	fInSecret = flag.String("secret-in", "", "LiveKit API secret (inbound)")

	fTimeout = flag.Duration("timeout", time.Minute, "timeout for the test")
)

func main() {
	lktest.TestMain(run)
}

func run(ctx context.Context, t lktest.TB) {
	flag.Parse()

	ctx, cancel := context.WithTimeout(ctx, *fTimeout)
	defer cancel()

	lkOut := lktest.New(*fOutURL, *fOutKey, *fOutSecret)
	lkIn := lktest.New(*fInURL, *fInKey, *fInSecret)

	lktest.TestSIPOutbound(t, ctx, lkOut, lkIn, lktest.SIPOutboundTestParams{
		TrunkOut: *fOutTrunk,
		RoomOut:  *fOutRoom,
	})
}
