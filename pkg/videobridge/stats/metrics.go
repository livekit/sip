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

package stats

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

const namespace = "livekit_sip_video"

var (
	SessionsActive = promauto.NewGauge(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "sessions_active",
		Help:      "Number of currently active SIP video sessions",
	})

	SessionsTotal = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "sessions_total",
		Help:      "Total number of SIP video sessions created",
	})

	SessionErrors = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "session_errors_total",
		Help:      "Total number of session errors by type",
	}, []string{"error_type"})

	RTPPacketsReceived = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "rtp_packets_received_total",
		Help:      "Total RTP packets received by media type",
	}, []string{"media_type"})

	RTPPacketsSent = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "rtp_packets_sent_total",
		Help:      "Total RTP packets sent by media type",
	}, []string{"media_type"})

	RTPPacketsLost = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "rtp_packets_lost_total",
		Help:      "Total RTP packets lost",
	})

	RTPJitterMs = promauto.NewHistogram(prometheus.HistogramOpts{
		Namespace: namespace,
		Name:      "rtp_jitter_ms",
		Help:      "RTP jitter in milliseconds",
		Buckets:   []float64{1, 5, 10, 20, 50, 100, 200, 500},
	})

	TranscodeLatencyMs = promauto.NewHistogram(prometheus.HistogramOpts{
		Namespace: namespace,
		Name:      "transcode_latency_ms",
		Help:      "Transcoding latency per frame in milliseconds",
		Buckets:   []float64{1, 2, 5, 10, 20, 33, 50, 100},
	})

	TranscodeActive = promauto.NewGauge(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "transcode_active",
		Help:      "Number of active transcode sessions",
	})

	KeyframeRequests = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "keyframe_requests_total",
		Help:      "Total keyframe (PLI/FIR) requests forwarded to SIP endpoint",
	})

	KeyframeInterval = promauto.NewHistogram(prometheus.HistogramOpts{
		Namespace: namespace,
		Name:      "keyframe_interval_seconds",
		Help:      "Interval between received keyframes",
		Buckets:   []float64{0.5, 1, 2, 5, 10, 30, 60},
	})

	CallSetupLatencyMs = promauto.NewHistogram(prometheus.HistogramOpts{
		Namespace: namespace,
		Name:      "call_setup_latency_ms",
		Help:      "Latency from SIP INVITE to first media packet published to LiveKit",
		Buckets:   []float64{100, 250, 500, 1000, 2000, 5000, 10000},
	})

	VideoBitrateKbps = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "video_bitrate_kbps",
		Help:      "Current video bitrate in kbps",
	}, []string{"direction", "codec"})

	AudioBitrateKbps = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "audio_bitrate_kbps",
		Help:      "Current audio bitrate in kbps",
	}, []string{"direction", "codec"})

	CodecPassthrough = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "codec_passthrough_total",
		Help:      "Total sessions using H.264 passthrough (no transcoding)",
	})

	CodecTranscode = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "codec_transcode_total",
		Help:      "Total sessions requiring transcoding",
	})
)
