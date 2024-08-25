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

import (
	"testing"

	"github.com/pion/sdp/v3"
	"github.com/stretchr/testify/require"

	"github.com/livekit/sip/pkg/media"
	"github.com/livekit/sip/pkg/media/g711"
	"github.com/livekit/sip/pkg/media/g722"
	"github.com/livekit/sip/pkg/media/rtp"
	lksdp "github.com/livekit/sip/pkg/media/sdp"
)

func TestSDPMediaOffer(t *testing.T) {
	const port = 12345
	offer := sdpMediaOffer(port)
	require.Equal(t, []*sdp.MediaDescription{
		{
			MediaName: sdp.MediaName{
				Media:   "audio",
				Port:    sdp.RangedPort{Value: port},
				Protos:  []string{"RTP", "AVP"},
				Formats: []string{"9", "0", "8", "101"},
			},
			Attributes: []sdp.Attribute{
				{Key: "rtpmap", Value: "9 G722/8000"},
				{Key: "rtpmap", Value: "0 PCMU/8000"},
				{Key: "rtpmap", Value: "8 PCMA/8000"},
				{Key: "rtpmap", Value: "101 telephone-event/8000"},
				{Key: "fmtp", Value: "101 0-16"},
				{Key: "ptime", Value: "20"},
				{Key: "sendrecv"},
			},
		},
	}, offer)

	media.CodecSetEnabled(g722.SDPName, false)
	defer media.CodecSetEnabled(g722.SDPName, true)

	offer = sdpMediaOffer(port)
	require.Equal(t, []*sdp.MediaDescription{
		{
			MediaName: sdp.MediaName{
				Media:   "audio",
				Port:    sdp.RangedPort{Value: port},
				Protos:  []string{"RTP", "AVP"},
				Formats: []string{"0", "8", "101"},
			},
			Attributes: []sdp.Attribute{
				{Key: "rtpmap", Value: "0 PCMU/8000"},
				{Key: "rtpmap", Value: "8 PCMA/8000"},
				{Key: "rtpmap", Value: "101 telephone-event/8000"},
				{Key: "fmtp", Value: "101 0-16"},
				{Key: "ptime", Value: "20"},
				{Key: "sendrecv"},
			},
		},
	}, offer)
}

func getCodec(name string) rtp.AudioCodec {
	return lksdp.CodecByName(name).(rtp.AudioCodec)
}

func TestSDPMediaAnswer(t *testing.T) {
	const port = 12345
	cases := []struct {
		name  string
		offer sdp.MediaDescription
		exp   *MediaConf
	}{
		{
			name: "default",
			offer: sdp.MediaDescription{
				MediaName: sdp.MediaName{
					Formats: []string{"0", "9", "8", "101"},
				},
				Attributes: []sdp.Attribute{
					{Key: "rtpmap", Value: "0 PCMU/8000"},
					{Key: "rtpmap", Value: "9 G722/8000"},
					{Key: "rtpmap", Value: "101 telephone-event/8000"},
				},
			},
			exp: &MediaConf{
				Audio:     getCodec(g722.SDPName),
				AudioType: 9,
				DTMFType:  101,
			},
		},
		{
			name: "lowercase",
			offer: sdp.MediaDescription{
				MediaName: sdp.MediaName{
					Formats: []string{"0", "9", "101"},
				},
				Attributes: []sdp.Attribute{
					{Key: "rtpmap", Value: "0 pcmu/8000"},
					{Key: "rtpmap", Value: "9 g722/8000"},
					{Key: "rtpmap", Value: "101 telephone-event/8000"},
				},
			},
			exp: &MediaConf{
				Audio:     getCodec(g722.SDPName),
				AudioType: 9,
				DTMFType:  101,
			},
		},
		{
			name: "no dtmf",
			offer: sdp.MediaDescription{
				MediaName: sdp.MediaName{
					Formats: []string{"0", "9"},
				},
				Attributes: []sdp.Attribute{
					{Key: "rtpmap", Value: "0 PCMU/8000"},
					{Key: "rtpmap", Value: "9 G722/8000"},
				},
			},
			exp: &MediaConf{
				Audio:     getCodec(g722.SDPName),
				AudioType: 9,
			},
		},
		{
			name: "custom dtmf",
			offer: sdp.MediaDescription{
				MediaName: sdp.MediaName{
					Formats: []string{"0", "9", "103"},
				},
				Attributes: []sdp.Attribute{
					{Key: "rtpmap", Value: "0 PCMU/8000"},
					{Key: "rtpmap", Value: "9 G722/8000"},
					{Key: "rtpmap", Value: "103 telephone-event/8000"},
				},
			},
			exp: &MediaConf{
				Audio:     getCodec(g722.SDPName),
				AudioType: 9,
				DTMFType:  103,
			},
		},
		{
			name: "only ulaw",
			offer: sdp.MediaDescription{
				MediaName: sdp.MediaName{
					Formats: []string{"0", "101"},
				},
				Attributes: []sdp.Attribute{
					{Key: "rtpmap", Value: "0 PCMU/8000"},
					{Key: "rtpmap", Value: "101 telephone-event/8000"},
				},
			},
			exp: &MediaConf{
				Audio:     getCodec(g711.ULawSDPName),
				AudioType: 0,
				DTMFType:  101,
			},
		},
		{
			name: "only g722",
			offer: sdp.MediaDescription{
				MediaName: sdp.MediaName{
					Formats: []string{"9", "101"},
				},
				Attributes: []sdp.Attribute{
					{Key: "rtpmap", Value: "9 G722/8000"},
					{Key: "rtpmap", Value: "101 telephone-event/8000"},
				},
			},
			exp: &MediaConf{
				Audio:     getCodec(g722.SDPName),
				AudioType: 9,
				DTMFType:  101,
			},
		},
		{
			name: "unsupported",
			offer: sdp.MediaDescription{
				MediaName: sdp.MediaName{
					Formats: []string{"101", "102"},
				},
				Attributes: []sdp.Attribute{
					{Key: "rtpmap", Value: "101 telephone-event/8000"},
					{Key: "rtpmap", Value: "102 FOOBAR/8000"},
				},
			},
			exp: nil,
		},
		{
			name: "format only",
			offer: sdp.MediaDescription{
				MediaName: sdp.MediaName{
					Formats: []string{"0", "101"},
				},
				Attributes: []sdp.Attribute{
					{Key: "rtpmap", Value: "101 telephone-event/8000"},
				},
			},
			exp: &MediaConf{
				Audio:     getCodec(g711.ULawSDPName),
				AudioType: 0,
				DTMFType:  101,
			},
		},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			got, err := sdpGetCodec(&c.offer)
			if c.exp == nil {
				require.Error(t, err)
				return
			}
			require.NotNil(t, c.exp.Audio)
			require.NoError(t, err)
			require.Equal(t, c.exp, got)
		})
	}
	offer := sdpMediaOffer(port)
	require.Equal(t, []*sdp.MediaDescription{
		{
			MediaName: sdp.MediaName{
				Media:   "audio",
				Port:    sdp.RangedPort{Value: port},
				Protos:  []string{"RTP", "AVP"},
				Formats: []string{"9", "0", "8", "101"},
			},
			Attributes: []sdp.Attribute{
				{Key: "rtpmap", Value: "9 G722/8000"},
				{Key: "rtpmap", Value: "0 PCMU/8000"},
				{Key: "rtpmap", Value: "8 PCMA/8000"},
				{Key: "rtpmap", Value: "101 telephone-event/8000"},
				{Key: "fmtp", Value: "101 0-16"},
				{Key: "ptime", Value: "20"},
				{Key: "sendrecv"},
			},
		},
	}, offer)
}
