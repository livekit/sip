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

func sdpMediaDesc(rtpListenerPort int) []*sdp.MediaDescription {
	// Static compiler check for sample rate hardcoded below.
	var _ = [1]struct{}{}[8000-sampleRate]

	return []*sdp.MediaDescription{
		{
			MediaName: sdp.MediaName{
				Media:   "audio",
				Port:    sdp.RangedPort{Value: rtpListenerPort},
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
		},
	}
}

func sdpGenerateOffer(publicIp string, rtpListenerPort int) ([]byte, error) {
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
		MediaDescriptions: sdpMediaDesc(rtpListenerPort),
	}

	return answer.Marshal()
}

func sdpGenerateAnswer(offer sdp.SessionDescription, publicIp string, rtpListenerPort int) ([]byte, error) {

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
		MediaDescriptions: sdpMediaDesc(rtpListenerPort),
	}

	return answer.Marshal()
}
