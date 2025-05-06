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
	"fmt"
	"strconv"

	prtp "github.com/pion/rtp"

	"github.com/livekit/sip/pkg/media/rtp"
	"github.com/livekit/sip/pkg/stats"
)

const (
	channels       = 1
	RoomSampleRate = 48000
)

func newRTPStatsHandler(mon *stats.CallMonitor, typ string, r rtp.Handler) rtp.Handler {
	if r == nil {
		r = rtp.HandlerFunc(nil)
	}
	return &rtpStatsHandler{h: r, typ: typ, mon: mon}
}

type rtpStatsHandler struct {
	h   rtp.Handler
	typ string
	mon *stats.CallMonitor
}

func (r *rtpStatsHandler) String() string {
	return fmt.Sprintf("StatsHandler(%s) -> %s", r.typ, r.h.String())
}

func (r *rtpStatsHandler) HandleRTP(h *rtp.Header, payload []byte) error {
	if r.mon != nil {
		typ := r.typ
		if typ == "" {
			typ = strconv.Itoa(int(h.PayloadType))
		}
		r.mon.RTPPacketRecv(typ)
	}
	return r.h.HandleRTP(h, payload)
}

func newRTPStatsWriter(mon *stats.CallMonitor, typ string, w rtp.WriteStream) rtp.WriteStream {
	return &rtpStatsWriter{w: w, typ: typ, mon: mon}
}

type rtpStatsWriter struct {
	w   rtp.WriteStream
	typ string
	mon *stats.CallMonitor
}

func (w *rtpStatsWriter) String() string {
	return fmt.Sprintf("StatsWriter(%s) -> %s", w.typ, w.w.String())
}

func (w *rtpStatsWriter) WriteRTP(h *prtp.Header, payload []byte) (int, error) {
	if w.mon != nil {
		typ := w.typ
		if typ == "" {
			typ = strconv.Itoa(int(h.PayloadType))
		}
		w.mon.RTPPacketSend(typ)
	}
	return w.w.WriteRTP(h, payload)
}
