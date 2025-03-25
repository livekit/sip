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

package sdp

import (
	"encoding/base64"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/pion/sdp/v3"

	"github.com/livekit/sip/pkg/media"
	"github.com/livekit/sip/pkg/media/dtmf"
	"github.com/livekit/sip/pkg/media/rtp"
	"github.com/livekit/sip/pkg/media/srtp"
)

var (
	ErrNoCommonMedia  = errors.New("common audio codec not found")
	ErrNoCommonCrypto = errors.New("no common encryption profiles")
)

type Encryption int

const (
	EncryptionNone Encryption = iota
	EncryptionAllow
	EncryptionRequire
)

type CodecInfo struct {
	Type  byte
	Codec media.Codec
}

func OfferCodecs() []CodecInfo {
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
	infos := make([]CodecInfo, 0, len(codecs))
	nextType := byte(dynamicType)
	for _, c := range codecs {
		cinfo := c.Info()
		info := CodecInfo{
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

type MediaDesc struct {
	Codecs         []CodecInfo
	DTMFType       byte // set to 0 if there's no DTMF
	CryptoProfiles []srtp.Profile
}

func appendCryptoProfiles(attrs []sdp.Attribute, profiles []srtp.Profile) []sdp.Attribute {
	var buf []byte
	for _, p := range profiles {
		buf = buf[:0]
		buf = append(buf, p.Key...)
		buf = append(buf, p.Salt...)
		skey := base64.StdEncoding.WithPadding(base64.StdPadding).EncodeToString(buf)
		attrs = append(attrs, sdp.Attribute{
			Key:   "crypto",
			Value: fmt.Sprintf("%d %s inline:%s", p.Index, p.Profile, skey),
		})
	}
	return attrs
}

func OfferMedia(rtpListenerPort int, encrypted Encryption) (MediaDesc, *sdp.MediaDescription, error) {
	// Static compiler check for frame duration hardcoded below.
	var _ = [1]struct{}{}[20*time.Millisecond-rtp.DefFrameDur]

	codecs := OfferCodecs()
	attrs := make([]sdp.Attribute, 0, len(codecs)+4)
	formats := make([]string, 0, len(codecs))
	dtmfType := byte(0)
	for _, codec := range codecs {
		if codec.Codec.Info().SDPName == dtmf.SDPName {
			dtmfType = codec.Type
		}
		styp := strconv.Itoa(int(codec.Type))
		formats = append(formats, styp)
		attrs = append(attrs, sdp.Attribute{
			Key:   "rtpmap",
			Value: styp + " " + codec.Codec.Info().SDPName,
		})
	}
	if dtmfType > 0 {
		attrs = append(attrs, sdp.Attribute{
			Key: "fmtp", Value: fmt.Sprintf("%d 0-16", dtmfType),
		})
	}
	var cryptoProfiles []srtp.Profile
	if encrypted != EncryptionNone {
		var err error
		cryptoProfiles, err = srtp.DefaultProfiles()
		if err != nil {
			return MediaDesc{}, nil, err
		}
		attrs = appendCryptoProfiles(attrs, cryptoProfiles)
	}

	attrs = append(attrs, []sdp.Attribute{
		{Key: "ptime", Value: "20"},
		{Key: "sendrecv"},
	}...)

	proto := "AVP"
	if encrypted != EncryptionNone {
		proto = "SAVP"
	}

	return MediaDesc{
			Codecs:         codecs,
			DTMFType:       dtmfType,
			CryptoProfiles: cryptoProfiles,
		}, &sdp.MediaDescription{
			MediaName: sdp.MediaName{
				Media:   "audio",
				Port:    sdp.RangedPort{Value: rtpListenerPort},
				Protos:  []string{"RTP", proto},
				Formats: formats,
			},
			Attributes: attrs,
		}, nil
}

func AnswerMedia(rtpListenerPort int, audio *AudioConfig, crypt *srtp.Profile) *sdp.MediaDescription {
	// Static compiler check for frame duration hardcoded below.
	var _ = [1]struct{}{}[20*time.Millisecond-rtp.DefFrameDur]

	attrs := make([]sdp.Attribute, 0, 6)
	attrs = append(attrs, sdp.Attribute{
		Key: "rtpmap", Value: fmt.Sprintf("%d %s", audio.Type, audio.Codec.Info().SDPName),
	})
	formats := make([]string, 0, 2)
	formats = append(formats, strconv.Itoa(int(audio.Type)))
	if audio.DTMFType != 0 {
		formats = append(formats, strconv.Itoa(int(audio.DTMFType)))
		attrs = append(attrs, []sdp.Attribute{
			{Key: "rtpmap", Value: fmt.Sprintf("%d %s", audio.DTMFType, dtmf.SDPName)},
			{Key: "fmtp", Value: fmt.Sprintf("%d 0-16", audio.DTMFType)},
		}...)
	}
	proto := "AVP"
	if crypt != nil {
		proto = "SAVP"
		attrs = appendCryptoProfiles(attrs, []srtp.Profile{*crypt})
	}
	attrs = append(attrs, []sdp.Attribute{
		{Key: "ptime", Value: "20"},
		{Key: "sendrecv"},
	}...)
	return &sdp.MediaDescription{
		MediaName: sdp.MediaName{
			Media:   "audio",
			Port:    sdp.RangedPort{Value: rtpListenerPort},
			Protos:  []string{"RTP", proto},
			Formats: formats,
		},
		Attributes: attrs,
	}
}

type Description struct {
	SDP  sdp.SessionDescription
	Addr netip.AddrPort
	MediaDesc
}

type Offer Description

type Answer Description

func NewOffer(publicIp netip.Addr, rtpListenerPort int, encrypted Encryption) (*Offer, error) {
	sessId := rand.Uint64() // TODO: do we need to track these?

	m, mediaDesc, err := OfferMedia(rtpListenerPort, encrypted)
	if err != nil {
		return nil, err
	}
	offer := sdp.SessionDescription{
		Version: 0,
		Origin: sdp.Origin{
			Username:       "-",
			SessionID:      sessId,
			SessionVersion: sessId,
			NetworkType:    "IN",
			AddressType:    "IP4",
			UnicastAddress: publicIp.String(),
		},
		SessionName: "LiveKit",
		ConnectionInformation: &sdp.ConnectionInformation{
			NetworkType: "IN",
			AddressType: "IP4",
			Address:     &sdp.Address{Address: publicIp.String()},
		},
		TimeDescriptions: []sdp.TimeDescription{
			{
				Timing: sdp.Timing{
					StartTime: 0,
					StopTime:  0,
				},
			},
		},
		MediaDescriptions: []*sdp.MediaDescription{mediaDesc},
	}
	return &Offer{
		SDP:       offer,
		Addr:      netip.AddrPortFrom(publicIp, uint16(rtpListenerPort)),
		MediaDesc: m,
	}, nil
}

func (d *Offer) Answer(publicIp netip.Addr, rtpListenerPort int, enc Encryption) (*Answer, *MediaConfig, error) {
	audio, err := SelectAudio(d.MediaDesc)
	if err != nil {
		return nil, nil, err
	}

	var (
		sconf *srtp.Config
		sprof *srtp.Profile
	)
	if len(d.CryptoProfiles) != 0 && enc != EncryptionNone {
		answer, err := srtp.DefaultProfiles()
		if err != nil {
			return nil, nil, err
		}
		sconf, sprof, err = SelectCrypto(d.CryptoProfiles, answer, true)
		if err != nil {
			return nil, nil, err
		}
	}
	if sprof == nil && enc == EncryptionRequire {
		return nil, nil, ErrNoCommonCrypto
	}

	mediaDesc := AnswerMedia(rtpListenerPort, audio, sprof)
	answer := sdp.SessionDescription{
		Version: 0,
		Origin: sdp.Origin{
			Username:       "-",
			SessionID:      d.SDP.Origin.SessionID,
			SessionVersion: d.SDP.Origin.SessionID + 2,
			NetworkType:    "IN",
			AddressType:    "IP4",
			UnicastAddress: publicIp.String(),
		},
		SessionName: "LiveKit",
		ConnectionInformation: &sdp.ConnectionInformation{
			NetworkType: "IN",
			AddressType: "IP4",
			Address:     &sdp.Address{Address: publicIp.String()},
		},
		TimeDescriptions: []sdp.TimeDescription{
			{
				Timing: sdp.Timing{
					StartTime: 0,
					StopTime:  0,
				},
			},
		},
		MediaDescriptions: []*sdp.MediaDescription{mediaDesc},
	}
	src := netip.AddrPortFrom(publicIp, uint16(rtpListenerPort))
	return &Answer{
			SDP:  answer,
			Addr: src,
			MediaDesc: MediaDesc{
				Codecs: []CodecInfo{
					{Type: audio.Type, Codec: audio.Codec},
				},
				DTMFType: audio.DTMFType,
			},
		}, &MediaConfig{
			Local:  src,
			Remote: d.Addr,
			Audio:  *audio,
			Crypto: sconf,
		}, nil
}

func (d *Answer) Apply(offer *Offer, enc Encryption) (*MediaConfig, error) {
	audio, err := SelectAudio(d.MediaDesc)
	if err != nil {
		return nil, err
	}
	var sconf *srtp.Config
	if len(d.CryptoProfiles) != 0 && enc != EncryptionNone {
		sconf, _, err = SelectCrypto(offer.CryptoProfiles, d.CryptoProfiles, false)
		if err != nil {
			return nil, err
		}
	}
	if sconf == nil && enc == EncryptionRequire {
		return nil, ErrNoCommonCrypto
	}
	return &MediaConfig{
		Local:  offer.Addr,
		Remote: d.Addr,
		Audio:  *audio,
		Crypto: sconf,
	}, nil
}

func Parse(data []byte) (*Description, error) {
	offer := new(Description)
	if err := offer.SDP.Unmarshal(data); err != nil {
		return nil, err
	}
	audio := GetAudio(&offer.SDP)
	if audio == nil {
		return nil, errors.New("no audio in sdp")
	}
	offer.Addr = GetAudioDest(&offer.SDP, audio)
	m, err := ParseMedia(audio)
	if err != nil {
		return nil, err
	}
	offer.MediaDesc = *m
	return offer, nil
}

func ParseOffer(data []byte) (*Offer, error) {
	d, err := Parse(data)
	if err != nil {
		return nil, err
	}
	return (*Offer)(d), nil
}

func ParseAnswer(data []byte) (*Answer, error) {
	d, err := Parse(data)
	if err != nil {
		return nil, err
	}
	return (*Answer)(d), nil
}

func parseSRTPProfile(val string) (*srtp.Profile, error) {
	val = strings.TrimSpace(val)
	sub := strings.SplitN(val, " ", 3)
	if len(sub) != 3 {
		return nil, nil // ignore
	}
	sind, prof, skey := sub[0], srtp.ProtectionProfile(sub[1]), sub[2]
	ind, err := strconv.Atoi(sind)
	if err != nil {
		return nil, err
	}
	var ok bool
	skey, ok = strings.CutPrefix(skey, "inline:")
	if !ok {
		return nil, nil // ignore
	}
	keys, err := base64.StdEncoding.WithPadding(base64.StdPadding).DecodeString(skey)
	if err != nil {
		return nil, fmt.Errorf("cannot parse crypto key %q: %v", skey, err)
	}
	var salt []byte
	if sp, err := prof.Parse(); err == nil {
		keyLen, err := sp.KeyLen()
		if err != nil {
			return nil, err
		}
		keys, salt = keys[:keyLen], keys[keyLen:]
	}
	return &srtp.Profile{
		Index:   ind,
		Profile: prof,
		Key:     keys,
		Salt:    salt,
	}, nil
}

func ParseMedia(d *sdp.MediaDescription) (*MediaDesc, error) {
	var out MediaDesc
	for _, m := range d.Attributes {
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
			if name == dtmf.SDPName || name == dtmf.SDPName+"/1" {
				out.DTMFType = byte(typ)
				continue
			}
			codec, _ := CodecByName(name).(rtp.AudioCodec)
			out.Codecs = append(out.Codecs, CodecInfo{
				Type:  byte(typ),
				Codec: codec,
			})
		case "crypto":
			p, err := parseSRTPProfile(m.Value)
			if err != nil {
				return nil, fmt.Errorf("cannot parse srtp profile %q: %v", m.Value, err)
			} else if p == nil {
				continue
			}
			out.CryptoProfiles = append(out.CryptoProfiles, *p)
		}
	}
	for _, f := range d.MediaName.Formats {
		typ, err := strconv.Atoi(f)
		if err != nil {
			continue
		}
		codec, _ := rtp.CodecByPayloadType(byte(typ)).(rtp.AudioCodec)
		out.Codecs = append(out.Codecs, CodecInfo{
			Type:  byte(typ),
			Codec: codec,
		})
	}
	return &out, nil
}

type MediaConfig struct {
	Local  netip.AddrPort
	Remote netip.AddrPort
	Audio  AudioConfig
	Crypto *srtp.Config
}

type AudioConfig struct {
	Codec    rtp.AudioCodec
	Type     byte
	DTMFType byte
}

func SelectAudio(desc MediaDesc) (*AudioConfig, error) {
	var (
		priority   int
		audioCodec rtp.AudioCodec
		audioType  byte
	)
	for _, c := range desc.Codecs {
		codec, ok := c.Codec.(rtp.AudioCodec)
		if !ok {
			continue
		}
		if audioCodec == nil || codec.Info().Priority > priority {
			audioType = c.Type
			audioCodec = codec
			priority = codec.Info().Priority
		}
	}
	if audioCodec == nil {
		return nil, ErrNoCommonMedia
	}
	return &AudioConfig{
		Codec:    audioCodec,
		Type:     audioType,
		DTMFType: desc.DTMFType,
	}, nil
}

func SelectCrypto(offer, answer []srtp.Profile, swap bool) (*srtp.Config, *srtp.Profile, error) {
	if len(offer) == 0 {
		return nil, nil, nil
	}
	for _, ans := range answer {
		sp, err := ans.Profile.Parse()
		if err != nil {
			continue
		}
		i := slices.IndexFunc(offer, func(off srtp.Profile) bool {
			return off.Profile == ans.Profile
		})
		if i >= 0 {
			off := offer[i]
			c := &srtp.Config{
				Keys: srtp.SessionKeys{
					LocalMasterKey:   off.Key,
					LocalMasterSalt:  off.Salt,
					RemoteMasterKey:  ans.Key,
					RemoteMasterSalt: ans.Salt,
				},
				Profile: sp,
			}
			if swap {
				c.Keys.LocalMasterKey, c.Keys.RemoteMasterKey = c.Keys.RemoteMasterKey, c.Keys.LocalMasterKey
				c.Keys.LocalMasterSalt, c.Keys.RemoteMasterSalt = c.Keys.RemoteMasterSalt, c.Keys.LocalMasterSalt
			}
			prof := &off
			if swap {
				prof = &ans
			}
			return c, prof, nil
		}
	}
	return nil, nil, nil
}
