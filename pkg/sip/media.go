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
	"encoding/json"
	"fmt"
	"strconv"
	"sync/atomic"

	"github.com/pion/interceptor"
	prtp "github.com/pion/rtp"

	msdk "github.com/livekit/media-sdk"

	"github.com/livekit/media-sdk/rtp"

	"github.com/livekit/sip/pkg/stats"
)

var _ json.Marshaler = (*Stats)(nil)

type Stats struct {
	Port   PortStats
	Room   RoomStats
	Closed atomic.Bool
}

type StatsSnapshot struct {
	Port   PortStatsSnapshot  `json:"port"`
	Room   RoomStatsSnapshot  `json:"room"`
	Mixer  MixerStatsSnapshot `json:"mixer"`
	Closed bool               `json:"closed"`
}

type PortStatsSnapshot struct {
	Streams        uint64 `json:"streams"`
	Packets        uint64 `json:"packets"`
	IgnoredPackets uint64 `json:"packets_ignored"`
	InputPackets   uint64 `json:"packets_input"`

	MuxPackets uint64 `json:"mux_packets"`
	MuxBytes   uint64 `json:"mux_bytes"`

	AudioPackets uint64 `json:"audio_packets"`
	AudioBytes   uint64 `json:"audio_bytes"`

	DTMFPackets uint64 `json:"dtmf_packets"`
	DTMFBytes   uint64 `json:"dtmf_bytes"`

	Closed bool `json:"closed"`
}

type RoomStatsSnapshot struct {
	InputPackets uint64 `json:"input_packets"`
	InputBytes   uint64 `json:"input_bytes"`

	MixerSamples uint64 `json:"mixer_samples"`
	MixerFrames  uint64 `json:"mixer_frames"`

	OutputSamples uint64 `json:"output_samples"`
	OutputFrames  uint64 `json:"output_frames"`

	Closed bool `json:"closed"`
}

type MixerStatsSnapshot struct {
	Tracks      int64  `json:"tracks"`
	TracksTotal uint64 `json:"tracks_total"`
	Restarts    uint64 `json:"restarts"`

	Mixes      uint64 `json:"mixes"`
	TimedMixes uint64 `json:"mixes_timed"`
	JumpMixes  uint64 `json:"mixes_jump"`
	ZeroMixes  uint64 `json:"mixes_zero"`

	InputSamples uint64 `json:"input_samples"`
	InputFrames  uint64 `json:"input_frames"`

	MixedSamples uint64 `json:"mixed_samples"`
	MixedFrames  uint64 `json:"mixed_frames"`

	OutputSamples uint64 `json:"output_samples"`
	OutputFrames  uint64 `json:"output_frames"`
}

func (s *Stats) Load() StatsSnapshot {
	p := &s.Port
	r := &s.Room
	m := &r.Mixer
	return StatsSnapshot{
		Port: PortStatsSnapshot{
			Streams:        p.Streams.Load(),
			Packets:        p.Packets.Load(),
			IgnoredPackets: p.IgnoredPackets.Load(),
			InputPackets:   p.InputPackets.Load(),
			MuxPackets:     p.MuxPackets.Load(),
			MuxBytes:       p.MuxBytes.Load(),
			AudioPackets:   p.AudioPackets.Load(),
			AudioBytes:     p.AudioBytes.Load(),
			DTMFPackets:    p.DTMFPackets.Load(),
			DTMFBytes:      p.DTMFBytes.Load(),
			Closed:         p.Closed.Load(),
		},
		Room: RoomStatsSnapshot{
			InputPackets:  r.InputPackets.Load(),
			InputBytes:    r.InputBytes.Load(),
			MixerSamples:  r.MixerSamples.Load(),
			MixerFrames:   r.MixerFrames.Load(),
			OutputSamples: r.OutputSamples.Load(),
			OutputFrames:  r.OutputFrames.Load(),
			Closed:        r.Closed.Load(),
		},
		Mixer: MixerStatsSnapshot{
			Tracks:        m.Tracks.Load(),
			TracksTotal:   m.TracksTotal.Load(),
			Restarts:      m.Restarts.Load(),
			Mixes:         m.Mixes.Load(),
			TimedMixes:    m.TimedMixes.Load(),
			JumpMixes:     m.JumpMixes.Load(),
			ZeroMixes:     m.ZeroMixes.Load(),
			InputSamples:  m.InputSamples.Load(),
			InputFrames:   m.InputFrames.Load(),
			MixedSamples:  m.MixedSamples.Load(),
			MixedFrames:   m.MixedFrames.Load(),
			OutputSamples: m.OutputSamples.Load(),
			OutputFrames:  m.OutputFrames.Load(),
		},
		Closed: s.Closed.Load(),
	}
}

func (s *Stats) MarshalJSON() ([]byte, error) {
	return json.Marshal(s.Load())
}

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

func newMediaWriterCount(w msdk.PCM16Writer, frames, samples *atomic.Uint64) msdk.PCM16Writer {
	return &mediaWriterCount{
		w:       w,
		frames:  frames,
		samples: samples,
	}
}

type mediaWriterCount struct {
	w       msdk.PCM16Writer
	frames  *atomic.Uint64
	samples *atomic.Uint64
}

func (w *mediaWriterCount) String() string {
	return w.w.String()
}

func (w *mediaWriterCount) SampleRate() int {
	return w.w.SampleRate()
}

func (w *mediaWriterCount) Close() error {
	return w.w.Close()
}

func (w *mediaWriterCount) WriteSample(sample msdk.PCM16Sample) error {
	w.frames.Add(1)
	w.samples.Add(uint64(len(sample)))
	return w.w.WriteSample(sample)
}

func newRTPReaderCount(r rtp.Reader, packets, bytes *atomic.Uint64) rtp.Reader {
	return &rtpReaderCount{
		r:       r,
		packets: packets,
		bytes:   bytes,
	}
}

type rtpReaderCount struct {
	r       rtp.Reader
	packets *atomic.Uint64
	bytes   *atomic.Uint64
}

func (r *rtpReaderCount) ReadRTP() (*prtp.Packet, interceptor.Attributes, error) {
	p, in, err := r.r.ReadRTP()
	if p != nil {
		r.packets.Add(1)
		r.bytes.Add(uint64(len(p.Payload)))
	}
	return p, in, err
}

func newRTPHandlerCount(h rtp.Handler, packets, bytes *atomic.Uint64) rtp.Handler {
	return &rtpHandlerCount{
		h:       h,
		packets: packets,
		bytes:   bytes,
	}
}

type rtpHandlerCount struct {
	h       rtp.Handler
	packets *atomic.Uint64
	bytes   *atomic.Uint64
}

func (h *rtpHandlerCount) String() string {
	return h.h.String()
}

func (h *rtpHandlerCount) HandleRTP(hdr *prtp.Header, payload []byte) error {
	h.packets.Add(1)
	h.bytes.Add(uint64(len(payload)))
	return h.h.HandleRTP(hdr, payload)
}
