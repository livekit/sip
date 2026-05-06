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

package sip

// Register supported audio codecs
import (
	"errors"
	"fmt"

	_ "github.com/livekit/media-sdk/all"
	"github.com/livekit/media-sdk/dtmf"
	"github.com/livekit/media-sdk/g711"
	"github.com/livekit/media-sdk/g722"
	"github.com/livekit/media-sdk/sdp"

	msdk "github.com/livekit/media-sdk"
	"github.com/livekit/protocol/livekit"
)

var defaultCodecs = msdk.NewCodecSet()

func init() {
	defaultCodecs.SetEnabledMap(map[string]bool{
		g711.ALawSDPName: true,
		g711.ULawSDPName: true,
		g722.SDPName:     true,
		dtmf.SDPName:     true,
	})
}

func newMediaConfig(m *livekit.SIPMediaConfig) (*sipMediaConfig, error) {
	enc, err := sdpEncryption(m.Encryption)
	if err != nil {
		return nil, err
	}
	s, err := codecSet(m)
	if err != nil {
		return nil, err
	}
	return &sipMediaConfig{
		Encryption: enc,
		Codecs:     s,
	}, nil
}

type sipMediaConfig struct {
	Encryption sdp.Encryption
	Codecs     *msdk.CodecSet
}

func codecSet(m *livekit.SIPMediaConfig) (*msdk.CodecSet, error) {
	var s *msdk.CodecSet
	if m.OnlyListedCodecs {
		if len(m.Codecs) == 0 {
			return nil, errors.New("no codecs specified")
		}
		s = msdk.NewCodecSet() // empty set
	} else {
		s = defaultCodecs.NewSet() // inherit from default
	}
	for _, codec := range m.Codecs {
		name := fmt.Sprintf("%s/%d", codec.Name, codec.Rate)
		s.SetEnabled(name, true)
	}
	return s, nil
}
