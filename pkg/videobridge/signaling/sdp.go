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

package signaling

import (
	"fmt"
	"net/netip"
	"strconv"
	"strings"
)

// SDPMediaDesc represents a parsed SDP media description.
type SDPMediaDesc struct {
	MediaType   string // "audio" or "video"
	Port        int
	Protocol    string // "RTP/AVP" or "RTP/SAVP"
	PayloadType uint8
	CodecName   string
	ClockRate   int
	Fmtp        string // format-specific parameters (e.g., profile-level-id)
}

// NegotiatedMedia holds the result of SDP negotiation.
type NegotiatedMedia struct {
	RemoteAddr netip.AddrPort

	// Video
	VideoPayloadType uint8
	VideoCodec       string
	VideoClockRate   int
	VideoFmtp        string

	// Audio
	AudioPayloadType uint8
	AudioCodec       string
	AudioClockRate   int

	// DTMF
	DTMFPayloadType uint8
}

// BuildVideoSDP creates an SDP offer/answer body for video + audio.
func BuildVideoSDP(localIP netip.Addr, videoPort, audioPort int, h264Profile string) string {
	sessionID := fmt.Sprintf("%d", videoPort*1000+audioPort)

	var sb strings.Builder
	sb.WriteString("v=0\r\n")
	sb.WriteString(fmt.Sprintf("o=livekit-video-bridge %s 1 IN IP4 %s\r\n", sessionID, localIP.String()))
	sb.WriteString("s=LiveKit SIP Video Bridge\r\n")
	sb.WriteString(fmt.Sprintf("c=IN IP4 %s\r\n", localIP.String()))
	sb.WriteString("t=0 0\r\n")

	// Video media line: H.264 on dynamic PT 96
	sb.WriteString(fmt.Sprintf("m=video %d RTP/AVP 96\r\n", videoPort))
	sb.WriteString(fmt.Sprintf("a=rtpmap:96 H264/%d\r\n", 90000))
	if h264Profile != "" {
		sb.WriteString(fmt.Sprintf("a=fmtp:96 profile-level-id=%s;packetization-mode=1\r\n", h264Profile))
	} else {
		sb.WriteString("a=fmtp:96 profile-level-id=42e01f;packetization-mode=1\r\n")
	}
	sb.WriteString("a=sendrecv\r\n")

	// Audio media line: PCMU (0), PCMA (8), telephone-event (101)
	sb.WriteString(fmt.Sprintf("m=audio %d RTP/AVP 0 8 101\r\n", audioPort))
	sb.WriteString("a=rtpmap:0 PCMU/8000\r\n")
	sb.WriteString("a=rtpmap:8 PCMA/8000\r\n")
	sb.WriteString("a=rtpmap:101 telephone-event/8000\r\n")
	sb.WriteString("a=fmtp:101 0-16\r\n")
	sb.WriteString("a=sendrecv\r\n")

	return sb.String()
}

// ParseSDP performs a minimal SDP parse to extract media descriptions.
// This handles the common case of SIP video endpoints offering H.264 + audio.
func ParseSDP(sdpBody string) (*ParsedSDP, error) {
	p := &ParsedSDP{}
	lines := strings.Split(sdpBody, "\n")

	var currentMedia *SDPMediaDesc
	for _, line := range lines {
		line = strings.TrimSpace(line)
		line = strings.TrimSuffix(line, "\r")

		switch {
		case strings.HasPrefix(line, "c="):
			addr := parseConnectionAddr(line)
			if addr != "" {
				p.ConnectionAddr = addr
			}

		case strings.HasPrefix(line, "m="):
			md := parseMediaLine(line)
			if md != nil {
				currentMedia = md
				p.Media = append(p.Media, md)
			}

		case strings.HasPrefix(line, "a=rtpmap:"):
			if currentMedia != nil {
				parseRtpmap(line, currentMedia)
			}

		case strings.HasPrefix(line, "a=fmtp:"):
			if currentMedia != nil {
				parseFmtp(line, currentMedia)
			}
		}
	}

	return p, nil
}

// ParsedSDP holds parsed SDP data.
type ParsedSDP struct {
	ConnectionAddr string
	Media          []*SDPMediaDesc
}

// VideoMedia returns the first video media description, if any.
func (p *ParsedSDP) VideoMedia() *SDPMediaDesc {
	for _, m := range p.Media {
		if m.MediaType == "video" {
			return m
		}
	}
	return nil
}

// AudioMedia returns the first audio media description, if any.
func (p *ParsedSDP) AudioMedia() *SDPMediaDesc {
	for _, m := range p.Media {
		if m.MediaType == "audio" {
			return m
		}
	}
	return nil
}

// Negotiate creates a NegotiatedMedia from the parsed remote SDP.
func (p *ParsedSDP) Negotiate() (*NegotiatedMedia, error) {
	nm := &NegotiatedMedia{}

	video := p.VideoMedia()
	if video == nil {
		return nil, fmt.Errorf("no video media in SDP")
	}
	nm.VideoPayloadType = video.PayloadType
	nm.VideoCodec = video.CodecName
	nm.VideoClockRate = video.ClockRate
	nm.VideoFmtp = video.Fmtp

	audio := p.AudioMedia()
	if audio != nil {
		nm.AudioPayloadType = audio.PayloadType
		nm.AudioCodec = audio.CodecName
		nm.AudioClockRate = audio.ClockRate
	}

	if p.ConnectionAddr != "" && video.Port > 0 {
		addr, err := netip.ParseAddr(p.ConnectionAddr)
		if err == nil {
			nm.RemoteAddr = netip.AddrPortFrom(addr, uint16(video.Port))
		}
	}

	return nm, nil
}

func parseConnectionAddr(line string) string {
	// c=IN IP4 192.168.1.100
	parts := strings.Fields(line[2:])
	if len(parts) >= 3 {
		return parts[2]
	}
	return ""
}

func parseMediaLine(line string) *SDPMediaDesc {
	// m=video 49170 RTP/AVP 96
	parts := strings.Fields(line[2:])
	if len(parts) < 4 {
		return nil
	}

	port, err := strconv.Atoi(parts[1])
	if err != nil {
		return nil
	}

	pt, err := strconv.Atoi(parts[3])
	if err != nil {
		return nil
	}

	return &SDPMediaDesc{
		MediaType:   parts[0],
		Port:        port,
		Protocol:    parts[2],
		PayloadType: uint8(pt),
	}
}

func parseRtpmap(line string, md *SDPMediaDesc) {
	// a=rtpmap:96 H264/90000
	rest := line[len("a=rtpmap:"):]
	parts := strings.SplitN(rest, " ", 2)
	if len(parts) != 2 {
		return
	}

	pt, err := strconv.Atoi(parts[0])
	if err != nil || uint8(pt) != md.PayloadType {
		return
	}

	codecParts := strings.SplitN(parts[1], "/", 2)
	md.CodecName = codecParts[0]
	if len(codecParts) > 1 {
		md.ClockRate, _ = strconv.Atoi(codecParts[1])
	}
}

func parseFmtp(line string, md *SDPMediaDesc) {
	// a=fmtp:96 profile-level-id=42e01f;packetization-mode=1
	rest := line[len("a=fmtp:"):]
	parts := strings.SplitN(rest, " ", 2)
	if len(parts) != 2 {
		return
	}
	pt, err := strconv.Atoi(parts[0])
	if err != nil || uint8(pt) != md.PayloadType {
		return
	}
	md.Fmtp = parts[1]
}
