package main

import (
	"errors"

	"go.uber.org/atomic"

	"github.com/livekit/media-sdk"
)

var ErrClosed = errors.New("writer is closed")

type RemoteTrackWriter struct {
	handler *RealtimeAPIHandler
	closed  atomic.Bool
}

func NewRemoteTrackWriter(handler *RealtimeAPIHandler) *RemoteTrackWriter {
	return &RemoteTrackWriter{
		handler: handler,
	}
}

func (w *RemoteTrackWriter) WriteSample(sample media.PCM16Sample) error {
	if w.closed.Load() {
		return ErrClosed
	}

	return w.handler.SendAudioChunk(sample)
}

func (w *RemoteTrackWriter) Close() error {
	w.closed.Swap(true)
	return nil
}
