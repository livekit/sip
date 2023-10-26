package sip

import "github.com/pion/sdp/v2"

func generateAnswer(offer sdp.SessionDescription, publicIp string, rtpListenerPort int) ([]byte, error) {

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
			sdp.TimeDescription{
				Timing: sdp.Timing{
					StartTime: 0,
					StopTime:  0,
				},
			},
		},
		MediaDescriptions: []*sdp.MediaDescription{
			&sdp.MediaDescription{
				MediaName: sdp.MediaName{
					Media:   "audio",
					Port:    sdp.RangedPort{Value: rtpListenerPort},
					Protos:  []string{"RTP", "AVP"},
					Formats: []string{"0", "101"},
				},
				Attributes: []sdp.Attribute{
					sdp.Attribute{Key: "rtpmap", Value: "0 PCMU/8000"},
					sdp.Attribute{Key: "rtpmap", Value: "101 telephone-event/8000"},
					sdp.Attribute{Key: "fmtp", Value: "101 0-16"},
					sdp.Attribute{Key: "ptime", Value: "20"},
					sdp.Attribute{Key: "maxptime", Value: "150"},
					sdp.Attribute{Key: "sendrecv"},
				},
			},
		},
	}

	return answer.Marshal()
}
