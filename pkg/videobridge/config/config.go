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

package config

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/livekit/sip/pkg/videobridge/security"
)

type Config struct {
	// LiveKit server credentials
	ApiKey    string `yaml:"api_key"`
	ApiSecret string `yaml:"api_secret"`
	WsUrl     string `yaml:"ws_url"`

	// Redis configuration (session state)
	Redis RedisConfig `yaml:"redis"`

	// SIP signaling configuration
	SIP SIPConfig `yaml:"sip"`

	// RTP media configuration
	RTP RTPConfig `yaml:"rtp"`

	// Video codec configuration
	Video VideoConfig `yaml:"video"`

	// Transcoding configuration
	Transcode TranscodeConfig `yaml:"transcode"`

	// Security
	TLS     security.TLSConfig     `yaml:"tls"`
	Auth    security.AuthConfig    `yaml:"auth"`
	SRTP    security.SRTPConfig    `yaml:"srtp"`
	Secrets security.SecretsConfig `yaml:"secrets"`

	// Observability
	Telemetry TelemetryConfig `yaml:"telemetry"`
	Alerting  AlertingConfig  `yaml:"alerting"`

	// Region for feature flag targeting
	Region string `yaml:"region"`

	// Service ports
	PrometheusPort int `yaml:"prometheus_port"`
	HealthPort     int `yaml:"health_port"`

	// Logging
	LogLevel string `yaml:"log_level"`
}

// TelemetryConfig configures OpenTelemetry tracing.
type TelemetryConfig struct {
	Enabled    bool    `yaml:"enabled"`
	Endpoint   string  `yaml:"endpoint"`    // OTLP gRPC endpoint (e.g., "localhost:4317")
	SampleRate float64 `yaml:"sample_rate"` // 0.0-1.0 (default: 1.0 = sample all)
	Insecure   bool    `yaml:"insecure"`    // use insecure gRPC connection
}

// AlertingConfig configures the webhook alerting system.
type AlertingConfig struct {
	Enabled        bool          `yaml:"enabled"`
	WebhookURL     string        `yaml:"webhook_url"`
	CooldownPeriod time.Duration `yaml:"cooldown_period"` // min interval between duplicate alerts
}

type RedisConfig struct {
	Address  string `yaml:"address"`
	Username string `yaml:"username"`
	Password string `yaml:"password"`
	DB       int    `yaml:"db"`
}

type SIPConfig struct {
	// SIP listen port (default 5080, separate from existing SIP service at 5060)
	Port int `yaml:"port"`
	// SIP transports to enable
	Transport []string `yaml:"transport"`
	// External IP for SDP
	ExternalIP string `yaml:"external_ip"`
	// User agent string
	UserAgent string `yaml:"user_agent"`
}

type RTPConfig struct {
	// Port range for RTP media (default 20000-30000)
	PortStart int `yaml:"port_start"`
	PortEnd   int `yaml:"port_end"`
	// Enable jitter buffer
	JitterBuffer bool `yaml:"jitter_buffer"`
	// Jitter buffer target latency
	JitterLatency time.Duration `yaml:"jitter_latency"`
	// Media timeout (no packets received)
	MediaTimeout        time.Duration `yaml:"media_timeout"`
	MediaTimeoutInitial time.Duration `yaml:"media_timeout_initial"`
}

type VideoConfig struct {
	// Default codec preference: "h264" (passthrough) or "vp8" (force transcode)
	DefaultCodec string `yaml:"default_codec"`
	// Maximum video bitrate in bps
	MaxBitrate int `yaml:"max_bitrate"`
	// Target keyframe interval
	KeyframeInterval time.Duration `yaml:"keyframe_interval"`
	// H.264 profile-level-id for SDP offers
	H264Profile string `yaml:"h264_profile"`
}

type TranscodeConfig struct {
	// Enable transcoding capability
	Enabled bool `yaml:"enabled"`
	// Transcoding engine: "gstreamer" or "ffmpeg"
	Engine string `yaml:"engine"`
	// Maximum concurrent transcode sessions
	MaxConcurrent int `yaml:"max_concurrent"`
	// Maximum output bitrate in kbps (default 1500)
	MaxBitrate int `yaml:"max_bitrate"`
	// Use GPU acceleration
	GPU bool `yaml:"gpu"`
	// GPU device path (e.g., /dev/dri/renderD128)
	GPUDevice string `yaml:"gpu_device"`
}

func DefaultConfig() *Config {
	return &Config{
		SIP: SIPConfig{
			Port:      5080,
			Transport: []string{"udp", "tcp"},
			UserAgent: "LiveKit-SIP-Video-Bridge/0.1",
		},
		RTP: RTPConfig{
			PortStart:           20000,
			PortEnd:             30000,
			JitterBuffer:        true,
			JitterLatency:       80 * time.Millisecond,
			MediaTimeout:        15 * time.Second,
			MediaTimeoutInitial: 30 * time.Second,
		},
		Video: VideoConfig{
			DefaultCodec:     "h264",
			MaxBitrate:       1_500_000,
			KeyframeInterval: 2 * time.Second,
			H264Profile:      "42e01f", // Baseline profile, level 3.1
		},
		Transcode: TranscodeConfig{
			Enabled:       true,
			Engine:        "gstreamer",
			MaxConcurrent: 10,
			GPU:           false,
		},
		HealthPort: 8081,
		LogLevel:   "info",
	}
}

func Load(path string) (*Config, error) {
	cfg := DefaultConfig()

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config file: %w", err)
	}

	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parsing config file: %w", err)
	}

	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("validating config: %w", err)
	}

	// Override with environment variables
	cfg.ApplyEnv()

	return cfg, nil
}

func LoadFromBody(body string) (*Config, error) {
	cfg := DefaultConfig()

	if err := yaml.Unmarshal([]byte(body), cfg); err != nil {
		return nil, fmt.Errorf("parsing config body: %w", err)
	}

	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("validating config: %w", err)
	}

	cfg.ApplyEnv()

	return cfg, nil
}

func (c *Config) ApplyEnv() {
	if v := os.Getenv("LIVEKIT_API_KEY"); v != "" {
		c.ApiKey = v
	}
	if v := os.Getenv("LIVEKIT_API_SECRET"); v != "" {
		c.ApiSecret = v
	}
	if v := os.Getenv("LIVEKIT_WS_URL"); v != "" {
		c.WsUrl = v
	}
}

func (c *Config) Validate() error {
	if c.SIP.Port <= 0 || c.SIP.Port > 65535 {
		return fmt.Errorf("invalid SIP port: %d", c.SIP.Port)
	}
	if c.RTP.PortStart <= 0 || c.RTP.PortEnd <= 0 || c.RTP.PortStart >= c.RTP.PortEnd {
		return fmt.Errorf("invalid RTP port range: %d-%d", c.RTP.PortStart, c.RTP.PortEnd)
	}
	if c.Video.DefaultCodec != "h264" && c.Video.DefaultCodec != "vp8" {
		return fmt.Errorf("invalid default video codec: %s (must be h264 or vp8)", c.Video.DefaultCodec)
	}
	if c.Video.MaxBitrate <= 0 {
		return fmt.Errorf("invalid max bitrate: %d", c.Video.MaxBitrate)
	}
	if c.Transcode.MaxConcurrent <= 0 {
		return fmt.Errorf("invalid max concurrent transcodes: %d", c.Transcode.MaxConcurrent)
	}
	return nil
}
