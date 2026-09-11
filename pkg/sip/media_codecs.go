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
	"slices"
	"time"

	_ "github.com/livekit/media-sdk/all"
	"github.com/livekit/media-sdk/amrwb"
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
		dtmf.SDPNameOnly:     true,
		g711.ALawSDPNameOnly: true,
		g711.ULawSDPNameOnly: true,
		g722.SDPNameOnly:     true,
		amrwb.SDPNameOnly:    false, // optional
	})
}

func DefaultCodecs() *msdk.CodecSet {
	return defaultCodecs
}

// Metric label used for advertised codecs that are not part of the internal
// codec set, since their name is dropped during SDP parsing and to keep the
// label bounded
const codecOther = "other"

func peerCodecNames(d sdp.MediaDesc) []string {
	names := make([]string, 0, len(d.Audio)+len(d.Data)+len(d.Unknown))
	add := func(list []sdp.CodecInfo) {
		for _, c := range list {
			name := c.Info.SDPFullName()
			if c.Info.Name == "" {
				name = "other"
			}
			if !slices.Contains(names, name) {
				names = append(names, name)
			}
		}
	}
	add(d.Audio)
	add(d.Data)
	add(d.Unknown)
	return names
}

func newMediaConfig(m *livekit.SIPMediaConfig, defaultTimeout time.Duration) (*sipMediaConfig, error) {
	enc, err := sdpEncryption(m.Encryption)
	if err != nil {
		return nil, err
	}
	s, err := codecSet(m)
	if err != nil {
		return nil, err
	}

	mediaTimeout := defaultTimeout
	if m.MediaTimeout != nil && m.MediaTimeout.AsDuration() > 0 {
		mediaTimeout = m.MediaTimeout.AsDuration()
	}
	return &sipMediaConfig{
		Encryption:   enc,
		Codecs:       s,
		MediaTimeout: mediaTimeout,
	}, nil
}

type sipMediaConfig struct {
	Encryption   sdp.Encryption
	Codecs       *msdk.CodecSet
	MediaTimeout time.Duration
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
		name := codec.Name
		if name == "" {
			return nil, errors.New("no codec name specified")
		}
		rate := codec.Rate
		s.SetEnabled(name, true)
		_ = rate // TODO: we only support fixed rate codecs so far; add whitelist for codec configs later
	}
	s.SetEnabled(dtmf.SDPNameOnly, true)
	return s, nil
}
