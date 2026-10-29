// Copyright 2026 LiveKit, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package sip

import (
	"io"
	"slices"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	msdk "github.com/livekit/media-sdk"
	"github.com/livekit/media-sdk/dtmf"
	"github.com/livekit/media-sdk/g711"
	"github.com/livekit/media-sdk/sdp"
	"github.com/livekit/protocol/logger"
	"github.com/pion/rtp"
	"github.com/stretchr/testify/require"
)

// Exercise the real UDP/session/mux pipeline, including the negotiated DTMF
// handler. A telephone-event sequence number must not delay or overwrite audio.
func TestMediaPipelineG711PlayoutReordersAudioAroundDTMF(t *testing.T) {
	for _, jitter := range []bool{false, true} {
		name := "without_jitter"
		if jitter {
			name = "with_jitter"
		}
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				h := newPipelineHarness(t, 8000)
				out := &playoutCodecSink{rate: 8000}
				h.audioIn.Swap(out)
				configurePlayoutPipeline(t, h, g711.ULawSDPNameOnly, jitter)
				var expected msdk.PCM16Sample
				packets := make([][]byte, 4)
				for i := range packets {
					pcm := make(msdk.PCM16Sample, 160)
					for j := range pcm {
						pcm[j] = int16((i + 1) * 2000)
					}
					payload, quantized := playoutCodecEncode(g711.ULawSDPNameAndRate, pcm)
					packets[i] = payload
					expected = append(expected, quantized...)
				}
				send := func(seq uint16, frame int) {
					h.injectRTP(&rtp.Packet{Header: rtp.Header{
						Version: 2, SSRC: 7, PayloadType: h.audio.Type,
						SequenceNumber: seq, Timestamp: 1000 + uint32(frame*160),
					}, Payload: packets[frame]})
					synctest.Wait()
				}
				send(1, 0)
				time.Sleep(20 * time.Millisecond)
				send(4, 2)
				time.Sleep(10 * time.Millisecond)
				send(2, 1)
				var payload [4]byte
				n, err := dtmf.Encode(payload[:], dtmf.Event{Digit: '5', Volume: 10, Dur: 160, End: true})
				require.NoError(t, err)
				digit := &rtp.Packet{Header: rtp.Header{
					Version: 2, SSRC: 7, PayloadType: testDTMFPT,
					SequenceNumber: 3, Timestamp: 1160,
				}, Payload: payload[:n]}
				h.injectRTP(digit)
				h.injectRTP(digit) // Telephone-event retransmission is deduplicated.
				synctest.Wait()
				require.Equal(t, []string{"5"}, h.roomDTMF.snapshot(), "DTMF must not wait for the audio playout deadline")
				require.Empty(t, out.snapshot(), "audio retains its initial playout delay")
				time.Sleep(30 * time.Millisecond)
				send(5, 3)
				time.Sleep(100 * time.Millisecond)
				synctest.Wait()
				var received msdk.PCM16Sample
				for _, frame := range out.snapshot() {
					received = append(received, frame.pcm...)
				}
				require.GreaterOrEqual(t, len(received), len(expected))
				require.Equal(t, expected, received[:len(expected)], "reordered audio must retain its original sample order around DTMF")
			})
		})
	}
}

// Each paced frame keeps its own original RTP arrival time, including time
// spent in the packet jitter buffer and the completed downstream write.
func TestMediaPipelineG711PlayoutInboundLatency(t *testing.T) {
	for _, codec := range []struct{ name, full string }{
		{g711.ULawSDPNameOnly, g711.ULawSDPNameAndRate},
		{g711.ALawSDPNameOnly, g711.ALawSDPNameAndRate},
	} {
		t.Run(codec.name, func(t *testing.T) {
			for _, jitter := range []bool{false, true} {
				name := "without_jitter"
				if jitter {
					name = "with_jitter"
				}
				t.Run(name, func(t *testing.T) {
					synctest.Test(t, func(t *testing.T) {
						h := newPipelineHarness(t, 8000)
						out := &pipelineLatencySink{playoutCodecSink: playoutCodecSink{rate: 8000}, delay: 5 * time.Millisecond}
						h.audioIn.Swap(out)
						configurePlayoutPipeline(t, h, codec.name, jitter)
						epoch := time.Now()
						at := func(d time.Duration) {
							time.Sleep(time.Until(epoch.Add(d)))
							synctest.Wait()
						}
						var expected msdk.PCM16Sample
						packets := make([][]byte, 6)
						for i := range packets {
							pcm := make(msdk.PCM16Sample, 160)
							for j := range pcm {
								pcm[j] = int16((i + 1) * 2000)
							}
							payload, quantized := playoutCodecEncode(codec.full, pcm)
							packets[i] = payload
							expected = append(expected, quantized...)
						}
						sendAudio := func(seq uint16, frame int) {
							h.injectRTP(&rtp.Packet{Header: rtp.Header{
								Version: 2, SSRC: 7, PayloadType: h.audio.Type,
								SequenceNumber: seq, Timestamp: 1000 + uint32(frame*160),
							}, Payload: packets[frame]})
							synctest.Wait()
						}
						sendDigit := func(seq uint16, digit byte, timestamp uint32) {
							var payload [4]byte
							n, err := dtmf.Encode(payload[:], dtmf.Event{Digit: digit, Volume: 10, Dur: 160, End: true})
							require.NoError(t, err)
							h.injectRTP(&rtp.Packet{Header: rtp.Header{
								Version: 2, SSRC: 7, PayloadType: testDTMFPT,
								SequenceNumber: seq, Timestamp: timestamp,
							}, Payload: payload[:n]})
							synctest.Wait()
						}
						sendAudio(1, 0)
						at(20 * time.Millisecond)
						sendAudio(4, 2) // Held by jitter until sequence numbers 2 and 3 arrive.
						at(30 * time.Millisecond)
						sendAudio(2, 1)
						sendDigit(3, '5', 1160)
						at(50 * time.Millisecond)
						sendAudio(5, 3)
						at(55 * time.Millisecond)
						sendDigit(6, '6', 1480)
						at(70 * time.Millisecond)
						sendAudio(7, 4)
						at(75 * time.Millisecond)
						sendDigit(8, '7', 1640)
						at(90 * time.Millisecond)
						sendAudio(9, 5)
						at(95 * time.Millisecond)
						sendDigit(10, '8', 1800)
						at(170 * time.Millisecond)
						frames := out.snapshot()
						require.Len(t, frames, 6)
						var received msdk.PCM16Sample
						for i, frame := range frames {
							received = append(received, frame.pcm...)
							require.Equal(t, time.Duration(65+i*20)*time.Millisecond, frame.at.Sub(epoch))
						}
						require.Equal(t, expected, received)
						stats := &h.conf.stats.LatencyInE2E
						// Original arrivals 0/30/20/50/70/90 ms and write completions
						// 65/85/105/125/145/165 ms give 65/55/85/75/75/75 ms of latency.
						require.Equal(t, uint64(6), stats.Count.Load(), "one end-to-end observation per received audio frame")
						require.Equal(t, uint64(430*time.Millisecond), stats.SumNs.Load())
						require.Equal(t, uint64(85*time.Millisecond), stats.MaxNs.Load(), "must include the reordered packet's pre-jitter arrival and completed downstream write")
						require.Equal(t, []string{"5", "6", "7", "8"}, h.roomDTMF.snapshot())

						beforeSilence := stats.Load()
						at(190 * time.Millisecond)
						sendDigit(11, '9', 2120)
						at(230 * time.Millisecond)
						require.Greater(t, len(out.snapshot()), len(frames), "playout continues generating silence")
						require.Equal(t, beforeSilence, stats.Load(), "generated silence has no RTP arrival and must not inherit a DTMF or previous audio timestamp")
					})
				})
			}
		})
	}
}

type pipelineLatencySink struct {
	playoutCodecSink
	delay time.Duration
}

func (s *pipelineLatencySink) WriteSample(pcm msdk.PCM16Sample) error {
	time.Sleep(s.delay)
	return s.playoutCodecSink.WriteSample(pcm)
}

// Negotiation owns the paced worker and its resampler; the reusable room sink
// belongs to the anchors and must stay open for the next pipeline generation.
func TestMediaPipelineG711PlayoutRebuildPreservesRoomSink(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newPipelineHarness(t, 48000)
		out := &playoutCodecSink{rate: 48000}
		h.audioIn.Swap(out)
		for generation, codec := range []string{g711.ULawSDPNameOnly, g711.ALawSDPNameOnly} {
			if generation > 0 {
				h.port.Reopen()
			}
			configurePlayoutPipeline(t, h, codec, true)
			start := len(out.snapshot())
			for i := range 8 {
				pcm := make(msdk.PCM16Sample, 160)
				for j := range pcm {
					pcm[j] = int16(6000 - generation*12000)
				}
				codecName := g711.ULawSDPNameAndRate
				if generation > 0 {
					codecName = g711.ALawSDPNameAndRate
				}
				payload, _ := playoutCodecEncode(codecName, pcm)
				h.injectRTP(&rtp.Packet{Header: rtp.Header{
					Version: 2, SSRC: uint32(generation + 1), PayloadType: h.audio.Type,
					SequenceNumber: uint16(i + 1), Timestamp: uint32((i + 1) * 160),
				}, Payload: payload})
				time.Sleep(20 * time.Millisecond)
				synctest.Wait()
			}
			time.Sleep(100 * time.Millisecond)
			synctest.Wait()
			frames := out.snapshot()[start:]
			require.NotEmpty(t, frames)
			found := false
			for _, frame := range frames {
				found = found || slices.ContainsFunc(frame.pcm, func(v int16) bool {
					return generation == 0 && v > 1000 || generation == 1 && v < -1000
				})
			}
			require.True(t, found, "new pipeline must deliver the current generation's speech through the real resampler")
			require.NoError(t, h.pipeline.Close())
			closedCount := len(out.snapshot())
			time.Sleep(200 * time.Millisecond)
			synctest.Wait()
			require.Len(t, out.snapshot(), closedCount, "closed pipeline must join its worker and stop writing")
			require.Zero(t, out.closeCount, "pipeline must not close the reusable room sink")
		}
	})
}

func configurePlayoutPipeline(t *testing.T, h *pipelineHarness, name string, jitter bool) {
	t.Helper()
	codec := audioCodecByName(t, name)
	info, create, ok := codec.Supports(msdk.CodecConfig{SampleRate: 8000})
	require.True(t, ok)
	h.codec = create().(msdk.AudioCodec)
	h.audio = &sdp.CodecInfo{Type: testAudioPT(codec), Codec: codec, Info: info}
	h.dtmf = dtmfCodec(nil, testDTMFPT, info.RTPClockRate, 0)
	h.conf.opts = &MediaOptions{EnableJitterBuffer: jitter}
	pipe, err := NewMediaPortPipeline(h.conf, h.mediaConfig(), h.port, h.audioIn, h.dtmfIn, h.audioIn.SampleRate())
	require.NoError(t, err)
	h.pipeline = pipe
}

// A transfer must not close a sink while its asynchronous PCM write is in flight.
// Closing that sink early can make the write fail and permanently stop playout.
func TestMediaPortG711PlayoutTransferWaitsForInFlightWrite(t *testing.T) {
	monitor := newTestCallMonitor(t)
	remote, receiverConn := newUDPPipe()
	defer remote.Close()
	codecs := testCodecSet(g711.ULawSDPNameOnly)
	port, err := NewMediaPortWith(logger.GetLogger(), monitor, receiverConn,
		&MediaOptions{IP: receiverConn.addr.Addr(), Codecs: codecs, EnableJitterBuffer: true}, 8000)
	require.NoError(t, err)
	defer port.Close()
	offer, err := sdp.NewOfferWith(codecs, remote.addr.Addr(), int(remote.addr.Port()), sdp.EncryptionNone)
	require.NoError(t, err)
	offerData, err := offer.SDP.Marshal()
	require.NoError(t, err)
	_, err = port.GenerateAnswer(offerData)
	require.NoError(t, err)
	conf := port.NegotiatedAudio()
	require.NotNil(t, conf)

	old := &mediaPortBlockingPlayoutSink{
		entered: make(chan struct{}), release: make(chan struct{}), closedSignal: make(chan struct{}),
	}
	var releaseOnce sync.Once
	releaseOld := func() { releaseOnce.Do(func() { close(old.release) }) }
	// Always unblock the writer before the deferred port shutdown, including
	// when a test assertion fails.
	defer releaseOld()
	require.Nil(t, port.WriteInboundAudioTo(old))

	epoch := time.Now()
	var seq uint16 = 100
	sendAudio := func(amplitude int16) {
		pcm := make(msdk.PCM16Sample, 160)
		for i := range pcm {
			pcm[i] = amplitude
		}
		var payload g711.ULawSample
		payload.Encode(pcm)
		packet := &rtp.Packet{Header: rtp.Header{
			Version: 2, SSRC: 17, SequenceNumber: seq,
			Timestamp: 10000 + uint32(time.Since(epoch)*8000/time.Second), PayloadType: conf.Type,
		}, Payload: payload}
		seq++
		data, err := packet.Marshal()
		require.NoError(t, err)
		_, err = remote.WriteToUDPAddrPort(data, receiverConn.addr)
		require.NoError(t, err)
	}
	sendAudio(8000)
	select {
	case <-old.entered:
	case <-time.After(time.Second):
		t.Fatal("the real RTP receive path did not reach the old sink")
	}

	replacement := &mediaPortPlayoutTrace{}
	transferStarted, transferred := make(chan struct{}), make(chan struct{})
	go func() {
		close(transferStarted)
		if previous := port.WriteInboundAudioTo(replacement); previous != nil {
			_ = previous.Close()
		}
		close(transferred)
	}()
	<-transferStarted
	// Use a bounded wall-clock wait here: mutex contention is not durably
	// blocked in synctest. Exact media cadence is covered by the test above.
	closedBeforeRelease := false
	select {
	case <-old.closedSignal:
		closedBeforeRelease = true
	case <-time.After(100 * time.Millisecond):
	}
	releaseOld()
	select {
	case <-transferred:
	case <-time.After(time.Second):
		t.Fatal("transfer did not finish after the old write was released")
	}
	require.False(t, closedBeforeRelease, "transfer must wait for the in-flight PCM write before closing its sink")
	old.mu.Lock()
	closedDuringWrite, oldWritesAfterClose := old.closedDuringWrite, old.writesAfterClose
	old.mu.Unlock()
	require.False(t, closedDuringWrite)
	require.Zero(t, oldWritesAfterClose)

	for range 5 {
		sendAudio(-8000)
		time.Sleep(20 * time.Millisecond)
	}
	require.Eventually(t, func() bool {
		frames, _, _ := replacement.snapshot()
		for _, frame := range frames {
			if slices.Min(frame.pcm) < -5000 {
				return true
			}
		}
		return false
	}, time.Second, 10*time.Millisecond, "the replacement sink must receive fresh decoded audio after transfer")

	port.Close()
	frames, closed, writesAfterClose := replacement.snapshot()
	require.True(t, closed)
	require.Zero(t, writesAfterClose)
	countAtClose := len(frames)
	time.Sleep(100 * time.Millisecond)
	frames, _, writesAfterClose = replacement.snapshot()
	require.Len(t, frames, countAtClose, "no PCM write may follow MediaPort.Close")
	require.Zero(t, writesAfterClose)
}

type mediaPortBlockingPlayoutSink struct {
	mu                sync.Mutex
	entered           chan struct{}
	release           chan struct{}
	closedSignal      chan struct{}
	enterOnce         sync.Once
	closeOnce         sync.Once
	writing           bool
	closed            bool
	closedDuringWrite bool
	writesAfterClose  int
}

func (w *mediaPortBlockingPlayoutSink) String() string  { return "blocking MediaPort transfer sink" }
func (w *mediaPortBlockingPlayoutSink) SampleRate() int { return 8000 }
func (w *mediaPortBlockingPlayoutSink) WriteSample(msdk.PCM16Sample) error {
	w.mu.Lock()
	w.writing = true
	w.mu.Unlock()
	w.enterOnce.Do(func() { close(w.entered) })
	<-w.release
	w.mu.Lock()
	defer w.mu.Unlock()
	w.writing = false
	if w.closed {
		w.writesAfterClose++
		return io.ErrClosedPipe
	}
	return nil
}
func (w *mediaPortBlockingPlayoutSink) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.closedDuringWrite = w.closedDuringWrite || w.writing
	w.closed = true
	w.closeOnce.Do(func() { close(w.closedSignal) })
	return nil
}

type mediaPortPlayoutFrame struct {
	at  time.Time
	pcm msdk.PCM16Sample
}

type mediaPortPlayoutTrace struct {
	mu               sync.Mutex
	frames           []mediaPortPlayoutFrame
	closed           bool
	writesAfterClose int
}

func (w *mediaPortPlayoutTrace) String() string  { return "MediaPort playout trace" }
func (w *mediaPortPlayoutTrace) SampleRate() int { return 8000 }
func (w *mediaPortPlayoutTrace) WriteSample(pcm msdk.PCM16Sample) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		w.writesAfterClose++
		return io.ErrClosedPipe
	}
	w.frames = append(w.frames, mediaPortPlayoutFrame{at: time.Now(), pcm: slices.Clone(pcm)})
	return nil
}
func (w *mediaPortPlayoutTrace) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.closed = true
	return nil
}
func (w *mediaPortPlayoutTrace) snapshot() ([]mediaPortPlayoutFrame, bool, int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.frames), w.closed, w.writesAfterClose
}

// Close may still be inside a downstream Close callback when a concurrent
// replacement arrives. The caller must receive that unused replacement back.
func TestMediaPortG711PlayoutRejectsSinkReplacementDuringClose(t *testing.T) {
	remote, receiver := newUDPPipe()
	defer remote.Close()
	port, err := NewMediaPortWith(logger.GetLogger(), newTestCallMonitor(t), receiver,
		&MediaOptions{IP: receiver.addr.Addr()}, 8000)
	require.NoError(t, err)
	defer port.Close()
	old := &playoutClosingSink{entered: make(chan struct{}), release: make(chan struct{})}
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(old.release) }) }
	defer release()
	require.Nil(t, port.WriteInboundAudioTo(old))
	done := make(chan struct{})
	go func() {
		port.Close()
		close(done)
	}()
	select {
	case <-old.entered:
	case <-time.After(time.Second):
		t.Fatal("port did not reach the downstream Close callback")
	}
	replacement := &mediaPortPlayoutTrace{}
	require.Same(t, replacement, port.WriteInboundAudioTo(replacement), "closing port must return the unused replacement to its caller")
	release()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("port.Close did not finish after the downstream Close callback returned")
	}
	_, closed, writes := replacement.snapshot()
	require.False(t, closed, "the rejected sink remains owned by its caller")
	require.Zero(t, writes)
	require.NoError(t, replacement.Close())
}

type playoutClosingSink struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (w *playoutClosingSink) String() string                     { return "closing playout test sink" }
func (w *playoutClosingSink) SampleRate() int                    { return 8000 }
func (w *playoutClosingSink) WriteSample(msdk.PCM16Sample) error { return nil }
func (w *playoutClosingSink) Close() error {
	w.once.Do(func() {
		close(w.entered)
		<-w.release
	})
	return nil
}

// Source selection stays with the existing jitter buffer. Once it accepts a
// source or sequence restart, playout must not impose another idle gate.
func TestMediaPipelineG711PlayoutAcceptsJitterSelectedTimeline(t *testing.T) {
	for _, changeSource := range []bool{false, true} {
		name := "sequence_restart"
		if changeSource {
			name = "ssrc_switch"
		}
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				h := newPipelineHarness(t, 8000)
				out := &playoutCodecSink{rate: 8000}
				h.audioIn.Swap(out)
				configurePlayoutPipeline(t, h, g711.ULawSDPNameOnly, true)
				ssrc, seq, timestamp := uint32(7), uint16(5000), uint32(10000)
				send := func(amplitude int16) {
					pcm := make(msdk.PCM16Sample, 160)
					for i := range pcm {
						pcm[i] = amplitude
					}
					payload, _ := playoutCodecEncode(g711.ULawSDPNameAndRate, pcm)
					h.injectRTP(&rtp.Packet{Header: rtp.Header{
						Version: 2, SSRC: ssrc, PayloadType: h.audio.Type,
						SequenceNumber: seq, Timestamp: timestamp,
					}, Payload: payload})
					seq++
					timestamp += 160
					synctest.Wait()
					time.Sleep(20 * time.Millisecond)
					synctest.Wait()
				}
				for range 8 {
					send(8000)
				}
				if changeSource {
					ssrc++
				} else {
					// Exceed the existing jitter buffer's sequence-line quiet gate.
					time.Sleep(80 * time.Millisecond)
					synctest.Wait()
				}
				seq, timestamp = 1, 160
				restartedAt := time.Now()
				for range 8 {
					send(-8000)
				}
				var recovered time.Time
				for _, frame := range out.snapshot() {
					if !frame.at.Before(restartedAt) && slices.ContainsFunc(frame.pcm, func(v int16) bool { return v < -5000 }) {
						recovered = frame.at
						break
					}
				}
				require.False(t, recovered.IsZero())
				require.LessOrEqual(t, recovered.Sub(restartedAt), 60*time.Millisecond, "jitter-selected timeline must not undergo another probation period")
				if changeSource {
					require.Equal(t, uint64(1), h.conf.stats.JitterBufferSSRCSwitches.Load())
				} else {
					require.Equal(t, uint64(1), h.conf.stats.JitterBufferSequenceRestarts.Load())
				}
			})
		})
	}
}
