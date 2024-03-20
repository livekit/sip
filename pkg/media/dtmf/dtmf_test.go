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

package dtmf

import (
	"encoding/hex"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDTMF(t *testing.T) {
	cases := []struct {
		name string
		data string
		exp  Event
	}{
		{
			name: "star end",
			data: `0a8a0820`,
			exp: Event{
				Code:   10,
				Digit:  '*',
				Volume: 10,
				End:    true,
				Dur:    2080,
			},
		},
		{
			name: "four",
			data: `040a0140`,
			exp: Event{
				Code:   4,
				Digit:  '4',
				Volume: 10,
				End:    false,
				Dur:    320,
			},
		},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			data, err := hex.DecodeString(c.data)
			require.NoError(t, err)

			got, err := Decode(data)
			require.NoError(t, err)
			require.Equal(t, c.exp, got)

			var buf [4]byte
			n, err := Encode(buf[:], got)
			require.NoError(t, err)
			require.Equal(t, len(data), n)
			require.Equal(t, c.data, hex.EncodeToString(buf[:n]))
		})
	}
}
