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

package g711

import (
	"encoding/binary"
	"os"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/livekit/sip/pkg/media"
)

func TestG711(t *testing.T) {
	const path = "testdata/sweep"
	src := readPCM16(t, path+".s16le")
	testLaw[ULawSample](t, path, "ulaw", src)
	testLaw[ALawSample](t, path, "alaw", src)
}

func testLaw[T ~[]byte, P interface {
	*T
	Decode() media.PCM16Sample
	Encode(data media.PCM16Sample)
}](t *testing.T, path, name string, src media.PCM16Sample) {
	t.Run(name, func(t *testing.T) {
		lpath := path + "." + name

		data, err := os.ReadFile(lpath)
		require.NoError(t, err)
		log := T(data)

		dec := readPCM16(t, lpath+".s16le")
		t.Run("encode", func(t *testing.T) {
			src := slices.Clone(src)
			var dst T
			P(&dst).Encode(src)
			require.Equal(t, log, dst)
		})
		t.Run("decode", func(t *testing.T) {
			src := slices.Clone(log)
			dst := P(&src).Decode()
			require.Equal(t, dec, dst)
		})
	})
}

func readPCM16(t testing.TB, path string) media.PCM16Sample {
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	out := make([]int16, len(data)/2)
	for i := range out {
		out[i] = int16(binary.LittleEndian.Uint16(data[i*2:]))
	}
	return out
}
