// Copyright 2026 LiveKit, Inc.
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

package sip

import (
	"io"
	"testing"

	msdk "github.com/livekit/media-sdk"
	"github.com/livekit/protocol/logger"
	"github.com/stretchr/testify/require"
)

// Pipelines borrow the room writer. Releasing that borrow must keep the writer
// usable for a replacement pipeline, while closing its owner revokes all borrows.
func TestMediaInputWriterFollowsPortLifetime(t *testing.T) {
	remote, receiver := newUDPPipe()
	defer remote.Close()
	port, err := NewMediaPortWith(logger.GetLogger(), newTestCallMonitor(t), receiver,
		&MediaOptions{IP: receiver.addr.Addr()}, 8000)
	require.NoError(t, err)
	defer port.Close()

	out := &mediaPortPlayoutTrace{}
	require.Nil(t, port.WriteInboundAudioTo(out))
	var borrowed msdk.PCM16Writer = mediaInputWriter{port: port.(*mediaPort)}
	first, second := msdk.PCM16Sample{1000, 2000}, msdk.PCM16Sample{-1000, -2000}
	require.NoError(t, borrowed.WriteSample(first))
	require.NoError(t, borrowed.Close())
	_, closed, writesAfterClose := out.snapshot()
	require.False(t, closed, "a pipeline must not close the shared room writer")
	require.Zero(t, writesAfterClose)
	require.NoError(t, borrowed.WriteSample(second))

	port.Close()
	require.ErrorIs(t, borrowed.WriteSample(msdk.PCM16Sample{3000}), io.ErrClosedPipe)
	frames, closed, writesAfterClose := out.snapshot()
	require.True(t, closed, "the media port owns closing the room writer")
	require.Zero(t, writesAfterClose, "writes after shutdown must be rejected before reaching the room writer")
	require.Len(t, frames, 2)
	require.Equal(t, first, frames[0].pcm)
	require.Equal(t, second, frames[1].pcm)
}
