package media

import (
	"errors"
	"fmt"
	"io"

	"github.com/livekit/media-sdk"
	"github.com/livekit/media-sdk/opus"
	"github.com/livekit/media-sdk/rtp"
	protoLogger "github.com/livekit/protocol/logger"
	"github.com/pion/webrtc/v4"
)

type PCMRemoteTrackWriter interface {
	WriteSample(sample media.PCM16Sample) error
	Close() error
}

type internalPCMRemoteTrackWriter struct {
	PCMRemoteTrackWriter
	sampleRate int
}

func (w *internalPCMRemoteTrackWriter) SampleRate() int {
	return w.sampleRate
}

func (w *internalPCMRemoteTrackWriter) String() string {
	return fmt.Sprintf("PCMRemoteTrackWriter(%d)", w.sampleRate)
}

type PCMRemoteTrackParams struct {
	HandleJitter     bool
	TargetSampleRate int
	TargetChannels   int
	Decryptor        Decryptor
	Logger           protoLogger.Logger
}

type PCMRemoteTrackOption func(*PCMRemoteTrackParams)

func WithHandleJitter(handleJitter bool) PCMRemoteTrackOption {
	return func(p *PCMRemoteTrackParams) {
		p.HandleJitter = handleJitter
	}
}

func WithTargetSampleRate(targetSampleRate int) PCMRemoteTrackOption {
	return func(p *PCMRemoteTrackParams) {
		p.TargetSampleRate = targetSampleRate
	}
}

func WithTargetChannels(targetChannels int) PCMRemoteTrackOption {
	return func(p *PCMRemoteTrackParams) {
		p.TargetChannels = targetChannels
	}
}

func WithDecryptor(decryptor Decryptor) PCMRemoteTrackOption {
	return func(p *PCMRemoteTrackParams) {
		p.Decryptor = decryptor
	}
}

// WithLogger sets the logger used by the PCMRemoteTrack. If not set, the
// package-global logger from protoLogger.GetLogger() is used.
func WithLogger(logger protoLogger.Logger) PCMRemoteTrackOption {
	return func(p *PCMRemoteTrackParams) {
		p.Logger = logger
	}
}

type PCMRemoteTrack struct {
	trackRemote *webrtc.TrackRemote
	channels    int
	sampleRate  int
	isResampled bool

	opusWriter         media.WriteCloser[opus.Sample]
	pcmMWriter         media.WriteCloser[media.PCM16Sample]
	resampledPCMWriter media.WriteCloser[media.PCM16Sample]
	logger             protoLogger.Logger

	decryptor Decryptor
}

// PCMRemoteTrack takes a remote track (currently only opus is supported)
// and a WriterCloser interface that writes implements a WriteSample method to write PCM16 samples, where the user desires.
// The PCMRemoteTrack will read RTP packets from the remote track, decode them to PCM16 samples, and write them to the writer.
// Audio is resampled to targetSampleRate and upmixed/downmixed to targetChannels.
// It also provides an option to handle jitter, which is enabled by default.
// Stereo remote tracks are currently not supported, and are known to have a lot of unpleasant noise.
func NewPCMRemoteTrack(track *webrtc.TrackRemote, writer PCMRemoteTrackWriter, opts ...PCMRemoteTrackOption) (*PCMRemoteTrack, error) {
	if track.Codec().MimeType != webrtc.MimeTypeOpus {
		return nil, errors.New("track is not opus")
	}

	options := &PCMRemoteTrackParams{
		HandleJitter:     true,
		TargetSampleRate: DefaultOpusSampleRate,
		TargetChannels:   1,
		Decryptor:        nil,
	}
	for _, opt := range opts {
		opt(options)
	}

	logger := options.Logger
	if logger == nil {
		logger = protoLogger.GetLogger()
	}

	targetChannels := options.TargetChannels
	targetSampleRate := options.TargetSampleRate
	if targetChannels <= 0 || targetChannels > 2 || targetSampleRate <= 0 {
		return nil, errors.New("invalid target channels or sample rate")
	}

	internalWriter := &internalPCMRemoteTrackWriter{
		PCMRemoteTrackWriter: writer,
		sampleRate:           targetSampleRate,
	}

	// resampledPCMWriter resamples the PCM16 samples from DefaultOpusSampleRate to targetSampleRate and
	// writes them to the writer. If no resampling is needed, we directly point resampledPCMWriter to writer.
	var isResampled bool
	var resampledPCMWriter media.WriteCloser[media.PCM16Sample]
	if targetSampleRate != DefaultOpusSampleRate {
		resampledPCMWriter = media.ResampleWriter(internalWriter, DefaultOpusSampleRate)
		isResampled = true
	} else {
		resampledPCMWriter = internalWriter
	}

	// opus writer takes opus samples, decodes them to PCM16 samples
	// and writes them to the pcmMWriter
	opusWriter, err := opus.Decode(resampledPCMWriter, targetChannels, logger)
	if err != nil {
		return nil, err
	}

	// the final chain of writers:
	// trackRemote -> rtp handlers (reads RTP packets from the track) -> opusWriter (decodes opus -> PCM16) -> resampledPCMWriter (resamples to target sample rate as necessary)
	// -> user provided writer (writes the final PCM16 samples)
	t := &PCMRemoteTrack{
		trackRemote:        track,
		opusWriter:         opusWriter,
		pcmMWriter:         internalWriter,
		resampledPCMWriter: resampledPCMWriter,
		sampleRate:         targetSampleRate,
		channels:           targetChannels,
		logger:             logger,
		isResampled:        isResampled,
		decryptor:          options.Decryptor,
	}

	go t.processSamples(options.HandleJitter)
	return t, nil
}

func (t *PCMRemoteTrack) processSamples(handleJitter bool) {
	// Handler takes RTP packets and writes the payload to opusWriter
	var h rtp.Handler = rtp.NewMediaStreamIn[opus.Sample](t.opusWriter)

	if t.decryptor != nil {
		// Ideally, we should check if the track is encrypted with the
		// the encryption type of the decryptor. But, encryption type is
		// found on the RemoteTrackPublication object which we don't have access to here.
		// So, the user is responsible for passing the correct decryptor.
		h = newDecryptionHandler(h, t.decryptor)
	}

	hc := newHandlerCloser(h)
	if handleJitter {
		hc = rtp.HandleJitter(hc)
	}

	// HandleLoop takes RTP packets from the track and writes them to the handler
	// TODO(anunaym14): handle concealment
	err := rtp.HandleLoop(t.trackRemote, hc)
	if err != nil && !errors.Is(err, io.EOF) {
		t.logger.Errorw("error handling rtp from track", err)
	}
}

func (t *PCMRemoteTrack) Close() {
	if t.isResampled {
		t.pcmMWriter.Close()
	}
	// opus writer closes resampledPCMWriter internally
	t.opusWriter.Close()
}

// -----------

type handlerCloser struct {
	h rtp.Handler
}

func newHandlerCloser(h rtp.Handler) rtp.HandlerCloser {
	return handlerCloser{h}
}

func (hc handlerCloser) String() string {
	return hc.h.String()
}

func (hc handlerCloser) HandleRTP(hdr *rtp.Header, payload []byte) error {
	return hc.h.HandleRTP(hdr, payload)
}

func (handlerCloser) Close() {}
