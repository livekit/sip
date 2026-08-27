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
	"time"

	"github.com/pion/webrtc/v4"

	"github.com/livekit/protocol/livekit"
	protoLogger "github.com/livekit/protocol/logger"
)

type RemoteParticipant struct {
	baseParticipant
	pliWriter PLIWriter
	engine    *RTCEngine
}

func newRemoteParticipant(pi *livekit.ParticipantInfo, roomCallback *RoomCallback, engine *RTCEngine, pliWriter PLIWriter, log protoLogger.Logger) *RemoteParticipant {
	p := &RemoteParticipant{
		baseParticipant: *newBaseParticipant(roomCallback, log.WithValues("isLocal", false)),
		engine:          engine,
		pliWriter:       pliWriter,
	}
	p.updateInfo(pi)
	return p
}

func (p *RemoteParticipant) updateInfo(pi *livekit.ParticipantInfo) {
	if !p.baseParticipant.updateInfo(pi, p) {
		// not a valid update, could be due to older version
		return
	}
	// update tracks
	validPubs := make(map[string]TrackPublication)
	newPubs := make(map[string]TrackPublication)

	for _, ti := range pi.Tracks {
		pub := p.getPublication(ti.Sid)
		if pub == nil {
			// new track
			remotePub := &RemoteTrackPublication{}
			remotePub.updateInfo(ti)
			remotePub.engine = p.engine
			remotePub.participantID = p.sid
			p.addPublication(remotePub)
			newPubs[ti.Sid] = remotePub
			pub = remotePub
		} else {
			wasMuted := pub.IsMuted()
			pub.updateInfo(ti)
			if ti.Muted != wasMuted {
				if ti.Muted {
					p.Callback.OnTrackMuted(pub, p)
					p.roomCallback.OnTrackMuted(pub, p)
				} else {
					p.Callback.OnTrackUnmuted(pub, p)
					p.roomCallback.OnTrackUnmuted(pub, p)
				}
			}
		}
		validPubs[ti.Sid] = pub
	}

	// send events for new publications
	for _, pub := range newPubs {
		p.Callback.OnTrackPublished(pub.(*RemoteTrackPublication), p)
		p.roomCallback.OnTrackPublished(pub.(*RemoteTrackPublication), p)
	}

	var toUnpublish []string
	p.tracks.Range(func(key, value interface{}) bool {
		sid := key.(string)
		if validPubs[sid] == nil {
			toUnpublish = append(toUnpublish, sid)
		}
		return true
	})
	for _, sid := range toUnpublish {
		p.unpublishTrack(sid, true)
	}
}

func (p *RemoteParticipant) addSubscribedMediaTrack(
	track *webrtc.TrackRemote,
	trackSID string,
	receiver *webrtc.RTPReceiver,
) {
	pub := p.getPublication(trackSID)
	if pub == nil {
		// wait for metadata to arrive
		go func() {
			start := time.Now()
			for time.Since(start) < 5*time.Second {
				pub := p.getPublication(trackSID)
				if pub != nil {
					p.addSubscribedMediaTrack(track, trackSID, receiver)
					return
				}
				time.Sleep(50 * time.Millisecond)
			}
			p.Callback.OnTrackSubscriptionFailed(trackSID, p)
			p.roomCallback.OnTrackSubscriptionFailed(trackSID, p)
		}()
		return
	}
	pub.setReceiverAndTrack(receiver, track)

	p.engine.log.Infow(
		"track subscribed",
		"participant", p.Identity(),
		"trackID", pub.sid.Load(),
		"kind", pub.kind.Load(),
	)
	p.Callback.OnTrackSubscribed(track, pub, p)
	p.roomCallback.OnTrackSubscribed(track, pub, p)
}

func (p *RemoteParticipant) getPublication(trackSID string) *RemoteTrackPublication {
	if pub, ok := p.baseParticipant.getPublication(trackSID).(*RemoteTrackPublication); ok {
		return pub
	}
	return nil
}

func (p *RemoteParticipant) unpublishTrack(sid string, sendUnpublish bool) {
	pub := p.getPublication(sid)
	if pub == nil {
		p.engine.log.Warnw("could not find track to unpublish", nil, "sid", sid)
		return
	}

	switch pub.Kind() {
	case TrackKindAudio:
		p.audioTracks.Delete(sid)
	case TrackKindVideo:
		p.videoTracks.Delete(sid)
	}
	p.tracks.Delete(sid)

	track := pub.TrackRemote()
	if track != nil {
		p.Callback.OnTrackUnsubscribed(track, pub, p)
		p.roomCallback.OnTrackUnsubscribed(track, pub, p)
	}
	if sendUnpublish {
		p.Callback.OnTrackUnpublished(pub, p)
		p.roomCallback.OnTrackUnpublished(pub, p)
	}
}

func (p *RemoteParticipant) WritePLI(ssrc webrtc.SSRC) {
	p.pliWriter(ssrc)
}

func (p *RemoteParticipant) unpublishAllTracks() {
	p.tracks.Range(func(_, value interface{}) bool {
		pub := value.(TrackPublication)
		if remoteTrack, ok := pub.Track().(*webrtc.TrackRemote); ok {
			if pub.Track() != nil {
				p.Callback.OnTrackUnsubscribed(remoteTrack, pub.(*RemoteTrackPublication), p)
				p.roomCallback.OnTrackUnsubscribed(remoteTrack, pub.(*RemoteTrackPublication), p)
			}
		}
		return true
	})
	p.tracks.Clear()
	p.audioTracks.Clear()
	p.videoTracks.Clear()
}

func (p *RemoteParticipant) SetLogger(logger protoLogger.Logger) {
	p.baseParticipant.SetLogger(logger.WithValues("isLocal", false))
}
