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
	"maps"
	"sync"

	"go.uber.org/atomic"
	"google.golang.org/protobuf/proto"

	"github.com/livekit/protocol/livekit"
	protoLogger "github.com/livekit/protocol/logger"
)

type Participant interface {
	SID() string
	Identity() string
	Name() string
	Kind() ParticipantKind
	IsSpeaking() bool
	AudioLevel() float32
	TrackPublications() []TrackPublication
	IsCameraEnabled() bool
	IsMicrophoneEnabled() bool
	IsScreenShareEnabled() bool
	IsScreenShareAudioEnabled() bool
	Metadata() string
	Attributes() map[string]string
	GetTrackPublication(source livekit.TrackSource) TrackPublication
	Permissions() *livekit.ParticipantPermission

	setAudioLevel(level float32)
	setIsSpeaking(speaking bool)
	setConnectionQualityInfo(info *livekit.ConnectionQualityInfo)
}

type baseParticipant struct {
	log               protoLogger.Logger
	sid               string
	identity          string
	name              string
	audioLevel        atomic.Float64
	metadata          string
	attributes        map[string]string
	isSpeaking        atomic.Bool
	info              *livekit.ParticipantInfo
	connectionQuality *livekit.ConnectionQualityInfo
	lock              sync.RWMutex

	Callback     *ParticipantCallback
	roomCallback *RoomCallback

	audioTracks *sync.Map
	videoTracks *sync.Map
	tracks      *sync.Map
}

func newBaseParticipant(roomCallback *RoomCallback, log protoLogger.Logger) *baseParticipant {
	p := &baseParticipant{
		audioTracks:  &sync.Map{},
		videoTracks:  &sync.Map{},
		tracks:       &sync.Map{},
		roomCallback: roomCallback,
		log:          log,
		Callback:     NewParticipantCallback(),
	}
	// need to initialize
	p.setAudioLevel(0)
	p.setIsSpeaking(false)
	return p
}

func (p *baseParticipant) SID() string {
	p.lock.RLock()
	defer p.lock.RUnlock()
	return p.sid
}

func (p *baseParticipant) Identity() string {
	p.lock.RLock()
	defer p.lock.RUnlock()
	return p.identity
}

func (p *baseParticipant) Name() string {
	p.lock.RLock()
	defer p.lock.RUnlock()
	return p.name
}

func (p *baseParticipant) Kind() ParticipantKind {
	p.lock.RLock()
	defer p.lock.RUnlock()
	return ParticipantKind(p.info.GetKind())
}

func (p *baseParticipant) Metadata() string {
	p.lock.RLock()
	defer p.lock.RUnlock()
	return p.metadata
}

func (p *baseParticipant) Attributes() map[string]string {
	p.lock.RLock()
	defer p.lock.RUnlock()
	return maps.Clone(p.attributes)
}

func (p *baseParticipant) Permissions() *livekit.ParticipantPermission {
	p.lock.RLock()
	defer p.lock.RUnlock()
	perm := p.info.GetPermission()
	if perm != nil {
		return proto.Clone(perm).(*livekit.ParticipantPermission)
	}
	return nil
}

func (p *baseParticipant) IsSpeaking() bool {
	return p.isSpeaking.Load()
}

func (p *baseParticipant) AudioLevel() float32 {
	return float32(p.audioLevel.Load())
}

func (p *baseParticipant) DisconnectReason() livekit.DisconnectReason {
	p.lock.RLock()
	defer p.lock.RUnlock()
	return p.info.GetDisconnectReason()
}

func (p *baseParticipant) TrackPublications() []TrackPublication {
	tracks := make([]TrackPublication, 0)
	p.tracks.Range(func(_, value interface{}) bool {
		track := value.(TrackPublication)
		tracks = append(tracks, track)
		return true
	})
	return tracks
}

func (p *baseParticipant) GetTrackPublication(source livekit.TrackSource) TrackPublication {
	var pub TrackPublication
	p.tracks.Range(func(_, value interface{}) bool {
		trackPub := value.(TrackPublication)
		if trackPub.Source() == source {
			pub = trackPub
			return false
		}
		return true
	})
	return pub
}

func (p *baseParticipant) IsCameraEnabled() bool {
	pub := p.GetTrackPublication(livekit.TrackSource_CAMERA)
	return pub != nil && !pub.IsMuted()
}

func (p *baseParticipant) IsMicrophoneEnabled() bool {
	pub := p.GetTrackPublication(livekit.TrackSource_MICROPHONE)
	return pub != nil && !pub.IsMuted()
}

func (p *baseParticipant) IsScreenShareEnabled() bool {
	pub := p.GetTrackPublication(livekit.TrackSource_SCREEN_SHARE)
	return pub != nil && !pub.IsMuted()
}

func (p *baseParticipant) IsScreenShareAudioEnabled() bool {
	pub := p.GetTrackPublication(livekit.TrackSource_SCREEN_SHARE_AUDIO)
	return pub != nil && !pub.IsMuted()
}

func (p *baseParticipant) SetLogger(l protoLogger.Logger) {
	p.log = l
}

func (p *baseParticipant) setAudioLevel(level float32) {
	p.audioLevel.Store(float64(level))
}

func (p *baseParticipant) setIsSpeaking(speaking bool) {
	if p.isSpeaking.Swap(speaking) == speaking {
		return
	}
	p.Callback.OnIsSpeakingChanged(p)
	p.roomCallback.OnIsSpeakingChanged(p)
}

func (p *baseParticipant) setConnectionQualityInfo(info *livekit.ConnectionQualityInfo) {
	p.lock.Lock()
	p.connectionQuality = info
	p.lock.Unlock()
	p.Callback.OnConnectionQualityChanged(info, p)
	p.roomCallback.OnConnectionQualityChanged(info, p)
}

func attributeChanges(old, cur map[string]string) map[string]string {
	diff := make(map[string]string)
	for k, v := range cur {
		if old[k] != v {
			diff[k] = v // added or changed
		}
	}
	for k := range old {
		if _, ok := cur[k]; !ok {
			diff[k] = "" // deleted
		}
	}
	return diff
}

func (p *baseParticipant) updateInfo(pi *livekit.ParticipantInfo, participant Participant) bool {
	p.lock.Lock()
	if p.info != nil && p.info.Version > pi.Version && p.sid == pi.Sid {
		// already updated with a later version
		p.lock.Unlock()
		return false
	}
	p.info = pi
	p.identity = pi.Identity
	p.sid = pi.Sid
	p.name = pi.Name
	oldMetadata := p.metadata
	p.metadata = pi.Metadata
	metaChanged := oldMetadata != p.metadata
	attrsChanges := attributeChanges(p.attributes, pi.Attributes)
	p.attributes = maps.Clone(pi.Attributes)
	p.lock.Unlock()

	if metaChanged {
		p.Callback.OnMetadataChanged(oldMetadata, participant)
		p.roomCallback.OnMetadataChanged(oldMetadata, participant)
	}
	if len(attrsChanges) != 0 {
		p.Callback.OnAttributesChanged(attrsChanges, participant)
		p.roomCallback.OnAttributesChanged(attrsChanges, participant)
	}
	return true
}

func (p *baseParticipant) addPublication(publication TrackPublication) {
	sid := publication.SID()
	p.tracks.Store(sid, publication)
	switch publication.Kind() {
	case TrackKindAudio:
		p.audioTracks.Store(sid, publication)
	case TrackKindVideo:
		p.videoTracks.Store(sid, publication)
	}
}

func (p *baseParticipant) getPublication(sid string) TrackPublication {
	p.lock.RLock()
	defer p.lock.RUnlock()

	track, ok := p.tracks.Load(sid)
	if !ok {
		return nil
	}
	return track.(TrackPublication)
}
