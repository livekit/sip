// Copyright 2026 LiveKit, Inc.
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

package opus

import (
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	libopus "gopkg.in/hraban/opus.v2"

	msdk "github.com/livekit/media-sdk"
	"github.com/livekit/media-sdk/rtp"
	"github.com/livekit/protocol/logger"
)

func TestEncoderClose(t *testing.T) {
	for _, rate := range []int{8000, 16000, 48000} {
		samples := rate / rtp.DefFramesPerSec
		for _, size := range []int{0, 1, samples / 2, samples - 1, samples, samples + 1} {
			t.Run(fmt.Sprintf("rate=%d/samples=%d", rate, size), func(t *testing.T) {
				var packets []Sample
				w := &testWriter{Writer: msdk.NewFrameWriter(&packets, rate)}
				enc, err := Encode(w, 1, logger.NewTestLogger(t))
				require.NoError(t, err)
				input := make(msdk.PCM16Sample, size)
				for i := range input {
					input[i] = int16((i*193)%20000 - 10000)
				}
				require.NoError(t, enc.WriteSample(input[:size/2]))
				require.NoError(t, enc.WriteSample(input[size/2:]))
				require.Len(t, packets, size/samples)
				closeErr := enc.Close()
				require.Equal(t, 1, w.closes)
				require.NoError(t, closeErr)
				require.Len(t, packets, (size+samples-1)/samples)

				// Compare against complete PCM frames encoded directly by libopus.
				// This also catches stale samples left in the reused input buffer.
				ref, err := libopus.NewEncoder(rate, 1, libopus.AppVoIP)
				require.NoError(t, err)
				dec, err := libopus.NewDecoder(rate, 1)
				require.NoError(t, err)
				for i, packet := range packets {
					frame := make(msdk.PCM16Sample, samples)
					copy(frame, input[i*samples:])
					buf := make([]byte, 4*samples)
					n, err := ref.Encode(frame, buf)
					require.NoError(t, err)
					require.Equal(t, Sample(buf[:n]), packet)
					n, err = dec.Decode(packet, frame)
					require.NoError(t, err)
					require.Equal(t, samples, n)
				}

				require.NoError(t, enc.Close())
				require.Len(t, packets, (size+samples-1)/samples, "close must not encode the tail again")
			})
		}
	}
}

func TestEncoderCloseWriterErrors(t *testing.T) {
	writeErr := errors.New("write failed")
	closeErr := errors.New("close failed")
	for _, err := range []error{nil, writeErr} {
		t.Run(fmt.Sprintf("write error=%v", err), func(t *testing.T) {
			var packets []Sample
			w := &testWriter{
				Writer:   msdk.NewFrameWriter(&packets, 48000),
				writeErr: err,
				closeErr: closeErr,
			}
			enc, err := Encode(w, 1, logger.NewTestLogger(t))
			require.NoError(t, err)
			require.NoError(t, enc.WriteSample(msdk.PCM16Sample{1000}))
			err = enc.Close()
			require.Equal(t, 1, w.closes)
			if w.writeErr != nil {
				require.ErrorIs(t, err, writeErr)
			} else {
				require.ErrorIs(t, err, closeErr)
			}
		})
	}
}

type testWriter struct {
	Writer
	closes   int
	writeErr error
	closeErr error
}

func (w *testWriter) WriteSample(s Sample) error {
	if w.writeErr != nil {
		return w.writeErr
	}
	return w.Writer.WriteSample(s)
}

func (w *testWriter) Close() error {
	w.closes++
	return w.closeErr
}
