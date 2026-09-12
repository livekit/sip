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
	"time"

	_ "github.com/livekit/media-sdk/all"
	"github.com/livekit/media-sdk/amrwb"
	"github.com/livekit/media-sdk/dtmf"
	"github.com/livekit/media-sdk/g711"
	"github.com/livekit/media-sdk/g722"
	"github.com/livekit/media-sdk/opus"
	"github.com/livekit/media-sdk/sdp"

	msdk "github.com/livekit/media-sdk"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
)

var defaultCodecs = msdk.NewCodecSet()

func init() {
	defaultCodecs.SetEnabledMap(map[string]bool{
		g711.ALawSDPNameAndRate: true,
		g711.ULawSDPNameAndRate: true,
		g722.SDPNameAndRate:     true,
		opus.SDPName:            true,
		amrwb.SDPNameAndRate:    false, // optional
		dtmf.SDPNameAndRate:     true,
	})
}

func DefaultCodecs() *msdk.CodecSet {
	return defaultCodecs
}

// CheckCodecAvailability logs warnings for codecs that are enabled in the
// default set but whose backing media-sdk CodecType is not registered —
// most commonly because a CGo codec (opus, amrwb) was skipped in a
// CGO_ENABLED=0 build.
func CheckCodecAvailability(log logger.Logger) {
	codecs := defaultCodecs.ListEnabled()
	for _, c := range codecs {
		name := c.Info().SDPName
		if sdp.CodecByNameWith(defaultCodecs, name) == nil {
			log.Warnw("codec enabled but not registered (missing CGo dependency?)",
				nil, "codec", name,
			)
		}
	}
}

// Metric label used for advertised codecs that are not part of the internal
// codec set, since their name is dropped during SDP parsing and to keep the
// label bounded
const codecOther = "other"

func peerCodecNames(d sdp.MediaDesc) []string {
	names := make([]string, 0, len(d.Codecs))
	for _, c := range d.Codecs {
		if d.DTMFType != 0 && c.Type == d.DTMFType {
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
	if d.DTMFType != 0 {
		names = append(names, dtmf.SDPNameAndRate)
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
	for _, codec := range m.Codecs {
		name := codec.Name
		if name == "" {
			return nil, errors.New("no codec name specified")
		}

		// Per RFC 7587 §6.1, Opus RTP clock rate is always 48000 Hz.
		// Different audio bandwidths (narrowband 8k, wideband 16k,
		// fullband 48k) are negotiated via fmtp:maxplaybackrate,
		// not via the rtpmap clock rate. Use the canonical SDP name.
		if name == opus.SDPNameOnly {
			s.SetEnabled(opus.SDPName, true)
			continue
		}

		rate := codec.Rate
		if rate == 0 {
			switch name {
			case g711.ALawSDPNameOnly, g711.ULawSDPNameOnly:
				rate = 8000
			case g722.SDPNameOnly:
				rate = 8000 // actually 16000, it's a known bug in the spec
			case amrwb.SDPNameOnly:
				rate = 16000
			default:
				return nil, fmt.Errorf("sample rate not specified for codec: %q", name)
			}
		}
		name = fmt.Sprintf("%s/%d", name, rate)
		s.SetEnabled(name, true)
	}
	s.SetEnabled(dtmf.SDPNameAndRate, true)
	return s, nil
}
