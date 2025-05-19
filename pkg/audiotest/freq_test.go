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

package audiotest

import (
	"testing"

	msdk "github.com/livekit/media-sdk"
	"github.com/stretchr/testify/require"
)

func TestFreq(t *testing.T) {
	sig := make(msdk.PCM16Sample, 160)
	const amp = 100
	inp := []Wave{
		{0, amp},
		{3, amp / 2},
		{1, amp / 4},
	}
	GenSignal(sig, inp)

	out := FindSignal(sig)
	require.Equal(t, inp, out)
}
