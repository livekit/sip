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
	"errors"
	"fmt"
	"math/rand"
	"net"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/pion/sdp/v2"

	"github.com/livekit/sip/pkg/media"
	"github.com/livekit/sip/pkg/media/dtmf"
	"github.com/livekit/sip/pkg/media/rtp"
	lksdp "github.com/livekit/sip/pkg/media/sdp"
)

func getCodecs() []sdpCodecInfo {
	const dynamicType = 101
	codecs := media.EnabledCodecs()
	slices.SortFunc(codecs, func(a, b media.Codec) int {
		ai, bi := a.Info(), b.Info()
		if ai.RTPIsStatic != bi.RTPIsStatic {
			if ai.RTPIsStatic {
				return -1
			} else if bi.RTPIsStatic {
				return 1
			}
		}
		return bi.Priority - ai.Priority
	})
	infos := make([]sdpCodecInfo, 0, len(codecs))
	nextType := byte(dynamicType)
	for _, c := range codecs {
		cinfo := c.Info()
		info := sdpCodecInfo{
			Codec: c,
		}
		if cinfo.RTPIsStatic {
			info.Type = cinfo.RTPDefType
		} else {
			typ := nextType
			nextType++
			info.Type = typ
		}
		infos = append(infos, info)
	}
	return infos
}

type sdpCodecInfo struct {
	Type  byte
	Codec media.Codec
}

func sdpMediaOffer(rtpListenerPort int) []*sdp.MediaDescription {
	// Static compiler check for frame duration hardcoded below.
	var _ = [1]struct{}{}[20*time.Millisecond-rtp.DefFrameDur]

	codecs := getCodecs()
	attrs := make([]sdp.Attribute, 0, len(codecs)+4)
	formats := make([]string, 0, len(codecs))
	dtmfType := -1
	for _, codec := range codecs {
		if codec.Codec.Info().SDPName == dtmf.SDPName {
			dtmfType = int(codec.Type)
		}
		styp := strconv.Itoa(int(codec.Type))
		formats = append(formats, styp)
		attrs = append(attrs, sdp.Attribute{
			Key:   "rtpmap",
			Value: styp + " " + codec.Codec.Info().SDPName,
		})
	}
	slices.SortFunc(formats, func(a, b string) int {
		if len(a) != len(b) {
			if len(a) < len(b) {
				return -1
			} else if len(a) > len(b) {
				return +1
			}
		}
		return strings.Compare(a, b)
	})
	if dtmfType > 0 {
		attrs = append(attrs, sdp.Attribute{
			Key: "fmtp", Value: fmt.Sprintf("%d 0-16", dtmfType),
		})
	}
	attrs = append(attrs, []sdp.Attribute{
		{Key: "ptime", Value: "20"},
		{Key: "sendrecv"},
	}...)

	return []*sdp.MediaDescription{
		{
			MediaName: sdp.MediaName{
				Media:   "audio",
				Port:    sdp.RangedPort{Value: rtpListenerPort},
				Protos:  []string{"RTP", "AVP"},
				Formats: formats,
			},
			Attributes: attrs,
		},
	}
}

func sdpAnswerMediaDesc(rtpListenerPort int, res *sdpCodecResult) []*sdp.MediaDescription {
	// Static compiler check for frame duration hardcoded below.
	var _ = [1]struct{}{}[20*time.Millisecond-rtp.DefFrameDur]

	attrs := make([]sdp.Attribute, 0, 6)
	attrs = append(attrs, sdp.Attribute{
		Key: "rtpmap", Value: fmt.Sprintf("%d %s", res.AudioType, res.Audio.Info().SDPName),
	})
	if res.DTMFType != 0 {
		attrs = append(attrs, []sdp.Attribute{
			{Key: "rtpmap", Value: fmt.Sprintf("%d %s", res.DTMFType, dtmf.SDPName)},
			{Key: "fmtp", Value: fmt.Sprintf("%d 0-16", res.DTMFType)},
		}...)
	}
	attrs = append(attrs, []sdp.Attribute{
		{Key: "ptime", Value: "20"},
		{Key: "sendrecv"},
	}...)
	return []*sdp.MediaDescription{
		{
			MediaName: sdp.MediaName{
				Media:  "audio",
				Port:   sdp.RangedPort{Value: rtpListenerPort},
				Protos: []string{"RTP", "AVP"},
				Formats: []string{
					strconv.Itoa(int(res.AudioType)),
					strconv.Itoa(int(res.DTMFType)),
				},
			},
			Attributes: attrs,
		},
	}
}

func sdpGenerateOffer(publicIp string, rtpListenerPort int) ([]byte, error) {
	sessId := rand.Uint64() // TODO: do we need to track these?

	mediaDesc := sdpMediaOffer(rtpListenerPort)
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
		MediaDescriptions: mediaDesc,
	}

	data, err := answer.Marshal()
	return data, err
}

func sdpGenerateAnswer(offer sdp.SessionDescription, publicIp string, rtpListenerPort int, res *sdpCodecResult) ([]byte, error) {

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
		MediaDescriptions: sdpAnswerMediaDesc(rtpListenerPort, res),
	}

	return answer.Marshal()
}

func sdpGetAudio(offer sdp.SessionDescription) *sdp.MediaDescription {
	for _, m := range offer.MediaDescriptions {
		if m.MediaName.Media == "audio" {
			return m
		}
	}
	return nil
}

func sdpGetAudioDest(offer sdp.SessionDescription) *net.UDPAddr {
	ci := offer.ConnectionInformation
	if ci.NetworkType != "IN" {
		return nil
	}
	ip, err := netip.ParseAddr(ci.Address.Address)
	if err != nil {
		return nil
	}
	audio := sdpGetAudio(offer)
	if audio == nil {
		return nil
	}
	return &net.UDPAddr{
		IP:   ip.AsSlice(),
		Port: audio.MediaName.Port.Value,
	}
}

type sdpCodecResult struct {
	Audio     rtp.AudioCodec
	AudioType byte
	DTMFType  byte
}

func sdpGetAudioCodec(offer sdp.SessionDescription) (*sdpCodecResult, error) {
	audio := sdpGetAudio(offer)
	if audio == nil {
		return nil, errors.New("no audio in sdp")
	}
	return sdpGetCodec(audio.Attributes)
}

func sdpGetCodec(attrs []sdp.Attribute) (*sdpCodecResult, error) {
	var (
		priority   int
		audioCodec rtp.AudioCodec
		audioType  byte
		dtmfType   byte
	)
	for _, m := range attrs {
		switch m.Key {
		case "rtpmap":
			sub := strings.SplitN(m.Value, " ", 2)
			if len(sub) != 2 {
				continue
			}
			typ, err := strconv.Atoi(sub[0])
			if err != nil {
				continue
			}
			name := sub[1]
			if name == dtmf.SDPName {
				dtmfType = byte(typ)
				continue
			}
			codec, ok := lksdp.CodecByName(name).(rtp.AudioCodec)
			if !ok {
				continue
			}
			if audioCodec == nil || codec.Info().Priority > priority {
				audioType = byte(typ)
				audioCodec = codec
				priority = codec.Info().Priority
			}
		}
	}
	if audioCodec == nil {
		return nil, fmt.Errorf("common audio codec not found")
	}
	return &sdpCodecResult{
		Audio:     audioCodec,
		AudioType: audioType,
		DTMFType:  dtmfType,
	}, nil
}
