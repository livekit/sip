package lksdk

import "github.com/pion/webrtc/v4"

type TrackLocalWithCodec interface {
	webrtc.TrackLocal
	Codec() webrtc.RTPCodecCapability
}

type LocalTrackPublishOptions struct {
	backupCodecTrack TrackLocalWithCodec
	// backup codec tracks for simulcast track
	backupCodecTracks []*LocalTrack
}

type LocalTrackPublishOption func(*LocalTrackPublishOptions)

func WithBackupCodec(backupCodecTrack TrackLocalWithCodec) LocalTrackPublishOption {
	return func(opts *LocalTrackPublishOptions) {
		opts.backupCodecTrack = backupCodecTrack
	}
}

func WithBackupCodecForSimulcastTrack(backupCodecTracks []*LocalTrack) LocalTrackPublishOption {
	return func(opts *LocalTrackPublishOptions) {
		opts.backupCodecTracks = backupCodecTracks
	}
}
