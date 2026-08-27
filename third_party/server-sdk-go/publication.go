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
	"errors"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v4"
	"go.uber.org/atomic"
	"google.golang.org/protobuf/proto"

	"github.com/livekit/protocol/livekit"
	protoLogger "github.com/livekit/protocol/logger"
)

type TrackPublication interface {
	Name() string
	SID() string
	Source() livekit.TrackSource
	Kind() TrackKind
	MimeType() string
	IsMuted() bool
	IsSubscribed() bool
	TrackInfo() *livekit.TrackInfo
	// Track is either a webrtc.TrackLocal or webrtc.TrackRemote
	Track() Track
	updateInfo(info *livekit.TrackInfo)
}

type trackPublicationBase struct {
	kind    atomic.String
	track   Track
	sid     atomic.String
	name    atomic.String
	isMuted atomic.Bool

	lock   sync.RWMutex
	info   atomic.Value
	engine *RTCEngine
}

func (p *trackPublicationBase) Name() string {
	return p.name.Load()
}

func (p *trackPublicationBase) SID() string {
	return p.sid.Load()
}

func (p *trackPublicationBase) Kind() TrackKind {
	return TrackKind(p.kind.Load())
}

func (p *trackPublicationBase) Track() Track {
	p.lock.RLock()
	defer p.lock.RUnlock()
	return p.track
}

func (p *trackPublicationBase) MimeType() string {
	// This requires some more work.
	// TrackInfo has a top level MimeType which is not set
	// on server side till the track is published.
	// So, it is not available in the TrackPublishedResponse.
	//
	// But, if client specified SimulcastCodecs in AddTrackRequest,
	// the TrackPublishedResponse will have Codecs populated and
	// that will have the MimeType specified in AddTrackRequest.
	// Just taking the first one here which has a non-nil MimeType.
	// This is okay (for tracks published from here)
	// as of 2025-05-12, 1:30 pm Pacific as
	// Go SDK does not (yet) support simulcast codec feature.
	//
	// When simulcast codec feature is added, this struct needs
	// to be updated to handle multiple mime types
	if info, ok := p.info.Load().(*livekit.TrackInfo); ok {
		if info.MimeType != "" {
			return info.MimeType
		}

		for _, codec := range info.Codecs {
			if codec.MimeType != "" {
				return codec.MimeType
			}
		}
	}
	return ""
}

func (p *trackPublicationBase) Source() livekit.TrackSource {
	if info, ok := p.info.Load().(*livekit.TrackInfo); ok {
		return info.Source
	}
	return livekit.TrackSource_UNKNOWN
}

func (p *trackPublicationBase) IsMuted() bool {
	return p.isMuted.Load()
}

func (p *trackPublicationBase) IsSubscribed() bool {
	return p.track != nil
}

func (p *trackPublicationBase) updateInfo(info *livekit.TrackInfo) {
	p.name.Store(info.Name)
	p.sid.Store(info.Sid)
	p.isMuted.Store(info.Muted)
	switch info.Type {
	case livekit.TrackType_AUDIO:
		p.kind.Store(string(TrackKindAudio))
	case livekit.TrackType_VIDEO:
		p.kind.Store(string(TrackKindVideo))
	}
	p.info.Store(info)
}

func (p *trackPublicationBase) TrackInfo() *livekit.TrackInfo {
	if info := p.info.Load(); info != nil {
		return proto.Clone(info.(*livekit.TrackInfo)).(*livekit.TrackInfo)
	}
	return nil
}

type RemoteTrackPublication struct {
	trackPublicationBase
	participantID string
	receiver      *webrtc.RTPReceiver
	onRTCP        func(rtcp.Packet)

	disabled bool

	// preferred video dimensions to subscribe
	videoWidth  *uint32
	videoHeight *uint32
	// preferred video quality to subscribe
	videoQuality *livekit.VideoQuality
}

// TrackRemote returns the underlying webrtc.TrackRemote if available.
// Returns nil if the track is not subscribed
func (p *RemoteTrackPublication) TrackRemote() *webrtc.TrackRemote {
	p.lock.RLock()
	defer p.lock.RUnlock()
	if t, ok := p.track.(*webrtc.TrackRemote); ok {
		return t
	}
	return nil
}

// Receiver returns the RTP receiver associated with this track publication.
func (p *RemoteTrackPublication) Receiver() *webrtc.RTPReceiver {
	p.lock.RLock()
	defer p.lock.RUnlock()
	return p.receiver
}

// SetSubscribed subscribes or unsubscribes from this track.
// When subscribed, track data will be received from the server.
func (p *RemoteTrackPublication) SetSubscribed(subscribed bool) error {
	return p.engine.SendUpdateSubscription(
		&livekit.UpdateSubscription{
			Subscribe: subscribed,
			ParticipantTracks: []*livekit.ParticipantTracks{
				{
					ParticipantSid: p.participantID,
					TrackSids:      []string{p.sid.Load()},
				},
			},
		},
	)
}

// IsEnabled returns whether the track is enabled (not disabled).
// Disabled tracks will not receive media data even when subscribed.
func (p *RemoteTrackPublication) IsEnabled() bool {
	p.lock.RLock()
	defer p.lock.RUnlock()
	return !p.disabled
}

// SetEnabled enables or disables the track.
// When disabled, the track will not receive media data even when subscribed.
func (p *RemoteTrackPublication) SetEnabled(enabled bool) {
	p.lock.Lock()
	p.disabled = !enabled
	p.lock.Unlock()

	p.updateSettings()
}

// SetVideoDimensions sets the preferred video dimensions to receive.
// This is a hint to the server about what resolution to send.
func (p *RemoteTrackPublication) SetVideoDimensions(width uint32, height uint32) {
	p.lock.Lock()
	p.videoWidth = &width
	p.videoHeight = &height
	p.lock.Unlock()

	p.updateSettings()
}

// SetVideoQuality sets the preferred video quality to receive.
func (p *RemoteTrackPublication) SetVideoQuality(quality livekit.VideoQuality) error {
	if quality == livekit.VideoQuality_OFF {
		return errors.New("cannot set video quality to OFF")
	}
	p.lock.Lock()
	p.videoQuality = &quality
	p.lock.Unlock()

	p.updateSettings()
	return nil
}

// OnRTCP sets a callback to receive RTCP packets for this track.
func (p *RemoteTrackPublication) OnRTCP(cb func(rtcp.Packet)) {
	p.lock.Lock()
	p.onRTCP = cb
	p.lock.Unlock()
}

func (p *RemoteTrackPublication) updateSettings() {
	p.lock.RLock()
	settings := &livekit.UpdateTrackSettings{
		TrackSids: []string{p.SID()},
		Disabled:  p.disabled,
		// default to high
		Quality: livekit.VideoQuality_HIGH,
	}
	if p.videoWidth != nil && p.videoHeight != nil {
		settings.Width = *p.videoWidth
		settings.Height = *p.videoHeight
	}
	if p.videoQuality != nil {
		settings.Quality = *p.videoQuality
	}
	p.lock.RUnlock()

	if err := p.engine.SendUpdateTrackSettings(settings); err != nil {
		p.engine.log.Errorw("could not send track settings", err, "trackID", p.SID())
	}
}

func (p *RemoteTrackPublication) setReceiverAndTrack(r *webrtc.RTPReceiver, t *webrtc.TrackRemote) {
	p.lock.Lock()
	p.receiver = r
	p.track = t
	p.lock.Unlock()
	if r != nil {
		go p.rtcpWorker()
	}
}

func (p *RemoteTrackPublication) rtcpWorker() {
	receiver := p.Receiver()
	if receiver == nil {
		return
	}
	// read incoming rtcp packets so interceptors can handle NACKs
	for {
		packets, _, err := receiver.ReadRTCP()
		if err != nil {
			// pipe closed
			return
		}

		p.lock.RLock()
		// rtcpCB could have changed along the way
		rtcpCB := p.onRTCP
		p.lock.RUnlock()
		if rtcpCB != nil {
			for _, packet := range packets {
				rtcpCB(packet)
			}
		}
	}
}

type LocalTrackPublication struct {
	trackPublicationBase
	// set for simulcasted tracks
	simulcastTracks map[livekit.VideoQuality]*LocalTrack
	// set for backup codecs
	backupCodecTrack              TrackLocalWithCodec
	backupCodecTracksForSimulcast map[livekit.VideoQuality]*LocalTrack
	backupCodecPublished          atomic.Bool

	opts          TrackPublicationOptions
	onMuteChanged func(*LocalTrackPublication, bool)

	log protoLogger.Logger
}

func NewLocalTrackPublication(kind TrackKind, track Track, opts TrackPublicationOptions, engine *RTCEngine, log protoLogger.Logger) *LocalTrackPublication {
	pub := &LocalTrackPublication{
		trackPublicationBase: trackPublicationBase{
			track:  track,
			engine: engine,
		},
		opts: opts,
		log:  log,
	}
	pub.kind.Store(string(kind))
	pub.name.Store(opts.Name)
	return pub
}

func (p *LocalTrackPublication) PublicationOptions() TrackPublicationOptions {
	return p.opts
}

func (p *LocalTrackPublication) TrackLocal() webrtc.TrackLocal {
	p.lock.RLock()
	defer p.lock.RUnlock()
	if t, ok := p.track.(webrtc.TrackLocal); ok {
		return t
	}
	return nil
}

func (p *LocalTrackPublication) TrackLocalForSimulcast() []*LocalTrack {
	p.lock.RLock()
	defer p.lock.RUnlock()
	return slices.Collect(maps.Values(p.simulcastTracks))
}

// GetSimulcastTrack returns the simulcast track for a specific quality level.
// Returns nil if simulcast is not enabled or the quality level doesn't exist.
func (p *LocalTrackPublication) GetSimulcastTrack(quality livekit.VideoQuality) *LocalTrack {
	p.lock.RLock()
	defer p.lock.RUnlock()
	if p.simulcastTracks == nil {
		return nil
	}
	return p.simulcastTracks[quality]
}

// SetMuted mutes or unmutes the track.
// When muted, no media data will be sent to other participants.
func (p *LocalTrackPublication) SetMuted(muted bool) {
	p.setMuted(muted, false)
}

// SimulateDisconnection simulates a network disconnection for testing purposes.
// If duration is 0, the disconnection persists until manually reconnected.
func (p *LocalTrackPublication) SimulateDisconnection(duration time.Duration) {
	if track := p.track; track != nil {
		switch t := track.(type) {
		case *LocalTrack:
			t.setDisconnected(true)
			if duration != 0 {
				time.AfterFunc(duration, func() {
					t.setDisconnected(false)
				})
			}
		}
	}
}

func (p *LocalTrackPublication) setMuted(muted bool, byRemote bool) {
	if p.isMuted.Swap(muted) == muted {
		return
	}

	if !byRemote {
		_ = p.engine.SendMuteTrack(p.sid.Load(), muted)
	}
	if track := p.track; track != nil {
		switch t := track.(type) {
		case *LocalTrack:
			t.setMuted(muted)
		case interface{ GetMuteFunc() Private[MuteFunc] }:
			t.GetMuteFunc().v(muted)
		}
	}

	if p.onMuteChanged != nil {
		p.onMuteChanged(p, muted)
	}
}

func (p *LocalTrackPublication) addSimulcastTrack(st *LocalTrack) {
	p.lock.Lock()
	defer p.lock.Unlock()
	if p.simulcastTracks == nil {
		p.simulcastTracks = make(map[livekit.VideoQuality]*LocalTrack)
	}
	if st != nil {
		p.simulcastTracks[st.videoLayer.Quality] = st
	}
}

func (p *LocalTrackPublication) setBackupCodecTrack(st TrackLocalWithCodec) {
	p.lock.Lock()
	defer p.lock.Unlock()

	p.backupCodecTrack = st
}

func (p *LocalTrackPublication) setBackupCodecTracksForSimulcast(st []*LocalTrack) {
	p.lock.Lock()
	defer p.lock.Unlock()
	if p.backupCodecTracksForSimulcast == nil {
		p.backupCodecTracksForSimulcast = make(map[livekit.VideoQuality]*LocalTrack)
	}
	for _, track := range st {
		if track != nil {
			p.backupCodecTracksForSimulcast[track.videoLayer.Quality] = track
		}
	}
}

func (p *LocalTrackPublication) getBackupCodecTrack() (TrackLocalWithCodec, []*LocalTrack) {
	p.lock.RLock()
	defer p.lock.RUnlock()
	return p.backupCodecTrack, slices.Collect(maps.Values(p.backupCodecTracksForSimulcast))
}

func (p *LocalTrackPublication) setBackupCodecPublished() {
	p.backupCodecPublished.Store(true)
}

func (p *LocalTrackPublication) readRTCP(sender *webrtc.RTPSender) {
	// consume RTCP packets so interceptors can handle them (rtt, nacks...)
	go func() {
		for {
			_, _, err := sender.ReadRTCP()
			if err != nil {
				// pipe closed
				return
			}
		}
	}()
}

// CloseTrack closes the underlying track and all simulcast tracks.
// This should be called when the track is no longer needed.
func (p *LocalTrackPublication) CloseTrack() {
	var tracks []LocalTrackWithClose

	p.lock.RLock()
	if localTrack, ok := p.track.(LocalTrackWithClose); ok {
		tracks = append(tracks, localTrack)
	}

	for _, st := range p.simulcastTracks {
		tracks = append(tracks, st)
	}

	if localTrackWithClose, ok := p.backupCodecTrack.(LocalTrackWithClose); ok {
		tracks = append(tracks, localTrackWithClose)
	}

	for _, st := range p.backupCodecTracksForSimulcast {
		tracks = append(tracks, st)
	}
	p.lock.RUnlock()

	for _, track := range tracks {
		track.Close()
	}
}

// SetPublishingCodecsQuality sets the publishing codecs based on the subscribed codecs quality, return
// the track(s) to be published for backup codec that is required by subscriber but not published yet.
func (p *LocalTrackPublication) setPublishingCodecsQuality(subscribedCodecs []*livekit.SubscribedCodec) []string {
	var codecNeedPub []string
	for _, subscribedCodec := range subscribedCodecs {
		if strings.HasSuffix(strings.ToLower(p.MimeType()), subscribedCodec.Codec) {
			// primary codec
			for _, subscribedQuality := range subscribedCodec.Qualities {
				track := p.GetSimulcastTrack(subscribedQuality.Quality)
				if track != nil {
					track.setDisabled(!subscribedQuality.Enabled)
					p.log.Infow(
						"updating layer enable",
						"trackID", p.SID(),
						"quality", subscribedQuality.Quality,
						"enabled", subscribedQuality.Enabled,
						"codec", subscribedCodec.Codec,
					)
				}
			}
			continue
		}

		// backup codec
		p.lock.RLock()
		backupCodecTrack, backupCodecTracksForSimulcast := p.backupCodecTrack, p.backupCodecTracksForSimulcast
		p.lock.RUnlock()

		mainTrack := backupCodecTrack
		if len(backupCodecTracksForSimulcast) > 0 {
			iter := maps.Values(backupCodecTracksForSimulcast)
			iter(func(l *LocalTrack) bool {
				mainTrack = l
				return false
			})
		}
		if mainTrack == nil || !strings.HasSuffix(strings.ToLower(mainTrack.Codec().MimeType), subscribedCodec.Codec) {
			p.log.Warnw("subscriber requested backup codec but no track found", nil, "trackID", p.SID(), "codec", subscribedCodec.Codec)
			continue
		}

		// check if need to publish backup codec
		if !p.backupCodecPublished.Load() {
			var needPub bool
			for _, subscribedQuality := range subscribedCodec.Qualities {
				if subscribedQuality.Enabled {
					needPub = true
					break
				}
			}
			if needPub {
				codecNeedPub = append(codecNeedPub, subscribedCodec.Codec)
			}
			continue
		}

		// dynacast for backup codec simulcast
		if len(backupCodecTracksForSimulcast) == 0 {
			continue
		}

		for _, subscribedQuality := range subscribedCodec.Qualities {
			track := backupCodecTracksForSimulcast[subscribedQuality.Quality]
			if track != nil {
				track.setDisabled(!subscribedQuality.Enabled)
				p.log.Infow(
					"updating layer enable",
					"trackID", p.SID(),
					"quality", subscribedQuality.Quality,
					"enabled", subscribedQuality.Enabled,
					"codec", subscribedCodec.Codec,
				)
			}
		}
	}

	return codecNeedPub
}

func (p *LocalTrackPublication) setAudioCodecSubscribed(subscribedCodecs []*livekit.SubscribedAudioCodec) []string {
	var codecNeedPub []string
	for _, subscribedCodec := range subscribedCodecs {
		if strings.HasSuffix(strings.ToLower(p.MimeType()), subscribedCodec.Codec) {
			// primary codec
			continue
		}

		// backup codec
		p.lock.RLock()
		backupCodecTrack := p.backupCodecTrack
		p.lock.RUnlock()

		if backupCodecTrack == nil || !strings.HasSuffix(strings.ToLower(backupCodecTrack.Codec().MimeType), subscribedCodec.Codec) {
			p.log.Warnw("subscriber requested backup codec but no backup codec track found", nil, "trackID", p.SID(), "codec", subscribedCodec.Codec)
			continue
		}

		// check if need to publish backup codec
		if !p.backupCodecPublished.Load() && subscribedCodec.Enabled {
			codecNeedPub = append(codecNeedPub, subscribedCodec.Codec)
			continue
		}
	}

	return codecNeedPub
}

func (p *LocalTrackPublication) unpublish(transport *PCTransport) error {
	var tracks []webrtc.TrackLocal
	p.lock.RLock()

	if localTrack, ok := p.track.(webrtc.TrackLocal); ok {
		tracks = append(tracks, localTrack)
	}

	for _, st := range p.simulcastTracks {
		tracks = append(tracks, st)
	}

	if p.backupCodecTrack != nil {
		tracks = append(tracks, p.backupCodecTrack)
	}

	for _, st := range p.backupCodecTracksForSimulcast {
		tracks = append(tracks, st)
	}
	p.lock.RUnlock()

	for _, sender := range transport.pc.GetSenders() {
		for _, track := range tracks {
			if sender.Track() == track {
				if err := transport.pc.RemoveTrack(sender); err != nil {
					return err
				}
				break
			}
		}
	}
	return nil
}

func (p *LocalTrackPublication) SetLogger(log protoLogger.Logger) {
	p.log = log
}

type TrackPublicationOptions struct {
	Name   string
	Source livekit.TrackSource
	// Set dimensions for video
	VideoWidth  int
	VideoHeight int
	// Opus only
	DisableDTX bool
	Stereo     bool
	// which stream the track belongs to, used to group tracks together.
	// if not specified, server will infer it from track source to bundle camera/microphone, screenshare/audio together
	Stream string
	// encryption type
	Encryption        livekit.Encryption_Type
	BackupCodecPolicy livekit.BackupCodecPolicy
	// Packet trailer features signaled to the server.
	AttachUserTimestamp bool
	AttachFrameId       bool
}

type MuteFunc func(muted bool) error
