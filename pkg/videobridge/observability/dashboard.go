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

package observability

// GrafanaDashboardJSON returns a Grafana dashboard JSON string for the
// SIP video bridge. All panels use the livekit_sip_video_* Prometheus metrics.
// Import this into Grafana via Dashboard → Import → Paste JSON.
func GrafanaDashboardJSON() string {
	return `{
  "dashboard": {
    "title": "SIP Video Bridge",
    "uid": "sip-video-bridge",
    "tags": ["livekit", "sip", "video"],
    "timezone": "browser",
    "refresh": "10s",
    "time": {"from": "now-1h", "to": "now"},
    "panels": [
      {
        "title": "Active Sessions",
        "type": "stat",
        "gridPos": {"h": 4, "w": 6, "x": 0, "y": 0},
        "targets": [{"expr": "livekit_sip_video_sessions_active", "legendFormat": "{{instance}}"}],
        "fieldConfig": {"defaults": {"thresholds": {"steps": [{"color": "green", "value": null}, {"color": "yellow", "value": 50}, {"color": "red", "value": 100}]}}}
      },
      {
        "title": "Total Sessions",
        "type": "stat",
        "gridPos": {"h": 4, "w": 6, "x": 6, "y": 0},
        "targets": [{"expr": "livekit_sip_video_sessions_total", "legendFormat": "total"}]
      },
      {
        "title": "Active Transcodes",
        "type": "stat",
        "gridPos": {"h": 4, "w": 6, "x": 12, "y": 0},
        "targets": [{"expr": "livekit_sip_video_transcode_active", "legendFormat": "active"}]
      },
      {
        "title": "Error Rate",
        "type": "timeseries",
        "gridPos": {"h": 8, "w": 12, "x": 0, "y": 4},
        "targets": [{"expr": "rate(livekit_sip_video_session_errors_total[5m])", "legendFormat": "{{error_type}}"}],
        "fieldConfig": {"defaults": {"unit": "ops"}}
      },
      {
        "title": "Call Setup Latency (p50/p95/p99)",
        "type": "timeseries",
        "gridPos": {"h": 8, "w": 12, "x": 12, "y": 4},
        "targets": [
          {"expr": "histogram_quantile(0.50, rate(livekit_sip_video_call_setup_latency_ms_bucket[5m]))", "legendFormat": "p50"},
          {"expr": "histogram_quantile(0.95, rate(livekit_sip_video_call_setup_latency_ms_bucket[5m]))", "legendFormat": "p95"},
          {"expr": "histogram_quantile(0.99, rate(livekit_sip_video_call_setup_latency_ms_bucket[5m]))", "legendFormat": "p99"}
        ],
        "fieldConfig": {"defaults": {"unit": "ms"}}
      },
      {
        "title": "RTP Jitter",
        "type": "timeseries",
        "gridPos": {"h": 8, "w": 12, "x": 0, "y": 12},
        "targets": [
          {"expr": "histogram_quantile(0.95, rate(livekit_sip_video_rtp_jitter_ms_bucket[5m]))", "legendFormat": "p95"},
          {"expr": "histogram_quantile(0.50, rate(livekit_sip_video_rtp_jitter_ms_bucket[5m]))", "legendFormat": "p50"}
        ],
        "fieldConfig": {"defaults": {"unit": "ms"}}
      },
      {
        "title": "Transcode Latency (per frame)",
        "type": "timeseries",
        "gridPos": {"h": 8, "w": 12, "x": 12, "y": 12},
        "targets": [
          {"expr": "histogram_quantile(0.95, rate(livekit_sip_video_transcode_latency_ms_bucket[5m]))", "legendFormat": "p95"},
          {"expr": "histogram_quantile(0.50, rate(livekit_sip_video_transcode_latency_ms_bucket[5m]))", "legendFormat": "p50"}
        ],
        "fieldConfig": {"defaults": {"unit": "ms"}}
      },
      {
        "title": "Video Bitrate",
        "type": "timeseries",
        "gridPos": {"h": 8, "w": 12, "x": 0, "y": 20},
        "targets": [{"expr": "livekit_sip_video_video_bitrate_kbps", "legendFormat": "{{direction}} {{codec}}"}],
        "fieldConfig": {"defaults": {"unit": "kbps"}}
      },
      {
        "title": "Audio Bitrate",
        "type": "timeseries",
        "gridPos": {"h": 8, "w": 12, "x": 12, "y": 20},
        "targets": [{"expr": "livekit_sip_video_audio_bitrate_kbps", "legendFormat": "{{direction}} {{codec}}"}],
        "fieldConfig": {"defaults": {"unit": "kbps"}}
      },
      {
        "title": "RTP Packets (recv/sent)",
        "type": "timeseries",
        "gridPos": {"h": 8, "w": 12, "x": 0, "y": 28},
        "targets": [
          {"expr": "rate(livekit_sip_video_rtp_packets_received_total[5m])", "legendFormat": "recv {{media_type}}"},
          {"expr": "rate(livekit_sip_video_rtp_packets_sent_total[5m])", "legendFormat": "sent {{media_type}}"}
        ],
        "fieldConfig": {"defaults": {"unit": "pps"}}
      },
      {
        "title": "Keyframe Requests",
        "type": "timeseries",
        "gridPos": {"h": 8, "w": 12, "x": 12, "y": 28},
        "targets": [
          {"expr": "rate(livekit_sip_video_keyframe_requests_total[5m])", "legendFormat": "PLI/FIR rate"},
          {"expr": "histogram_quantile(0.50, rate(livekit_sip_video_keyframe_interval_seconds_bucket[5m]))", "legendFormat": "interval p50"}
        ]
      },
      {
        "title": "Codec Distribution",
        "type": "piechart",
        "gridPos": {"h": 8, "w": 6, "x": 0, "y": 36},
        "targets": [
          {"expr": "livekit_sip_video_codec_passthrough_total", "legendFormat": "H.264 passthrough"},
          {"expr": "livekit_sip_video_codec_transcode_total", "legendFormat": "transcoded"}
        ]
      },
      {
        "title": "Packet Loss",
        "type": "stat",
        "gridPos": {"h": 4, "w": 6, "x": 6, "y": 36},
        "targets": [{"expr": "rate(livekit_sip_video_rtp_packets_lost_total[5m])", "legendFormat": "loss/sec"}],
        "fieldConfig": {"defaults": {"unit": "pps", "thresholds": {"steps": [{"color": "green", "value": null}, {"color": "red", "value": 10}]}}}
      }
    ]
  }
}`
}
