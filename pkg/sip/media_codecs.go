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
	"slices"
	"strings"
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
		g711.ALawSDPNameAndRate: true,
		g711.ULawSDPNameAndRate: true,
		g722.SDPNameAndRate:     true,
		amrwb.SDPNameAndRate:    false, // optional
	})
	for _, c := range msdk.Codecs() {
		info := c.Info()
		if strings.HasPrefix(info.SDPName, dtmf.SDPNameOnly+"/") {
			defaultCodecs.SetEnabled(info.SDPName, true)
		}
	}
}

func DefaultCodecs() *msdk.CodecSet {
	return defaultCodecs
}

// Metric label used for advertised codecs that are not part of the internal
// codec set, since their name is dropped during SDP parsing and to keep the
// label bounded
const codecOther = "other"

func peerCodecNames(d sdp.MediaDesc) []string {
	names := make([]string, 0, len(d.Codecs))
	for _, c := range d.Codecs {
		if slices.ContainsFunc(d.DTMF, func(info sdp.DTMFInfo) bool {
			return c.Type == info.Type
		}) {
			// DTMF is parsed out of a=rtpmap into DTMFType, but its payload type is
			// still listed in m=audio, where it resolves to no codec. Appended below.
			continue
		}
		name := codecOther
		if c.Codec != nil {
			name = c.Codec.Info().SDPName
		}
		if !slices.Contains(names, name) {
			names = append(names, name)
		}
	}
	for _, c := range d.DTMF {
		names = append(names, fmt.Sprintf("%s/%d", dtmf.SDPNameOnly, c.Rate))
	}
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
	var dtmfRates []uint32
	for _, codec := range m.Codecs {
		name := codec.Name
		if name == "" {
			return nil, errors.New("no codec name specified")
		}
		rate := codec.Rate
		if rate == 0 {
			// Set default rate
			switch name {
			case g711.ALawSDPNameOnly, g711.ULawSDPNameOnly:
				rate = 8000
			case g722.SDPNameOnly:
				rate = 8000 // actually 16000, it's a know bug in the spec
			case amrwb.SDPNameOnly:
				rate = 16000
			default:
				return nil, fmt.Errorf("sample rate not specified for codec: %q", name)
			}
		}
		name = fmt.Sprintf("%s/%d", name, rate)
		s.SetEnabled(name, true)
		if !slices.Contains(dtmfRates, rate) {
			dtmfRates = append(dtmfRates, rate)
		}
	}
	slices.Sort(dtmfRates)
	for _, rate := range dtmfRates {
		s.SetEnabled(fmt.Sprintf("%s/%d", dtmf.SDPNameOnly, rate), true)
	}
	return s, nil
}
