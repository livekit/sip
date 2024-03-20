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

	"github.com/livekit/sip/pkg/media/rtp"
	"github.com/livekit/sip/pkg/stats"
)

const (
	channels = 1
)

func newRTPStatsHandler(mon *stats.CallMonitor, typ string, h rtp.Handler) rtp.Handler {
	if h == nil {
		h = rtp.HandlerFunc(func(p *rtp.Packet) error {
			return nil
		})
	}
	return &rtpStatsHandler{h: h, typ: typ, mon: mon}
}

type rtpStatsHandler struct {
	h   rtp.Handler
	typ string
	mon *stats.CallMonitor
}

func (h *rtpStatsHandler) HandleRTP(p *rtp.Packet) error {
	if h.mon != nil {
		typ := h.typ
		if typ == "" {
			typ = strconv.Itoa(int(p.PayloadType))
		}
		h.mon.RTPPacketRecv(typ)
	}
	return h.h.HandleRTP(p)
}

func newRTPStatsWriter(mon *stats.CallMonitor, typ string, w rtp.Writer) rtp.Writer {
	return &rtpStatsWriter{w: w, typ: typ, mon: mon}
}

type rtpStatsWriter struct {
	w   rtp.Writer
	typ string
	mon *stats.CallMonitor
}

func (h *rtpStatsWriter) WriteRTP(p *rtp.Packet) error {
	if h.mon != nil {
		typ := h.typ
		if typ == "" {
			typ = strconv.Itoa(int(p.PayloadType))
		}
		h.mon.RTPPacketSend(typ)
	}
	return h.w.WriteRTP(p)
}
