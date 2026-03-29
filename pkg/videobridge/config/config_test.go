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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDefaultConfig(t *testing.T) {
	cfg := DefaultConfig()

	assert.Equal(t, 5080, cfg.SIP.Port)
	assert.Equal(t, 20000, cfg.RTP.PortStart)
	assert.Equal(t, 30000, cfg.RTP.PortEnd)
	assert.Equal(t, "h264", cfg.Video.DefaultCodec)
	assert.Equal(t, 1_500_000, cfg.Video.MaxBitrate)
	assert.Equal(t, "42e01f", cfg.Video.H264Profile)
	assert.Equal(t, 2*time.Second, cfg.Video.KeyframeInterval)
	assert.True(t, cfg.Transcode.Enabled)
	assert.Equal(t, "gstreamer", cfg.Transcode.Engine)
	assert.Equal(t, 10, cfg.Transcode.MaxConcurrent)
	assert.Equal(t, 8081, cfg.HealthPort)
}

func TestDefaultConfig_Validates(t *testing.T) {
	cfg := DefaultConfig()
	err := cfg.Validate()
	assert.NoError(t, err)
}

func TestLoadFromBody(t *testing.T) {
	body := `
log_level: debug
sip:
  port: 5090
rtp:
  port_start: 25000
  port_end: 35000
video:
  default_codec: h264
  max_bitrate: 2000000
transcode:
  max_concurrent: 5
`
	cfg, err := LoadFromBody(body)
	require.NoError(t, err)

	assert.Equal(t, "debug", cfg.LogLevel)
	assert.Equal(t, 5090, cfg.SIP.Port)
	assert.Equal(t, 25000, cfg.RTP.PortStart)
	assert.Equal(t, 35000, cfg.RTP.PortEnd)
	assert.Equal(t, 2_000_000, cfg.Video.MaxBitrate)
	assert.Equal(t, 5, cfg.Transcode.MaxConcurrent)
}

func TestValidate_InvalidPort(t *testing.T) {
	cfg := DefaultConfig()
	cfg.SIP.Port = -1
	assert.Error(t, cfg.Validate())
}

func TestValidate_InvalidRTPRange(t *testing.T) {
	cfg := DefaultConfig()
	cfg.RTP.PortStart = 30000
	cfg.RTP.PortEnd = 20000
	assert.Error(t, cfg.Validate())
}

func TestValidate_InvalidCodec(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Video.DefaultCodec = "av1"
	assert.Error(t, cfg.Validate())
}

func TestValidate_InvalidBitrate(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Video.MaxBitrate = 0
	assert.Error(t, cfg.Validate())
}

func TestValidate_VP8Codec(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Video.DefaultCodec = "vp8"
	assert.NoError(t, cfg.Validate())
}

func TestApplyEnv(t *testing.T) {
	cfg := DefaultConfig()

	t.Setenv("LIVEKIT_API_KEY", "test-key")
	t.Setenv("LIVEKIT_API_SECRET", "test-secret")
	t.Setenv("LIVEKIT_WS_URL", "ws://test:7880")

	cfg.ApplyEnv()

	assert.Equal(t, "test-key", cfg.ApiKey)
	assert.Equal(t, "test-secret", cfg.ApiSecret)
	assert.Equal(t, "ws://test:7880", cfg.WsUrl)
}
