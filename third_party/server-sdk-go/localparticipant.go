// Copyright 2023 LiveKit, Inc.
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

package lksdk

import (
	"fmt"
	"mime"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/pion/webrtc/v4"
	"google.golang.org/protobuf/proto"

	"github.com/livekit/protocol/livekit"
	protoLogger "github.com/livekit/protocol/logger"
)

const (
	trackPublishTimeout = 10 * time.Second
)

type LocalParticipant struct {
	baseParticipant
	engine                 *RTCEngine
	subscriptionPermission *livekit.SubscriptionPermission
	serverInfo             *livekit.ServerInfo

	rpcPendingAcks      *sync.Map
	rpcPendingResponses *sync.Map
}

func newLocalParticipant(engine *RTCEngine, roomcallback *RoomCallback, serverInfo *livekit.ServerInfo, log protoLogger.Logger) *LocalParticipant {
	return &LocalParticipant{
		baseParticipant:     *newBaseParticipant(roomcallback, log.WithValues("isLocal", true)),
		engine:              engine,
		serverInfo:          serverInfo,
		rpcPendingAcks:      &sync.Map{},
		rpcPendingResponses: &sync.Map{},
	}
}

func (p *LocalParticipant) prepareTrackPublication(track webrtc.TrackLocal, opts *TrackPublicationOptions, pubOpts ...LocalTrackPublishOption) (*LocalTrackPublication, *livekit.AddTrackRequest, error) {
	pubOptions := &LocalTrackPublishOptions{}
	for _, opt := range pubOpts {
		opt(pubOptions)
	}
	if pubOptions.backupCodecTrack != nil {
		if _, ok := track.(TrackLocalWithCodec); !ok {
			return nil, nil, ErrMissingPrimaryCodec
		}
	}

	if opts == nil {
		opts = &TrackPublicationOptions{}
	}
	kind := KindFromRTPType(track.Kind())
	// default sources, since clients generally look for camera/mic
	if opts.Source == livekit.TrackSource_UNKNOWN {
		switch kind {
		case TrackKindVideo:
			opts.Source = livekit.TrackSource_CAMERA
		case TrackKindAudio:
			opts.Source = livekit.TrackSource_MICROPHONE
		}
	}

	transport := p.getPublishTransport()
	if transport == nil {
		return nil, nil, ErrNoPeerConnection
	}

	pub := NewLocalTrackPublication(kind, track, *opts, p.engine, p.log)
	pub.onMuteChanged = p.onTrackMuted

	// add transceivers - re-use if possible, AddTrack will try to re-use.
	// NOTE: `AddTrack` technically cannot re-use transceiver if it was ever
	// used to send media, i. e. if it was ever in a `sendrecv` or `sendonly`
	// direction. But, pion does not enforce that based on browser behaviour
	// observed in practice.
	sender, err := transport.PeerConnection().AddTrack(track)
	if err != nil {
		return nil, nil, err
	}

	// LocalTrack will consume rtcp packets so we don't need to consume again
	lt, isSampleTrack := track.(*LocalTrack)
	var primaryCodec webrtc.RTPCodecCapability
	if isSampleTrack {
		primaryCodec = lt.Codec()
	} else {
		if _, ok := track.(TrackLocalWithCodec); ok {
			primaryCodec = track.(TrackLocalWithCodec).Codec()
		}
		pub.readRTCP(sender)
	}
	if primaryCodec.MimeType != "" {
		for _, tr := range transport.PeerConnection().GetTransceivers() {
			if tr.Sender() == sender {
				codecs := append([]webrtc.RTPCodecParameters{}, sender.GetParameters().Codecs...)
				for i, c := range codecs {
					if strings.EqualFold(c.MimeType, primaryCodec.MimeType) {
						codecs[0], codecs[i] = codecs[i], codecs[0]
						break
					}
				}
				tr.SetCodecPreferences(codecs)
			}
		}
	}

	req := &livekit.AddTrackRequest{
		Cid:                   track.ID(),
		Name:                  opts.Name,
		Source:                opts.Source,
		Type:                  kind.ProtoType(),
		Width:                 uint32(opts.VideoWidth),
		Height:                uint32(opts.VideoHeight),
		DisableDtx:            opts.DisableDTX,
		Stereo:                opts.Stereo,
		Stream:                opts.Stream,
		Encryption:            opts.Encryption,
		BackupCodecPolicy:     opts.BackupCodecPolicy,
		PacketTrailerFeatures: packetTrailerFeaturesFromOpts(opts),
	}
	if kind == TrackKindVideo {
		// single layer
		req.Layers = []*livekit.VideoLayer{
			{
				Quality: livekit.VideoQuality_HIGH,
				Width:   uint32(opts.VideoWidth),
				Height:  uint32(opts.VideoHeight),
			},
		}
	}

	// TODO: support e2ee for backup codecs
	if pubOptions.backupCodecTrack != nil {
		if req.Encryption == livekit.Encryption_NONE {
			req.SimulcastCodecs = []*livekit.SimulcastCodec{
				{
					Codec: primaryCodec.MimeType,
					Cid:   track.ID(),
				},
				{
					Codec: pubOptions.backupCodecTrack.Codec().MimeType,
					Cid:   pubOptions.backupCodecTrack.ID(),
				},
			}
			pub.setBackupCodecTrack(pubOptions.backupCodecTrack)
		} else {
			p.log.Warnw("backup codec publication with encryption is not supported, ignoring backup codec", nil)
		}
	}
	return pub, req, nil
}

func (p *LocalParticipant) notifyTrackPublished(pub *LocalTrackPublication) {
	p.Callback.OnLocalTrackPublished(pub, p)
	p.roomCallback.OnLocalTrackPublished(pub, p)
}

// PublishTrack publishes a local track to the room.
// The track will be available to other participants in the room.
func (p *LocalParticipant) PublishTrack(track webrtc.TrackLocal, opts *TrackPublicationOptions, pubOpts ...LocalTrackPublishOption) (*LocalTrackPublication, error) {
	transport := p.getPublishTransport()
	if transport == nil {
		return nil, ErrNoPeerConnection
	}

	pub, req, err := p.prepareTrackPublication(track, opts, pubOpts...)
	if err != nil {
		return nil, err
	}

	pubChan := make(chan *livekit.TrackPublishedResponse, 1)
	p.engine.RegisterTrackPublishedListener(track.ID(), pubChan)
	defer p.engine.UnregisterTrackPublishedListener(track.ID())

	if err := p.engine.SendAddTrack(req); err != nil {
		return nil, err
	}

	transport.Negotiate()

	var pubRes *livekit.TrackPublishedResponse
	select {
	case pubRes = <-pubChan:
		break
	case <-time.After(trackPublishTimeout):
		return nil, ErrTrackPublishTimeout
	}

	pub.updateInfo(pubRes.Track)
	p.addPublication(pub)

	p.notifyTrackPublished(pub)
	p.engine.log.Infow("published track", "name", opts.Name, "source", opts.Source.String(), "trackID", pubRes.Track.Sid)
	return pub, nil
}

func (p *LocalParticipant) prepareSimulcastTrackPublication(tracks []*LocalTrack, opts *TrackPublicationOptions, pubOpts ...LocalTrackPublishOption) (*LocalTrackPublication, *livekit.AddTrackRequest, *LocalTrack, error) {
	if len(tracks) == 0 {
		return nil, nil, nil, ErrInvalidSimulcastTrack
	}

	for _, track := range tracks {
		if track.Kind() != webrtc.RTPCodecTypeVideo {
			return nil, nil, nil, ErrUnsupportedSimulcastKind
		}
		if track.videoLayer == nil || track.RID() == "" {
			return nil, nil, nil, ErrInvalidSimulcastTrack
		}

		// disable dynacast on a re-publication
		track.setDisabled(false)
	}

	pubOptions := &LocalTrackPublishOptions{}
	for _, opt := range pubOpts {
		opt(pubOptions)
	}

	tracksCopy := make([]*LocalTrack, len(tracks))
	copy(tracksCopy, tracks)

	// tracks should be low to high
	sort.Slice(tracksCopy, func(i, j int) bool {
		return tracksCopy[i].videoLayer.Width < tracksCopy[j].videoLayer.Width
	})

	if opts == nil {
		opts = &TrackPublicationOptions{}
	}
	// default sources, since clients generally look for camera/mic
	if opts.Source == livekit.TrackSource_UNKNOWN {
		opts.Source = livekit.TrackSource_CAMERA
	}

	mainTrack := tracksCopy[len(tracksCopy)-1]

	pub := NewLocalTrackPublication(KindFromRTPType(mainTrack.Kind()), nil, *opts, p.engine, p.log)
	pub.onMuteChanged = p.onTrackMuted

	transport := p.getPublishTransport()
	if transport == nil {
		return nil, nil, nil, ErrNoPeerConnection
	}

	// add transceivers
	var (
		transceiver *webrtc.RTPTransceiver
		sender      *webrtc.RTPSender
		err         error
	)
	pc := transport.PeerConnection()
	for idx, st := range tracksCopy {
		if idx == 0 {
			// add transceivers - re-use if possible, AddTrack will try to re-use.
			// NOTE: `AddTrack` technically cannot re-use transceiver if it was ever
			// used to send media, i. e. if it was ever in a `sendrecv` or `sendonly`
			// direction. But, pion does not enforce that based on browser behaviour
			// observed in practice.
			sender, err = pc.AddTrack(st)
			if err != nil {
				return nil, nil, nil, err
			}

			// as there is no way to get transceiver from sender, search
			for _, tr := range pc.GetTransceivers() {
				if tr.Sender() == sender {
					transceiver = tr
					break
				}
			}
		} else {
			if err = sender.AddEncoding(st); err != nil {
				return nil, nil, nil, err
			}
		}
		pub.addSimulcastTrack(st)
		st.SetTransceiver(transceiver)
	}

	var layers []*livekit.VideoLayer
	for _, st := range tracksCopy {
		layers = append(layers, st.videoLayer)
	}
	req := &livekit.AddTrackRequest{
		Cid:                   mainTrack.ID(),
		Name:                  opts.Name,
		Source:                opts.Source,
		Type:                  pub.Kind().ProtoType(),
		Width:                 mainTrack.videoLayer.Width,
		Height:                mainTrack.videoLayer.Height,
		Layers:                layers,
		PacketTrailerFeatures: packetTrailerFeaturesFromOpts(opts),
		SimulcastCodecs: []*livekit.SimulcastCodec{
			{
				Codec:          mainTrack.Codec().MimeType,
				Cid:            mainTrack.ID(),
				Layers:         layers,
				VideoLayerMode: livekit.VideoLayer_ONE_SPATIAL_LAYER_PER_STREAM,
			},
		},
	}
	if len(pubOptions.backupCodecTracks) > 0 {
		for _, track := range pubOptions.backupCodecTracks {
			// disable dynacast on a re-publication
			track.setDisabled(false)
		}

		backupTracksCopy := make([]*LocalTrack, len(pubOptions.backupCodecTracks))
		copy(backupTracksCopy, pubOptions.backupCodecTracks)

		// tracks should be low to high
		sort.Slice(backupTracksCopy, func(i, j int) bool {
			return backupTracksCopy[i].videoLayer.Width < backupTracksCopy[j].videoLayer.Width
		})

		var backupLayers []*livekit.VideoLayer
		for _, st := range backupTracksCopy {
			backupLayers = append(backupLayers, st.videoLayer)
		}

		backupTrackMain := backupTracksCopy[len(backupTracksCopy)-1]
		req.SimulcastCodecs = append(req.SimulcastCodecs, &livekit.SimulcastCodec{
			Codec:          backupTrackMain.Codec().MimeType,
			Cid:            backupTrackMain.ID(),
			Layers:         backupLayers,
			VideoLayerMode: livekit.VideoLayer_ONE_SPATIAL_LAYER_PER_STREAM,
		})

		pub.setBackupCodecTracksForSimulcast(backupTracksCopy)
	}
	return pub, req, mainTrack, nil
}

// PublishSimulcastTrack publishes a simulcast track with up to three quality layers to the server.
// This allows the server to dynamically switch between different quality levels based on network conditions.
func (p *LocalParticipant) PublishSimulcastTrack(tracks []*LocalTrack, opts *TrackPublicationOptions, pubOpts ...LocalTrackPublishOption) (*LocalTrackPublication, error) {
	if len(tracks) == 0 {
		return nil, nil
	}

	transport := p.getPublishTransport()
	if transport == nil {
		return nil, ErrNoPeerConnection
	}

	pub, req, mainTrack, err := p.prepareSimulcastTrackPublication(tracks, opts, pubOpts...)
	if err != nil {
		return nil, err
	}

	pubChan := make(chan *livekit.TrackPublishedResponse, 1)
	p.engine.RegisterTrackPublishedListener(mainTrack.ID(), pubChan)
	defer p.engine.UnregisterTrackPublishedListener(mainTrack.ID())

	if err := p.engine.SendAddTrack(req); err != nil {
		return nil, err
	}

	var pubRes *livekit.TrackPublishedResponse
	select {
	case pubRes = <-pubChan:
		break
	case <-time.After(trackPublishTimeout):
		return nil, ErrTrackPublishTimeout
	}

	pub.updateInfo(pubRes.Track)
	p.addPublication(pub)

	transport.Negotiate()

	p.notifyTrackPublished(pub)

	p.engine.log.Infow("published simulcast track", "name", opts.Name, "source", opts.Source.String(), "trackID", pubRes.Track.Sid)

	return pub, nil
}

func (p *LocalParticipant) handleSubscribedQualityUpdate(subscribedQualityUpdate *livekit.SubscribedQualityUpdate) {
	trackPublication := p.getLocalPublication(subscribedQualityUpdate.TrackSid)
	if trackPublication == nil {
		p.log.Infow("recieved subscribed quality update for unknown track", "trackID", subscribedQualityUpdate.TrackSid)
		return
	}

	p.log.Infow(
		"handling subscribed quality update",
		"trackID", trackPublication.SID(),
		"mime", trackPublication.MimeType(),
		"subscribedQualityUpdate", protoLogger.Proto(subscribedQualityUpdate),
	)
	newCodecs := trackPublication.setPublishingCodecsQuality(subscribedQualityUpdate.SubscribedCodecs)
	if len(newCodecs) > 0 {
		go func() {
			for _, codec := range newCodecs {
				p.log.Infow("publishing backup codec from subscribed quality update", "trackID", trackPublication.SID(), "codec", codec)
				if err := p.publishAdditionalCodecForTrack(trackPublication, codec); err != nil {
					p.log.Warnw("failed to publish backup codec", err, "trackID", trackPublication.SID(), "codec", codec)
				}
			}
		}()
	}
}

func (p *LocalParticipant) handleSubscribedAudioCodecUpdate(subscribedAudioCodecUpdate *livekit.SubscribedAudioCodecUpdate) {
	trackPublication := p.getLocalPublication(subscribedAudioCodecUpdate.TrackSid)
	if trackPublication == nil {
		p.log.Debugw("recieved subscribed audio codec update for unknown track", "trackID", subscribedAudioCodecUpdate.TrackSid)
		return
	}

	p.log.Infow(
		"handling subscribed audio codec update",
		"trackID", trackPublication.SID(),
		"mime", trackPublication.MimeType(),
		"subscribedAudioCodecUpdate", protoLogger.Proto(subscribedAudioCodecUpdate),
	)
	newCodecs := trackPublication.setAudioCodecSubscribed(subscribedAudioCodecUpdate.SubscribedAudioCodecs)
	if len(newCodecs) > 0 {
		go func() {
			for _, codec := range newCodecs {
				p.log.Infow("publishing backup codec from subscribed audio codec update", "trackID", trackPublication.SID(), "codec", codec)
				if err := p.publishAdditionalCodecForTrack(trackPublication, codec); err != nil {
					p.log.Warnw("failed to publish backup codec", err, "trackID", trackPublication.SID(), "codec", codec)
				}
			}
		}()
	}
}

func (p *LocalParticipant) publishAdditionalCodecForTrack(trackPublication *LocalTrackPublication, codec string) error {
	track, tracks := trackPublication.getBackupCodecTrack()
	if track == nil && len(tracks) == 0 {
		return fmt.Errorf("%w: required codec %s", ErrCannotFindTrack, codec)
	}

	transport := p.getPublishTransport()
	if transport == nil {
		return ErrNoPeerConnection
	}

	mainTrack := track
	if track == nil {
		sort.Slice(tracks, func(i, j int) bool {
			return tracks[i].videoLayer.Width < tracks[j].videoLayer.Width
		})
		mainTrack = tracks[len(tracks)-1]
	}

	if !strings.Contains(strings.ToLower(mainTrack.Codec().MimeType), strings.ToLower(codec)) {
		return fmt.Errorf("%w: required codec %s, available codec %s", ErrCannotFindTrack, codec, mainTrack.Codec().MimeType)
	}

	p.log.Debugw("publishing additional codec for track", "cID", mainTrack.ID(), "codec", codec, "trackID", trackPublication.SID())

	pubChan := make(chan *livekit.TrackPublishedResponse, 1)
	p.engine.RegisterTrackPublishedListener(mainTrack.ID(), pubChan)
	defer p.engine.UnregisterTrackPublishedListener(mainTrack.ID())

	pc := transport.PeerConnection()
	if track != nil {
		sender, err := pc.AddTrack(track)
		if err != nil {
			return err
		}

		for _, tr := range pc.GetTransceivers() {
			if tr.Sender() == sender {
				codecs := append([]webrtc.RTPCodecParameters{}, sender.GetParameters().Codecs...)
				for i, c := range codecs {
					if strings.EqualFold(c.MimeType, mainTrack.Codec().MimeType) {
						codecs[0], codecs[i] = codecs[i], codecs[0]
						break
					}
				}
				tr.SetCodecPreferences(codecs)
			}
		}
	} else { // simulcast backup tracks
		var (
			transceiver *webrtc.RTPTransceiver
			sender      *webrtc.RTPSender
			err         error
		)
		for idx, st := range tracks {
			if idx == 0 {
				sender, err = pc.AddTrack(st)
				if err != nil {
					return err
				}

				for _, tr := range pc.GetTransceivers() {
					if tr.Sender() == sender {
						transceiver = tr
						break
					}
				}
			} else {
				if err = sender.AddEncoding(st); err != nil {
					return err
				}
			}
			st.SetTransceiver(transceiver)
		}
	}

	req := &livekit.AddTrackRequest{
		Cid: mainTrack.ID(),
		Sid: trackPublication.SID(),
		SimulcastCodecs: []*livekit.SimulcastCodec{
			{
				Codec: codec,
				Cid:   mainTrack.ID(),
			},
		},
	}
	if err := p.engine.SendAddTrack(req); err != nil {
		return err
	}
	trackPublication.setBackupCodecPublished()

	transport.Negotiate()

	select {
	case <-pubChan:
		break
	case <-time.After(trackPublishTimeout):
		return ErrTrackPublishTimeout
	}

	p.log.Infow("published additional codec for track", "trackID", trackPublication.SID(), "codec", codec)
	return nil
}

func (p *LocalParticipant) republishTracks() {
	var localPubs []*LocalTrackPublication
	p.tracks.Range(func(key, value interface{}) bool {
		track := value.(*LocalTrackPublication)

		if track.Track() != nil || len(track.simulcastTracks) > 0 {
			localPubs = append(localPubs, track)
		}
		p.tracks.Delete(key)
		p.audioTracks.Delete(key)
		p.videoTracks.Delete(key)

		p.Callback.OnLocalTrackUnpublished(track, p)
		p.roomCallback.OnLocalTrackUnpublished(track, p)
		return true
	})

	for _, pub := range localPubs {
		opt := pub.PublicationOptions()
		backupCodecTrack, backupCodecTracksForSimulcast := pub.getBackupCodecTrack()
		if tracks := pub.TrackLocalForSimulcast(); len(tracks) > 0 {
			p.PublishSimulcastTrack(tracks, &opt, WithBackupCodecForSimulcastTrack(backupCodecTracksForSimulcast))
		} else if track := pub.TrackLocal(); track != nil {
			_, err := p.PublishTrack(track, &opt, WithBackupCodec(backupCodecTrack))
			if err != nil {
				p.engine.log.Warnw("could not republish track", err, "track", pub.SID())
			}
		} else {
			p.engine.log.Warnw("could not republish track as no track local found", nil, "track", pub.SID())
		}
	}
}

func (p *LocalParticipant) closeTracks() {
	var localPubs []*LocalTrackPublication
	p.tracks.Range(func(key, value interface{}) bool {
		track := value.(*LocalTrackPublication)
		if track.Track() != nil || len(track.simulcastTracks) > 0 {
			localPubs = append(localPubs, track)
		}
		p.tracks.Delete(key)
		p.audioTracks.Delete(key)
		p.videoTracks.Delete(key)
		return true
	})

	for _, pub := range localPubs {
		pub.CloseTrack()
	}
}

// PublishData sends custom user data via WebRTC data channel.
//
// By default, the message can be received by all participants in a room,
// see WithDataPublishDestination for choosing specific participants.
//
// Messages are sent via a LOSSY channel by default, see WithDataPublishReliable for sending reliable data.
//
// Deprecated: Use PublishDataPacket with UserData instead.
func (p *LocalParticipant) PublishData(payload []byte, opts ...DataPublishOption) error {
	options := &dataPublishOptions{}
	for _, opt := range opts {
		opt(options)
	}
	return p.PublishDataPacket(UserData(payload), opts...)
}

// PublishDataPacket sends a packet via a WebRTC data channel. UserData can be used for sending custom user data.
//
// By default, the message can be received by all participants in a room,
// see WithDataPublishDestination for choosing specific participants.
//
// Messages are sent via UDP and offer no delivery guarantees, see WithDataPublishReliable for sending data reliably (with retries).
func (p *LocalParticipant) PublishDataPacket(pck DataPacket, opts ...DataPublishOption) error {
	options := &dataPublishOptions{}
	for _, opt := range opts {
		opt(options)
	}
	dataPacket := pck.ToProto()
	if options.Topic != "" {
		if u, ok := dataPacket.Value.(*livekit.DataPacket_User); ok && u.User != nil {
			u.User.Topic = proto.String(options.Topic)
		}
	}

	// This matches the default value of Kind on protobuf level.
	kind := livekit.DataPacket_LOSSY
	if options.Reliable != nil && *options.Reliable {
		kind = livekit.DataPacket_RELIABLE
	}

	dataPacket.DestinationIdentities = options.DestinationIdentities
	if u, ok := dataPacket.Value.(*livekit.DataPacket_User); ok && u.User != nil {
		//lint:ignore SA1019 backward compatibility
		u.User.DestinationIdentities = options.DestinationIdentities
	}

	return p.engine.publishDataPacket(dataPacket, kind)
}

// UnpublishTrack stops publishing a track and removes it from the room.
func (p *LocalParticipant) UnpublishTrack(sid string) error {
	obj, loaded := p.tracks.LoadAndDelete(sid)
	if !loaded {
		return ErrCannotFindTrack
	}
	p.audioTracks.Delete(sid)
	p.videoTracks.Delete(sid)

	pub, ok := obj.(*LocalTrackPublication)
	if !ok {
		return nil
	}

	transport := p.getPublishTransport()
	if transport == nil {
		return ErrNoPeerConnection
	}
	err := pub.unpublish(transport)
	transport.Negotiate()

	pub.CloseTrack()

	p.Callback.OnLocalTrackUnpublished(pub, p)
	p.roomCallback.OnLocalTrackUnpublished(pub, p)

	p.engine.log.Infow("unpublished track", "name", pub.Name(), "trackID", sid)

	return err
}

// GetSubscriberPeerConnection is a power-user API that gives access to the underlying subscriber peer connection.
// Subscribed tracks are received using this PeerConnection.
func (p *LocalParticipant) GetSubscriberPeerConnection() *webrtc.PeerConnection {
	if subscriber, ok := p.engine.Subscriber(); ok {
		return subscriber.PeerConnection()
	}
	return nil
}

// GetPublisherPeerConnection is a power-user API that gives access to the underlying publisher peer connection.
// Local tracks are published to server via this PeerConnection.
func (p *LocalParticipant) GetPublisherPeerConnection() *webrtc.PeerConnection {
	if publisher, ok := p.engine.Publisher(); ok {
		return publisher.PeerConnection()
	}
	return nil
}

// SetName sets the name of the current participant.
// Updates will be performed only if the participant has canUpdateOwnMetadata grant.
func (p *LocalParticipant) SetName(name string) {
	_ = p.engine.SendUpdateParticipantMetadata(&livekit.UpdateParticipantMetadata{
		Name: name,
	})
}

// SetMetadata sets the metadata of the current participant.
// Updates will be performed only if the participant has canUpdateOwnMetadata grant.
func (p *LocalParticipant) SetMetadata(metadata string) {
	_ = p.engine.SendUpdateParticipantMetadata(&livekit.UpdateParticipantMetadata{
		Metadata: metadata,
	})
}

// SetAttributes sets the KV attributes of the current participant.
// To remove an attribute, set it to empty value.
// Updates will be performed only if the participant has canUpdateOwnMetadata grant.
func (p *LocalParticipant) SetAttributes(attrs map[string]string) {
	_ = p.engine.SendUpdateParticipantMetadata(&livekit.UpdateParticipantMetadata{
		Attributes: attrs,
	})
}

func (p *LocalParticipant) updateInfo(info *livekit.ParticipantInfo) {
	p.baseParticipant.updateInfo(info, p)

	// detect tracks that have been muted remotely, and apply changes
	for _, ti := range info.Tracks {
		pub := p.getLocalPublication(ti.Sid)
		if pub == nil {
			continue
		}
		if pub.IsMuted() != ti.Muted {
			_ = p.engine.SendMuteTrack(pub.SID(), pub.IsMuted())
		}
	}
}

func (p *LocalParticipant) getLocalPublication(sid string) *LocalTrackPublication {
	if pub, ok := p.getPublication(sid).(*LocalTrackPublication); ok {
		return pub
	}
	return nil
}

func (p *LocalParticipant) onTrackMuted(pub *LocalTrackPublication, muted bool) {
	if muted {
		p.Callback.OnTrackMuted(pub, p)
		p.roomCallback.OnTrackMuted(pub, p)
	} else {
		p.Callback.OnTrackUnmuted(pub, p)
		p.roomCallback.OnTrackUnmuted(pub, p)
	}
}

// SetSubscriptionPermission controls who can subscribe to LocalParticipant's published tracks.
//
// By default, all participants can subscribe. This allows fine-grained control over
// who is able to subscribe at a participant and track level.
//
// Note: if access is given at a track-level (i.e. both `AllParticipants` and
// `TrackPermission.AllTracks` are false), any newer published tracks
// will not grant permissions to any participants and will require a subsequent
// permissions update to allow subscription.
func (p *LocalParticipant) SetSubscriptionPermission(sp *livekit.SubscriptionPermission) {
	p.lock.Lock()
	p.subscriptionPermission = proto.Clone(sp).(*livekit.SubscriptionPermission)
	p.updateSubscriptionPermissionLocked()
	p.lock.Unlock()
}

func (p *LocalParticipant) updateSubscriptionPermission() {
	p.lock.RLock()
	defer p.lock.RUnlock()

	p.updateSubscriptionPermissionLocked()
}

func (p *LocalParticipant) updateSubscriptionPermissionLocked() {
	if p.subscriptionPermission == nil {
		return
	}

	if err := p.engine.SendSubscriptionPermission(p.subscriptionPermission); err != nil {
		logger.Errorw(
			"could not send subscription permission", err,
			"participant", p.identity,
			"participantID", p.sid,
		)
	}
}

func (p *LocalParticipant) handleParticipantDisconnected(identity string) {
	p.rpcPendingAcks.Range(func(key, value interface{}) bool {
		if value.(rpcPendingAckHandler).participantIdentity == identity {
			p.rpcPendingAcks.Delete(key)
		}
		return true
	})

	p.rpcPendingResponses.Range(func(key, value interface{}) bool {
		if value.(rpcPendingResponseHandler).participantIdentity == identity {
			value, ok := p.rpcPendingResponses.LoadAndDelete(key)
			if ok {
				value.(rpcPendingResponseHandler).resolve(nil, rpcErrorFromBuiltInCodes(RpcRecipientDisconnected, nil))
			}
		}
		return true
	})
}

func (p *LocalParticipant) HandleIncomingRpcAck(requestId string) {
	handler, ok := p.rpcPendingAcks.Load(requestId)
	if !ok {
		p.engine.log.Errorw("ack received for unexpected RPC request", nil, "requestId", requestId)
	} else {
		handler.(rpcPendingAckHandler).resolve()
		p.rpcPendingAcks.Delete(requestId)
	}
}

func (p *LocalParticipant) HandleIncomingRpcResponse(requestId string, payload *string, error *RpcError) {
	handler, ok := p.rpcPendingResponses.Load(requestId)
	if !ok {
		p.engine.log.Errorw("response received for unexpected RPC request", nil, "requestId", requestId)
	} else {
		handler.(rpcPendingResponseHandler).resolve(payload, error)
		p.rpcPendingResponses.Delete(requestId)
	}
}

// PerformRpc initiates an RPC call to a remote participant.
// Returns the response payload or an error if the call fails or times out.
func (p *LocalParticipant) PerformRpc(params PerformRpcParams) (*string, error) {
	responseTimeout := 15000 * time.Millisecond
	if params.ResponseTimeout != nil {
		responseTimeout = *params.ResponseTimeout
	}

	resultChan := make(chan *string, 1)
	errorChan := make(chan error, 1)

	maxRoundTripLatency := 7000 * time.Millisecond
	minEffectiveResponseTimeout := 1 * time.Second

	go func() {
		if byteLength(params.Payload) > MaxPayloadBytes {
			errorChan <- rpcErrorFromBuiltInCodes(RpcRequestPayloadTooLarge, nil)
			return
		}

		if p.serverInfo != nil && compareVersions(p.serverInfo.Version, "1.8.0") < 0 {
			errorChan <- rpcErrorFromBuiltInCodes(RpcUnsupportedServer, nil)
			return
		}

		effectiveResponseTimeout := responseTimeout - maxRoundTripLatency
		if effectiveResponseTimeout < minEffectiveResponseTimeout {
			effectiveResponseTimeout = minEffectiveResponseTimeout
		}
		id := uuid.New().String()
		p.engine.publishRpcRequest(params.DestinationIdentity, id, params.Method, params.Payload, effectiveResponseTimeout)

		// Client-side timers:
		// - responseTimer: total time client is willing to wait for a response.
		// - ackTimer: time allowed for initial ACK/round-trip.
		responseTimer := time.AfterFunc(responseTimeout, func() {
			p.rpcPendingResponses.Delete(id)

			select {
			case errorChan <- rpcErrorFromBuiltInCodes(RpcResponseTimeout, nil):
			default:
			}
		})

		ackTimer := time.AfterFunc(maxRoundTripLatency, func() {
			p.rpcPendingAcks.Delete(id)
			p.rpcPendingResponses.Delete(id)
			responseTimer.Stop()

			select {
			case errorChan <- rpcErrorFromBuiltInCodes(RpcConnectionTimeout, nil):
			default:
			}
		})

		p.rpcPendingAcks.Store(id, rpcPendingAckHandler{
			resolve: func() {
				ackTimer.Stop()
			},
			participantIdentity: params.DestinationIdentity,
		})

		p.rpcPendingResponses.Store(id, rpcPendingResponseHandler{
			resolve: func(payload *string, error *RpcError) {
				responseTimer.Stop()
				if _, ok := p.rpcPendingAcks.Load(id); ok {
					p.engine.log.Warnw("RPC response received before ack", nil, "requestId", id)
					p.rpcPendingAcks.Delete(id)
					ackTimer.Stop()
				}

				if error != nil {
					errorChan <- error
				} else {
					if payload != nil {
						resultChan <- payload
					} else {
						emptyStr := ""
						resultChan <- &emptyStr
					}
				}
			},
			participantIdentity: params.DestinationIdentity,
		})
	}()

	select {
	case result := <-resultChan:
		return result, nil
	case err := <-errorChan:
		return nil, err
	}
}

func (p *LocalParticipant) cleanup() {
	p.rpcPendingAcks.Clear()
	p.rpcPendingResponses.Clear()
}

// StreamText creates a new text stream writer with the provided options.
func (p *LocalParticipant) StreamText(options StreamTextOptions) *TextStreamWriter {
	if options.StreamId == nil {
		streamId := uuid.New().String()
		options.StreamId = &streamId
	}

	if options.Attributes == nil {
		options.Attributes = make(map[string]string)
	}

	var totalSize *uint64
	if options.TotalSize != 0 {
		totalSize = &options.TotalSize
	}

	info := TextStreamInfo{
		baseStreamInfo: &baseStreamInfo{
			Id:         *options.StreamId,
			MimeType:   "text/plain",
			Topic:      options.Topic,
			Timestamp:  time.Now().UnixMilli(),
			Size:       totalSize,
			Attributes: options.Attributes,
		},
	}

	header := &livekit.DataStream_Header{
		StreamId:    info.Id,
		MimeType:    info.MimeType,
		Topic:       info.Topic,
		Timestamp:   info.Timestamp,
		TotalLength: info.Size,
		Attributes:  info.Attributes,
		ContentHeader: &livekit.DataStream_Header_TextHeader{
			TextHeader: &livekit.DataStream_TextHeader{
				OperationType:     livekit.DataStream_CREATE,
				AttachedStreamIds: options.AttachedStreamIds,
			},
		},
	}
	if options.ReplyToStreamId != nil {
		if textHeader, ok := header.ContentHeader.(*livekit.DataStream_Header_TextHeader); ok {
			textHeader.TextHeader.ReplyToStreamId = *options.ReplyToStreamId
		}
	}

	writer := newTextStreamWriter(info, header, p.engine, options.DestinationIdentities, options.OnProgress)

	p.engine.OnClose(func() {
		writer.Close()
	})

	return writer
}

// SendText sends a text message as a stream to other participants.
// Returns TextStreamInfo that can be used to get metadata about the stream.
func (p *LocalParticipant) SendText(text string, options StreamTextOptions) *TextStreamInfo {
	if options.TotalSize == 0 {
		textInBytes := []byte(text)
		options.TotalSize = uint64(len(textInBytes))
	}

	// Ensure that the number of attached stream ids matches the number of attachments, generate if necessary
	attachedStreamIds := options.AttachedStreamIds
	numberOfAttachments := len(options.Attachments)
	numberOfAttachedStreamIds := len(attachedStreamIds)
	if numberOfAttachments > 0 {
		if numberOfAttachedStreamIds != numberOfAttachments {
			for i := numberOfAttachedStreamIds; i < numberOfAttachments; i++ {
				attachedStreamIds = append(attachedStreamIds, uuid.New().String())
			}
		}
	}
	options.AttachedStreamIds = attachedStreamIds

	var progresses sync.Map
	for i := range numberOfAttachments + 1 {
		progresses.Store(i, float64(0))
	}

	handleProgress := func(progress float64, id int) {
		progresses.Store(id, progress)

		var totalProgress float64
		progresses.Range(func(_, value interface{}) bool {
			totalProgress += value.(float64)
			return true
		})

		if options.OnProgress != nil {
			options.OnProgress(totalProgress / float64(numberOfAttachments+1))
		}
	}

	textOptions := options
	textOnProgress := func(progress float64) {
		handleProgress(progress, 0)
	}
	textOptions.OnProgress = textOnProgress
	writer := p.StreamText(textOptions)

	onDone := func() {
		writer.Close()
	}
	writer.Write(text, &onDone)

	for i, attachment := range options.Attachments {
		onProgress := func(progress float64) {
			handleProgress(progress, i+1)
		}
		p.SendFile(attachment, StreamBytesOptions{
			Topic:                 options.Topic,
			DestinationIdentities: options.DestinationIdentities,
			StreamId:              &attachedStreamIds[i],
			OnProgress:            onProgress,
			Attributes:            options.Attributes,
		})
	}

	return &writer.Info
}

// StreamBytes creates a new byte stream writer with the provided options.
func (p *LocalParticipant) StreamBytes(options StreamBytesOptions) *ByteStreamWriter {
	if options.StreamId == nil {
		streamId := uuid.New().String()
		options.StreamId = &streamId
	}

	if options.Attributes == nil {
		options.Attributes = make(map[string]string)
	}

	var totalSize *uint64
	if options.TotalSize != 0 {
		totalSize = &options.TotalSize
	}

	info := ByteStreamInfo{
		baseStreamInfo: &baseStreamInfo{
			Id:         *options.StreamId,
			MimeType:   options.MimeType,
			Topic:      options.Topic,
			Timestamp:  time.Now().UnixMilli(),
			Size:       totalSize,
			Attributes: options.Attributes,
		},
	}

	header := &livekit.DataStream_Header{
		StreamId:    info.Id,
		MimeType:    info.MimeType,
		Topic:       info.Topic,
		Timestamp:   info.Timestamp,
		TotalLength: info.Size,
		Attributes:  info.Attributes,
		ContentHeader: &livekit.DataStream_Header_ByteHeader{
			ByteHeader: &livekit.DataStream_ByteHeader{},
		},
	}

	if options.FileName != nil {
		if byteHeader, ok := header.ContentHeader.(*livekit.DataStream_Header_ByteHeader); ok {
			byteHeader.ByteHeader.Name = *options.FileName
		}
		info.Name = options.FileName
	}

	writer := newByteStreamWriter(info, header, p.engine, options.DestinationIdentities, options.OnProgress)

	p.engine.OnClose(func() {
		writer.Close()
	})

	return writer
}

// SendFile sends a file to other participants as a byte stream.
// Returns ByteStreamInfo that can be used to get metadata about the stream.
// Returns an error if the file cannot be read.
func (p *LocalParticipant) SendFile(filePath string, options StreamBytesOptions) (*ByteStreamInfo, error) {
	if options.TotalSize == 0 {
		fileInfo, err := os.Stat(filePath)
		if err != nil {
			return nil, err
		}
		options.TotalSize = uint64(fileInfo.Size())
	}

	if options.MimeType == "" {
		mimeType := mime.TypeByExtension(filepath.Ext(filePath))
		options.MimeType = mimeType
	}

	writer := p.StreamBytes(options)

	fileBytes, err := os.ReadFile(filePath)
	if err != nil {
		writer.Close()
		return nil, err
	}

	onDone := func() {
		writer.Close()
	}
	writer.Write(fileBytes, &onDone)

	return &writer.Info, nil
}

func packetTrailerFeaturesFromOpts(opts *TrackPublicationOptions) []livekit.PacketTrailerFeature {
	if opts == nil {
		return nil
	}
	var features []livekit.PacketTrailerFeature
	if opts.AttachUserTimestamp {
		features = append(features, livekit.PacketTrailerFeature_PTF_USER_TIMESTAMP)
	}
	if opts.AttachFrameId {
		features = append(features, livekit.PacketTrailerFeature_PTF_FRAME_ID)
	}
	if len(features) == 0 {
		return nil
	}
	return features
}

func (p *LocalParticipant) getPublishTransport() *PCTransport {
	publisher, ok := p.engine.Publisher()
	if ok {
		return publisher
	}

	return nil
}

func (p *LocalParticipant) SetLogger(log protoLogger.Logger) {
	p.baseParticipant.SetLogger(log.WithValues("isLocal", true))
	p.tracks.Range(func(key, value interface{}) bool {
		pub := value.(*LocalTrackPublication)
		pub.SetLogger(p.log)
		return true
	})
}
