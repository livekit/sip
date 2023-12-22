// Copyright 2023 LiveKit, Inc.
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
	"math/rand"

	"github.com/pion/sdp/v2"
)

func sdpMediaDesc(audioListenerPort int, videoListenerPort int) []*sdp.MediaDescription {
	// Static compiler check for sample rate hardcoded below.
	var _ = [1]struct{}{}[8000-sampleRate]

	return []*sdp.MediaDescription{
		{
			MediaName: sdp.MediaName{
				Media:   "audio",
				Port:    sdp.RangedPort{Value: audioListenerPort},
				Protos:  []string{"RTP", "AVP"},
				Formats: []string{"0", "101"},
			},
			Attributes: []sdp.Attribute{
				{Key: "rtpmap", Value: "0 PCMU/8000"},
				{Key: "rtpmap", Value: "101 telephone-event/8000"},
				{Key: "fmtp", Value: "101 0-16"},
				{Key: "ptime", Value: "20"},
				{Key: "maxptime", Value: "150"},
				{Key: "sendrecv"},
			},
		}, {
			MediaName: sdp.MediaName{
				Media:   "video",
				Port:    sdp.RangedPort{Value: videoListenerPort},
				Protos:  []string{"RTP", "AVP"},
				Formats: []string{"102","97","125"},
			},
			Attributes: []sdp.Attribute{
				{Key: "rtpmap", Value: "102 H264/90000"},
                                {Key: "fmtp", Value: "102 profile-level-id=42001f"},
				{Key: "rtpmap", Value: "97 H264/90000"},
                                {Key: "fmtp", Value: "97 profile-level-id=42801F"},
				/*{Key: "rtpmap", Value: "104 H264/90000"},
				{Key: "fmtp", Value: "104 level-asymmetry-allowed=1;packetization-mode=0;profile-level-id=42001f"},
				{Key: "rtpmap", Value: "106 H264/90000"},
				{Key: "fmtp", Value: "106 level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f"},
				{Key: "rtpmap", Value: "108 H264/90000"},
				{Key: "fmtp", Value: "108 level-asymmetry-allowed=1;packetization-mode=0;profile-level-id=42e01f"},
				{Key: "rtpmap", Value: "112 H264/90000"},
				{Key: "fmtp", Value: "112 level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=64001f"},*/
				{Key: "rtpmap", Value: "125 H264/90000"},
				{Key: "fmtp", Value: "125 profile-level-id=42801E;packetization-mode=0"},
				/*{Key: "rtpmap", Value: "127 H264/90000"},
				{Key: "fmtp", Value: "127 level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=4d001f"},*/
			},
		},
	}
}

func sdpGenerateOffer(publicIp string, audioRtpListenerPort int, videoRtpListenerPort int) ([]byte, error) {
	sessId := rand.Uint64() // TODO: do we need to track these?

	answer := sdp.SessionDescription{
		Version: 0,
		Origin: sdp.Origin{
			Username:       "-",
			SessionID:      sessId,
			SessionVersion: sessId,
			NetworkType:    "IN",
			AddressType:    "IP4",
			UnicastAddress: publicIp,
		},
		SessionName: "LiveKit",
		ConnectionInformation: &sdp.ConnectionInformation{
			NetworkType: "IN",
			AddressType: "IP4",
			Address:     &sdp.Address{Address: publicIp},
		},
		TimeDescriptions: []sdp.TimeDescription{
			{
				Timing: sdp.Timing{
					StartTime: 0,
					StopTime:  0,
				},
			},
		},
		MediaDescriptions: sdpMediaDesc(audioRtpListenerPort, videoRtpListenerPort),
	}

	return answer.Marshal()
}

func sdpGenerateAnswer(offer sdp.SessionDescription, publicIp string, audioListenerPort int, videoListenerPort int) ([]byte, error) {

	answer := sdp.SessionDescription{
		Version: 0,
		Origin: sdp.Origin{
			Username:       "-",
			SessionID:      offer.Origin.SessionID,
			SessionVersion: offer.Origin.SessionID + 2,
			NetworkType:    "IN",
			AddressType:    "IP4",
			UnicastAddress: publicIp,
		},
		SessionName: "LiveKit",
		ConnectionInformation: &sdp.ConnectionInformation{
			NetworkType: "IN",
			AddressType: "IP4",
			Address:     &sdp.Address{Address: publicIp},
		},
		TimeDescriptions: []sdp.TimeDescription{
			{
				Timing: sdp.Timing{
					StartTime: 0,
					StopTime:  0,
				},
			},
		},
		MediaDescriptions: sdpMediaDesc(audioListenerPort, videoListenerPort),
	}

	return answer.Marshal()
}

