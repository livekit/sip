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

package transcode

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"

	"github.com/livekit/protocol/logger"

	"github.com/livekit/sip/pkg/videobridge/config"
)

// GPUType identifies the GPU acceleration backend.
type GPUType string

const (
	GPUNone  GPUType = "none"
	GPUNVENC GPUType = "nvenc"  // NVIDIA NVENC (via CUDA)
	GPUVAAPI GPUType = "vaapi"  // Intel/AMD VA-API (Linux)
	GPUVTool GPUType = "vtool"  // Apple VideoToolbox (macOS)
)

// GPUInfo holds detected GPU capabilities.
type GPUInfo struct {
	Available bool
	Type      GPUType
	Device    string // e.g. /dev/dri/renderD128 for VAAPI
	Name      string // human-readable GPU name
}

// DetectGPU probes the system for available GPU acceleration.
func DetectGPU(log logger.Logger, conf *config.TranscodeConfig) GPUInfo {
	if !conf.GPU {
		return GPUInfo{Type: GPUNone}
	}

	// Check NVIDIA first (highest priority)
	if info := detectNVENC(log, conf.GPUDevice); info.Available {
		return info
	}

	// Check VA-API (Intel/AMD on Linux)
	if runtime.GOOS == "linux" {
		if info := detectVAAPI(log, conf.GPUDevice); info.Available {
			return info
		}
	}

	// Check VideoToolbox (macOS)
	if runtime.GOOS == "darwin" {
		return GPUInfo{
			Available: true,
			Type:      GPUVTool,
			Name:      "Apple VideoToolbox",
		}
	}

	log.Infow("no GPU acceleration detected, using software transcoding")
	return GPUInfo{Type: GPUNone}
}

func detectNVENC(log logger.Logger, device string) GPUInfo {
	// Check if nvidia-smi is available
	out, err := exec.Command("nvidia-smi", "--query-gpu=name", "--format=csv,noheader").Output()
	if err != nil {
		return GPUInfo{Type: GPUNone}
	}

	name := strings.TrimSpace(string(out))
	if name == "" {
		return GPUInfo{Type: GPUNone}
	}

	// Take first GPU name if multiple
	if idx := strings.Index(name, "\n"); idx > 0 {
		name = name[:idx]
	}

	log.Infow("NVIDIA GPU detected", "gpu", name)
	return GPUInfo{
		Available: true,
		Type:      GPUNVENC,
		Name:      name,
	}
}

func detectVAAPI(log logger.Logger, device string) GPUInfo {
	if device == "" {
		device = "/dev/dri/renderD128"
	}

	if _, err := os.Stat(device); err != nil {
		return GPUInfo{Type: GPUNone}
	}

	// Verify vainfo works
	out, err := exec.Command("vainfo", "--display", "drm", "--device", device).CombinedOutput()
	if err != nil {
		return GPUInfo{Type: GPUNone}
	}

	outStr := string(out)
	if !strings.Contains(outStr, "VAProfileVP8") && !strings.Contains(outStr, "VAEntrypointEncSlice") {
		log.Infow("VA-API device found but VP8 encode not supported", "device", device)
		return GPUInfo{Type: GPUNone}
	}

	log.Infow("VA-API GPU detected", "device", device)
	return GPUInfo{
		Available: true,
		Type:      GPUVAAPI,
		Device:    device,
		Name:      "VA-API (" + device + ")",
	}
}

// BuildGStreamerGPUArgs returns the GStreamer pipeline elements for GPU-accelerated transcoding.
func BuildGStreamerGPUArgs(gpu GPUInfo, bitrate int) []string {
	switch gpu.Type {
	case GPUNVENC:
		// NVIDIA: nvh264dec for decode, vp8enc still software (NVENC doesn't support VP8)
		// But we can use GPU for H.264 decode which is the expensive part
		return []string{
			"-q",
			"fdsrc", "fd=0", "!",
			"h264parse", "!",
			"nvh264dec", "!",
			"videoconvert", "!",
			fmt.Sprintf("vp8enc target-bitrate=%d deadline=1 cpu-used=4 threads=4 keyframe-max-dist=60", bitrate*1000),
			"!",
			"ivfmux", "!",
			"fdsink", "fd=1",
		}

	case GPUVAAPI:
		// VA-API: vaapih264dec for decode, vaapivp8enc for encode (if available)
		return []string{
			"-q",
			"fdsrc", "fd=0", "!",
			"h264parse", "!",
			fmt.Sprintf("vaapih264dec ! vaapipostproc ! vaapivp8enc rate-control=cbr bitrate=%d keyframe-period=60", bitrate),
			"!",
			"ivfmux", "!",
			"fdsink", "fd=1",
		}

	case GPUVTool:
		// macOS VideoToolbox: vtdec for H.264 decode, vp8enc software encode
		return []string{
			"-q",
			"fdsrc", "fd=0", "!",
			"h264parse", "!",
			"vtdec", "!",
			"videoconvert", "!",
			fmt.Sprintf("vp8enc target-bitrate=%d deadline=1 cpu-used=4 threads=4 keyframe-max-dist=60", bitrate*1000),
			"!",
			"ivfmux", "!",
			"fdsink", "fd=1",
		}

	default:
		return nil // use default software pipeline
	}
}

// BuildFFmpegGPUArgs returns FFmpeg arguments for GPU-accelerated transcoding.
func BuildFFmpegGPUArgs(gpu GPUInfo, bitrate string) []string {
	switch gpu.Type {
	case GPUNVENC:
		// NVIDIA CUVID for H.264 decode
		return []string{
			"-hide_banner", "-loglevel", "warning",
			"-hwaccel", "cuda", "-hwaccel_output_format", "cuda",
			"-c:v", "h264_cuvid",
			"-f", "h264", "-i", "pipe:0",
			"-c:v", "libvpx",
			"-b:v", bitrate,
			"-deadline", "realtime",
			"-cpu-used", "4",
			"-g", "60",
			"-f", "ivf",
			"pipe:1",
		}

	case GPUVAAPI:
		return []string{
			"-hide_banner", "-loglevel", "warning",
			"-hwaccel", "vaapi",
			"-hwaccel_device", gpu.Device,
			"-hwaccel_output_format", "vaapi",
			"-f", "h264", "-i", "pipe:0",
			"-vf", "format=nv12|vaapi,hwupload",
			"-c:v", "vp8_vaapi",
			"-b:v", bitrate,
			"-g", "60",
			"-f", "ivf",
			"pipe:1",
		}

	case GPUVTool:
		// macOS VideoToolbox for H.264 decode
		return []string{
			"-hide_banner", "-loglevel", "warning",
			"-hwaccel", "videotoolbox",
			"-f", "h264", "-i", "pipe:0",
			"-c:v", "libvpx",
			"-b:v", bitrate,
			"-deadline", "realtime",
			"-cpu-used", "4",
			"-g", "60",
			"-f", "ivf",
			"pipe:1",
		}

	default:
		return nil
	}
}
