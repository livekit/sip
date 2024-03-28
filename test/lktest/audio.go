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

package lktest

import (
	"context"
	"io"

	"github.com/stretchr/testify/require"
)

type AudioParticipant interface {
	SendSignal(ctx context.Context, n int, val int) error
	WaitSignals(ctx context.Context, vals []int, w io.WriteCloser) error
}

func CheckAudioForParticipants(t TB, ctx context.Context, participants ...AudioParticipant) {
	// Participants can only subscribe to tracks that are "live", so give them the chance to do so.
	for _, p := range participants {
		err := p.SendSignal(ctx, 3, 0)
		require.NoError(t, err)
	}

	// Each client will emit its own audio signal.
	var allSig []int
	for i, p := range participants {
		p := p
		sig := i + 1
		allSig = append(allSig, sig)
		go func() {
			err := p.SendSignal(ctx, -1, sig)
			require.NoError(t, err)
		}()
	}

	// And they will wait for the other one's signal.
	errc := make(chan error, len(participants))
	for i, p := range participants {
		p := p
		// Expect all signals except its own.
		var signals []int
		signals = append(signals, allSig[:i]...)
		signals = append(signals, allSig[i+1:]...)
		go func() {
			errc <- p.WaitSignals(ctx, signals, nil)
		}()
	}

	// Wait for the signal sequence to be received by both (or the timeout).
	for i := 0; i < len(participants); i++ {
		err := <-errc
		require.NoError(t, err)
	}
}
