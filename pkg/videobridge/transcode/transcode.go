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

// Package transcode provides the interface and implementations for video transcoding.
// Phase 1 (MVP) uses H.264 passthrough and does not require transcoding.
// Phase 2 adds GStreamer-based H.264 → VP8 transcoding for clients that require VP8.
package transcode

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"os/exec"
	"sync"
	"sync/atomic"
	"time"

	"github.com/livekit/protocol/logger"

	"github.com/livekit/sip/pkg/videobridge/codec"
	"github.com/livekit/sip/pkg/videobridge/config"
	"github.com/livekit/sip/pkg/videobridge/stats"
)

// FrameSink receives encoded VP8 frames from the transcoder.
type FrameSink interface {
	WriteEncodedFrame(data []byte, timestamp uint32, keyframe bool) error
}

// Transcoder decodes H.264 NAL units and re-encodes to VP8.
type Transcoder interface {
	// Start initializes the transcoding pipeline.
	Start() error
	// WriteNAL feeds an H.264 NAL unit into the decoder.
	WriteNAL(nal codec.NALUnit, timestamp uint32) error
	// SetOutput sets the sink for encoded VP8 output.
	SetOutput(sink FrameSink)
	// RequestKeyframe forces the encoder to produce a keyframe.
	RequestKeyframe()
	// SetBitrate adjusts the output bitrate dynamically.
	SetBitrate(bps int)
	// Close shuts down the transcoding pipeline.
	Close() error
}

// Engine identifies the transcoding backend.
type Engine string

const (
	EngineGStreamer Engine = "gstreamer"
	EngineFFmpeg    Engine = "ffmpeg"
	EngineNone      Engine = "none"
)

// Pool manages a pool of transcoder instances with concurrency control.
type Pool struct {
	log  logger.Logger
	conf *config.TranscodeConfig

	mu     sync.Mutex
	active int32
}

// NewPool creates a new transcoder pool.
func NewPool(log logger.Logger, conf *config.TranscodeConfig) *Pool {
	return &Pool{
		log:  log,
		conf: conf,
	}
}

// Acquire attempts to allocate a transcoder from the pool.
// Returns an error if the concurrency limit is reached.
func (p *Pool) Acquire() (Transcoder, error) {
	if !p.conf.Enabled {
		return nil, fmt.Errorf("transcoding is disabled")
	}

	current := atomic.LoadInt32(&p.active)
	if int(current) >= p.conf.MaxConcurrent {
		stats.SessionErrors.WithLabelValues("transcode_limit").Inc()
		return nil, fmt.Errorf("transcoder pool exhausted (%d/%d)", current, p.conf.MaxConcurrent)
	}

	atomic.AddInt32(&p.active, 1)
	stats.TranscodeActive.Inc()

	engine := Engine(p.conf.Engine)
	switch engine {
	case EngineGStreamer:
		return newGStreamerTranscoder(p.log, p.conf, p.release), nil
	case EngineFFmpeg:
		return newFFmpegTranscoder(p.log, p.conf, p.release), nil
	default:
		return newStubTranscoder(p.log, p.release), nil
	}
}

// ActiveCount returns the number of active transcoders.
func (p *Pool) ActiveCount() int {
	return int(atomic.LoadInt32(&p.active))
}

func (p *Pool) release() {
	atomic.AddInt32(&p.active, -1)
	stats.TranscodeActive.Dec()
}

// stubTranscoder is a placeholder that logs frames without transcoding.
// Used when transcoding is not yet implemented or for testing.
type stubTranscoder struct {
	log       logger.Logger
	releaseFn func()
	output    FrameSink
}

func newStubTranscoder(log logger.Logger, releaseFn func()) *stubTranscoder {
	return &stubTranscoder{log: log, releaseFn: releaseFn}
}

func (t *stubTranscoder) Start() error {
	t.log.Infow("stub transcoder started (no actual transcoding)")
	return nil
}

func (t *stubTranscoder) WriteNAL(nal codec.NALUnit, timestamp uint32) error {
	stats.TranscodeLatencyMs.Observe(0)
	return nil
}

func (t *stubTranscoder) SetOutput(sink FrameSink) {
	t.output = sink
}

func (t *stubTranscoder) RequestKeyframe() {}

func (t *stubTranscoder) SetBitrate(bps int) {}

func (t *stubTranscoder) Close() error {
	if t.releaseFn != nil {
		t.releaseFn()
	}
	t.log.Infow("stub transcoder closed")
	return nil
}

// subprocessTranscoder is the base for GStreamer and FFmpeg subprocess-based transcoders.
// It communicates with the child process via stdin (H.264 Annex B NALs) and stdout (IVF VP8 frames).
type subprocessTranscoder struct {
	log       logger.Logger
	conf      *config.TranscodeConfig
	releaseFn func()
	output    FrameSink

	mu        sync.Mutex
	cmd       *exec.Cmd
	stdin     io.WriteCloser
	stdout    io.ReadCloser
	started   bool
	closed    bool
	forceKF   atomic.Bool
	bitrate   atomic.Int64
	lastWrite atomic.Int64
}

// annexBStartCode is the 4-byte start code prefix for H.264 Annex B byte stream.
var annexBStartCode = []byte{0x00, 0x00, 0x00, 0x01}

func (t *subprocessTranscoder) Start() error {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.cmd == nil {
		return fmt.Errorf("command not configured")
	}

	var err error
	t.stdin, err = t.cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("stdin pipe: %w", err)
	}

	t.stdout, err = t.cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("stdout pipe: %w", err)
	}

	// Capture stderr for diagnostics
	var stderrBuf bytes.Buffer
	t.cmd.Stderr = &stderrBuf

	if err := t.cmd.Start(); err != nil {
		return fmt.Errorf("starting transcode process: %w", err)
	}

	t.started = true
	t.log.Infow("transcoder subprocess started", "pid", t.cmd.Process.Pid)

	// Read VP8 frames from stdout in background
	go t.readOutputLoop(&stderrBuf)

	return nil
}

func (t *subprocessTranscoder) WriteNAL(nal codec.NALUnit, timestamp uint32) error {
	t.mu.Lock()
	if !t.started || t.closed {
		t.mu.Unlock()
		return nil
	}
	w := t.stdin
	t.mu.Unlock()

	start := time.Now()

	// Write NAL in Annex B format: [start code][NAL data]
	if _, err := w.Write(annexBStartCode); err != nil {
		return fmt.Errorf("writing start code: %w", err)
	}
	if _, err := w.Write(nal.Data); err != nil {
		return fmt.Errorf("writing NAL data: %w", err)
	}

	t.lastWrite.Store(int64(timestamp))
	elapsed := time.Since(start)
	stats.TranscodeLatencyMs.Observe(float64(elapsed.Milliseconds()))

	return nil
}

func (t *subprocessTranscoder) SetOutput(sink FrameSink) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.output = sink
}

func (t *subprocessTranscoder) RequestKeyframe() {
	t.forceKF.Store(true)
	stats.KeyframeRequests.Inc()
}

func (t *subprocessTranscoder) SetBitrate(bps int) {
	t.bitrate.Store(int64(bps))
	t.log.Debugw("bitrate update requested", "bps", bps)
}

func (t *subprocessTranscoder) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.closed {
		return nil
	}
	t.closed = true

	if t.stdin != nil {
		t.stdin.Close()
	}

	var err error
	if t.cmd != nil && t.cmd.Process != nil {
		// Give it a moment to flush, then kill
		done := make(chan error, 1)
		go func() { done <- t.cmd.Wait() }()

		select {
		case err = <-done:
		case <-time.After(3 * time.Second):
			t.cmd.Process.Kill()
			err = <-done
		}
	}

	if t.releaseFn != nil {
		t.releaseFn()
	}

	t.log.Infow("transcoder subprocess closed")
	return err
}

// readOutputLoop reads IVF-framed VP8 data from the subprocess stdout.
// IVF format: 32-byte file header, then repeating [12-byte frame header][frame data].
func (t *subprocessTranscoder) readOutputLoop(stderrBuf *bytes.Buffer) {
	defer func() {
		if stderrBuf.Len() > 0 {
			t.log.Debugw("transcoder stderr", "output", stderrBuf.String())
		}
	}()

	r := t.stdout

	// Read and skip 32-byte IVF file header
	fileHeader := make([]byte, 32)
	if _, err := io.ReadFull(r, fileHeader); err != nil {
		if !t.closed {
			t.log.Warnw("failed to read IVF file header", err)
		}
		return
	}

	// Read IVF frames
	frameHeader := make([]byte, 12)
	for {
		_, err := io.ReadFull(r, frameHeader)
		if err != nil {
			if !t.closed {
				t.log.Debugw("IVF frame read ended", "error", err)
			}
			return
		}

		// IVF frame header: bytes 0-3 = frame size (little-endian), bytes 4-11 = timestamp
		frameSize := binary.LittleEndian.Uint32(frameHeader[0:4])
		timestamp := binary.LittleEndian.Uint64(frameHeader[4:12])

		if frameSize == 0 || frameSize > 4*1024*1024 {
			t.log.Warnw("invalid IVF frame size", nil, "size", frameSize)
			continue
		}

		frameData := make([]byte, frameSize)
		if _, err := io.ReadFull(r, frameData); err != nil {
			if !t.closed {
				t.log.Warnw("failed to read IVF frame data", err)
			}
			return
		}

		// VP8 keyframe detection: first byte bit 0 == 0 means keyframe
		keyframe := len(frameData) > 0 && (frameData[0]&0x01) == 0

		t.mu.Lock()
		sink := t.output
		t.mu.Unlock()

		if sink != nil {
			if err := sink.WriteEncodedFrame(frameData, uint32(timestamp), keyframe); err != nil {
				t.log.Debugw("failed to write VP8 frame to sink", "error", err)
			}
		}
	}
}

// gstreamerTranscoder uses GStreamer for H.264 → VP8 transcoding.
// Pipeline: fdsrc ! h264parse ! avdec_h264 ! videoconvert ! vp8enc ! ivfmux ! fdsink
type gstreamerTranscoder struct {
	subprocessTranscoder
}

func newGStreamerTranscoder(log logger.Logger, conf *config.TranscodeConfig, releaseFn func()) *gstreamerTranscoder {
	bitrate := 1500 // kbps default
	if conf.MaxBitrate > 0 {
		bitrate = conf.MaxBitrate
	}

	// Try GPU-accelerated pipeline first
	gpu := DetectGPU(log, conf)
	args := BuildGStreamerGPUArgs(gpu, bitrate)

	if args == nil {
		// Fallback: software pipeline
		args = []string{
			"-q",
			"fdsrc", "fd=0", "!",
			"h264parse", "!",
			"avdec_h264", "!",
			"videoconvert", "!",
			fmt.Sprintf("vp8enc target-bitrate=%d deadline=1 cpu-used=4 threads=2 keyframe-max-dist=60", bitrate*1000),
			"!",
			"ivfmux", "!",
			"fdsink", "fd=1",
		}
		log.Infow("using software GStreamer pipeline")
	} else {
		log.Infow("using GPU-accelerated GStreamer pipeline", "gpu", gpu.Name, "type", string(gpu.Type))
	}

	t := &gstreamerTranscoder{}
	t.log = log
	t.conf = conf
	t.releaseFn = releaseFn
	t.cmd = exec.Command("gst-launch-1.0", args...)
	t.bitrate.Store(int64(bitrate * 1000))

	return t
}

// ffmpegTranscoder uses FFmpeg for H.264 → VP8 transcoding.
// Pipeline: read H.264 Annex B from stdin, output IVF VP8 to stdout.
type ffmpegTranscoder struct {
	subprocessTranscoder
}

func newFFmpegTranscoder(log logger.Logger, conf *config.TranscodeConfig, releaseFn func()) *ffmpegTranscoder {
	bitrate := "1500k"
	if conf.MaxBitrate > 0 {
		bitrate = fmt.Sprintf("%dk", conf.MaxBitrate)
	}

	// Try GPU-accelerated pipeline first
	gpu := DetectGPU(log, conf)
	args := BuildFFmpegGPUArgs(gpu, bitrate)

	if args == nil {
		// Fallback: software pipeline
		args = []string{
			"-hide_banner", "-loglevel", "warning",
			"-f", "h264", "-i", "pipe:0",
			"-c:v", "libvpx",
			"-b:v", bitrate,
			"-deadline", "realtime",
			"-cpu-used", "4",
			"-g", "60",
			"-f", "ivf",
			"pipe:1",
		}
		log.Infow("using software FFmpeg pipeline")
	} else {
		log.Infow("using GPU-accelerated FFmpeg pipeline", "gpu", gpu.Name, "type", string(gpu.Type))
	}

	t := &ffmpegTranscoder{}
	t.log = log
	t.conf = conf
	t.releaseFn = releaseFn
	t.cmd = exec.Command("ffmpeg", args...)

	return t
}
