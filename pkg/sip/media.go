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
	"strconv"
	"time"

	"github.com/pion/rtp"

	srtp "github.com/livekit/sip/pkg/media/rtp"
	"github.com/livekit/sip/pkg/stats"
)

const (
	channels      = 1
	sampleRate    = 8000
	sampleDur     = 20 * time.Millisecond
	sampleDurPart = int(time.Second / sampleDur)
	rtpPacketDur  = uint32(sampleRate / sampleDurPart)
)

type rtpStatsHandler struct {
	h   srtp.Handler
	mon *stats.CallMonitor
}

func (h *rtpStatsHandler) HandleRTP(p *rtp.Packet) error {
	if h.mon != nil {
		if typ, ok := rtpPacketType(p); ok {
			h.mon.RTPPacketRecv(typ)
		}
	}
	return h.h.HandleRTP(p)
}

type rtpStatsWriter struct {
	w   srtp.Writer
	mon *stats.CallMonitor
}

func (h *rtpStatsWriter) WriteRTP(p *rtp.Packet) error {
	if h.mon != nil {
		if typ, ok := rtpPacketType(p); ok {
			h.mon.RTPPacketSend(typ)
		}
	}
	return h.w.WriteRTP(p)
}

func rtpPacketType(p *rtp.Packet) (string, bool) {
	switch p.PayloadType {
	case 101:
		if p.Marker {
			return "dtmf", true
		}
	default:
		if p.PayloadType == 0 {
			return "audio", true
		} else {
			return strconv.Itoa(int(p.PayloadType)), true
		}
	}
	return "", false
}
