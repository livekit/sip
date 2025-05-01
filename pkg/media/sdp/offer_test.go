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

package sdp_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/pion/sdp/v3"
	"github.com/stretchr/testify/require"

	"github.com/livekit/sip/pkg/media"
	"github.com/livekit/sip/pkg/media/g711"
	"github.com/livekit/sip/pkg/media/g722"
	"github.com/livekit/sip/pkg/media/rtp"
	. "github.com/livekit/sip/pkg/media/sdp"
	"github.com/livekit/sip/pkg/media/srtp"
)

func getInline(s string) string {
	const word = "inline:"
	i := strings.Index(s, word)
	if i < 0 {
		return s
	}
	return s[i+len(word):]
}

func TestSDPMediaOffer(t *testing.T) {
	const port = 12345
	_, offer, err := OfferMedia(port, EncryptionNone)
	require.NoError(t, err)
	require.Equal(t, &sdp.MediaDescription{
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
	}, offer)

	_, offer, err = OfferMedia(port, EncryptionRequire)
	require.NoError(t, err)
	i := slices.IndexFunc(offer.Attributes, func(a sdp.Attribute) bool {
		return a.Key == "crypto"
	})
	require.True(t, i > 0)
	require.Equal(t, &sdp.MediaDescription{
		MediaName: sdp.MediaName{
			Media:   "audio",
			Port:    sdp.RangedPort{Value: port},
			Protos:  []string{"RTP", "SAVP"},
			Formats: []string{"9", "0", "8", "101"},
		},
		Attributes: []sdp.Attribute{
			{Key: "rtpmap", Value: "9 G722/8000"},
			{Key: "rtpmap", Value: "0 PCMU/8000"},
			{Key: "rtpmap", Value: "8 PCMA/8000"},
			{Key: "rtpmap", Value: "101 telephone-event/8000"},
			{Key: "fmtp", Value: "101 0-16"},
			{Key: "crypto", Value: "1 AES_CM_128_HMAC_SHA1_80 inline:" + getInline(offer.Attributes[i+0].Value)},
			{Key: "crypto", Value: "2 AES_CM_128_HMAC_SHA1_32 inline:" + getInline(offer.Attributes[i+1].Value)},
			{Key: "crypto", Value: "3 AES_256_CM_HMAC_SHA1_80 inline:" + getInline(offer.Attributes[i+2].Value)},
			{Key: "crypto", Value: "4 AES_256_CM_HMAC_SHA1_32 inline:" + getInline(offer.Attributes[i+3].Value)},
			{Key: "ptime", Value: "20"},
			{Key: "sendrecv"},
		},
	}, offer)

	media.CodecSetEnabled(g722.SDPName, false)
	defer media.CodecSetEnabled(g722.SDPName, true)

	_, offer, err = OfferMedia(port, EncryptionNone)
	require.NoError(t, err)
	require.Equal(t, &sdp.MediaDescription{
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
	}, offer)
}

func getCodec(name string) rtp.AudioCodec {
	return CodecByName(name).(rtp.AudioCodec)
}

func TestSDPMediaAnswer(t *testing.T) {
	const port = 12345
	cases := []struct {
		name  string
		offer sdp.MediaDescription
		exp   *AudioConfig
	}{
		{
			name: "default",
			offer: sdp.MediaDescription{
				MediaName: sdp.MediaName{
					Formats: []string{"9", "0", "8", "101"},
				},
				Attributes: []sdp.Attribute{
					{Key: "rtpmap", Value: "9 G722/8000"},
					{Key: "rtpmap", Value: "0 PCMU/8000"},
					{Key: "rtpmap", Value: "101 telephone-event/8000"},
				},
			},
			exp: &AudioConfig{
				Codec:    getCodec(g722.SDPName),
				Type:     9,
				DTMFType: 101,
			},
		},
		{
			name: "lowercase",
			offer: sdp.MediaDescription{
				MediaName: sdp.MediaName{
					Formats: []string{"9", "0", "101"},
				},
				Attributes: []sdp.Attribute{
					{Key: "rtpmap", Value: "9 g722/8000"},
					{Key: "rtpmap", Value: "0 pcmu/8000"},
					{Key: "rtpmap", Value: "101 telephone-event/8000"},
				},
			},
			exp: &AudioConfig{
				Codec:    getCodec(g722.SDPName),
				Type:     9,
				DTMFType: 101,
			},
		},
		{
			name: "no dtmf",
			offer: sdp.MediaDescription{
				MediaName: sdp.MediaName{
					Formats: []string{"9", "0"},
				},
				Attributes: []sdp.Attribute{
					{Key: "rtpmap", Value: "9 G722/8000"},
					{Key: "rtpmap", Value: "0 PCMU/8000"},
				},
			},
			exp: &AudioConfig{
				Codec: getCodec(g722.SDPName),
				Type:  9,
			},
		},
		{
			name: "custom dtmf",
			offer: sdp.MediaDescription{
				MediaName: sdp.MediaName{
					Formats: []string{"9", "0", "103"},
				},
				Attributes: []sdp.Attribute{
					{Key: "rtpmap", Value: "9 G722/8000"},
					{Key: "rtpmap", Value: "0 PCMU/8000"},
					{Key: "rtpmap", Value: "103 telephone-event/8000"},
				},
			},
			exp: &AudioConfig{
				Codec:    getCodec(g722.SDPName),
				Type:     9,
				DTMFType: 103,
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
			exp: &AudioConfig{
				Codec:    getCodec(g711.ULawSDPName),
				Type:     0,
				DTMFType: 101,
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
			exp: &AudioConfig{
				Codec:    getCodec(g722.SDPName),
				Type:     9,
				DTMFType: 101,
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
			exp: &AudioConfig{
				Codec:    getCodec(g711.ULawSDPName),
				Type:     0,
				DTMFType: 101,
			},
		},
		{
			name: "explicit mono channel",
			offer: sdp.MediaDescription{
				Attributes: []sdp.Attribute{
					{Key: "rtpmap", Value: "0 PCMU/8000/1"},
					{Key: "rtpmap", Value: "101 telephone-event/8000/1"},
				},
			},
			exp: &AudioConfig{
				Codec:    getCodec(g711.ULawSDPName),
				Type:     0,
				DTMFType: 101,
			},
		},
		{
			name: "changed order",
			offer: sdp.MediaDescription{
				MediaName: sdp.MediaName{
					Formats: []string{"0", "9"},
				},
				Attributes: []sdp.Attribute{
					{Key: "rtpmap", Value: "0 PCMU/8000"},
					{Key: "rtpmap", Value: "9 G722/8000"},
				},
			},
			exp: &AudioConfig{
				Codec: getCodec(g711.ULawSDPName),
				Type:  0,
			},
		},
		{
			name: "changed order g711",
			offer: sdp.MediaDescription{
				MediaName: sdp.MediaName{
					Formats: []string{"8", "0"},
				},
				Attributes: []sdp.Attribute{
					{Key: "rtpmap", Value: "8 PCMA/8000"},
					{Key: "rtpmap", Value: "0 PCMU/8000"},
				},
			},
			exp: &AudioConfig{
				Codec: getCodec(g711.ALawSDPName),
				Type:  8,
			},
		},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			m, err := ParseMedia(&c.offer)
			require.NoError(t, err)
			got, err := SelectAudio(*m, true)
			if c.exp == nil {
				require.Error(t, err)
				return
			}
			require.NotNil(t, c.exp.Codec)
			require.NoError(t, err)
			require.Equal(t, c.exp, got)
		})
	}
	_, offer, err := OfferMedia(port, EncryptionNone)
	require.NoError(t, err)
	require.Equal(t, &sdp.MediaDescription{
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
	}, offer)
}

func TestParseOffer(t *testing.T) {
	tests := []struct {
		name    string
		sdp     string
		wantErr bool
	}{
		{
			name: "media level c= only",
			sdp: `v=0
o=Test 1 1 IN IP4 127.0.0.1
s=Stream1
t=0 0
m=audio 59236 RTP/AVP 0 101
c=IN IP4 127.0.0.1
a=rtpmap:0 PCMU/8000
a=rtpmap:101 telephone-event/8000
a=sendrecv
a=rtcp:59237
a=ptime:20
`,
			wantErr: false,
		},
		{
			name: "invalid network type",
			sdp: `v=0
o=- 1234567890 1234567890 IN IP4 1.2.3.4
s=LiveKit
c=FOO IP4 1.2.3.4
t=0 0
m=audio 1234 RTP/AVP 9 0 8 101
a=rtpmap:9 G722/8000
a=rtpmap:0 PCMU/8000
a=rtpmap:8 PCMA/8000
a=rtpmap:101 telephone-event/8000
a=fmtp:101 0-16
a=ptime:20
a=sendrecv
`,
			wantErr: true,
		},
		{
			name: "invalid ip address",
			sdp: `v=0
o=- 1234567890 1234567890 IN IP4 1.2.3.4
s=LiveKit
c=IN IP4 invalid.ip
t=0 0
m=audio 1234 RTP/AVP 9 0 8 101
a=rtpmap:9 G722/8000
a=rtpmap:0 PCMU/8000
a=rtpmap:8 PCMA/8000
a=rtpmap:101 telephone-event/8000
a=fmtp:101 0-16
a=ptime:20
a=sendrecv
`,
			wantErr: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := ParseOffer([]byte(test.sdp))
			if test.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestParseOfferSRTP(t *testing.T) {
	const sdpData = `v=0 
o=lin 3723 713 IN IP4 192.168.0.2 
s=Talk 
c=IN IP4 192.168.0.2 
t=0 0 
a=rtcp-xr:rcvr-rtt=all:10000 stat-summary=loss,dup,jitt,TTL voip-metrics 
a=record:off 
m=audio 11200 RTP/SAVP 96 0 9 97 101 
a=rtpmap:96 opus/48000/2 
a=fmtp:96 useinbandfec=1 
a=rtpmap:97 telephone-event/48000 
a=rtpmap:101 telephone-event/8000 
a=crypto:1 AES_CM_128_HMAC_SHA1_80 inline:pMIPxjzYIG5TQuIWfkjTnaACVrzohhFfOGhSMgV1 
a=crypto:2 AES_CM_128_HMAC_SHA1_32 inline:ZKkTQfuCsliegVZtFSya3Z6oEVUtSwjGCfHlbrMf 
a=crypto:3 AES_256_CM_HMAC_SHA1_80 inline:BvoLeRu5IcBkgNN14qtqaxi0r7ei2rwuBSodd0SANggS9JHsp5IU7lhEsyna1A== 
a=crypto:4 AES_256_CM_HMAC_SHA1_32 inline:j92SDyNTUe0BNGk4LeeCsPqX0qwPP/e9TLafvd7L9waM8r4arjzgUqs7uUERyg== 
a=crypto:5 AEAD_AES_128_GCM inline:gGetEkQgGk4NZIoKj/cbFpRkHdocmKlP0u3VMw== 
a=crypto:6 AEAD_AES_256_GCM inline:EFFzS2FMyNoYcVcaARU+nvk+JhHmVbvdFtRxZuRi9rDmLYpLms5ySv93iy0= 
a=rtcp-fb:* trr-int 5000 
a=rtcp-fb:* ccm tmmbr 
`
	v, err := ParseOffer([]byte(sdpData))
	require.NoError(t, err)
	require.Equal(t, []srtp.Profile{
		{
			Index:   1,
			Profile: "AES_CM_128_HMAC_SHA1_80",
			Key:     []byte{0xa4, 0xc2, 0xf, 0xc6, 0x3c, 0xd8, 0x20, 0x6e, 0x53, 0x42, 0xe2, 0x16, 0x7e, 0x48, 0xd3, 0x9d},
			Salt:    []byte{0xa0, 0x2, 0x56, 0xbc, 0xe8, 0x86, 0x11, 0x5f, 0x38, 0x68, 0x52, 0x32, 0x5, 0x75},
		},
		{
			Index:   2,
			Profile: "AES_CM_128_HMAC_SHA1_32",
			Key:     []byte{0x64, 0xa9, 0x13, 0x41, 0xfb, 0x82, 0xb2, 0x58, 0x9e, 0x81, 0x56, 0x6d, 0x15, 0x2c, 0x9a, 0xdd},
			Salt:    []byte{0x9e, 0xa8, 0x11, 0x55, 0x2d, 0x4b, 0x8, 0xc6, 0x9, 0xf1, 0xe5, 0x6e, 0xb3, 0x1f},
		},
		{
			Index:   3,
			Profile: "AES_256_CM_HMAC_SHA1_80",
			Key:     []byte{0x6, 0xfa, 0xb, 0x79, 0x1b, 0xb9, 0x21, 0xc0, 0x64, 0x80, 0xd3, 0x75, 0xe2, 0xab, 0x6a, 0x6b, 0x18, 0xb4, 0xaf, 0xb7, 0xa2, 0xda, 0xbc, 0x2e, 0x5, 0x2a, 0x1d, 0x77, 0x44, 0x80, 0x36, 0x8},
			Salt:    []uint8{0x12, 0xf4, 0x91, 0xec, 0xa7, 0x92, 0x14, 0xee, 0x58, 0x44, 0xb3, 0x29, 0xda, 0xd4},
		},
		{
			Index:   4,
			Profile: "AES_256_CM_HMAC_SHA1_32",
			Key:     []uint8{0x8f, 0xdd, 0x92, 0xf, 0x23, 0x53, 0x51, 0xed, 0x1, 0x34, 0x69, 0x38, 0x2d, 0xe7, 0x82, 0xb0, 0xfa, 0x97, 0xd2, 0xac, 0xf, 0x3f, 0xf7, 0xbd, 0x4c, 0xb6, 0x9f, 0xbd, 0xde, 0xcb, 0xf7, 0x6},
			Salt:    []uint8{0x8c, 0xf2, 0xbe, 0x1a, 0xae, 0x3c, 0xe0, 0x52, 0xab, 0x3b, 0xb9, 0x41, 0x11, 0xca},
		},
		{
			Index:   5,
			Profile: "AEAD_AES_128_GCM",
			Key:     []uint8{0x80, 0x67, 0xad, 0x12, 0x44, 0x20, 0x1a, 0x4e, 0xd, 0x64, 0x8a, 0xa, 0x8f, 0xf7, 0x1b, 0x16, 0x94, 0x64, 0x1d, 0xda, 0x1c, 0x98, 0xa9, 0x4f, 0xd2, 0xed, 0xd5, 0x33},
		},
		{
			Index:   6,
			Profile: "AEAD_AES_256_GCM",
			Key:     []uint8{0x10, 0x51, 0x73, 0x4b, 0x61, 0x4c, 0xc8, 0xda, 0x18, 0x71, 0x57, 0x1a, 0x1, 0x15, 0x3e, 0x9e, 0xf9, 0x3e, 0x26, 0x11, 0xe6, 0x55, 0xbb, 0xdd, 0x16, 0xd4, 0x71, 0x66, 0xe4, 0x62, 0xf6, 0xb0, 0xe6, 0x2d, 0x8a, 0x4b, 0x9a, 0xce, 0x72, 0x4a, 0xff, 0x77, 0x8b, 0x2d},
		},
	},
		v.CryptoProfiles)
}
