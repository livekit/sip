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

const OpusSDPName = "opus/48000/2"

// OpusEncodeOptions tunes the Opus encoder. Zero values keep libopus defaults.
type OpusEncodeOptions struct {
	Bitrate           int  // target bitrate in bits/sec (e.g. 24000); 0 = auto
	Complexity        int  // encoder complexity 1-10; 0 = default
	FEC               bool // enable in-band Forward Error Correction
	PacketLossPercent int  // expected packet loss 0-100, tunes FEC
}

var defaultCodecs = msdk.NewCodecSet()

func init() {
	defaultCodecs.SetEnabledMap(map[string]bool{
		g711.ALawSDPNameAndRate: true,
		g711.ULawSDPNameAndRate: true,
		g722.SDPNameAndRate:     true,
		amrwb.SDPNameAndRate:    false, // optional
		OpusSDPName:             false, // opt-in via enable_opus config flag
		dtmf.SDPNameAndRate:     true,
	})
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
		if sdpName := resolveSDPName(name); sdpName != "" {
			s.SetEnabled(sdpName, true)
		}
	}
	s.SetEnabled(dtmf.SDPNameAndRate, true)
	return s, nil
}

// resolveSDPName finds the full SDP name for a codec specified as "name/rate"
// by matching against registered codecs. This handles codecs like Opus whose
// SDP name includes a channel count suffix (e.g., "opus/48000/2").
func resolveSDPName(name string) string {
	name = strings.ToLower(name)
	for _, c := range msdk.Codecs() {
		sdpName := strings.ToLower(c.Info().SDPName)
		if sdpName == name {
			return ""
		}
		if strings.HasPrefix(sdpName, name+"/") {
			return c.Info().SDPName
		}
	}
	return ""
}
