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
	"time"

	prtp "github.com/pion/rtp"

	msdk "github.com/livekit/media-sdk"

	"github.com/livekit/media-sdk/rtp"

	"github.com/livekit/protocol/logger"
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

type MixerStatsSnapshot struct {
	Tracks       int64  `json:"tracks"`
	TracksTotal  uint64 `json:"tracks_total"`
	Restarts     uint64 `json:"restarts"`
	TimingResets uint64 `json:"timing_resets"`

	Mixes         uint64 `json:"mixes"`
	TimedMixes    uint64 `json:"mixes_timed"`
	JumpMixes     uint64 `json:"mixes_jump"`
	ZeroMixes     uint64 `json:"mixes_zero"`
	NegativeMixes uint64 `json:"mixes_negative"`

	InputSamples        uint64 `json:"input_samples"`
	InputFrames         uint64 `json:"input_frames"`
	InputSamplesDropped uint64 `json:"input_samples_dropped"`
	InputFramesDropped  uint64 `json:"input_frames_dropped"`

	MixedSamples uint64 `json:"mixed_samples"`
	MixedFrames  uint64 `json:"mixed_frames"`

	OutputSamples uint64 `json:"output_samples"`
	OutputFrames  uint64 `json:"output_frames"`

	WriteErrors  uint64 `json:"write_errors"`
	BlockedMixes uint64 `json:"blocked_mixes"`
}

func (s *Stats) Update() {
	if s == nil {
		return
	}
	s.Port.Update()
	s.Room.Update()
}

func (s *Stats) Load() StatsSnapshot {
	p := &s.Port
	r := &s.Room
	m := &r.Mixer
	return StatsSnapshot{
		Port: p.Load(),
		Room: r.Load(),
		Mixer: MixerStatsSnapshot{
			Tracks:              m.Tracks.Load(),
			TracksTotal:         m.TracksTotal.Load(),
			Restarts:            m.Restarts.Load(),
			TimingResets:        m.TimingResets.Load(),
			Mixes:               m.Mixes.Load(),
			TimedMixes:          m.TimedMixes.Load(),
			JumpMixes:           m.JumpMixes.Load(),
			ZeroMixes:           m.ZeroMixes.Load(),
			NegativeMixes:       m.NegativeMixes.Load(),
			InputSamples:        m.InputSamples.Load(),
			InputFrames:         m.InputFrames.Load(),
			InputFramesDropped:  m.InputFramesDropped.Load(),
			InputSamplesDropped: m.InputSamplesDropped.Load(),
			MixedSamples:        m.MixedSamples.Load(),
			MixedFrames:         m.MixedFrames.Load(),
			OutputSamples:       m.OutputSamples.Load(),
			OutputFrames:        m.OutputFrames.Load(),
			WriteErrors:         m.WriteErrors.Load(),
			BlockedMixes:        m.BlockedMixes.Load(),
		},
		Closed: s.Closed.Load(),
	}
}

func (s *Stats) Log(log logger.Logger, callStart time.Time) {
	const expectedSampleRate = RoomSampleRate
	st := s.Load()
	log.Infow("call statistics",
		"stats", st,
		"durMin", int(time.Since(callStart).Minutes()),
		"sip_rx_ppm", ratePPM(st.Port.AudioRX, expectedSampleRate),
		"sip_tx_ppm", ratePPM(st.Port.AudioTX, expectedSampleRate),
		"lk_publish_ppm", ratePPM(st.Room.PublishTX, expectedSampleRate),
		"expected_pcm_hz", expectedSampleRate,
	)
}

func (s *Stats) MarshalJSON() ([]byte, error) {
	return json.Marshal(s.Load())
}

func ratePPM(rate float64, expected int) float64 {
	if expected <= 0 {
		return 0
	}
	return (rate - float64(expected)) / float64(expected) * 1_000_000
}

const (
	channels       = 1
	RoomSampleRate = 48000
	RoomResample   = false
)

var staticPayloadTypes = map[uint8]string{
	0:  "Auto_PCMU/8000",
	3:  "Auto_GSM/8000",
	4:  "Auto_G723/8000",
	5:  "Auto_DVI4/8000",
	6:  "Auto_DVI4/16000",
	7:  "Auto_LPC/8000",
	8:  "Auto_PCMA/8000",
	9:  "Auto_G722/8000",
	10: "Auto_L16/44100/2",
	11: "Auto_L16/44100",
	12: "Auto_QCELP/8000",
	13: "Auto_CN/8000",
	14: "Auto_MPA/90000",
	15: "Auto_G728/16000",
	16: "Auto_DVI4/11025",
	17: "Auto_DVI4/22050",
	18: "Auto_G729/8000",
	25: "Auto_CELLB/90000",
	26: "Auto_JPEG/90000",
	28: "Auto_NV/90000",
	31: "Auto_H261/90000",
	32: "Auto_MPV/90000",
	33: "Auto_MP2T/90000",
	34: "Auto_H263/90000",
}

func newRTPStatsHandler(mon *stats.CallMonitor, typ string, r rtp.Handler) *rtpStatsHandler {
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
		if typ == "" && h.PayloadType < 96 {
			typ, _ = staticPayloadTypes[h.PayloadType]
		}
		if typ == "" {
			typ = strconv.Itoa(int(h.PayloadType))
		}
		r.mon.RTPPacketRecv(typ)
	}
	return r.h.HandleRTP(h, payload)
}

func (r *rtpStatsHandler) Close() {
	if closer, ok := r.h.(rtp.HandlerCloser); ok {
		closer.Close()
	}
}

func newRTPStatsWriter(mon *stats.CallMonitor, audioPayloadType uint8, dtmfPayloadType uint8, audioType string, dtmfType string, w rtp.WriteStream) rtp.WriteStream {
	return &rtpStatsWriter{
		w:                w,
		audioPayloadType: audioPayloadType,
		dtmfPayloadType:  dtmfPayloadType,
		audioType:        audioType,
		dtmfType:         dtmfType,
		mon:              mon,
	}
}

type rtpStatsWriter struct {
	w                rtp.WriteStream
	audioPayloadType uint8
	dtmfPayloadType  uint8
	audioType        string
	dtmfType         string
	mon              *stats.CallMonitor
}

func (w *rtpStatsWriter) String() string {
	return fmt.Sprintf("StatsWriter(%s) -> %s", w.audioType, w.w.String())
}

func (w *rtpStatsWriter) WriteRTP(h *prtp.Header, payload []byte) (int, error) {
	if w.mon != nil {
		typ := ""
		switch h.PayloadType {
		case w.audioPayloadType:
			typ = w.audioType
		case w.dtmfPayloadType:
			typ = w.dtmfType
		}
		if typ == "" && h.PayloadType < 96 {
			typ, _ = staticPayloadTypes[h.PayloadType]
		}
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

func newRTPHandlerCount(h rtp.Handler, packets, bytes *atomic.Uint64) *rtpHandlerCount {
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

func (h *rtpHandlerCount) Close() {
	if closer, ok := h.h.(rtp.HandlerCloser); ok {
		closer.Close()
	}
}

func (h *rtpHandlerCount) HandleRTP(hdr *prtp.Header, payload []byte) error {
	h.packets.Add(1)
	h.bytes.Add(uint64(len(payload)))
	return h.h.HandleRTP(hdr, payload)
}

const maxPositiveSeqDiff = int16(30 * rtp.DefFramesPerSec)
const maxNegativeSeqDiff = int16(-5 * rtp.DefFramesPerSec)
const rapidPacketThreshold = int64(float64(1000/rtp.DefFramesPerSec) * 0.5)
const delayedPacketThreshold = int64(float64(1000/rtp.DefFramesPerSec) * 1.5)

func Diff16(cur, prev uint16) int16 {
	return int16(cur - prev)
}

type rtpCountingStats struct {
	packets        atomic.Uint64
	bytes          atomic.Uint64
	resets         atomic.Uint64
	gaps           atomic.Uint64
	gapsSum        atomic.Uint64
	late           atomic.Uint64
	lateSum        atomic.Uint64
	delayedPackets atomic.Uint64
	delayedSum     atomic.Uint64 // total duration of delayed packets in milliseconds
	rapidPackets   atomic.Uint64 // Number of packets that arrived in less than half the expected duration
}

func newRTPStreamStats(h rtp.Handler, stats *rtpCountingStats) *rtpStreamStats {
	if stats == nil {
		stats = &rtpCountingStats{}
	}
	return &rtpStreamStats{
		h:     h,
		stats: stats,
	}
}

type rtpStreamStats struct {
	packets    atomic.Uint64 // per rtpStreamStats, where rtpStreamStats.stats.packets may not be exclusive to this stream
	lastSeq    atomic.Uint64
	lastPacket atomic.Int64
	h          rtp.Handler
	stats      *rtpCountingStats
}

func (h *rtpStreamStats) String() string {
	return h.h.String()
}

func (h *rtpStreamStats) Close() {
	if closer, ok := h.h.(rtp.HandlerCloser); ok {
		closer.Close()
	}
}

func (h *rtpStreamStats) HandleRTP(hdr *prtp.Header, payload []byte) error {
	count := h.packets.Add(1)
	h.stats.packets.Add(1)
	h.stats.bytes.Add(uint64(len(payload)))

	now := time.Now().UnixMilli()
	lastSeq := uint16(h.lastSeq.Swap(uint64(hdr.SequenceNumber)))
	lastPacket := h.lastPacket.Swap(now)
	if count > 1 {
		diff := Diff16(hdr.SequenceNumber, lastSeq)
		if diff > maxPositiveSeqDiff || diff < maxNegativeSeqDiff {
			h.stats.resets.Add(1)
		} else {
			if diff < 0 {
				h.stats.late.Add(1)
				h.stats.lateSum.Add(uint64(-diff))
			} else if diff > 1 {
				h.stats.gaps.Add(1)
				h.stats.gapsSum.Add(uint64(diff))
			}
		}

		sinceLastPacket := now - lastPacket
		if sinceLastPacket < rapidPacketThreshold {
			h.stats.rapidPackets.Add(1)
		} else if sinceLastPacket > delayedPacketThreshold {
			h.stats.delayedPackets.Add(1)
			h.stats.delayedSum.Add(uint64(sinceLastPacket))
		}
	}

	return h.h.HandleRTP(hdr, payload)
}
